package main

import (
	"context"
	"net"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/laat/laatmux/internal/client"
	"github.com/laat/laatmux/internal/home"
	"github.com/laat/laatmux/internal/protocol"
)

// fakeDaemon stands in for the local daemon: a loopback listener the
// runtime file under a scratch LAATMUX_HOME points at, answering the
// hello with the given capabilities and every later message through
// serve. Dial finds it as it would the real one, and never starts one.
type fakeDaemon struct {
	caps  []string
	serve func(pc *protocol.Conn, m protocol.Message) bool // false ends the connection
	mu    sync.Mutex
	conns int
}

func startFakeDaemon(t *testing.T, caps []string, serve func(pc *protocol.Conn, m protocol.Message) bool) *fakeDaemon {
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
	f := &fakeDaemon{caps: caps, serve: serve}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			f.mu.Lock()
			f.conns++
			f.mu.Unlock()
			go func() {
				defer c.Close()
				pc := protocol.NewConn(c)
				pc.Write(protocol.Message{Type: protocol.TypeHello, Protocol: protocol.Version, EnvironmentID: "lenv", Version: "fake", Capabilities: f.caps})
				for {
					m, err := pc.Read()
					if err != nil || !f.serve(pc, m) {
						return
					}
				}
			}()
		}
	}()
	return f
}

func (f *fakeDaemon) connections() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.conns
}

// A daemon without the capability sends the client down the direct path,
// where the local host is the same daemon with a plain subscribe.
func TestSnapshotFallsBackToDirect(t *testing.T) {
	var plain, merged int
	var mu sync.Mutex
	startFakeDaemon(t, []string{protocol.CapStatus, protocol.CapWorktrees}, func(pc *protocol.Conn, m protocol.Message) bool {
		if m.Type != protocol.TypeSubscribe {
			return true
		}
		mu.Lock()
		if m.Merged {
			merged++
		} else {
			plain++
		}
		mu.Unlock()
		pc.Write(protocol.Message{Type: protocol.TypeSnapshot, Seq: 1, Worktrees: []protocol.Worktree{{ID: "lenv/worktree//w/x", EnvironmentID: "lenv", Repo: "proj", Branch: "x", Root: "/w/x"}}})
		return true
	})
	if _, ok := dialMerged(context.Background()); ok {
		t.Fatal("dialMerged accepted a daemon without the capability")
	}
	hello, snap, err := snapshot(context.Background(), client.Host{Name: "mac"}, protocol.CapWorktrees)
	if err != nil || hello.EnvironmentID != "lenv" || len(snap.Worktrees) != 1 {
		t.Fatalf("direct snapshot = %+v %+v %v", hello, snap, err)
	}
	mu.Lock()
	defer mu.Unlock()
	if plain != 1 || merged != 0 {
		t.Errorf("subscribes: plain %d merged %d", plain, merged)
	}
}

// Through the merged stream, snapshot waits for the one host to be
// listed, takes its records alone, and falls back for a host the daemon
// does not know.
func TestMergedSnapshotWaitsForHost(t *testing.T) {
	var direct int
	var mu sync.Mutex
	startFakeDaemon(t, []string{protocol.CapStatus, protocol.CapMerged}, func(pc *protocol.Conn, m protocol.Message) bool {
		if m.Type != protocol.TypeSubscribe {
			return true
		}
		if !m.Merged {
			mu.Lock()
			direct++
			mu.Unlock()
			pc.Write(protocol.Message{Type: protocol.TypeSnapshot, Seq: 1})
			return true
		}
		pc.Write(protocol.Message{Type: protocol.TypeSnapshot, Seq: 1, Hosts: []protocol.HostStatus{
			{Name: "mac", EnvironmentID: "lenv", Connected: true, Listed: true, Capabilities: []string{"status", "merged"}},
			{Name: "vm", SSH: "vm"},
		}})
		time.Sleep(50 * time.Millisecond)
		pc.Write(protocol.Message{Type: protocol.TypeUpsert, Seq: 2, HostStatus: &protocol.HostStatus{Name: "vm", SSH: "vm", EnvironmentID: "venv", Connected: true, Version: "v1", Capabilities: []string{"status", "worktrees", "rm"}}})
		pc.Write(protocol.Message{Type: protocol.TypeUpsert, Seq: 3, Worktree: &protocol.Worktree{ID: "venv/worktree//r/y", EnvironmentID: "venv", Repo: "proj", Branch: "y", Root: "/r/y"}})
		pc.Write(protocol.Message{Type: protocol.TypeUpsert, Seq: 4, Agent: &protocol.Agent{ID: "lenv/default/%3", EnvironmentID: "lenv"}})
		pc.Write(protocol.Message{Type: protocol.TypeUpsert, Seq: 5, HostStatus: &protocol.HostStatus{Name: "vm", SSH: "vm", EnvironmentID: "venv", Connected: true, Listed: true, Version: "v1", Capabilities: []string{"status", "worktrees", "rm"}}})
		return true
	})
	hello, snap, err := snapshot(context.Background(), client.Host{Name: "vm", SSH: "vm"}, protocol.CapRm)
	if err != nil || hello.EnvironmentID != "venv" || hello.Version != "v1" {
		t.Fatalf("hello = %+v %v", hello, err)
	}
	if len(snap.Worktrees) != 1 || snap.Worktrees[0].Root != "/r/y" || len(snap.Agents) != 0 {
		t.Errorf("vm's records = %+v", snap)
	}
	if _, _, err := snapshot(context.Background(), client.Host{Name: "vm", SSH: "vm"}, protocol.CapAdd); err == nil || !strings.Contains(err.Error(), "does not support add") {
		t.Errorf("missing capability not reported: %v", err)
	}
	// A host the merged stream lacks is dialled directly; here that is
	// the fake daemon again, with a plain subscribe.
	if _, _, err := snapshot(context.Background(), client.Host{Name: "other"}, ""); err != nil {
		t.Errorf("fallback for an unknown host: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if direct != 1 {
		t.Errorf("direct subscribes = %d, want 1", direct)
	}
}

// A daemon that answers the hello but never sends the snapshot is a
// timeout, not an empty listing.
func TestReadMergedTimesOutWithoutSnapshot(t *testing.T) {
	startFakeDaemon(t, []string{protocol.CapStatus, protocol.CapMerged}, func(pc *protocol.Conn, m protocol.Message) bool { return true })
	c, ok := dialMerged(context.Background())
	if !ok {
		t.Fatal("dialMerged failed")
	}
	defer c.Close()
	m := newMerged()
	_, err := m.readMerged(context.Background(), c, 200*time.Millisecond, func(*merged) bool { return false })
	if err == nil || !strings.Contains(err.Error(), "no snapshot after") {
		t.Fatalf("err = %v", err)
	}
	// With a snapshot but a host that never lists, the wait ends with
	// the host pending and no error.
	startFakeDaemon(t, []string{protocol.CapStatus, protocol.CapMerged}, func(pc *protocol.Conn, m protocol.Message) bool {
		if m.Type == protocol.TypeSubscribe {
			pc.Write(protocol.Message{Type: protocol.TypeSnapshot, Seq: 1, Hosts: []protocol.HostStatus{{Name: "vm", SSH: "vm"}}})
		}
		return true
	})
	c2, ok := dialMerged(context.Background())
	if !ok {
		t.Fatal("dialMerged failed")
	}
	defer c2.Close()
	m = newMerged()
	pending, err := m.readMerged(context.Background(), c2, 200*time.Millisecond, func(m *merged) bool { return len(m.pending()) == 0 })
	if err != nil || len(pending) != 1 || pending[0] != "vm" {
		t.Fatalf("pending = %v, err = %v", pending, err)
	}
}

// watch backs off after a subscription the daemon drops, not only after
// a failed dial, and stops when its context ends.
func TestFollowMergedBacksOff(t *testing.T) {
	followBackoffMin = 300 * time.Millisecond
	defer func() { followBackoffMin = time.Second }()
	f := startFakeDaemon(t, []string{protocol.CapStatus, protocol.CapMerged}, func(pc *protocol.Conn, m protocol.Message) bool {
		return m.Type != protocol.TypeSubscribe // hang up on subscribe
	})
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	m := newMerged()
	done := make(chan struct{})
	go func() {
		m.followMerged(ctx, nil)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("followMerged did not stop with its context")
	}
	if n := f.connections(); n < 1 || n > 3 {
		t.Errorf("%d connections in 500 ms with a 300 ms backoff", n)
	}
	if !m.daemonErrIs("disconnected; reconnecting") {
		t.Error("daemon row not marked while reconnecting")
	}
}
