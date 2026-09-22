package client

import (
	"context"
	"errors"
	"io"
	"net"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/laat/laatmux/internal/protocol"
)

// Review finding 8: a pending Request returns when its context is cancelled.
func TestRequestHonoursCancel(t *testing.T) {
	server, client := net.Pipe()
	defer server.Close()
	c := &Conn{Host: Host{Name: "t"}, pc: protocol.NewConn(client), close: func() { client.Close() }}
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
			c := &Conn{Host: Host{Name: "cat"}, pc: protocol.NewConnRW(r, w), close: closeFn}
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
	c := &Conn{Host: Host{Name: "p"}, pc: protocol.NewConnRW(r, w), close: closeFn}
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
	c := &Conn{Host: Host{Name: "vm"}, pc: protocol.NewConn(client), close: func() { client.Close() }, diag: diag}
	diag.Write([]byte("Connection closed by remote host\n"))
	server.Close()
	_, err := c.Read()
	if err == nil || !strings.Contains(err.Error(), "ssh: Connection closed by remote host") {
		t.Fatalf("err = %v", err)
	}
	if !errors.Is(err, io.EOF) {
		t.Errorf("err = %v, want io.EOF underneath", err)
	}
	plain := &Conn{Host: Host{Name: "t"}, pc: protocol.NewConn(client), close: func() { client.Close() }}
	if _, err := plain.Read(); err == nil || err != io.EOF {
		t.Errorf("local err = %v, want bare io.EOF", err)
	}
}
