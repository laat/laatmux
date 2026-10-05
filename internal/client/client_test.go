package client

import (
	"context"
	"errors"
	"io"
	"net"
	"os/exec"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/laat/laatmux/internal/peer"
	"github.com/laat/laatmux/internal/protocol"
)

// Review finding 8: a pending Request returns when its context is cancelled.
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

// Review 2 finding 4: cancellation and ordinary cleanup both close a
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
		"laat mux":             `'laat mux'`,
	}
	for in, want := range cases {
		if got := RemoteBin(in); got != want {
			t.Errorf("%q: got %s want %s", in, got, want)
		}
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

// SSH's argv for each kind of command laatmux runs over ssh, as the
// sites wrote them by hand before: the options in a fixed order, the
// alias, the command as one argument.
func TestSSHArgv(t *testing.T) {
	cases := []struct {
		o    SSHOptions
		want []string
	}{
		{bridgeSSH, []string{"ssh", "-T", "-o", "BatchMode=yes", "-o", "ServerAliveInterval=15", "-o", "ServerAliveCountMax=3", "vm", "laatmux bridge"}},
		{SSHOptions{ConnectTimeout: 10 * time.Second, KeepAlive: 5 * time.Second, KeepAliveCount: 2}, []string{"ssh", "-T", "-o", "BatchMode=yes", "-o", "ConnectTimeout=10", "-o", "ServerAliveInterval=5", "-o", "ServerAliveCountMax=2", "vm", "laatmux bridge"}},
		{SSHOptions{ConnectTimeout: 15 * time.Second}, []string{"ssh", "-T", "-o", "BatchMode=yes", "-o", "ConnectTimeout=15", "vm", "laatmux bridge"}},
		{SSHOptions{TTY: true, KeepAlive: 15 * time.Second, KeepAliveCount: 3}, []string{"ssh", "-t", "-o", "ServerAliveInterval=15", "-o", "ServerAliveCountMax=3", "vm", "laatmux bridge"}},
		{SSHOptions{TTY: true}, []string{"ssh", "-t", "vm", "laatmux bridge"}},
		// A keepalive without a count leaves ssh's count; fractions of
		// a second round up rather than to 0, which would mean none.
		{SSHOptions{KeepAlive: 15 * time.Second}, []string{"ssh", "-T", "-o", "BatchMode=yes", "-o", "ServerAliveInterval=15", "vm", "laatmux bridge"}},
		{SSHOptions{ConnectTimeout: 500 * time.Millisecond, KeepAlive: 1500 * time.Millisecond, KeepAliveCount: 1}, []string{"ssh", "-T", "-o", "BatchMode=yes", "-o", "ConnectTimeout=1", "-o", "ServerAliveInterval=2", "-o", "ServerAliveCountMax=1", "vm", "laatmux bridge"}},
	}
	for _, c := range cases {
		if got := SSH("vm", c.o, "laatmux bridge"); !slices.Equal(got, c.want) {
			t.Errorf("SSH(%+v):\n got %q\nwant %q", c.o, got, c.want)
		}
	}
}
