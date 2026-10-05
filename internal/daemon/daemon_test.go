package daemon

import (
	"context"
	"net"
	"os"
	"strings"
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
		d.broadcastLocked(protocol.Message{Type: protocol.TypeUpsert, Agent: &protocol.Agent{ID: "x"}})
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
	d.agents["laatmux/%1"] = protocol.Agent{ID: "env/laatmux/%1"}
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

// shutdown ends the daemon as SIGTERM does, answered first; a daemon
// without the hook refuses and does not advertise it.
func TestShutdownMessage(t *testing.T) {
	called := make(chan struct{}, 1)
	d := New(Config{EnvironmentID: "env", Shutdown: func() { called <- struct{}{} }})
	server, client := net.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go d.HandleConn(ctx, server, func() { server.Close() })
	pc := protocol.NewConn(client)
	client.SetDeadline(time.Now().Add(5 * time.Second))
	if m, _ := pc.Read(); !protocol.Has(m.Capabilities, protocol.CapShutdown) {
		t.Fatalf("hello %+v", m)
	}
	pc.Write(protocol.Message{Type: protocol.TypeShutdown, ID: "s1"})
	if m, err := pc.Read(); err != nil || m.Type != protocol.TypeResult || !m.OK || m.ID != "s1" {
		t.Fatalf("result %+v %v", m, err)
	}
	select {
	case <-called:
	case <-time.After(2 * time.Second):
		t.Fatal("shutdown not called")
	}
	d = New(Config{EnvironmentID: "env"})
	server, client = net.Pipe()
	go d.HandleConn(ctx, server, func() { server.Close() })
	pc = protocol.NewConn(client)
	client.SetDeadline(time.Now().Add(5 * time.Second))
	if m, _ := pc.Read(); protocol.Has(m.Capabilities, protocol.CapShutdown) {
		t.Fatalf("hello %+v", m)
	}
	pc.Write(protocol.Message{Type: protocol.TypeShutdown, ID: "s2"})
	if m, err := pc.Read(); err != nil || m.OK || m.Error == "" {
		t.Fatalf("result %+v %v", m, err)
	}
}

// The interrupted fixture, Claude Code 2.1.284 after Esc during a turn,
// is idle at once from working: the prompt box is on screen, so the
// working-to-idle debounce does not hold it, and the finish is a
// transition the attention rule sees in the same poll.
func TestInterruptedIsIdleAtOnce(t *testing.T) {
	raw, err := os.ReadFile("../detect/testdata/claude-interrupted.screen")
	if err != nil {
		t.Fatal(err)
	}
	title, err := os.ReadFile("../detect/testdata/claude-interrupted.title")
	if err != nil {
		t.Fatal(err)
	}
	res := detect.Detect(detect.Input{Agent: "claude", Title: strings.TrimSpace(string(title)),
		Screen: strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n")})
	d := newTestDaemon()
	st := &paneState{
		identity:    procs.Identity{Agent: "claude", PID: 4242, Start: time.Unix(1, 0)},
		hasIdentity: true,
		activity:    protocol.Working,
	}
	now := time.Now()
	if got := d.nextActivity(st, res, now); got != protocol.Idle || st.pendingIdle != nil || !st.activityAt.Equal(now) {
		t.Fatalf("interrupted: %s, pending %v (%s via %s)", got, st.pendingIdle, res.State, res.Rule)
	}
}

// The plain stream is numbered by the broadcaster, one up per message
// from the snapshot's number, which is the last broadcast's, whatever
// kind of record the message carries.
func TestBroadcastNumbers(t *testing.T) {
	d := newTestDaemon()
	d.mu.Lock()
	d.broadcastLocked(protocol.Message{Type: protocol.TypeUpsert, Agent: &protocol.Agent{ID: "before"}})
	d.broadcastLocked(protocol.Message{Type: protocol.TypeUpsert, Agent: &protocol.Agent{ID: "before"}})
	d.mu.Unlock()
	s, snap := d.subscribe(nil)
	if snap.Seq != 2 {
		t.Fatalf("snapshot numbered %d after two broadcasts", snap.Seq)
	}
	d.mu.Lock()
	d.broadcastLocked(protocol.Message{Type: protocol.TypeUpsert, Agent: &protocol.Agent{ID: "x"}})
	d.broadcastLocked(protocol.Message{Type: protocol.TypeUpsert, Worktree: &protocol.Worktree{ID: "w"}})
	d.broadcastLocked(protocol.Message{Type: protocol.TypeRemove, AgentID: "x"})
	d.mu.Unlock()
	for i := uint64(1); i <= 3; i++ {
		// The broadcasts are done: a message not there is missing, not
		// late.
		select {
		case m := <-s.ch:
			if m.Seq != snap.Seq+i {
				t.Fatalf("message %d numbered %d after snapshot %d", i, m.Seq, snap.Seq)
			}
		default:
			t.Fatalf("message %d never sent", i)
		}
	}
	d.unsubscribe(s)
}
