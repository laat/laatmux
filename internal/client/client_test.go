package client

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/laat/laatmux/internal/peer"
	"github.com/laat/laatmux/internal/protocol"
)

// A state directory that cannot be made, or whose daemon.log cannot be
// opened, fails the start with os's error, the path in it as
// tmux.Printable shows it: the dashboard prints it after "local
// daemon:". Neither gets as far as starting a daemon.
func TestStartDaemonStateDirQuoted(t *testing.T) {
	base := t.TempDir()
	file := filepath.Join(base, "fi\tle\x1b[31m")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(base, "st\tate\x1b[31m")
	if err := os.MkdirAll(filepath.Join(dir, "daemon.log"), 0o700); err != nil {
		t.Fatal(err)
	}
	for home, want := range map[string]string{
		filepath.Join(file, "state"): "mkdir " + strconv.Quote(file) + ": ",
		dir:                          "open " + strconv.Quote(filepath.Join(dir, "daemon.log")) + ": ",
	} {
		t.Setenv("LAATMUX_HOME", home)
		if err := StartDaemon(context.Background()); err == nil || !strings.Contains(err.Error(), want) || strings.ContainsAny(err.Error(), "\t\x1b") {
			t.Errorf("state directory %q: %v, want %s", home, err, want)
		}
	}
}

// A local connection's errors name the daemon's socket, under the
// state directory, as tmux.Printable shows it: the hello's write and a
// Conn's write to a daemon that has closed the connection; the hello's
// read and Snapshot's write and read, on a transport that gives the
// errors a reset connection does. The directory is under /tmp, as the
// other socket tests' are: a t.TempDir on macOS makes a socket path
// too long.
func TestLocalConnErrorsQuoted(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "st\tate\x1b[31m")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "d.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	closed := func() net.Conn {
		t.Helper()
		nc, err := net.Dial("unix", sock)
		if err != nil {
			t.Fatal(err)
		}
		sc, err := ln.Accept()
		if err != nil {
			t.Fatal(err)
		}
		sc.Close()
		return nc
	}
	check := func(what string, err error) {
		t.Helper()
		if err == nil || !strings.Contains(err.Error(), strconv.Quote(sock)) || strings.ContainsAny(err.Error(), "\t\x1b") {
			t.Errorf("%s: %v, want %s in it", what, err, strconv.Quote(sock))
		}
	}
	nc := closed()
	_, err = Connect(context.Background(), peer.Host{Name: "local"}, nc, nc, func() { nc.Close() })
	check("hello", err)
	nc = closed()
	defer nc.Close()
	c := &Conn{Host: peer.Host{Name: "local"}, pc: protocol.NewConn(nc), close: func() { nc.Close() }}
	check("write", c.Write(protocol.Message{Type: protocol.TypePing}))
	reset := func(op string) *net.OpError {
		return &net.OpError{Op: op, Net: "unix", Source: &net.UnixAddr{Net: "unix"}, Addr: &net.UnixAddr{Name: sock, Net: "unix"}, Err: syscall.ECONNRESET}
	}
	failing := failingRW{read: reset("read"), write: reset("write")}
	_, err = Connect(context.Background(), peer.Host{Name: "local"}, failing, io.Discard, func() {})
	check("hello read", err)
	c = &Conn{Host: peer.Host{Name: "local"}, pc: protocol.NewConnRW(failing, failing), close: func() {}}
	_, err = c.Snapshot(context.Background())
	check("snapshot write", err)
	c = &Conn{Host: peer.Host{Name: "local"}, pc: protocol.NewConnRW(failing, io.Discard), close: func() {}}
	_, err = c.Snapshot(context.Background())
	check("snapshot read", err)
}

// failingRW's reads and writes fail with its errors.
type failingRW struct{ read, write error }

func (f failingRW) Read([]byte) (int, error)  { return 0, f.read }
func (f failingRW) Write([]byte) (int, error) { return 0, f.write }

// The bridge's error from a daemon that has closed the connection
// names its socket as tmux.Printable shows it. The daemon sends a byte
// and closes; the output's copy is held writing that byte, so the
// input's copy, writing to the closed connection, fails first. The
// test dials the socket itself, so a dial that fails cannot reach
// StartDaemon.
func TestBridgeErrorQuoted(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "st\tate\x1b[31m")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "d.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	nc, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	closed := make(chan struct{})
	go func() {
		defer close(closed)
		sc, err := ln.Accept()
		if err != nil {
			return
		}
		sc.Write([]byte("x"))
		sc.Close()
	}()
	out := &heldWriter{release: make(chan struct{})}
	defer close(out.release)
	in := readerFunc(func(p []byte) (int, error) {
		<-closed
		return copy(p, "y\n"), nil
	})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err = bridge(ctx, nc, in, out)
	if err == nil || !strings.Contains(err.Error(), strconv.Quote(sock)) || strings.ContainsAny(err.Error(), "\t\x1b") {
		t.Fatalf("bridge: %v, want %s in it", err, strconv.Quote(sock))
	}
}

// heldWriter's writes wait for release.
type heldWriter struct{ release chan struct{} }

func (w *heldWriter) Write(p []byte) (int, error) {
	<-w.release
	return len(p), nil
}

type readerFunc func([]byte) (int, error)

func (f readerFunc) Read(p []byte) (int, error) { return f(p) }

// A pending Request returns when its context is cancelled.
func TestRequestHonoursCancel(t *testing.T) {
	server, client := net.Pipe()
	defer server.Close()
	c := &Conn{Host: peer.Host{Name: "t"}, pc: protocol.NewConn(client), close: func() { client.Close() }}
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		protocol.NewConn(server).Read() // accept the request, never answer
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()
	done := make(chan error, 1)
	go func() {
		_, err := c.Request(ctx, protocol.Message{Type: protocol.TypeNew, Name: "x"})
		done <- err
	}()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Request ignored cancellation")
	}
}

// Cancellation and ordinary cleanup both close a
// subprocess-backed connection. Under -race this must not double-reap.
// `cat` echoes our hello back, which completes the handshake; it then echoes
// the request, which is never a result, so Request waits until cancelled.
func TestProcessTransportCancelAndCloseRace(t *testing.T) {
	for _, mode := range []string{"request", "snapshot"} {
		t.Run(mode, func(t *testing.T) {
			r, w, closeFn, err := startProcessTransport(exec.Command("cat"))
			if err != nil {
				t.Fatal(err)
			}
			c := &Conn{Host: peer.Host{Name: "cat"}, pc: protocol.NewConnRW(r, w), close: closeFn}
			defer c.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
			defer cancel()
			var rerr error
			switch mode {
			case "request":
				_, rerr = c.Request(ctx, protocol.Message{Type: protocol.TypeNew, Name: "x"})
			case "snapshot":
				_, rerr = c.Snapshot(ctx)
			}
			if !errors.Is(rerr, context.DeadlineExceeded) {
				t.Fatalf("err = %v", rerr)
			}
			c.Close()
			c.Close()
		})
	}
}

// A daemon speaking another protocol number is refused, not tolerated.
func TestProtocolMismatchRefused(t *testing.T) {
	r, w, closeFn, err := startProcessTransport(exec.Command("sh", "-c",
		`read line; printf '{"type":"hello","protocol":99,"version":"9.9"}\n'; sleep 5`))
	if err != nil {
		t.Fatal(err)
	}
	c := &Conn{Host: peer.Host{Name: "p"}, pc: protocol.NewConnRW(r, w), close: closeFn}
	defer c.Close()
	_, err = completeHello(context.Background(), c)
	if err == nil || !strings.Contains(err.Error(), "protocol 99") {
		t.Fatalf("err = %v", err)
	}
}

// A transport error carries what ssh wrote to stderr, and still matches
// the underlying error.
func TestErrorsCarrySSHDiagnostic(t *testing.T) {
	server, client := net.Pipe()
	diag := &tailBuffer{}
	c := &Conn{Host: peer.Host{Name: "vm"}, pc: protocol.NewConn(client), close: func() { client.Close() }, diag: diag}
	diag.Write([]byte("Connection closed by remote host\n"))
	server.Close()
	_, err := c.Read()
	if err == nil || !strings.Contains(err.Error(), "ssh: Connection closed by remote host") {
		t.Fatalf("err = %v", err)
	}
	if !errors.Is(err, io.EOF) {
		t.Errorf("err = %v, want io.EOF underneath", err)
	}
	plain := &Conn{Host: peer.Host{Name: "t"}, pc: protocol.NewConn(client), close: func() { client.Close() }}
	if _, err := plain.Read(); err == nil || err != io.EOF {
		t.Errorf("local err = %v, want bare io.EOF", err)
	}
}

// The remote binary is one shell word: a path under ~ is the remote
// home, spelled so the shell expands it; anything else is quoted, so a
// space or a shell character in the path is the path.
func TestRemoteBin(t *testing.T) {
	cases := map[string]string{
		"":                     "laatmux",
		"laatmux":              "laatmux",
		"~/.local/bin/laatmux": `"$HOME"/.local/bin/laatmux`,
		"~/my bin/laatmux":     `"$HOME"/'my bin/laatmux'`,
		"/opt/lm/laatmux":      "/opt/lm/laatmux",
		"/opt/my bin/laatmux":  `'/opt/my bin/laatmux'`,
		"/opt/it's/laatmux":    `'/opt/it'\''s/laatmux'`,
		"$HOME/bin/laatmux":    `'$HOME/bin/laatmux'`,
		"/opt/a=b/laatmux":     `'/opt/a=b/laatmux'`,
		"/opt/a^b/laatmux":     `'/opt/a^b/laatmux'`,
		"laat mux":             `'laat mux'`,
		"+x/laatmux":           `'+x/laatmux'`,
		"+it's":                `'+it'\''s'`,
		"~/+x/laatmux":         `"$HOME"/+x/laatmux`,
	}
	for in, want := range cases {
		if got := RemoteBin(in); got != want {
			t.Errorf("%q: got %s want %s", in, got, want)
		}
	}
}

// The bridge's command, run as sshd runs it, $SHELL -c with the line,
// runs a binary whose path starts with +, which unquoted the shell
// would read as its own options. The binary is echo under that path.
func TestRemoteBinPlusRuns(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "+x"), 0o755); err != nil {
		t.Fatal(err)
	}
	echo, err := exec.LookPath("echo")
	if err != nil {
		t.Skip(err)
	}
	if err := os.Symlink(echo, filepath.Join(dir, "+x", "laatmux")); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("sh", "-c", RemoteBin("+x/laatmux")+" bridge")
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil || string(out) != "bridge\n" {
		t.Errorf("%v: %q", err, out)
	}
}

// Exchange passes the request's progress on, passes over other ids,
// and returns a refusal as the message it was, which Refused turns
// into the error Request gives; the transport's diagnostic reaches
// Request's error as it does any read's.
func TestExchangeAndRefused(t *testing.T) {
	server, client := net.Pipe()
	defer server.Close()
	c := &Conn{Host: peer.Host{Name: "t"}, pc: protocol.NewConn(client), close: func() { client.Close() }}
	go func() {
		sc := protocol.NewConn(server)
		req, _ := sc.Read()
		sc.Write(protocol.Message{Type: protocol.TypeProgress, ID: "other", Detail: "not ours"})
		sc.Write(protocol.Message{Type: protocol.TypeProgress, ID: req.ID, N: 1, Detail: "one"})
		sc.Write(protocol.Message{Type: protocol.TypeResult, ID: "other", OK: true})
		sc.Write(protocol.Message{Type: protocol.TypeResult, ID: req.ID, OK: false, Error: "refused", Stage: "worktree"})
	}()
	var progress []string
	res, err := c.Exchange(context.Background(), protocol.Message{Type: protocol.TypeAdd, ID: "r1"}, func(m protocol.Message) { progress = append(progress, m.Detail) })
	if err != nil || res.Type != protocol.TypeResult || res.OK || res.Error != "refused" || res.Stage != "worktree" {
		t.Fatalf("exchange: %+v %v", res, err)
	}
	if strings.Join(progress, ",") != "one" {
		t.Fatalf("progress %v", progress)
	}
	if res, err := Refused(res, nil); err == nil || err.Error() != "refused" || res.Stage != "worktree" {
		t.Fatalf("refused: %+v %v", res, err)
	}
	if _, err := Refused(protocol.Message{Type: protocol.TypeError, Error: "bad request"}, nil); err == nil || err.Error() != "bad request" {
		t.Fatalf("error message: %v", err)
	}
	if res, err := Refused(protocol.Message{Type: protocol.TypeResult, OK: true, Root: "/r"}, nil); err != nil || res.Root != "/r" {
		t.Fatalf("ok: %+v %v", res, err)
	}
	if _, err := Refused(protocol.Message{}, io.EOF); err != io.EOF {
		t.Fatalf("transport: %v", err)
	}
	// An error message is a value too, and progress for the id with no
	// callback is passed over.
	go func() {
		sc := protocol.NewConn(server)
		req, _ := sc.Read()
		sc.Write(protocol.Message{Type: protocol.TypeProgress, ID: req.ID, N: 1, Detail: "one"})
		sc.Write(protocol.Message{Type: protocol.TypeError, ID: req.ID, Error: "bad request"})
	}()
	res, err = c.Exchange(context.Background(), protocol.Message{Type: protocol.TypeAdd, ID: "r2"}, nil)
	if err != nil || res.Type != protocol.TypeError || res.Error != "bad request" {
		t.Fatalf("error message: %+v %v", res, err)
	}

	// Request on a connection the other side closes: the diagnostic
	// is in the error.
	server2, client2 := net.Pipe()
	diag := &tailBuffer{}
	c2 := &Conn{Host: peer.Host{Name: "vm"}, pc: protocol.NewConn(client2), close: func() { client2.Close() }, diag: diag}
	diag.Write([]byte("Connection closed by remote host\n"))
	go func() {
		protocol.NewConn(server2).Read()
		server2.Close()
	}()
	if _, err := c2.Request(context.Background(), protocol.Message{Type: protocol.TypeAdd}); err == nil || !strings.Contains(err.Error(), "ssh: Connection closed by remote host") {
		t.Fatalf("request err = %v", err)
	}
}

// Snapshot subscribes asking for the main checkouts' records: rm, path
// and jump against a host the local daemon does not follow see them.
func TestSnapshotAsksForCheckouts(t *testing.T) {
	server, client := net.Pipe()
	defer server.Close()
	c := &Conn{Host: peer.Host{Name: "t"}, pc: protocol.NewConn(client), close: func() { client.Close() }}
	sub := make(chan protocol.Message, 1)
	go func() {
		sc := protocol.NewConn(server)
		m, _ := sc.Read()
		sub <- m
		sc.Write(protocol.Message{Type: protocol.TypeSnapshot})
	}()
	if _, err := c.Snapshot(context.Background()); err != nil {
		t.Fatal(err)
	}
	if m := <-sub; m.Type != protocol.TypeSubscribe || !m.Checkouts || m.Merged {
		t.Fatalf("subscribe %+v", m)
	}
}

// SSH's argv for each kind of command laatmux runs over ssh, as the
// sites wrote them by hand before: the options in a fixed order, --, the
// alias, the command as one argument.
func TestSSHArgv(t *testing.T) {
	cases := []struct {
		o    SSHOptions
		want []string
	}{
		{bridgeSSH, []string{"ssh", "-T", "-o", "BatchMode=yes", "-o", "ServerAliveInterval=15", "-o", "ServerAliveCountMax=3", "--", "vm", "laatmux bridge"}},
		{SSHOptions{ConnectTimeout: 10 * time.Second, KeepAlive: 5 * time.Second, KeepAliveCount: 2}, []string{"ssh", "-T", "-o", "BatchMode=yes", "-o", "ConnectTimeout=10", "-o", "ServerAliveInterval=5", "-o", "ServerAliveCountMax=2", "--", "vm", "laatmux bridge"}},
		{SSHOptions{ConnectTimeout: 15 * time.Second}, []string{"ssh", "-T", "-o", "BatchMode=yes", "-o", "ConnectTimeout=15", "--", "vm", "laatmux bridge"}},
		{SSHOptions{TTY: true, KeepAlive: 15 * time.Second, KeepAliveCount: 3}, []string{"ssh", "-t", "-o", "ServerAliveInterval=15", "-o", "ServerAliveCountMax=3", "--", "vm", "laatmux bridge"}},
		{SSHOptions{TTY: true}, []string{"ssh", "-t", "--", "vm", "laatmux bridge"}},
		// A keepalive without a count leaves ssh's count; fractions of
		// a second round up rather than to 0, which would mean none.
		{SSHOptions{KeepAlive: 15 * time.Second}, []string{"ssh", "-T", "-o", "BatchMode=yes", "-o", "ServerAliveInterval=15", "--", "vm", "laatmux bridge"}},
		{SSHOptions{ConnectTimeout: 500 * time.Millisecond, KeepAlive: 1500 * time.Millisecond, KeepAliveCount: 1}, []string{"ssh", "-T", "-o", "BatchMode=yes", "-o", "ConnectTimeout=1", "-o", "ServerAliveInterval=2", "-o", "ServerAliveCountMax=1", "--", "vm", "laatmux bridge"}},
	}
	for _, c := range cases {
		if got := SSH("vm", c.o, "laatmux bridge"); !slices.Equal(got, c.want) {
			t.Errorf("SSH(%+v):\n got %q\nwant %q", c.o, got, c.want)
		}
	}
	// An alias or a command that starts with - comes after the --, where
	// ssh reads it as the destination or the command, not as an option.
	for _, c := range []struct {
		alias, command string
		want           []string
	}{
		{"-oProxyCommand=true", "laatmux bridge", []string{"ssh", "-t", "--", "-oProxyCommand=true", "laatmux bridge"}},
		{"u@vm", "-oPort=2222", []string{"ssh", "-t", "--", "u@vm", "-oPort=2222"}},
	} {
		if got := SSH(c.alias, SSHOptions{TTY: true}, c.command); !slices.Equal(got, c.want) {
			t.Errorf("SSH(%q, %q):\n got %q\nwant %q", c.alias, c.command, got, c.want)
		}
	}
}

// ssh itself reads SSH's argv as meant: the options before the --, the
// alias after it as the destination, a user@ included, and the command
// not as an option, as ssh would read a word that starts with - after
// the alias without the --. ssh -G prints the configuration it would
// connect with and exits without connecting; -F /dev/null leaves the
// user's and the system's ssh config out. Where ssh cannot print a
// configuration at all, an ssh without -G or a uid with no passwd entry,
// the test is skipped with ssh's reason.
func TestSSHArgvAsSSHReadsIt(t *testing.T) {
	if out, err := exec.Command("ssh", "-G", "-F", "/dev/null", "probe.invalid").CombinedOutput(); err != nil {
		t.Skipf("ssh -G does not run here: %v %s", err, out)
	}
	for _, c := range []struct {
		alias string
		o     SSHOptions
		want  []string
	}{
		{"box.invalid", bridgeSSH, []string{"hostname box.invalid", "batchmode yes", "requesttty false", "serveraliveinterval 15", "serveralivecountmax 3", "port 22"}},
		{"u@box.invalid", SSHOptions{TTY: true, ConnectTimeout: 10 * time.Second}, []string{"user u", "hostname box.invalid", "batchmode no", "requesttty true", "connecttimeout 10", "port 22"}},
	} {
		argv := SSH(c.alias, c.o, "-oPort=2222")
		var stderr strings.Builder
		cmd := exec.Command(argv[0], append([]string{"-G", "-F", "/dev/null"}, argv[1:]...)...)
		cmd.Stderr = &stderr
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("%q: %v %s", argv, err, stderr.String())
		}
		lines := strings.Split(string(out), "\n")
		for _, w := range c.want {
			if !slices.Contains(lines, w) {
				t.Errorf("%q: ssh -G has no %q", argv, w)
			}
		}
	}
}
