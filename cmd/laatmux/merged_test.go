package main

import (
	"strings"
	"testing"
	"time"

	"github.com/laat/laatmux/internal/protocol"
)

// The merged stream, applied: records are attributed to hosts through
// the environment id in the host records, hosts are ready when listed or
// failed, and one host's part comes back out as the hello and snapshot
// the direct path would have fetched.
func TestApplyMerged(t *testing.T) {
	m := newMerged()
	m.applyMerged(protocol.Message{Type: protocol.TypeSnapshot, Seq: 3,
		Hosts: []protocol.HostStatus{
			{Name: "mac", EnvironmentID: "menv", Connected: true, Listed: true, Version: "v2", Capabilities: []string{"status", "worktrees", "merged"}},
			{Name: "vm", SSH: "vm"},
		},
		Agents:    []protocol.Agent{{ID: "menv/laatmux/%1", EnvironmentID: "menv", Session: "proj/x", Agent: "claude", Activity: protocol.Working}},
		Worktrees: []protocol.Worktree{{ID: "menv/worktree//w/proj/x", EnvironmentID: "menv", Repo: "proj", Branch: "x", Root: "/w/proj/x", Session: "proj/x"}},
		Sessions:  []protocol.Session{{Name: "mac/proj/x", Key: "menv//w/proj/x", Host: "mac", Settled: true}},
	})
	if got := m.pending(); len(got) != 1 || got[0] != "vm" {
		t.Errorf("pending = %v, want [vm]", got)
	}
	if h := m.byHost["menv/laatmux/%1"]; h != "mac" {
		t.Errorf("agent attributed to %q", h)
	}
	locals := m.locals()
	if len(locals) != 1 || !locals[0].Settled || !locals[0].Workspace() {
		t.Errorf("locals = %+v", locals)
	}
	out := m.render(locals)
	for _, want := range []string{"mac  connected  v2", "vm  connecting", "settled", "proj/x"} {
		if !strings.Contains(out, want) {
			t.Errorf("render lacks %q:\n%s", want, out)
		}
	}

	// The remote comes up: records arrive, then the host is listed.
	m.applyMerged(protocol.Message{Type: protocol.TypeUpsert, Seq: 4, HostStatus: &protocol.HostStatus{Name: "vm", SSH: "vm", EnvironmentID: "venv", Connected: true, Version: "v1", Capabilities: []string{"status", "worktrees"}}})
	m.applyMerged(protocol.Message{Type: protocol.TypeUpsert, Seq: 5, Worktree: &protocol.Worktree{ID: "venv/worktree//r/proj/y", EnvironmentID: "venv", Repo: "proj", Branch: "y", Root: "/r/proj/y"}})
	if got := m.pending(); len(got) != 1 {
		t.Errorf("pending while snapshot pending = %v", got)
	}
	m.applyMerged(protocol.Message{Type: protocol.TypeUpsert, Seq: 6, HostStatus: &protocol.HostStatus{Name: "vm", SSH: "vm", EnvironmentID: "venv", Connected: true, Listed: true, Version: "v1", Capabilities: []string{"status", "worktrees"}}})
	if got := m.pending(); len(got) != 0 {
		t.Errorf("pending after listed = %v", got)
	}
	hello, snap, ok, err := m.hostSnapshot("vm")
	if !ok || err != nil || hello.EnvironmentID != "venv" || !protocol.Has(hello.Capabilities, protocol.CapWorktrees) {
		t.Fatalf("hostSnapshot = %+v %v %v", hello, ok, err)
	}
	if len(snap.Worktrees) != 1 || snap.Worktrees[0].Root != "/r/proj/y" || len(snap.Agents) != 0 {
		t.Errorf("vm snapshot = %+v", snap)
	}
	if _, _, ok, _ := m.hostSnapshot("box"); ok {
		t.Error("unknown host reported as in the stream")
	}

	// Down: the error is the direct dial's failure; records stay.
	m.applyMerged(protocol.Message{Type: protocol.TypeUpsert, Seq: 7, HostStatus: &protocol.HostStatus{Name: "vm", SSH: "vm", EnvironmentID: "venv", Error: "disconnected"}})
	if _, _, ok, err := m.hostSnapshot("vm"); !ok || err == nil || !strings.Contains(err.Error(), "vm: disconnected") {
		t.Errorf("hostSnapshot while down = %v %v", ok, err)
	}
	if !strings.Contains(m.render(m.locals()), "vm  DOWN  disconnected") {
		t.Error("host down not rendered")
	}
	if len(m.worktrees) != 2 {
		t.Error("records dropped on host down")
	}

	// Removed from the config: the host and everything of its go.
	m.applyMerged(protocol.Message{Type: protocol.TypeRemove, Seq: 8, HostName: "vm"})
	if _, ok := m.hosts["vm"]; ok || len(m.worktrees) != 1 {
		t.Errorf("host removal left %+v %+v", m.hosts, m.worktrees)
	}
	m.applyMerged(protocol.Message{Type: protocol.TypeRemove, Seq: 9, LocalSessionName: "mac/proj/x"})
	if len(m.locals()) != 0 {
		t.Error("session removal ignored")
	}
}

// A one-shot client that gave up on a host says so in its row; a stale
// local session is judged only against a host that is connected and
// listed.
func TestMergedTimedOutAndStale(t *testing.T) {
	m := newMerged()
	m.applyMerged(protocol.Message{Type: protocol.TypeSnapshot,
		Hosts: []protocol.HostStatus{
			{Name: "mac", EnvironmentID: "menv", Connected: true, Listed: true, Capabilities: []string{"status", "worktrees"}},
			{Name: "vm", SSH: "vm", EnvironmentID: "venv", Connected: true, Capabilities: []string{"status", "worktrees"}},
			{Name: "box", SSH: "box", EnvironmentID: "benv", Error: "disconnected", Reconnecting: true, Capabilities: []string{"status", "worktrees"}},
		},
		Sessions: []protocol.Session{
			{Name: "mac/proj/gone", Key: "menv//w/proj/gone", Host: "mac"},
			{Name: "vm/proj/maybe", Key: "venv//r/proj/maybe", Host: "vm"},
		},
		SessionsError: "",
	})
	if p := m.pending(); len(p) != 2 || p[0] != "box" || p[1] != "vm" {
		t.Fatalf("pending = %v", p)
	}
	m.timedOut(m.pending(), 20*time.Second)
	out := m.render(m.locals())
	if !strings.Contains(out, "vm  DOWN  no snapshot after 20s") {
		t.Errorf("timed out host not marked:\n%s", out)
	}
	// A host still reconnecting when the wait ended is timed out the
	// same way, and the state is terminal: the reconnect note is gone.
	if !strings.Contains(out, "box  DOWN  no snapshot after 20s\n") || strings.Contains(out, "reconnecting") || !m.hosts["box"].ready() {
		t.Errorf("reconnecting host after the timeout:\n%s", out)
	}
	if !strings.Contains(out, "mac/proj/gone") || strings.Contains(out, "vm/proj/maybe") {
		t.Errorf("stale judged wrongly:\n%s", out)
	}
	m.mu.Lock()
	m.sessionsErr = "tmux: permission denied"
	m.mu.Unlock()
	if !strings.Contains(m.render(m.locals()), "local sessions not listed: tmux: permission denied") {
		t.Error("sessions error not printed")
	}
}

// A host is ready when listed or failed; a dropped connection being
// dialled again is neither, and the header says the reconnect is on.
func TestHostReady(t *testing.T) {
	cases := []struct {
		st    hostState
		ready bool
		down  string
	}{
		{hostState{Listed: true, Connected: true}, true, ""},
		{hostState{Error: "ssh: refused"}, true, "ssh: refused"},
		{hostState{Error: "disconnected", Reconnecting: true}, false, "disconnected (reconnecting)"},
		{hostState{Connected: true}, false, ""},
		{hostState{}, false, ""},
	}
	for _, c := range cases {
		if got := c.st.ready(); got != c.ready {
			t.Errorf("%+v ready = %v", c.st, got)
		}
		if got := c.st.down(); got != c.down {
			t.Errorf("%+v down = %q", c.st, got)
		}
	}
	st := fromStatus(protocol.HostStatus{Name: "vm", Error: "disconnected", Reconnecting: true})
	if st.ready() || !st.Reconnecting {
		t.Errorf("fromStatus: %+v", st)
	}
}
