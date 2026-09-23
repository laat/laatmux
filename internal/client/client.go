// Package client connects to laatmux daemons, local or over ssh.
//
// Local: dial the address in the runtime file, starting the daemon on demand.
// Remote: `ssh -T <alias> laatmux bridge`, which on the far side does the
// local dance and pipes the socket to stdio. Same protocol on both.
package client

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/laat/laatmux/internal/home"
	"github.com/laat/laatmux/internal/protocol"
)

// Host is one environment the client talks to.
type Host struct {
	Name string // label shown in the sidebar
	SSH  string // ssh alias; "" means this machine
	Bin  string // remote laatmux binary, default "laatmux"
}

func (h Host) Local() bool { return h.SSH == "" }

// Conn is an open, hello-completed connection to one daemon.
type Conn struct {
	Host  Host
	Hello protocol.Message
	pc    *protocol.Conn
	close func()
	once  sync.Once
	diag  *tailBuffer // ssh's stderr; nil for a local connection
}

// Diag is what the transport has written to stderr so far, trimmed: for a
// remote host, ssh's own messages, which say why a connection failed or
// dropped when the protocol only sees EOF. "" for a local connection.
func (c *Conn) Diag() string {
	if c.diag == nil {
		return ""
	}
	return c.diag.String()
}

// Close tears the connection down. Safe to call concurrently and repeatedly:
// cancellation watchers and normal cleanup both call it, and the ssh
// transport must reap its process exactly once.
func (c *Conn) Close() {
	c.once.Do(func() {
		if c.close != nil {
			c.close()
		}
	})
}

// Write and Read speak the protocol. A transport error carries what ssh
// has said so far, since it is no longer passed through to stderr: a
// "Connection closed by remote host" belongs in the error every caller
// prints, not only in the ones that ask for Diag.
func (c *Conn) Write(m protocol.Message) error { return c.wrap(c.pc.Write(m)) }
func (c *Conn) Read() (protocol.Message, error) {
	m, err := c.pc.Read()
	return m, c.wrap(err)
}

// wrap adds the transport's diagnostic to err. Best effort: ssh may not
// have written its reason yet when its stdout closes; a caller that has
// closed the connection, and so reaped ssh, reads the complete text with
// Diag.
func (c *Conn) wrap(err error) error {
	if err == nil {
		return nil
	}
	if d := c.Diag(); d != "" {
		return fmt.Errorf("%w (ssh: %s)", err, d)
	}
	return err
}

// Dial connects and completes the hello exchange.
func Dial(ctx context.Context, h Host) (*Conn, error) {
	var (
		r     io.Reader
		w     io.Writer
		close func()
	)
	if h.Local() {
		nc, err := DialLocal(ctx, true)
		if err != nil {
			return nil, err
		}
		r, w, close = nc, nc, func() { nc.Close() }
	} else {
		// ServerAlive turns a silent network loss into an ssh exit within
		// about 45 s, so the client sees EOF and reconnects rather than
		// showing a connected host with frozen state.
		cmd := exec.CommandContext(ctx, "ssh", "-T",
			"-o", "BatchMode=yes",
			"-o", "ServerAliveInterval=15",
			"-o", "ServerAliveCountMax=3",
			h.SSH, RemoteBin(h.Bin)+" bridge")
		// ssh's stderr is kept rather than passed through: a client shows
		// it in the host's row, and the merging daemon puts it in the host
		// record, where the user sees it. On the terminal it would
		// interleave with the listing, or land in the daemon's log.
		diag := &tailBuffer{}
		cmd.Stderr = diag
		var err error
		r, w, close, err = startProcessTransport(cmd)
		if err != nil {
			return nil, fmt.Errorf("ssh %s: %w", h.SSH, err)
		}
		c := &Conn{Host: h, pc: protocol.NewConnRW(r, w), close: close, diag: diag}
		return completeHello(ctx, c)
	}
	return Connect(ctx, h, r, w, close)
}

// RemoteBin is the configured binary as a word for the remote login
// shell: a path under ~ is the remote home, spelled so the shell expands
// it whatever it does with quotes; anything else is quoted as one word,
// a bare name included, which the shell then finds on its PATH. The
// bridge and upgrade's install use the same word, so the binary the
// bridge runs is the one upgrade replaces.
func RemoteBin(bin string) string {
	if bin == "" {
		bin = "laatmux"
	}
	if rest, ok := strings.CutPrefix(bin, "~/"); ok {
		return `"$HOME"/` + shellQuote(rest)
	}
	return shellQuote(bin)
}

// shellQuote makes s one word for a POSIX shell; a plain word stays as
// it is.
func shellQuote(s string) string {
	if s != "" && !strings.ContainsAny(s, " \t\n'\"\\$`!*?[]{}()<>|&;#~=") {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// Connect completes the hello exchange over an open transport: r and w
// carry the protocol, close tears the transport down. It is what Dial does
// once a connection is up, exposed so a daemon under test can be dialled
// over a pipe.
func Connect(ctx context.Context, h Host, r io.Reader, w io.Writer, close func()) (*Conn, error) {
	return completeHello(ctx, &Conn{Host: h, pc: protocol.NewConnRW(r, w), close: close})
}

// tailBuffer keeps the last tailKeep bytes written to it.
type tailBuffer struct {
	mu sync.Mutex
	b  []byte
}

const tailKeep = 4096

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.b = append(t.b, p...)
	if len(t.b) > tailKeep {
		t.b = append([]byte(nil), t.b[len(t.b)-tailKeep:]...)
	}
	return len(p), nil
}

func (t *tailBuffer) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return strings.TrimSpace(string(t.b))
}

// completeHello sends the client hello and validates the daemon's reply.
// On any failure the connection is closed.
func completeHello(ctx context.Context, c *Conn) (*Conn, error) {
	h := c.Host
	if err := c.pc.Write(protocol.Message{Type: protocol.TypeHello, Protocol: protocol.Version, Client: "laatmux"}); err != nil {
		c.Close()
		return nil, err
	}
	type res struct {
		m   protocol.Message
		err error
	}
	ch := make(chan res, 1)
	go func() {
		m, err := c.pc.Read()
		ch <- res{m, err}
	}()
	select {
	case <-ctx.Done():
		c.Close()
		return nil, ctx.Err()
	case <-time.After(15 * time.Second):
		c.Close()
		return nil, fmt.Errorf("%s: timeout waiting for hello", h.Name)
	case x := <-ch:
		if x.err != nil {
			c.Close()
			// Closing reaps ssh, so its stderr is complete: a refused
			// connection or a missing remote binary is in it, and is
			// what the user needs to see rather than EOF.
			if d := c.Diag(); d != "" {
				return nil, fmt.Errorf("%s: %s", h.Name, d)
			}
			return nil, fmt.Errorf("%s: %w", h.Name, x.err)
		}
		if x.m.Type != protocol.TypeHello {
			c.Close()
			return nil, fmt.Errorf("%s: expected hello, got %q", h.Name, x.m.Type)
		}
		if x.m.Protocol != protocol.Version {
			// Within one protocol number, clients branch on capabilities and
			// daemon version strings are irrelevant. A different number means
			// the envelope itself may differ, so refuse rather than guess.
			c.Close()
			return nil, fmt.Errorf("%s: daemon %s speaks protocol %d, this client speaks %d", h.Name, x.m.Version, x.m.Protocol, protocol.Version)
		}
		c.Hello = x.m
		return c, nil
	}
}

// startProcessTransport runs cmd and returns its stdout/stdin as the
// transport. The returned close kills and reaps the process; callers wrap it
// in Conn.Close, which guarantees a single call.
func startProcessTransport(cmd *exec.Cmd) (r io.Reader, w io.Writer, close func(), err error) {
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, nil, nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, nil, nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, nil, nil, err
	}
	close = func() {
		stdin.Close()
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}
	return stdout, stdin, close, nil
}

// CloseOnDone closes the connection when ctx is done, which unblocks a
// pending Read. The returned func stops the watcher.
func (c *Conn) CloseOnDone(ctx context.Context) (stop func()) {
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			c.Close()
		case <-done:
		}
	}()
	return func() { close(done) }
}

// Request sends a command and waits for its result. Use on a connection that
// is not subscribed, so nothing interleaves. Cancelling ctx closes the
// connection and returns.
func (c *Conn) Request(ctx context.Context, m protocol.Message) (protocol.Message, error) {
	if m.ID == "" {
		m.ID = fmt.Sprintf("%d-%d", os.Getpid(), time.Now().UnixNano())
	}
	defer c.CloseOnDone(ctx)()
	if err := c.pc.Write(m); err != nil {
		return protocol.Message{}, err
	}
	for {
		r, err := c.pc.Read()
		if err != nil {
			if ctx.Err() != nil {
				return protocol.Message{}, ctx.Err()
			}
			return protocol.Message{}, err
		}
		if (r.Type == protocol.TypeResult || r.Type == protocol.TypeError) && r.ID == m.ID {
			if r.Type == protocol.TypeError {
				return r, errors.New(r.Error)
			}
			if !r.OK {
				return r, errors.New(r.Error)
			}
			return r, nil
		}
	}
}

// Snapshot subscribes and returns the first snapshot. Cancelling ctx closes
// the connection and returns.
func (c *Conn) Snapshot(ctx context.Context) (protocol.Message, error) {
	defer c.CloseOnDone(ctx)()
	if err := c.pc.Write(protocol.Message{Type: protocol.TypeSubscribe}); err != nil {
		return protocol.Message{}, err
	}
	for {
		m, err := c.pc.Read()
		if err != nil {
			if ctx.Err() != nil {
				return protocol.Message{}, ctx.Err()
			}
			return protocol.Message{}, err
		}
		if m.Type == protocol.TypeSnapshot {
			return m, nil
		}
	}
}

// DialLocal connects to this host's daemon. With start, a missing daemon is
// started and waited for.
func DialLocal(ctx context.Context, start bool) (net.Conn, error) {
	if c, err := dialRuntime(); err == nil {
		return c, nil
	}
	if !start {
		return nil, errors.New("laatmux: daemon not running")
	}
	if err := StartDaemon(ctx); err != nil {
		return nil, err
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if c, err := dialRuntime(); err == nil {
			return c, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
	return nil, errors.New("laatmux: daemon did not come up")
}

func dialRuntime() (net.Conn, error) {
	rt, err := home.ReadRuntime()
	if err != nil {
		return nil, err
	}
	network, addr, ok := strings.Cut(rt.Address, ":")
	if !ok {
		return nil, fmt.Errorf("laatmux: bad runtime address %q", rt.Address)
	}
	return net.DialTimeout(network, addr, 2*time.Second)
}

// StartDaemon launches `laatmux serve` detached from this process. The
// daemon takes its own lock, so two concurrent starts yield one daemon.
func StartDaemon(ctx context.Context) error {
	self, err := os.Executable()
	if err != nil {
		return err
	}
	args := []string{"serve"}
	if v := os.Getenv("LAATMUX_SERVE_ARGS"); v != "" {
		args = append(args, strings.Fields(v)...)
	}
	if err := os.MkdirAll(home.Dir(), 0o700); err != nil {
		return err
	}
	logf, err := os.OpenFile(home.Dir()+"/daemon.log", os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer logf.Close()
	cmd := exec.Command(self, args...)
	cmd.Stdin = nil
	cmd.Stdout = logf
	cmd.Stderr = logf
	cmd.Env = os.Environ()
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start daemon: %w", err)
	}
	// Do not wait; the daemon outlives us by design.
	go cmd.Wait() //nolint:errcheck
	return nil
}

// Bridge connects stdio to the local daemon. This is what runs on the far
// side of ssh.
func Bridge(ctx context.Context, r io.Reader, w io.Writer) error {
	nc, err := DialLocal(ctx, true)
	if err != nil {
		return err
	}
	defer nc.Close()
	errc := make(chan error, 2)
	go func() { _, err := io.Copy(nc, r); errc <- err }()
	go func() { _, err := io.Copy(w, nc); errc <- err }()
	select {
	case <-ctx.Done():
		return nil
	case err := <-errc:
		if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, net.ErrClosed) {
			return err
		}
		return nil
	}
}

// Stream sends a command and follows its progress until the result, calling
// onProgress for each progress message with the command's id. A transport
// failure before the result closes the connection and returns it; the
// caller may dial again and send the same id, which the daemon answers by
// replaying what it already sent and following. A result with ok false is
// returned with its error, as Request does.
func (c *Conn) Stream(ctx context.Context, m protocol.Message, onProgress func(protocol.Message)) (protocol.Message, error) {
	defer c.CloseOnDone(ctx)()
	if err := c.pc.Write(m); err != nil {
		return protocol.Message{}, err
	}
	for {
		r, err := c.pc.Read()
		if err != nil {
			if ctx.Err() != nil {
				return protocol.Message{}, ctx.Err()
			}
			return protocol.Message{}, err
		}
		if r.ID != m.ID {
			continue
		}
		switch r.Type {
		case protocol.TypeProgress:
			if onProgress != nil {
				onProgress(r)
			}
		case protocol.TypeError:
			return r, errors.New(r.Error)
		case protocol.TypeResult:
			if !r.OK {
				return r, errors.New(r.Error)
			}
			return r, nil
		}
	}
}
