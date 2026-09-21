package daemon

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/laat/laatmux/internal/detect"
	"github.com/laat/laatmux/internal/procs"
	"github.com/laat/laatmux/internal/protocol"
)

func newTestDaemon() *Daemon {
	return New(Config{EnvironmentID: "env", Version: "test"})
}

// Review finding 3: a subscriber that falls behind must be disconnected, not
// silently frozen, so it reconnects and gets a fresh snapshot.
func TestOverflowClosesTransport(t *testing.T) {
	d := newTestDaemon()
	d.discoveredOnce.Do(func() { close(d.discovered) })
	server, client := net.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go d.HandleConn(ctx, server, func() { server.Close() })

	pc := protocol.NewConn(client)
	if _, err := pc.Read(); err != nil { // hello
		t.Fatal(err)
	}
	if err := pc.Write(protocol.Message{Type: protocol.TypeSubscribe}); err != nil {
		t.Fatal(err)
	}
	if m, err := pc.Read(); err != nil || m.Type != protocol.TypeSnapshot {
		t.Fatalf("snapshot: %+v %v", m, err)
	}
	// Overflow without reading. net.Pipe is unbuffered, so the writer
	// goroutine blocks on the first message and the channel fills.
	d.mu.Lock()
	for i := 0; i < subscriberBuffer+8; i++ {
		d.seq++
		d.broadcastLocked(protocol.Message{Type: protocol.TypeUpsert, Seq: d.seq, Agent: &protocol.Agent{ID: "x"}})
	}
	d.mu.Unlock()
	// Drain until EOF; it must arrive.
	client.SetReadDeadline(time.Now().Add(3 * time.Second))
	for {
		if _, err := pc.Read(); err != nil {
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				t.Fatal("subscriber overflow did not close the connection")
			}
			return
		}
	}
}

// Review finding 6: a subscribe before the first poll must wait for it.
func TestSubscribeWaitsForDiscovery(t *testing.T) {
	d := newTestDaemon()
	server, client := net.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go d.HandleConn(ctx, server, func() { server.Close() })
	pc := protocol.NewConn(client)
	pc.Read() // hello
	pc.Write(protocol.Message{Type: protocol.TypeSubscribe})

	got := make(chan protocol.Message, 1)
	go func() {
		m, _ := pc.Read()
		got <- m
	}()
	select {
	case m := <-got:
		t.Fatalf("snapshot before discovery: %+v", m)
	case <-time.After(200 * time.Millisecond):
	}
	d.mu.Lock()
	d.agents["%1"] = protocol.Agent{ID: "env/%1"}
	d.mu.Unlock()
	d.discoveredOnce.Do(func() { close(d.discovered) })
	select {
	case m := <-got:
		if m.Type != protocol.TypeSnapshot || len(m.Agents) != 1 {
			t.Fatalf("got %+v", m)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no snapshot after discovery")
	}
}

// Review finding 2: when the agent exits, its record stays with liveness
// gone and its last activity, rather than being replaced by the shell.
func TestGoneAgentKeepsActivity(t *testing.T) {
	d := newTestDaemon()
	st := &paneState{
		identity:    procs.Identity{Agent: "claude", PID: 4242, Start: time.Unix(1, 0)},
		hasIdentity: true,
		activity:    protocol.Working,
	}
	st.gone = true
	if got := d.nextActivity(st, detectUnknown(), time.Now()); got != protocol.Working {
		t.Fatalf("activity after exit = %s, want working retained", got)
	}
}

func detectUnknown() detect.Result {
	return detect.Result{State: detect.Unknown, Reason: "no_known_agent"}
}
