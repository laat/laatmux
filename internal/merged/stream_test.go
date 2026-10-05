package merged

import (
	"context"
	"net"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/laat/laatmux/internal/home"
	"github.com/laat/laatmux/internal/protocol"
)

// fakeDaemon stands in for the local daemon: a loopback listener the
// runtime file under a scratch LAATMUX_HOME points at, answering the
// hello with the merged capability and every subscribe through serve.
// Dial finds it as it would the real one, and never starts one.
func fakeDaemon(t *testing.T, serve func(pc *protocol.Conn) bool) func() int {
	t.Helper()
	t.Setenv("LAATMUX_HOME", t.TempDir())
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	if err := home.WriteRuntime(home.Runtime{Address: "tcp:" + ln.Addr().String(), PID: os.Getpid(), Version: "fake", EnvironmentID: "lenv"}); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	conns := 0
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			conns++
			mu.Unlock()
			go func() {
				defer c.Close()
				pc := protocol.NewConn(c)
				pc.Write(protocol.Message{Type: protocol.TypeHello, Protocol: protocol.Version, EnvironmentID: "lenv", Version: "fake", Capabilities: []string{protocol.CapStatus, protocol.CapMerged}})
				for {
					m, err := pc.Read()
					if err != nil || m.Type == protocol.TypeSubscribe && !serve(pc) {
						return
					}
				}
			}()
		}
	}()
	return func() int {
		mu.Lock()
		defer mu.Unlock()
		return conns
	}
}

// Read applies the stream until ready holds of the hosts waited on:
// with a snapshot but a host that never lists, the wait ends with the
// host still waited on and no error; a daemon that answers the hello but never sends
// the snapshot is a timeout, not an empty listing.
func TestRead(t *testing.T) {
	fakeDaemon(t, func(pc *protocol.Conn) bool {
		pc.Write(protocol.Message{Type: protocol.TypeSnapshot, Seq: 1, Hosts: []protocol.HostStatus{{Name: "vm", SSH: "vm"}, {Name: "mac", EnvironmentID: "lenv", Connected: true, Listed: true}}})
		return true
	})
	c, ok := Dial(context.Background())
	if !ok {
		t.Fatal("Dial failed")
	}
	defer c.Close()
	m := New()
	var seen [][]string
	waiting, err := m.Read(context.Background(), c, 200*time.Millisecond, func(waiting []string) bool {
		seen = append(seen, waiting)
		return len(waiting) == 0
	})
	if err != nil || len(waiting) != 1 || waiting[0] != "vm" || len(seen) != 1 || len(seen[0]) != 1 {
		t.Fatalf("waiting = %v, err = %v, ready saw %v", waiting, err, seen)
	}
	if !m.Status("").Loaded {
		t.Error("the snapshot was not applied")
	}
	// Ready at once: the read stops on the snapshot with the host still
	// waited on, well before the wait is over.
	c2, ok := Dial(context.Background())
	if !ok {
		t.Fatal("Dial failed")
	}
	defer c2.Close()
	start := time.Now()
	if waiting, err := New().Read(context.Background(), c2, 5*time.Second, func([]string) bool { return true }); err != nil || len(waiting) != 1 {
		t.Errorf("ready at once: waiting = %v, err = %v", waiting, err)
	}
	if time.Since(start) > time.Second {
		t.Error("ready at once waited for the timeout")
	}

	fakeDaemon(t, func(pc *protocol.Conn) bool { return true })
	c3, ok := Dial(context.Background())
	if !ok {
		t.Fatal("Dial failed")
	}
	defer c3.Close()
	if _, err := New().Read(context.Background(), c3, 200*time.Millisecond, func([]string) bool { return false }); err == nil || err.Error() != "local daemon: no snapshot after 200ms" {
		t.Fatalf("err = %v", err)
	}
}

// Follow backs off after a subscription the daemon drops, not only
// after a failed dial, marks the daemon row while reconnecting, and
// stops when its context ends.
func TestFollowBacksOff(t *testing.T) {
	followBackoffMin = 300 * time.Millisecond
	defer func() { followBackoffMin = time.Second }()
	connections := fakeDaemon(t, func(pc *protocol.Conn) bool { return false }) // hang up on subscribe
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	m := New()
	done := make(chan struct{})
	go func() {
		m.Follow(ctx, nil)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Follow did not stop with its context")
	}
	if n := connections(); n < 1 || n > 3 {
		t.Errorf("%d connections in 500 ms with a 300 ms backoff", n)
	}
	if s := m.Status(""); s.DaemonErr != "disconnected; reconnecting" {
		t.Errorf("daemon row while reconnecting: %q", s.DaemonErr)
	}
}
