package main

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/laat/laatmux/internal/config"
	"github.com/laat/laatmux/internal/home"
	"github.com/laat/laatmux/internal/merged"
	"github.com/laat/laatmux/internal/peer"
	"github.com/laat/laatmux/internal/protocol"
	"github.com/laat/laatmux/internal/workspace"
)

// startFakeDaemon starts a stand-in for the local daemon: a loopback listener
// the runtime file under a scratch LAATMUX_HOME points at, answering
// the hello with the given capabilities and every later message
// through serve, false ending the connection. Dial finds it as it
// would the real one, and never starts one.
func startFakeDaemon(t *testing.T, caps []string, serve func(pc *protocol.Conn, m protocol.Message) bool) {
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
	go serveFake(ln, protocol.Message{Type: protocol.TypeHello, Protocol: protocol.Version, EnvironmentID: "lenv", Version: "fake", Capabilities: caps}, serve)
}

// serveFake accepts on ln until it closes, answering each connection
// with hello and then every message through serve, false ending the
// connection; nil serve reads and ignores.
func serveFake(ln net.Listener, hello protocol.Message, serve func(pc *protocol.Conn, m protocol.Message) bool) {
	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		go func() {
			defer c.Close()
			pc := protocol.NewConn(c)
			pc.Write(hello)
			for {
				m, err := pc.Read()
				if err != nil || serve != nil && !serve(pc, m) {
					return
				}
			}
		}()
	}
}

// A daemon without the capability sends the client down the direct path,
// where the local host is the same daemon with a plain subscribe.
func TestSnapshotFallsBackToDirect(t *testing.T) {
	var plain, mergedSubs int
	var mu sync.Mutex
	startFakeDaemon(t, []string{protocol.CapStatus, protocol.CapWorktrees}, func(pc *protocol.Conn, m protocol.Message) bool {
		if m.Type != protocol.TypeSubscribe {
			return true
		}
		mu.Lock()
		if m.Merged {
			mergedSubs++
		} else {
			plain++
		}
		mu.Unlock()
		pc.Write(protocol.Message{Type: protocol.TypeSnapshot, Seq: 1, Worktrees: []protocol.Worktree{{ID: "lenv/worktree//w/x", EnvironmentID: "lenv", Repo: "proj", Branch: "x", Root: "/w/x"}}})
		return true
	})
	if _, ok := merged.Dial(context.Background()); ok {
		t.Fatal("Dial accepted a daemon without the capability")
	}
	// ls and watch refuse such a daemon and say how to replace it.
	if _, err := dialMergedOrExplain(context.Background()); err == nil || !strings.Contains(err.Error(), "older build") || !strings.Contains(err.Error(), "laatmux stop") {
		t.Fatalf("an older daemon explained as: %v", err)
	}
	hello, snap, err := snapshot(context.Background(), peer.Host{Name: "mac"}, protocol.CapWorktrees)
	if err != nil || hello.EnvironmentID != "lenv" || len(snap.Worktrees) != 1 {
		t.Fatalf("direct snapshot = %+v %+v %v", hello, snap, err)
	}
	mu.Lock()
	defer mu.Unlock()
	if plain != 1 || mergedSubs != 0 {
		t.Errorf("subscribes: plain %d merged %d", plain, mergedSubs)
	}
}

// Through the merged stream, snapshot waits for the one host to be
// listed, not for the others, takes its records alone, and falls back
// for a host the daemon does not know.
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
			{Name: "slow", SSH: "slow"}, // never lists
		}})
		time.Sleep(50 * time.Millisecond)
		pc.Write(protocol.Message{Type: protocol.TypeUpsert, Seq: 2, HostStatus: &protocol.HostStatus{Name: "vm", SSH: "vm", EnvironmentID: "venv", Connected: true, Version: "v1", Capabilities: []string{"status", "worktrees", "rm"}}})
		pc.Write(protocol.Message{Type: protocol.TypeUpsert, Seq: 3, Worktree: &protocol.Worktree{ID: "venv/worktree//r/y", EnvironmentID: "venv", Repo: "proj", Branch: "y", Root: "/r/y"}})
		pc.Write(protocol.Message{Type: protocol.TypeUpsert, Seq: 4, Agent: &protocol.Agent{ID: "lenv/default/%3", Server: "default", EnvironmentID: "lenv"}})
		pc.Write(protocol.Message{Type: protocol.TypeUpsert, Seq: 5, HostStatus: &protocol.HostStatus{Name: "vm", SSH: "vm", EnvironmentID: "venv", Connected: true, Listed: true, Version: "v1", Capabilities: []string{"status", "worktrees", "rm"}}})
		return true
	})
	start := time.Now()
	hello, snap, err := snapshot(context.Background(), peer.Host{Name: "vm", SSH: "vm"}, protocol.CapRm)
	if err != nil || hello.EnvironmentID != "venv" || hello.Version != "v1" {
		t.Fatalf("hello = %+v %v", hello, err)
	}
	if time.Since(start) > 5*time.Second {
		t.Error("the slow host was waited on")
	}
	if len(snap.Worktrees) != 1 || snap.Worktrees[0].Root != "/r/y" || len(snap.Agents) != 0 {
		t.Errorf("vm's records = %+v", snap)
	}
	if _, _, err := snapshot(context.Background(), peer.Host{Name: "vm", SSH: "vm"}, protocol.CapAdd); err == nil || !strings.Contains(err.Error(), "does not support add") {
		t.Errorf("missing capability not reported: %v", err)
	}
	// A host the merged stream lacks is dialled directly; here that is
	// the fake daemon again, with a plain subscribe.
	if _, _, err := snapshot(context.Background(), peer.Host{Name: "other"}, ""); err != nil {
		t.Errorf("fallback for an unknown host: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if direct != 1 {
		t.Errorf("direct subscribes = %d, want 1", direct)
	}
}

// A host whose connection dropped and is being dialled again is waited
// for, as a cold host is, and the snapshot arrives once it is back; a
// host whose dial failed is an error at once.
func TestMergedSnapshotWaitsThroughReconnect(t *testing.T) {
	caps := []string{"status", "worktrees"}
	startFakeDaemon(t, []string{protocol.CapStatus, protocol.CapMerged}, func(pc *protocol.Conn, m protocol.Message) bool {
		if m.Type != protocol.TypeSubscribe {
			return true
		}
		pc.Write(protocol.Message{Type: protocol.TypeSnapshot, Seq: 1, Hosts: []protocol.HostStatus{
			{Name: "mac", EnvironmentID: "lenv", Connected: true, Listed: true, Capabilities: []string{"status", "merged"}},
			{Name: "vm", SSH: "vm", EnvironmentID: "venv", Error: "disconnected", Reconnecting: true, Version: "v1", Capabilities: caps},
		}})
		time.Sleep(50 * time.Millisecond)
		pc.Write(protocol.Message{Type: protocol.TypeUpsert, Seq: 2, HostStatus: &protocol.HostStatus{Name: "vm", SSH: "vm", EnvironmentID: "venv", Connected: true, Version: "v1", Capabilities: caps}})
		pc.Write(protocol.Message{Type: protocol.TypeUpsert, Seq: 3, Worktree: &protocol.Worktree{ID: "venv/worktree//r/y", EnvironmentID: "venv", Repo: "proj", Branch: "y", Root: "/r/y"}})
		pc.Write(protocol.Message{Type: protocol.TypeUpsert, Seq: 4, HostStatus: &protocol.HostStatus{Name: "vm", SSH: "vm", EnvironmentID: "venv", Connected: true, Listed: true, Version: "v1", Capabilities: caps}})
		return true
	})
	hello, snap, err := snapshot(context.Background(), peer.Host{Name: "vm", SSH: "vm"}, protocol.CapWorktrees)
	if err != nil || hello.EnvironmentID != "venv" || len(snap.Worktrees) != 1 {
		t.Fatalf("through a reconnect: %+v %+v %v", hello, snap, err)
	}
	// The dial failed: ssh's error, no reconnect under way, fail at once.
	startFakeDaemon(t, []string{protocol.CapStatus, protocol.CapMerged}, func(pc *protocol.Conn, m protocol.Message) bool {
		if m.Type == protocol.TypeSubscribe {
			pc.Write(protocol.Message{Type: protocol.TypeSnapshot, Seq: 1, Hosts: []protocol.HostStatus{
				{Name: "vm", SSH: "vm", Error: "ssh: connect to host vm port 22: Connection refused"},
			}})
		}
		return true
	})
	start := time.Now()
	if _, _, err := snapshot(context.Background(), peer.Host{Name: "vm", SSH: "vm"}, protocol.CapWorktrees); err == nil || !strings.Contains(err.Error(), "Connection refused") {
		t.Fatalf("refused dial: %v", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatal("a refused dial was waited on")
	}
}

// The stream alone is what ls, watch and the sidebar take; the add
// form, the dashboard, compose and tasks refuse a daemon without the
// relay, an older build, before they need a terminal or a repository.
func TestNeedRelay(t *testing.T) {
	startFakeDaemon(t, []string{protocol.CapStatus, protocol.CapMerged}, func(pc *protocol.Conn, m protocol.Message) bool { return true })
	if c2, err := dialMergedOrExplain(context.Background()); err != nil {
		t.Fatalf("the stream alone refused: %v", err)
	} else if err := needRelay(c2); err == nil || !strings.Contains(err.Error(), "no relay") || !strings.Contains(err.Error(), "laatmux stop") {
		t.Fatalf("a daemon without the relay taken by the form: %v", err)
	}
	// Each of the form's hosts and tasks refuses it before it needs a
	// terminal or a repository; with the relay, tasks goes on.
	t.Setenv("LAATMUX_CONFIG", filepath.Join(t.TempDir(), "none.yaml"))
	for name, run := range map[string]func() error{
		"tasks":     func() error { return listTasks(context.Background()) },
		"dashboard": func() error { return cmdDashboard(context.Background(), nil) },
		"compose":   func() error { return cmdCompose(context.Background(), nil) },
	} {
		if err := run(); err == nil || !strings.Contains(err.Error(), "no relay") {
			t.Errorf("%s on a daemon without the relay: %v", name, err)
		}
	}
	startFakeDaemon(t, []string{protocol.CapStatus, protocol.CapMerged, protocol.CapRelay}, func(pc *protocol.Conn, m protocol.Message) bool {
		if m.Type == protocol.TypeSubscribe {
			pc.Write(protocol.Message{Type: protocol.TypeSnapshot, Seq: 1})
		}
		return true
	})
	if err := listTasks(context.Background()); err != nil {
		t.Fatalf("tasks on a daemon with the relay: %v", err)
	}
}

// A session whose host tag names nothing after a rename is routed by
// the key's environment: the configured host whose daemon answers as
// it, found through the merged stream's host rows.
func TestHostByEnvironment(t *testing.T) {
	startFakeDaemon(t, []string{protocol.CapStatus, protocol.CapMerged}, func(pc *protocol.Conn, m protocol.Message) bool {
		if m.Type == protocol.TypeSubscribe {
			pc.Write(protocol.Message{Type: protocol.TypeSnapshot, Seq: 1, Hosts: []protocol.HostStatus{
				{Name: "mac", EnvironmentID: "lenv", Connected: true, Listed: true, Capabilities: []string{"status", "merged", "rm"}},
				{Name: "box", SSH: "box", EnvironmentID: "benv", Connected: true, Listed: true, Version: "v2", Capabilities: []string{"status", "worktrees", "rm"}},
			}, Worktrees: []protocol.Worktree{{ID: "benv/worktree//r/x", EnvironmentID: "benv", Repo: "proj", Branch: "x", Root: "/r/x"}}})
		}
		return true
	})
	cfg := config.Config{Hosts: []config.Host{{Host: peer.Host{Name: "mac"}}, {Host: peer.Host{Name: "box", SSH: "box"}}}}
	h, hello, snap, err := hostByEnvironment(context.Background(), cfg, "benv", protocol.CapRm)
	if err != nil || h.Name != "box" || hello.Version != "v2" || len(snap.Worktrees) != 1 {
		t.Fatalf("box by environment: %+v %+v %+v %v", h, hello, snap, err)
	}
	if _, _, _, err := hostByEnvironment(context.Background(), cfg, "nope", protocol.CapRm); err == nil || !strings.Contains(err.Error(), "no configured host answers as environment nope") {
		t.Fatalf("unknown environment: %v", err)
	}
	// A capability the host lacks passes it over, as a host that does
	// not answer is.
	if _, _, _, err := hostByEnvironment(context.Background(), cfg, "benv", protocol.CapAdd); err == nil || !strings.Contains(err.Error(), "does not support add") {
		t.Fatalf("missing capability: %v", err)
	}
	// A session's host: the tag when it names a configured host; else,
	// for a renamed host or a session without the tag, the environment.
	// An empty tag is not the local host, though Find("") is.
	for _, c := range []struct {
		host, want string
	}{{"box", "box"}, {"old", "box"}, {"", "box"}} {
		h, hello, _, err := hostForSession(context.Background(), cfg, protocol.Session{Name: "s", Key: "benv//r/x", Host: c.host})
		if err != nil || h.Name != c.want || hello.EnvironmentID != "benv" {
			t.Errorf("tag %q: %+v %+v %v", c.host, h, hello, err)
		}
	}
	if _, _, _, err := hostForSession(context.Background(), cfg, protocol.Session{Name: "s", Key: "nope//r/x"}); err == nil || !strings.Contains(err.Error(), "carries no host tag") {
		t.Errorf("no tag, unknown environment: %v", err)
	}
	// What shell, run and split run inside a session use: the tag, else
	// the environment, with no capability asked for; the retag is best
	// effort (no session s exists here).
	for _, c := range []struct {
		host, want string
	}{{"box", "box"}, {"old", "box"}, {"", "box"}} {
		h, err := workspaceHost(context.Background(), cfg, protocol.Session{Name: "s", Key: "benv//r/x", Host: c.host}, "")
		if err != nil || h.Name != c.want {
			t.Errorf("workspaceHost tag %q: %+v %v", c.host, h, err)
		}
	}
	if _, err := workspaceHost(context.Background(), cfg, protocol.Session{Name: "s", Key: "nope//r/x", Host: "old"}, ""); err == nil || !strings.Contains(err.Error(), `is on host "old", which is not configured, and no configured host answers`) {
		t.Errorf("renamed host, unknown environment: %v", err)
	}
}

// A workspace session whose tag names a host since renamed is retagged
// with the name the config has now, by the key's environment, so the
// next command inside it finds the host by the tag alone.
func TestWorkspaceHostRetags(t *testing.T) {
	isolatedDefault(t)
	startFakeDaemon(t, []string{protocol.CapStatus, protocol.CapMerged}, func(pc *protocol.Conn, m protocol.Message) bool {
		if m.Type == protocol.TypeSubscribe {
			pc.Write(protocol.Message{Type: protocol.TypeSnapshot, Seq: 1, Hosts: []protocol.HostStatus{
				{Name: "box", SSH: "box", EnvironmentID: "benv", Connected: true, Listed: true, Capabilities: []string{"status"}},
			}})
		}
		return true
	})
	ctx := context.Background()
	for _, args := range [][]string{
		{"new-session", "-d", "-s", "old/proj/x", "sleep 1000"},
		{"set-option", "-t", "old/proj/x", "@laatmux_workspace", "benv//r/x"},
		{"set-option", "-t", "old/proj/x", "@laatmux_host", "old"},
	} {
		if _, err := workspace.Server.Run(ctx, args...); err != nil {
			t.Fatal(err)
		}
	}
	cfg := config.Config{Hosts: []config.Host{{Host: peer.Host{Name: "box", SSH: "box"}}}}
	cur := protocol.Session{Name: "old/proj/x", Key: "benv//r/x", Host: "old"}
	if h, err := workspaceHost(ctx, cfg, cur, ""); err != nil || h.Name != "box" {
		t.Fatalf("renamed host: %+v %v", h, err)
	}
	out, err := workspace.Server.Run(ctx, "display", "-p", "-t", "old/proj/x", "#{@laatmux_host}")
	if err != nil || strings.TrimSpace(string(out)) != "box" {
		t.Fatalf("tag after the lookup: %q %v", out, err)
	}
}
