package merged

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/laat/laatmux/internal/client"
	"github.com/laat/laatmux/internal/config"
	"github.com/laat/laatmux/internal/protocol"
	"github.com/laat/laatmux/internal/rows"
)

// The merged stream, applied: records are attributed to hosts through
// the environment id in the host records, hosts are ready when listed or
// failed, and one host's part comes back out as the hello and snapshot
// a direct dial would have fetched.
func TestApply(t *testing.T) {
	m := New()
	if s := m.Status(""); s.Loaded {
		t.Error("loaded before a snapshot")
	}
	m.Apply(protocol.Message{Type: protocol.TypeSnapshot, Seq: 3,
		Hosts: []protocol.HostStatus{
			{Name: "mac", EnvironmentID: "menv", Connected: true, Listed: true, Version: "v2", Capabilities: []string{"status", "worktrees", "merged"}},
			{Name: "vm", SSH: "vm"},
		},
		Agents:    []protocol.Agent{{ID: "menv/laatmux/%1", EnvironmentID: "menv", Server: "laatmux", Session: "proj/x", Agent: "claude", Activity: protocol.Working}},
		Worktrees: []protocol.Worktree{{ID: "menv/worktree//w/proj/x", EnvironmentID: "menv", Repo: "proj", Branch: "x", Root: "/w/proj/x", Session: "proj/x"}},
		Sessions:  []protocol.Session{{Name: "mac/proj/x", Key: "menv//w/proj/x", Host: "mac", Settled: true}},
	})
	if got := m.Waiting(); len(got) != 1 || got[0] != "vm" {
		t.Errorf("waiting = %v, want [vm]", got)
	}
	if _, ok := m.HostCaps("vm"); ok {
		t.Error("HostCaps of a host that has not answered a hello")
	}
	s := m.Status("")
	if !s.Loaded || s.ByHost["menv/laatmux/%1"] != "mac" || m.HostOf("menv") != "mac" || m.HostOf("venv") != "" {
		t.Errorf("attribution: loaded %v, byHost %v", s.Loaded, s.ByHost)
	}
	if names := hostNames(s); !slices.Equal(names, []string{"mac", "vm"}) {
		t.Errorf("hosts = %v", names)
	}
	if h, ok := s.Host("mac"); !ok || !h.Local || !h.Connected || !h.Listed || h.Version != "v2" || !h.Worktrees || h.EnvID != "menv" {
		t.Errorf("mac = %+v", h)
	}
	if h, ok := s.Host("vm"); !ok || h.Local || h.Connected || h.Listed || h.EnvID != "" {
		t.Errorf("vm = %+v", h)
	}
	if locals := s.Input.Locals; len(locals) != 1 || !locals[0].Settled || !locals[0].Workspace() {
		t.Errorf("locals = %+v", locals)
	}
	if len(s.Input.Agents) != 1 || len(s.Input.Worktrees) != 1 {
		t.Errorf("input: %+v", s.Input)
	}

	// The remote comes up: records arrive, then the host is listed.
	m.Apply(protocol.Message{Type: protocol.TypeUpsert, Seq: 4, HostStatus: &protocol.HostStatus{Name: "vm", SSH: "vm", EnvironmentID: "venv", Connected: true, Version: "v1", Capabilities: []string{"status", "worktrees"}}})
	m.Apply(protocol.Message{Type: protocol.TypeUpsert, Seq: 5, Worktree: &protocol.Worktree{ID: "venv/worktree//r/proj/y", EnvironmentID: "venv", Repo: "proj", Branch: "y", Root: "/r/proj/y"}})
	if got := m.Waiting(); len(got) != 1 {
		t.Errorf("waiting while the snapshot is pending = %v", got)
	}
	if caps, ok := m.HostCaps("vm"); !ok || !slices.Equal(caps, []string{"status", "worktrees"}) {
		t.Errorf("HostCaps = %v %v", caps, ok)
	}
	if _, ok := m.HostCaps("box"); ok {
		t.Error("HostCaps of a host not in the stream")
	}
	m.Apply(protocol.Message{Type: protocol.TypeUpsert, Seq: 6, HostStatus: &protocol.HostStatus{Name: "vm", SSH: "vm", EnvironmentID: "venv", Connected: true, Listed: true, Version: "v1", Capabilities: []string{"status", "worktrees"}}})
	if got := m.Waiting(); len(got) != 0 {
		t.Errorf("waiting after listed = %v", got)
	}
	hello, snap, ok, err := m.HostSnapshot("vm")
	if !ok || err != nil || hello.EnvironmentID != "venv" || hello.Host != "vm" || hello.Version != "v1" || !protocol.Has(hello.Capabilities, protocol.CapWorktrees) {
		t.Fatalf("HostSnapshot = %+v %v %v", hello, ok, err)
	}
	if len(snap.Worktrees) != 1 || snap.Worktrees[0].Root != "/r/proj/y" || len(snap.Agents) != 0 {
		t.Errorf("vm snapshot = %+v", snap)
	}
	if _, _, ok, _ := m.HostSnapshot("box"); ok {
		t.Error("unknown host reported as in the stream")
	}

	// Down: the error is the direct dial's failure; records stay.
	m.Apply(protocol.Message{Type: protocol.TypeUpsert, Seq: 7, HostStatus: &protocol.HostStatus{Name: "vm", SSH: "vm", EnvironmentID: "venv", Error: "disconnected"}})
	if _, _, ok, err := m.HostSnapshot("vm"); !ok || err == nil || !strings.Contains(err.Error(), "vm: disconnected") {
		t.Errorf("HostSnapshot while down = %v %v", ok, err)
	}
	if s := m.Status(""); len(s.Input.Worktrees) != 2 || s.ByHost["venv/worktree//r/proj/y"] != "vm" {
		t.Errorf("records dropped on host down: %+v", s.Input.Worktrees)
	}

	// Removed from the config: the host and everything of its go.
	m.Apply(protocol.Message{Type: protocol.TypeRemove, Seq: 8, HostName: "vm"})
	if s := m.Status(""); len(s.Hosts) != 1 || len(s.Input.Worktrees) != 1 || len(s.ByHost) != 2 {
		t.Errorf("host removal left %+v %+v %v", s.Hosts, s.Input.Worktrees, s.ByHost)
	}
	m.Apply(protocol.Message{Type: protocol.TypeRemove, Seq: 9, LocalSessionName: "mac/proj/x"})
	if s := m.Status(""); len(s.Input.Locals) != 0 {
		t.Error("session removal ignored")
	}
}

func hostNames(s Status) []string {
	var out []string
	for _, h := range s.Hosts {
		out = append(out, h.Name)
	}
	return out
}

// A host's removal drops its panes and runs by environment id too, and
// a record remove drops the one record.
func TestRemoveRecords(t *testing.T) {
	m := New()
	m.Apply(protocol.Message{Type: protocol.TypeSnapshot,
		Hosts:  []protocol.HostStatus{{Name: "vm", SSH: "vm", EnvironmentID: "venv", Connected: true, Listed: true}, {Name: "box", SSH: "box", EnvironmentID: "benv", Connected: true, Listed: true}},
		Agents: []protocol.Agent{{ID: "a1", EnvironmentID: "venv"}, {ID: "a2", EnvironmentID: "benv"}},
		Panes:  []protocol.Pane{{ID: "p1", EnvironmentID: "venv"}, {ID: "p2", EnvironmentID: "benv"}},
		Runs:   []protocol.Run{{ID: "r1", EnvironmentID: "venv"}, {ID: "r2", EnvironmentID: "benv"}},
	})
	m.Apply(protocol.Message{Type: protocol.TypeRemove, HostName: "vm"})
	s := m.Status("")
	if len(s.Input.Agents) != 1 || s.Input.Agents[0].ID != "a2" || len(s.Input.Panes) != 1 || s.Input.Panes[0].ID != "p2" || len(s.Input.Runs) != 1 || s.Input.Runs[0].ID != "r2" {
		t.Errorf("after the host removal: %+v", s.Input)
	}
	m.Apply(protocol.Message{Type: protocol.TypeRemove, AgentID: "a2", PaneRecordID: "p2", RunID: "r2"})
	if s := m.Status(""); len(s.Input.Agents)+len(s.Input.Panes)+len(s.Input.Runs) != 0 || len(s.ByHost) != 0 {
		t.Errorf("after the record removes: %+v %v", s.Input, s.ByHost)
	}
	// A host that never answered a hello has no environment id, and
	// its removal takes no pane or run, not those without one.
	m.Apply(protocol.Message{Type: protocol.TypeSnapshot,
		Hosts: []protocol.HostStatus{{Name: "new", SSH: "new"}},
		Panes: []protocol.Pane{{ID: "p3"}}, Runs: []protocol.Run{{ID: "r3"}}})
	m.Apply(protocol.Message{Type: protocol.TypeRemove, HostName: "new"})
	if s := m.Status(""); len(s.Input.Panes) != 1 || len(s.Input.Runs) != 1 || len(s.Hosts) != 0 {
		t.Errorf("after removing a host without an environment: %+v", s.Input)
	}
}

// A one-shot client that gave up on a host marks it down with the wait,
// a reconnect it was in included, and the state is terminal.
func TestTimedOut(t *testing.T) {
	m := New()
	m.Apply(protocol.Message{Type: protocol.TypeSnapshot,
		Hosts: []protocol.HostStatus{
			{Name: "mac", EnvironmentID: "menv", Connected: true, Listed: true, Capabilities: []string{"status", "worktrees"}},
			{Name: "vm", SSH: "vm", EnvironmentID: "venv", Connected: true, Capabilities: []string{"status", "worktrees"}},
			{Name: "box", SSH: "box", EnvironmentID: "benv", Error: "disconnected", Reconnecting: true, Capabilities: []string{"status", "worktrees"}},
		},
	})
	if p := m.Waiting(); !slices.Equal(p, []string{"box", "vm"}) {
		t.Fatalf("waiting = %v", p)
	}
	m.TimedOut(m.Waiting(), 20*time.Second)
	if p := m.Waiting(); len(p) != 0 {
		t.Errorf("waiting after the timeout = %v", p)
	}
	s := m.Status("")
	for _, n := range []string{"vm", "box"} {
		h, _ := s.Host(n)
		if !h.ready() || h.Connected || h.Listed || h.Reconnecting || h.Down() != "no snapshot after 20s" || h.EnvID == "" {
			t.Errorf("%s after the timeout: %+v", n, h)
		}
	}
	if h, _ := s.Host("mac"); !h.Listed || h.Error != "" {
		t.Errorf("mac after the timeout: %+v", h)
	}
}

// A host is ready when listed or failed; a dropped connection being
// dialled again is neither, and the header says the reconnect is on.
func TestHostReady(t *testing.T) {
	cases := []struct {
		st    Host
		ready bool
		down  string
	}{
		{Host{Listed: true, Connected: true}, true, ""},
		{Host{Error: "ssh: refused"}, true, "ssh: refused"},
		{Host{Error: "disconnected", Reconnecting: true}, false, "disconnected (reconnecting)"},
		{Host{Connected: true}, false, ""},
		{Host{}, false, ""},
	}
	for _, c := range cases {
		if got := c.st.ready(); got != c.ready {
			t.Errorf("%+v ready = %v", c.st, got)
		}
		if got := c.st.Down(); got != c.down {
			t.Errorf("%+v down = %q", c.st, got)
		}
	}
	st := fromStatus(protocol.HostStatus{Name: "vm", Error: "disconnected", Reconnecting: true})
	if st.ready() || !st.Reconnecting || st.Name != "vm" {
		t.Errorf("fromStatus: %+v", st)
	}
	st = fromStatus(protocol.HostStatus{Name: "mac", Capabilities: []string{protocol.CapWorktrees, protocol.CapAttribution}})
	if !st.Local || !st.Worktrees || !st.Attribution || len(st.Caps) != 2 {
		t.Errorf("fromStatus: %+v", st)
	}
}

// The session listing's error is kept from a snapshot or an upsert, and
// cleared by a listing that succeeded.
func TestSessionsError(t *testing.T) {
	m := New()
	m.Apply(protocol.Message{Type: protocol.TypeSnapshot, SessionsError: "tmux: permission denied"})
	if s := m.Status(""); s.SessionsErr != "tmux: permission denied" {
		t.Errorf("after the snapshot: %q", s.SessionsErr)
	}
	m.Apply(protocol.Message{Type: protocol.TypeUpsert, SessionsListed: true})
	if s := m.Status(""); s.SessionsErr != "" {
		t.Errorf("after a listing: %q", s.SessionsErr)
	}
	m.Apply(protocol.Message{Type: protocol.TypeUpsert, SessionsError: "tmux: gone"})
	if s := m.Status(""); s.SessionsErr != "tmux: gone" {
		t.Errorf("after an upsert: %q", s.SessionsErr)
	}
}

// The pending records and handoffs: a snapshot replaces the pendings and
// merges the handoffs; a remove with a replacement makes a handoff; a
// handoff seen again keeps its first sight; past the day it goes.
func TestPendingsAndHandoffs(t *testing.T) {
	m := New()
	m.Apply(protocol.Message{Type: protocol.TypeSnapshot,
		Pendings: []protocol.Pending{{ID: "add-1", Host: "vm"}},
		Handoffs: []protocol.Handoff{{ID: "add-0", ReplacedBy: "venv/worktree//w/proj/old"}},
	})
	m.Apply(protocol.Message{Type: protocol.TypeUpsert, Pending: &protocol.Pending{ID: "add-2", Host: "vm"}})
	s := m.Status("")
	if len(s.Input.Pendings) != 2 || s.Handoffs["add-0"] != "venv/worktree//w/proj/old" {
		t.Fatalf("status: pendings %+v, handoffs %v", s.Input.Pendings, s.Handoffs)
	}
	m.Apply(protocol.Message{Type: protocol.TypeRemove, PendingID: "add-2", ReplacedBy: "venv/worktree//w/proj/new"})
	m.Apply(protocol.Message{Type: protocol.TypeRemove, PendingID: "add-1"})
	s = m.Status("")
	if len(s.Input.Pendings) != 0 || len(s.Handoffs) != 2 || s.Handoffs["add-2"] != "venv/worktree//w/proj/new" {
		t.Fatalf("after the removes: pendings %+v, handoffs %v", s.Input.Pendings, s.Handoffs)
	}
	m.mu.Lock()
	first := m.handoffs["add-0"].at
	m.mu.Unlock()
	m.Apply(protocol.Message{Type: protocol.TypeSnapshot, Handoffs: []protocol.Handoff{{ID: "add-0", ReplacedBy: "venv/worktree//w/proj/old"}}})
	m.mu.Lock()
	again := m.handoffs["add-0"].at
	m.mu.Unlock()
	if !again.Equal(first) {
		t.Fatal("a handoff seen again was dated again")
	}
	if s := m.Status(""); len(s.Handoffs) != 2 {
		t.Fatalf("a resnapshot dropped a handoff: %v", s.Handoffs)
	}
	was := handoffRetention
	handoffRetention = 0
	defer func() { handoffRetention = was }()
	if s := m.Status(""); len(s.Handoffs) != 0 {
		t.Fatalf("handoffs past the day: %v", s.Handoffs)
	}
}

// The rows name a host's worktree by this machine's name for its
// source, in any form: a host labels a checkout its config does not
// list by its directory. A source this machine does not know keeps the
// host's label, and the stored record keeps the host's label either way;
// so do the rows, beside, for a worktree they name otherwise, by which
// add named its managed session.
func TestRowsUseLocalNames(t *testing.T) {
	cfg, err := config.Parse([]byte("repos:\n  - source: https://example.com/o/proj\n    name: mine\n"))
	if err != nil {
		t.Fatal(err)
	}
	m := New()
	m.Configure(cfg)
	m.Apply(protocol.Message{Type: protocol.TypeSnapshot, Worktrees: []protocol.Worktree{
		{ID: "a", EnvironmentID: "e", Repo: "checkout-dir", Source: "git@example.com:o/proj.git", Branch: "x", Root: "/w/a"},
		{ID: "b", EnvironmentID: "e", Repo: "theirs", Source: "git@example.com:o/other.git", Branch: "y", Root: "/w/b"},
	}})
	got := map[string]string{}
	in := m.Status("").Input
	for _, w := range in.Worktrees {
		got[w.ID] = w.Repo
	}
	if got["a"] != "mine" || got["b"] != "theirs" || m.worktrees["a"].Repo != "checkout-dir" {
		t.Fatalf("labels %v, stored %q", got, m.worktrees["a"].Repo)
	}
	if len(in.HostRepos) != 1 || in.HostRepos["a"] != "checkout-dir" {
		t.Fatalf("the host's labels %v", in.HostRepos)
	}
}

// Configure gives the rows the config's sort order and stale settings;
// without it the defaults hold.
func TestConfigureSidebar(t *testing.T) {
	m := New()
	in := m.Status("").Input
	if in.StaleAfter == 0 || !in.DimStale || !in.CollapseStale {
		t.Errorf("stale defaults: %+v", in)
	}
	cfg, err := config.Parse([]byte("sidebar:\n  sort: recency\n  stale_after: 3h\n  dim_stale: false\n"))
	if err != nil {
		t.Fatal(err)
	}
	m.Configure(cfg)
	in = m.Status("").Input
	if in.Sort != "recency" || in.StaleAfter != 3*time.Hour || in.DimStale {
		t.Errorf("configured: %+v", in)
	}
}

// A host's attribution reaches the rows only when the merging daemon
// forwards it: one older than attribution drops the worktree from every
// agent it forwards, and the rows then pair by session name.
func TestAttributionNeedsForwarding(t *testing.T) {
	m := New()
	m.Apply(protocol.Message{Type: protocol.TypeSnapshot,
		Hosts: []protocol.HostStatus{{Name: "vm", SSH: "vm", EnvironmentID: "venv", Connected: true, Listed: true,
			Capabilities: []string{protocol.CapStatus, protocol.CapWorktrees, protocol.CapAttribution}}}})
	attribution := func() bool {
		for _, h := range m.Status("").Input.Hosts {
			if h.Name == "vm" {
				return h.Attribution
			}
		}
		t.Fatal("no host vm")
		return false
	}
	if !attribution() {
		t.Fatal("attribution lost with a forwarding daemon")
	}
	m.via(&client.Conn{Hello: protocol.Message{Capabilities: []string{protocol.CapStatus, protocol.CapMerged}}})
	if attribution() {
		t.Fatal("attribution through a merging daemon that drops it")
	}
	m.via(&client.Conn{Hello: protocol.Message{Capabilities: []string{protocol.CapStatus, protocol.CapMerged, protocol.CapAttribution}}})
	if !attribution() {
		t.Fatal("attribution lost through a merging daemon that forwards it")
	}
}

// The merged stream's attention records: a snapshot sets them, an upsert
// changes one, a remove drops one, and the rows see them.
func TestAttention(t *testing.T) {
	m := New()
	m.Apply(protocol.Message{Type: protocol.TypeSnapshot, Attentions: []protocol.Attention{{AgentID: "a"}, {AgentID: "b"}}})
	m.Apply(protocol.Message{Type: protocol.TypeUpsert, Attention: &protocol.Attention{AgentID: "c"}})
	m.Apply(protocol.Message{Type: protocol.TypeRemove, AttentionID: "a"})
	in := m.Status("").Input
	if len(in.Attention) != 2 || in.Attention["b"].AgentID != "b" || in.Attention["c"].AgentID != "c" {
		t.Errorf("attention: %+v", in.Attention)
	}
}

// The merged stream's branch records: a snapshot sets them, an upsert
// changes one, a remove drops one. The GitHub error beside them is
// `hosts`'s to print; the views keep nothing of it.
func TestBranches(t *testing.T) {
	m := New()
	a, b := protocol.BranchKey{Source: "s", Branch: "a"}, protocol.BranchKey{Source: "s", Branch: "b"}
	m.Apply(protocol.Message{Type: protocol.TypeSnapshot, BranchStatuses: []protocol.BranchStatus{{BranchKey: a}}, GitHubError: "gh is not installed"})
	m.Apply(protocol.Message{Type: protocol.TypeUpsert, BranchStatus: &protocol.BranchStatus{BranchKey: b}})
	m.Apply(protocol.Message{Type: protocol.TypeRemove, BranchStatusKey: &a})
	if in := m.Status("").Input; len(in.Branches) != 1 || in.Branches[b].BranchKey != b {
		t.Errorf("%+v", in.Branches)
	}
}

// A Status's maps and slices are its own: the rows are built from it
// after the lock is released, while the stream goes on being applied.
func TestStatusIsACopy(t *testing.T) {
	m := New()
	m.Apply(protocol.Message{Type: protocol.TypeSnapshot,
		Hosts:          []protocol.HostStatus{{Name: "vm", SSH: "vm", EnvironmentID: "venv", Connected: true, Listed: true, Capabilities: []string{"status", "worktrees"}}},
		Worktrees:      []protocol.Worktree{{ID: "w1", EnvironmentID: "venv", Repo: "proj", Source: "s", Branch: "b0", Root: "/w/b0", Session: "proj/b0"}},
		Attentions:     []protocol.Attention{{AgentID: "a"}},
		BranchStatuses: []protocol.BranchStatus{{BranchKey: protocol.BranchKey{Source: "s", Branch: "b"}}},
		Handoffs:       []protocol.Handoff{{ID: "t", ReplacedBy: "w"}},
		Agents:         []protocol.Agent{{ID: "a", EnvironmentID: "venv"}},
	})
	s := m.Status("")
	m.Apply(protocol.Message{Type: protocol.TypeRemove, AttentionID: "a", BranchStatusKey: &protocol.BranchKey{Source: "s", Branch: "b"}, HostName: "vm"})
	m.Apply(protocol.Message{Type: protocol.TypeUpsert, Attention: &protocol.Attention{AgentID: "c"}})
	if _, ok := s.Input.Attention["a"]; !ok || len(s.Input.Attention) != 1 || len(s.Input.Branches) != 1 || len(s.Hosts) != 1 || len(s.ByHost) != 2 || len(s.Handoffs) != 1 || len(s.Input.Agents) != 1 {
		t.Errorf("a status changed under a later apply: %+v", s)
	}
	// The rows built from one while the stream applies attention and
	// branch upserts for a live agent on a worktree, which the rows look
	// up in both maps: a race here is a crash in a sidebar pane.
	m.Apply(protocol.Message{Type: protocol.TypeSnapshot,
		Hosts:     []protocol.HostStatus{{Name: "vm", SSH: "vm", EnvironmentID: "venv", Connected: true, Listed: true, Capabilities: []string{"status", "worktrees"}}},
		Agents:    []protocol.Agent{{ID: "a0", EnvironmentID: "venv", Server: "laatmux", Session: "proj/b0", Agent: "claude", Activity: protocol.Idle, Liveness: protocol.Alive, Managed: true}},
		Worktrees: []protocol.Worktree{{ID: "w1", EnvironmentID: "venv", Repo: "proj", Source: "s", Branch: "b0", Root: "/w/b0", Session: "proj/b0"}},
	})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 500; i++ {
			m.Apply(protocol.Message{Type: protocol.TypeUpsert, Attention: &protocol.Attention{AgentID: fmt.Sprint("a", i%3)},
				BranchStatus: &protocol.BranchStatus{BranchKey: protocol.BranchKey{Source: "s", Branch: fmt.Sprint("b", i%3)}}})
		}
	}()
	for i := 0; i < 500; i++ {
		if tree := rows.Tree(m.Status("").Input); len(tree) < 3 {
			t.Fatalf("rows built from a status: %d, want the repository, its worktree and the agent", len(tree))
		}
	}
	<-done
}

// Changed is signalled by a message applied and by Notify, keeps one
// signal while nobody waits, and the daemon error signals only when it
// changes.
func TestChanged(t *testing.T) {
	m := New()
	signalled := func() bool {
		select {
		case <-m.Changed():
			return true
		default:
			return false
		}
	}
	if signalled() {
		t.Fatal("signalled before any change")
	}
	m.Apply(protocol.Message{Type: protocol.TypeSnapshot})
	if !signalled() {
		t.Fatal("not signalled after a message")
	}
	m.Notify()
	m.Notify()
	if !signalled() {
		t.Fatal("not signalled after Notify")
	}
	if signalled() {
		t.Fatal("two signals kept")
	}
	m.setDaemonErr("down")
	if !signalled() {
		t.Fatal("not signalled when the daemon error changes")
	}
	m.setDaemonErr("down")
	if signalled() {
		t.Fatal("signalled when the daemon error did not change")
	}
	if s := m.Status(""); s.DaemonErr != "down" {
		t.Errorf("daemon error: %q", s.DaemonErr)
	}
}
