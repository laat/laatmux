package rows

import (
	"strings"
	"testing"
	"time"

	"github.com/laat/laatmux/internal/config"
	"github.com/laat/laatmux/internal/protocol"
	"github.com/laat/laatmux/internal/workspace"
)

// nodesByID is the tree's nodes, or the agent view's tiles, by id.
func nodesByID(nodes []Row) map[string]Row {
	out := map[string]Row{}
	for _, n := range nodes {
		out[n.ID()] = n
	}
	return out
}

// treeLines is the tree's depth-1 nodes: the worktree and task lines,
// and the agents and orphaned sessions in other sessions.
func treeLines(nodes []Row) []Row {
	var out []Row
	for _, n := range nodes {
		if n.Depth == 1 {
			out = append(out, n)
		}
	}
	return out
}

// The join: worktrees hold the agent in the session the record names,
// leftovers are in other sessions, local sessions join by key or attach
// tag, and sessions are orphaned when their worktree is gone from a host
// that can say so.
func TestJoin(t *testing.T) {
	now := time.Now()
	in := Input{
		Hosts: []Host{
			{Name: "mac", Local: true, EnvironmentID: "menv", Connected: true, Listed: true, Worktrees: true},
			{Name: "vm", EnvironmentID: "venv", Connected: true, Listed: true, Worktrees: true},
			{Name: "box", EnvironmentID: "benv", Error: "unreachable"},
			{Name: "slow", EnvironmentID: "senv", Connected: true, Worktrees: true},
			{Name: "old", EnvironmentID: "oenv", Connected: true, Listed: true},
		},
		Agents: []protocol.Agent{
			{ID: "venv/laatmux/%1", EnvironmentID: "venv", Session: "proj/fix", Agent: "claude", Activity: protocol.Working, ActivityAt: now, Liveness: protocol.Alive, Managed: true, Title: "fixing"},
			{ID: "venv/laatmux/%2", EnvironmentID: "venv", Session: "proj/old", Agent: "codex", Activity: protocol.Idle, ActivityAt: now.Add(-time.Hour), Liveness: protocol.Alive, Managed: true},
			{ID: "venv/laatmux/%3", EnvironmentID: "venv", Session: "scratch", Agent: "claude", Activity: protocol.Idle, ActivityAt: now, Liveness: protocol.Alive, Managed: true},
			{ID: "venv/laatmux/%5", EnvironmentID: "venv", Session: "proj/dead", Agent: "claude", Activity: protocol.Idle, ActivityAt: now, Liveness: protocol.Gone, Managed: true},
			{ID: "menv/default/%4", EnvironmentID: "menv", Server: "default", Session: "notes", Agent: "claude", Activity: protocol.Blocked, ActivityAt: now, Liveness: protocol.Alive},
			{ID: "venv/default/%6", EnvironmentID: "venv", Server: "default", Session: "remote-notes", Agent: "claude", Activity: protocol.Idle, ActivityAt: now, Liveness: protocol.Alive},
			{ID: "benv/laatmux/%7", EnvironmentID: "benv", Session: "proj/down", Agent: "claude", Activity: protocol.Working, ActivityAt: now, Liveness: protocol.Alive, Managed: true},
		},
		Worktrees: []protocol.Worktree{
			{ID: "venv/worktree//r/fix", EnvironmentID: "venv", Repo: "proj", Branch: "fix", Root: "/r/fix", Session: "proj/fix"},
			{ID: "venv/worktree//r/old", EnvironmentID: "venv", Repo: "proj", Branch: "old", Root: "/r/old", Session: "proj/old"},
			{ID: "venv/worktree//r/bare", EnvironmentID: "venv", Repo: "proj", Branch: "bare", Root: "/r/bare"},
			{ID: "venv/worktree//r/shell", EnvironmentID: "venv", Repo: "proj", Branch: "shell", Root: "/r/shell", Session: "proj/shell"},
			{ID: "venv/worktree//r/dead", EnvironmentID: "venv", Repo: "proj", Branch: "dead", Root: "/r/dead", Session: "proj/dead"},
			{ID: "venv/worktree//r/det", EnvironmentID: "venv", Repo: "proj", Branch: "", Root: "/r/det"},
			{ID: "benv/worktree//r/down", EnvironmentID: "benv", Repo: "proj", Branch: "down", Root: "/r/down", Session: "proj/down"},
		},
		Locals: []workspace.Local{
			{Name: "vm/proj/fix", Key: "venv//r/fix", Host: "vm"},
			{Name: "vm/proj/old", Key: "venv//r/old", Host: "vm", Settled: true},
			{Name: "vm/proj/gone", Key: "venv//r/gone", Host: "vm"},
			{Name: "vm/scratch", Attach: "vm/scratch", Host: "vm"},
			{Name: "box/proj/x", Key: "benv//r/x", Host: "box"},
			{Name: "slow/proj/y", Key: "senv//r/y", Host: "slow"},
			{Name: "old/proj/z", Key: "oenv//r/z", Host: "old"},
			{Name: "notes"},
		},
		Current: "vm/proj/fix",
	}
	nodes := Tree(in)
	names := func(rs []Row) string {
		var out []string
		for _, r := range rs {
			out = append(out, r.Name)
		}
		return strings.Join(out, " ")
	}
	// The tree: the worktrees by branch under their repository, each
	// holding the agent in its session, those without an agent too; the
	// managed agent in no worktree, the observed agents and the orphaned
	// session in other sessions.
	want := `proj
  bare
  dead
    proj/dead
  det
  down
    proj/down
  fix *
    proj/fix
  old
    proj/old
  shell
other sessions
  scratch
  notes
  remote-notes
  vm/proj/gone
`
	if got := outline(nodes); got != want {
		t.Errorf("tree:\n%s\nwant:\n%s", got, want)
	}
	// The agent view's order: blocked, working, idle (most recent first),
	// ties by host then name; the settled workspace's agent in the Stale
	// fold. A worktree with no agent has no tile.
	rs := Agents(in)
	if want := "notes proj/down proj/fix proj/dead remote-notes scratch"; names(rs.Main) != want {
		t.Errorf("main = %q\nwant  %q", names(rs.Main), want)
	}
	if want := "proj/old"; names(rs.Stale) != want || !rs.Stale[0].Settled || !rs.Stale[0].Dim {
		t.Errorf("stale = %q, want %q settled and dim", names(rs.Stale), want)
	}
	var orphaned []Row
	for _, n := range nodes {
		if n.Orphaned {
			orphaned = append(orphaned, n)
		}
	}
	if want := "vm/proj/gone"; names(orphaned) != want {
		t.Errorf("orphaned = %q, want %q: a host down, unlisted or without worktrees says nothing", names(orphaned), want)
	}
	byName := map[string]Row{}
	for _, n := range treeLines(nodes) {
		byName[n.Name] = n
	}
	fix := byName["proj/fix"]
	if fix.Agent == nil || fix.Agent.Title != "fixing" || fix.Local == nil || !fix.Current || fix.Dim || fix.Host != "vm" || fix.HostDown {
		t.Errorf("worktree with agent and local session: %+v", fix)
	}
	if tile := rs.Main[2]; tile.Worktree == nil || tile.Worktree.ID != "venv/worktree//r/fix" || tile.Local == nil || !tile.Current || tile.Dim {
		t.Errorf("the worktree's agent's tile: %+v", tile)
	}
	if r := byName["proj/old"]; !r.Settled || !r.Dim || r.Agent == nil {
		t.Errorf("settled: %+v", r)
	}
	if r := byName["proj/bare"]; r.Agent != nil || r.State() != "no session" || !r.Dim {
		t.Errorf("worktree without session: %+v", r)
	}
	if r := byName["proj/shell"]; r.Agent != nil || r.State() != "no agent" || !r.Dim {
		t.Errorf("worktree with a session but no agent: %+v", r)
	}
	if r := byName["proj/dead"]; r.Agent == nil || !r.Dim {
		t.Errorf("gone agent not dim: %+v", r)
	}
	if r := byName["scratch"]; r.Worktree != nil || r.Local == nil || r.Local.Attach != "vm/scratch" {
		t.Errorf("managed agent without worktree joins its attachment: %+v", r)
	}
	if r := byName["notes"]; r.Local == nil || r.Local.Name != "notes" || r.Dim {
		t.Errorf("observed agent on the local default server is a local session: %+v", r)
	}
	if r := byName["remote-notes"]; r.Local != nil {
		t.Errorf("observed agent on a remote default server has no local session: %+v", r)
	}
	if r := byName["proj/down"]; !r.HostDown || !r.Dim {
		t.Errorf("host down not dim: %+v", r)
	}
	if r := byName["vm/proj/gone"]; !r.Orphaned || !r.Dim || r.State() != "no worktree" || r.Local == nil || r.Host != "vm" {
		t.Errorf("orphaned: %+v", r)
	}
	// A orphaned session tagged with a host's old name is the host's that
	// answers for its environment id now.
	var renamed []Row
	for _, n := range Tree(Input{
		Hosts:  []Host{{Name: "box", EnvironmentID: "venv", Connected: true, Listed: true, Worktrees: true}},
		Locals: []workspace.Local{{Name: "vm/proj/gone", Key: "venv//r/gone", Host: "vm"}},
	}) {
		if n.Orphaned {
			renamed = append(renamed, n)
		}
	}
	if len(renamed) != 1 || renamed[0].Host != "box" || renamed[0].HostDown {
		t.Errorf("orphaned line after a host rename: %+v", renamed)
	}
	if r := byName["proj (detached) /r/det"]; r.Worktree == nil {
		t.Errorf("detached worktree: %+v", r)
	}
	if rs.Main[0].Mark() != "!" || fix.Mark() != "*" || byName["scratch"].Mark() != "-" || byName["proj/bare"].Mark() != " " {
		t.Error("marks")
	}
	if byName["proj/bare"].AgentName() != "" || fix.AgentName() != "claude" {
		t.Error("agent names")
	}
}

// A record no host record claims has no host and counts as down; two
// hosts with one environment id attribute to the first by name.
func TestJoinAttribution(t *testing.T) {
	nodes := Tree(Input{
		Hosts:     []Host{{Name: "b", EnvironmentID: "e", Connected: true, Listed: true}, {Name: "a", EnvironmentID: "e", Connected: true, Listed: true}},
		Worktrees: []protocol.Worktree{{ID: "x", EnvironmentID: "e", Repo: "p", Branch: "b", Root: "/r"}, {ID: "y", EnvironmentID: "other", Repo: "p", Branch: "c", Root: "/s"}},
	})
	lines := treeLines(nodes)
	byID := nodesByID(lines)
	if x, y := byID["x"], byID["y"]; len(lines) != 2 || x.Kind != KindWorktree || x.Host != "a" || x.HostDown || y.Kind != KindWorktree || y.Host != "" || !y.HostDown {
		t.Errorf("lines = %+v", lines)
	}
}

// The join is by environment id: two hosts that are down keep their
// records paired with their own agents, and the same session name on
// two hosts never crosses.
func TestJoinByEnvironment(t *testing.T) {
	now := time.Now()
	in := Input{
		Hosts: []Host{{Name: "a", EnvironmentID: "aenv", Error: "down"}, {Name: "b", EnvironmentID: "benv", Error: "down"}},
		Agents: []protocol.Agent{
			{ID: "aenv/laatmux/%1", EnvironmentID: "aenv", Session: "proj/x", Agent: "claude", Activity: protocol.Working, ActivityAt: now, Liveness: protocol.Alive, Managed: true},
			{ID: "benv/laatmux/%1", EnvironmentID: "benv", Session: "proj/x", Agent: "codex", Activity: protocol.Idle, ActivityAt: now, Liveness: protocol.Alive, Managed: true},
		},
		Worktrees: []protocol.Worktree{
			{ID: "aenv/worktree//r/x", EnvironmentID: "aenv", Repo: "proj", Branch: "x", Root: "/r/x", Session: "proj/x"},
			{ID: "benv/worktree//r/x", EnvironmentID: "benv", Repo: "proj", Branch: "x", Root: "/r/x", Session: "proj/x"},
		},
	}
	nodes := Tree(in)
	lines := treeLines(nodes)
	if len(lines) != 2 {
		t.Fatalf("lines = %+v", lines)
	}
	for i, n := range nodes {
		if n.Kind != KindWorktree {
			continue
		}
		if n.Agent == nil || n.Agent.EnvironmentID != n.Worktree.EnvironmentID || !n.HostDown || !n.Dim || n.Children != 1 {
			t.Errorf("line %+v paired with %+v", n.Worktree, n.Agent)
		}
		if c := nodes[i+1]; c.Kind != KindAgent || c.Agent.EnvironmentID != n.Worktree.EnvironmentID {
			t.Errorf("line %+v holds %+v", n.Worktree, c.Agent)
		}
	}
	if lines[0].ID() == lines[1].ID() {
		t.Error("ids collide")
	}
	// The agent view: one tile each, with its own host's worktree.
	tiles := Agents(in).Main
	if len(tiles) != 2 {
		t.Fatalf("tiles = %+v", tiles)
	}
	for _, r := range tiles {
		if r.Agent == nil || r.Worktree == nil || r.Agent.EnvironmentID != r.Worktree.EnvironmentID || !r.HostDown || !r.Dim {
			t.Errorf("tile %+v paired with %+v", r.Worktree, r.Agent)
		}
	}
	if tiles[0].ID() == tiles[1].ID() {
		t.Error("tile ids collide")
	}
}

func TestAgo(t *testing.T) {
	for d, want := range map[time.Duration]string{5 * time.Second: " 5s", 3 * time.Minute: " 3m", 26 * time.Hour: "26h", -time.Second: " 0s"} {
		if got := Ago(d); got != want {
			t.Errorf("Ago(%s) = %q, want %q", d, got, want)
		}
	}
}

// Pending tasks: first in the agent view, newest first; a task stands
// for the worktree line at its root, which is not drawn while any task
// for it stands; every such task takes that line's local session, and
// the newest its place, its agent and its children; a task whose host
// is gone from the config says so first. Build gave the worktree's
// agent to every task standing for it, and before the listing the
// adopted agent to every standing task with the same session and root;
// the tree gives it to the newest only, the others being lines of their
// own beside it.
func TestPending(t *testing.T) {
	now := time.Now()
	in := Input{
		Hosts: []Host{{Name: "vm", EnvironmentID: "venv", Connected: true, Listed: true, Worktrees: true}},
		Agents: []protocol.Agent{
			{ID: "venv/laatmux/%1", EnvironmentID: "venv", Session: "proj/task", Cwd: "/r/task", Agent: "claude", Activity: protocol.Working, ActivityAt: now, Liveness: protocol.Alive, Managed: true},
			{ID: "venv/laatmux/%2", EnvironmentID: "venv", Session: "proj/other", Agent: "claude", Activity: protocol.Blocked, ActivityAt: now, Liveness: protocol.Alive, Managed: true},
		},
		Worktrees: []protocol.Worktree{
			{ID: "venv/worktree//r/task", EnvironmentID: "venv", Repo: "proj", Branch: "task", Root: "/r/task", Session: "proj/task"},
			{ID: "venv/worktree//r/other", EnvironmentID: "venv", Repo: "proj", Branch: "other", Root: "/r/other", Session: "proj/other"},
		},
		Locals: []workspace.Local{{Name: "vm/proj/task", Key: "venv//r/task", Host: "vm", Settled: true}},
		Pendings: []protocol.Pending{
			{ID: "add-1", Host: "vm", EnvironmentID: "venv", Repo: "proj", Branch: "task", Root: "/r/task", Session: "proj/task", Taken: true, Reachable: true,
				Done: true, OK: true, Prompt: protocol.DeliveryDelivered, SubmittedAt: now.Add(-time.Minute)},
			{ID: "add-2", Host: "vm", EnvironmentID: "venv", Repo: "proj", Branch: "task", Root: "/r/task", Taken: true, Reachable: true,
				Done: true, OK: true, Prompt: protocol.DeliveryNotDelivered, Error: "the pane was not ready", SubmittedAt: now.Add(-2 * time.Minute)},
			{ID: "add-3", Host: "vm", Repo: "proj", Branch: "new", Taken: true, Reachable: true, Stage: protocol.StageFetch, SubmittedAt: now},
			{ID: "add-4", Host: "gone", Repo: "proj", Branch: "lost", SubmittedAt: now.Add(-time.Hour)},
		},
		Current: "vm/proj/task",
	}
	got := Agents(in)
	var ids []string
	for _, r := range got.Main {
		ids = append(ids, r.ID())
	}
	// The tasks, then the agents: the blocked one, then the worktree's,
	// settled but the viewer's.
	want := "add-3 add-1 add-2 add-4 venv/laatmux/%2 venv/laatmux/%1"
	if strings.Join(ids, " ") != want {
		t.Fatalf("main %q, want %q", strings.Join(ids, " "), want)
	}
	if len(got.Stale) != 0 {
		t.Fatalf("stale %d: the settled worktree hides behind its tasks, its agent the viewer's", len(got.Stale))
	}
	// The tree: the worktree's line is the newest task's, holding its
	// agent, with the other task a line after it.
	nodes := Tree(in)
	var tree []string
	for _, n := range nodes {
		tree = append(tree, strings.Repeat("  ", n.Depth)+n.ID())
		if n.Kind == KindWorktree && (n.Orphaned || n.Worktree.ID == "venv/worktree//r/task") {
			t.Fatalf("%s drawn: the settled worktree hides behind its tasks", n.ID())
		}
	}
	if want := []string{LabelRepoNode("proj"), "  add-4", "  add-3", "  venv/worktree//r/other", "    venv/laatmux/%2", "  add-1", "    venv/laatmux/%1", "  add-2"}; strings.Join(tree, "\n") != strings.Join(want, "\n") {
		t.Fatalf("tree:\n%s\nwant:\n%s", strings.Join(tree, "\n"), strings.Join(want, "\n"))
	}
	byID := nodesByID(got.Main)
	// Both tasks for the one branch stand for its worktree line, carry
	// its worktree and session, and alias it; the newest carries its
	// agent, the other none.
	for _, id := range []string{"add-1", "add-2"} {
		r := byID[id]
		if r.Alias() != "venv/worktree//r/task" || r.Worktree == nil || r.Local == nil || !r.Current || r.Settled {
			t.Fatalf("%s: alias %q worktree %v local %v current %v settled %v", id, r.Alias(), r.Worktree, r.Local, r.Current, r.Settled)
		}
	}
	if r := byID["add-1"]; r.Agent == nil || r.Agent.ID != "venv/laatmux/%1" {
		t.Fatalf("add-1 did not take the worktree's agent: %v", r.Agent)
	}
	if r := byID["add-2"]; r.Agent != nil {
		t.Fatalf("add-2 took the agent the newer task holds: %v", r.Agent)
	}
	// Running and complete: the spinner's mark; the ones that need the
	// user "!". None is dim: the icon says which wants the user.
	for _, c := range []struct {
		id, mark, state, detail string
		dim                     bool
	}{
		{"add-1", "*", "done, awaiting the listing", "", false},
		{"add-2", "!", "prompt not delivered", "the pane was not ready", false},
		{"add-3", "*", "adding: fetch", "", false},
		{"add-4", "!", "host removed", "", false},
	} {
		r := byID[c.id]
		if r.Mark() != c.mark || r.State() != c.state || r.Detail() != c.detail || r.Dim != c.dim || r.Name == "" {
			t.Errorf("%s: mark %q state %q detail %q dim %v", c.id, r.Mark(), r.State(), r.Detail(), r.Dim)
		}
	}
	if !byID["add-4"].Removed || byID["add-3"].Removed {
		t.Error("removed is the host's absence from the host list")
	}
	if byID["add-3"].Alias() != "" {
		t.Error("an alias before the root is known")
	}
	// The agent and the local session before the worktree listing: the
	// task that reported the session holds the agent, both tasks for the
	// root take the local session, and neither is a line of its own.
	in.Worktrees = in.Worktrees[1:]
	nodes = Tree(in)
	under := ""
	for _, n := range nodes {
		if n.Depth <= 1 {
			under = n.ID()
		}
		if n.Pending == nil && (n.Depth != 2 || under != "add-1") && ((n.Agent != nil && n.Agent.Session == "proj/task") || (n.Local != nil && n.Local.Name == "vm/proj/task")) {
			t.Fatalf("the task's agent or session is a line of its own: %q orphaned %v", n.ID(), n.Orphaned)
		}
	}
	byID = nodesByID(nodes)
	if r := byID["add-1"]; r.Agent == nil || r.Agent.Session != "proj/task" || r.Children != 1 {
		t.Fatalf("add-1 did not take the agent in its session: %v", r.Agent)
	}
	for _, id := range []string{"add-1", "add-2"} {
		if r := byID[id]; r.Local == nil || !r.Current {
			t.Fatalf("%s did not take the local session: %v current %v", id, r.Local, r.Current)
		}
	}
	// Two tasks reporting the session: the newest holds the agent, the
	// other none.
	two := in
	two.Pendings = append([]protocol.Pending(nil), in.Pendings...)
	two.Pendings[1].Session = "proj/task"
	byID = nodesByID(Tree(two))
	if a, b := byID["add-1"], byID["add-2"]; a.Agent == nil || a.Children != 1 || b.Agent != nil || b.Children != 0 {
		t.Fatalf("two tasks in one session: add-1 %v add-2 %v", a.Agent, b.Agent)
	}
	// A later session that took the name, in another directory, is not
	// the task's: it is in other sessions.
	in.Agents[0].Cwd = "/scratch"
	own := false
	for _, n := range Tree(in) {
		if n.Depth == 1 && n.Kind == KindAgent && n.Agent.Session == "proj/task" {
			own = true
		}
		if n.ID() == "add-1" && (n.Agent != nil || n.Children != 0) {
			t.Fatal("the task took an agent in another directory")
		}
	}
	if !own {
		t.Fatal("the agent in another directory is not in other sessions")
	}
	// The host answering as another machine: the task says so and
	// needs the user, before the relay has recorded it.
	in.Agents[0].Cwd = "/r/task"
	in.Hosts[0].EnvironmentID = "wenv"
	for _, r := range Tree(in) {
		if r.ID() == "add-1" && (!r.Replaced || r.State() != "host replaced" || !r.NeedsUser()) {
			t.Fatalf("replaced: %v %q needs %v", r.Replaced, r.State(), r.NeedsUser())
		}
		if r.ID() == "add-3" && r.Replaced {
			t.Fatal("a task with no environment yet is replaced")
		}
	}
}

// PendingState says where a task is in a few words, the detail or the
// reason apart.
func TestPendingState(t *testing.T) {
	for _, c := range []struct {
		p             protocol.Pending
		removed       bool
		state, detail string
	}{
		{protocol.Pending{Mismatch: "venv is now wenv"}, false, "host replaced", "venv is now wenv"},
		{protocol.Pending{Done: true, Stage: protocol.StageFetch, Error: "no such ref"}, false, "failed at fetch", "no such ref"},
		{protocol.Pending{Done: true, Error: "outcome unknown: the daemon no longer knows it"}, false, "outcome unknown", "the daemon no longer knows it"},
		{protocol.Pending{Done: true, OK: true, Gone: true}, false, "done, worktree gone", ""},
		{protocol.Pending{Done: true, OK: true, Prompt: protocol.DeliveryUnknown, AttemptOpen: true, Attempt: 2}, false, "delivering the prompt", "attempt 2"},
		{protocol.Pending{Done: true, OK: true, Prompt: protocol.DeliveryUnknown, Error: "sent, not seen"}, false, "prompt delivery unknown", "sent, not seen"},
		{protocol.Pending{Done: true, OK: true, Prompt: protocol.DeliveryNotDelivered, AttemptError: "recovery expired", Error: "x"}, false, "prompt not delivered", "recovery expired"},
		{protocol.Pending{Done: true, OK: true, Prompt: protocol.DeliveryNone, ListingError: "git failed"}, false, "done, awaiting the listing", "git failed"},
		{protocol.Pending{Done: true, OK: true, Prompt: protocol.DeliveryNone, Listed: true}, false, "done", ""},
		{protocol.Pending{Unreachable: "ssh: timeout"}, false, "host unreachable, retrying", "ssh: timeout"},
		{protocol.Pending{}, false, "submitted", ""},
		{protocol.Pending{Taken: true, Reachable: true, Stage: protocol.StageAgent, Detail: "starting claude"}, false, "adding: agent", "starting claude"},
		{protocol.Pending{Taken: true, Reachable: true}, true, "host removed", ""},
	} {
		s, d := PendingState(c.p, c.removed)
		if s != c.state || d != c.detail {
			t.Errorf("%+v: %q %q, want %q %q", c.p, s, d, c.state, c.detail)
		}
	}
}

// A task that can no longer become the worktree line at its root does
// not stand for it: a worktree made again after one was gone, or after
// an add failed, is drawn, with its agent, beside the task's own line.
func TestPendingThatCannotStand(t *testing.T) {
	now := time.Now()
	for _, p := range []protocol.Pending{
		{ID: "add-1", Host: "vm", EnvironmentID: "venv", Repo: "proj", Branch: "b", Root: "/r/b", Session: "proj/b", Taken: true, Done: true, OK: true, Prompt: protocol.DeliveryNone, Listed: true, Gone: true, SubmittedAt: now},
		{ID: "add-1", Host: "vm", EnvironmentID: "venv", Repo: "proj", Branch: "b", Root: "/r/b", Session: "proj/b", Taken: true, Done: true, Stage: protocol.StageAgent, Error: "failed at agent: new-session: exit 1", SubmittedAt: now},
	} {
		nodes := nodesByID(Tree(Input{
			Hosts:     []Host{{Name: "vm", EnvironmentID: "venv", Connected: true, Listed: true, Worktrees: true}},
			Agents:    []protocol.Agent{{ID: "venv/laatmux/%1", EnvironmentID: "venv", Session: "proj/b", Cwd: "/r/b", Agent: "claude", Activity: protocol.Working, ActivityAt: now, Liveness: protocol.Alive, Managed: true}},
			Worktrees: []protocol.Worktree{{ID: "venv/worktree//r/b", EnvironmentID: "venv", Repo: "proj", Branch: "b", Root: "/r/b", Session: "proj/b"}},
			Pendings:  []protocol.Pending{p},
		}))
		task, wt := nodes["add-1"], nodes["venv/worktree//r/b"]
		if task.Kind != KindTask || wt.Kind != KindWorktree || wt.Agent == nil || wt.Children != 1 || task.Agent != nil || task.Worktree != nil || task.Children != 0 {
			t.Fatalf("%s: task %+v worktree %+v", p.Error, task, wt)
		}
		if p.Error != "" && (task.State() != "failed at agent" || task.Detail() != "new-session: exit 1") {
			t.Fatalf("failed: %q %q", task.State(), task.Detail())
		}
	}
}

// A task whose host was renamed in the config is on a removed host: it
// does not stand for the worktree line the new name lists, which is
// drawn with its agent.
func TestPendingOnRenamedHost(t *testing.T) {
	now := time.Now()
	lines := treeLines(Tree(Input{
		Hosts:     []Host{{Name: "new", EnvironmentID: "env", Connected: true, Listed: true, Worktrees: true}},
		Agents:    []protocol.Agent{{ID: "env/laatmux/%1", EnvironmentID: "env", Session: "proj/b", Cwd: "/r", Agent: "claude", Activity: protocol.Working, ActivityAt: now, Liveness: protocol.Alive, Managed: true}},
		Worktrees: []protocol.Worktree{{ID: "env/worktree//r", EnvironmentID: "env", Repo: "proj", Branch: "b", Root: "/r", Session: "proj/b"}},
		Pendings:  []protocol.Pending{{ID: "add-1", Host: "old", EnvironmentID: "env", Repo: "proj", Branch: "b", Root: "/r", Session: "proj/b", Taken: true, Done: true, OK: true, Prompt: protocol.DeliveryNotDelivered, SubmittedAt: now}},
	}))
	if len(lines) != 2 {
		t.Fatalf("%d lines", len(lines))
	}
	for _, r := range lines {
		if r.Pending != nil && (!r.Removed || r.Agent != nil || r.Alias() != "" || r.Children != 0) {
			t.Fatalf("task: removed %v agent %v alias %q", r.Removed, r.Agent, r.Alias())
		}
		if r.Pending == nil && (r.Kind != KindWorktree || r.Agent == nil || r.Host != "new") {
			t.Fatalf("worktree line: %+v", r)
		}
	}
	// Mid-add, the session a jump made at the task's root is not orphaned
	// while the renamed host has not listed the worktree yet.
	for _, n := range Tree(Input{
		Hosts:    []Host{{Name: "new", EnvironmentID: "env", Connected: true, Listed: true, Worktrees: true}},
		Locals:   []workspace.Local{{Name: "old/proj/b", Key: "env//r", Host: "old"}},
		Pendings: []protocol.Pending{{ID: "add-2", Host: "old", EnvironmentID: "env", Repo: "proj", Branch: "b", Root: "/r", Taken: true, Stage: protocol.StageSetup, SubmittedAt: now}},
	}) {
		if n.Orphaned {
			t.Fatalf("a session at a running add's root is orphaned: %+v", n)
		}
	}
	// The old name still configured, now answering as another machine,
	// and another name listing the task's machine: the task is replaced
	// and stands for nothing; the worktree line is drawn.
	for _, mismatch := range []string{"", "env is now other"} {
		p := protocol.Pending{ID: "add-1", Host: "vm", EnvironmentID: "env", Repo: "proj", Branch: "b", Root: "/r", Session: "proj/b", Taken: true, Done: true, OK: true, Prompt: protocol.DeliveryNotDelivered, Mismatch: mismatch, SubmittedAt: now}
		lines = treeLines(Tree(Input{
			Hosts:     []Host{{Name: "vm", EnvironmentID: "other", Connected: true, Listed: true, Worktrees: true}, {Name: "old-vm", EnvironmentID: "env", Connected: true, Listed: true, Worktrees: true}},
			Agents:    []protocol.Agent{{ID: "env/laatmux/%1", EnvironmentID: "env", Session: "proj/b", Cwd: "/r", Agent: "claude", Activity: protocol.Working, ActivityAt: now, Liveness: protocol.Alive, Managed: true}},
			Worktrees: []protocol.Worktree{{ID: "env/worktree//r", EnvironmentID: "env", Repo: "proj", Branch: "b", Root: "/r", Session: "proj/b"}},
			Pendings:  []protocol.Pending{p},
		}))
		if len(lines) != 2 {
			t.Fatalf("mismatch %q: %d lines", mismatch, len(lines))
		}
		for _, r := range lines {
			if r.Pending != nil && (r.Agent != nil || r.Alias() != "" || !r.NeedsUser() || r.Children != 0) {
				t.Fatalf("mismatch %q: task agent %v alias %q", mismatch, r.Agent, r.Alias())
			}
			if r.Pending == nil && (r.Kind != KindWorktree || r.Agent == nil || r.Host != "old-vm") {
				t.Fatalf("mismatch %q: worktree line %+v", mismatch, r)
			}
		}
	}
}

// From a host with attribution a worktree holds every agent the host
// attributed to it, from any session and server, by start time, and its
// line is jumped to through one of them: one in its home session, or,
// with no home session, the first started of the one laatmux made at its
// root and those on a default server. An agent attributed to no worktree
// is in other sessions. A host without attribution joins by session
// name. Build showed the chosen agent on the worktree's row and the
// others on rows of their own; the tree holds them all under the line,
// and the agent view has one tile per agent.
func TestJoinByWorktreeID(t *testing.T) {
	now := time.Now()
	wt := func(env, root, session string) protocol.Worktree {
		return protocol.Worktree{ID: env + "/worktree/" + root, EnvironmentID: env, Repo: "proj", Branch: root[3:], Root: root, Session: session}
	}
	in := Input{
		Hosts: []Host{
			{Name: "vm", EnvironmentID: "venv", Connected: true, Listed: true, Worktrees: true, Attribution: true},
			{Name: "old", EnvironmentID: "oenv", Connected: true, Listed: true, Worktrees: true},
		},
		Agents: []protocol.Agent{
			// /w/a: no home session, a split having left its session; the
			// agent laatmux made at the root and one on the default
			// server, the first started jumped through, and one in another
			// managed session, which the line holds too.
			{ID: "venv/default/%1", EnvironmentID: "venv", Server: "default", Session: "notes", Activity: protocol.Blocked, Liveness: protocol.Alive, ActivityAt: now, WorktreeID: "venv/worktree//w/a",
				Identity: &protocol.Identity{PID: 1, StartUnix: 200}},
			{ID: "venv/laatmux/%2", EnvironmentID: "venv", Session: "proj/a", Managed: true, Cwd: "/w/a", Activity: protocol.Idle, Liveness: protocol.Alive, ActivityAt: now.Add(-time.Hour), WorktreeID: "venv/worktree//w/a",
				Identity: &protocol.Identity{PID: 2, StartUnix: 100}},
			{ID: "venv/laatmux/%8", EnvironmentID: "venv", Session: "scratch", Managed: true, Cwd: "/w/a/sub", Activity: protocol.Blocked, Liveness: protocol.Alive, ActivityAt: now, WorktreeID: "venv/worktree//w/a",
				Identity: &protocol.Identity{PID: 8, StartUnix: 50}},
			// /w/b: the home session's idle agent is jumped through over a
			// working one elsewhere, which the line holds too.
			{ID: "venv/laatmux/%3", EnvironmentID: "venv", Session: "proj/b", Activity: protocol.Idle, Liveness: protocol.Alive, ActivityAt: now, WorktreeID: "venv/worktree//w/b"},
			{ID: "venv/default/%4", EnvironmentID: "venv", Server: "default", Session: "side", Activity: protocol.Working, Liveness: protocol.Alive, ActivityAt: now, WorktreeID: "venv/worktree//w/b"},
			// /w/c names a session whose agent is not attributed to it:
			// with attribution the name is not the link.
			{ID: "venv/laatmux/%5", EnvironmentID: "venv", Session: "proj/c", Activity: protocol.Idle, Liveness: protocol.Alive, ActivityAt: now},
			// /w/e has a home session with no agent in it: an agent
			// elsewhere is not jumped through; the line jumps to the home.
			{ID: "venv/default/%7", EnvironmentID: "venv", Server: "default", Session: "other", Activity: protocol.Idle, Liveness: protocol.Alive, ActivityAt: now, WorktreeID: "venv/worktree//w/e"},
			// On another observed server, which is not jumped to: never
			// the line's jump, though it started first.
			{ID: "venv//tmp/sock/%9", EnvironmentID: "venv", Server: "/tmp/sock", Session: "obs", Activity: protocol.Idle, Liveness: protocol.Alive, ActivityAt: now, WorktreeID: "venv/worktree//w/a",
				Identity: &protocol.Identity{PID: 9, StartUnix: 10}},
			// A host without attribution: by session, the field ignored.
			{ID: "oenv/laatmux/%6", EnvironmentID: "oenv", Session: "proj/d", Activity: protocol.Idle, Liveness: protocol.Alive, ActivityAt: now, WorktreeID: "oenv/worktree//w/x"},
		},
		Worktrees: []protocol.Worktree{wt("venv", "/w/a", ""), wt("venv", "/w/b", "proj/b"), wt("venv", "/w/c", "proj/c"), wt("oenv", "/w/d", "proj/d"), wt("venv", "/w/e", "proj/e")},
	}
	// under is the agents each line holds, and those in other sessions,
	// in the tree's order.
	under := func(nodes []Row) map[string]string {
		out := map[string][]string{}
		line := ""
		for _, n := range nodes {
			switch {
			case n.Kind == KindAgent && n.Depth == 1:
				out[NodeOther] = append(out[NodeOther], n.Agent.ID)
			case n.Kind == KindAgent:
				out[line] = append(out[line], n.Agent.ID)
			case n.Depth == 1:
				line = n.ID()
			}
		}
		joined := map[string]string{}
		for k, v := range out {
			joined[k] = strings.Join(v, " ")
		}
		return joined
	}
	nodes := Tree(in)
	byID := nodesByID(nodes)
	agentOf := func(id string) string {
		if a := byID[id].Agent; a != nil {
			return a.ID
		}
		return ""
	}
	for id, want := range map[string]string{
		"venv/worktree//w/a": "venv/laatmux/%2",
		"venv/worktree//w/b": "venv/laatmux/%3",
		"venv/worktree//w/c": "",
		"oenv/worktree//w/d": "oenv/laatmux/%6",
		"venv/worktree//w/e": "",
	} {
		if got := agentOf(id); got != want {
			t.Errorf("%s: agent %q, want %q", id, got, want)
		}
	}
	// What each line holds: every agent attributed to it, by start; the
	// unattributed one in other sessions.
	held := under(nodes)
	for id, want := range map[string]string{
		"venv/worktree//w/a": "venv//tmp/sock/%9 venv/laatmux/%8 venv/laatmux/%2 venv/default/%1",
		"venv/worktree//w/b": "venv/default/%4 venv/laatmux/%3",
		"venv/worktree//w/c": "",
		"oenv/worktree//w/d": "oenv/laatmux/%6",
		"venv/worktree//w/e": "venv/default/%7",
		NodeOther:            "venv/laatmux/%5",
	} {
		if held[id] != want {
			t.Errorf("%s holds %q, want %q", id, held[id], want)
		}
	}
	// One tile per agent, with the worktree it is attributed to; the
	// chosen ones are not repeated.
	rs := Agents(in)
	tiles := map[string][]string{}
	for _, r := range append(rs.Main, rs.Stale...) {
		w := ""
		if r.Worktree != nil {
			w = r.Worktree.ID
		}
		tiles[r.Agent.ID] = append(tiles[r.Agent.ID], w)
	}
	for id, want := range map[string]string{
		"venv/default/%1": "venv/worktree//w/a", "venv/laatmux/%2": "venv/worktree//w/a", "venv/laatmux/%8": "venv/worktree//w/a", "venv//tmp/sock/%9": "venv/worktree//w/a",
		"venv/laatmux/%3": "venv/worktree//w/b", "venv/default/%4": "venv/worktree//w/b",
		"venv/laatmux/%5": "", "oenv/laatmux/%6": "oenv/worktree//w/d", "venv/default/%7": "venv/worktree//w/e",
	} {
		if got := tiles[id]; len(got) != 1 || got[0] != want {
			t.Errorf("%s: tiles under %q, want one under %q", id, got, want)
		}
	}
	if len(tiles) != len(in.Agents) {
		t.Errorf("%d agents with tiles, want %d", len(tiles), len(in.Agents))
	}
	// The choice does not turn on activity: the two agents of /w/a
	// swapping which works keep the line's jump where it was.
	in.Agents[0].Activity, in.Agents[1].Activity = protocol.Idle, protocol.Blocked
	for _, r := range Tree(in) {
		if r.ID() == "venv/worktree//w/a" && (r.Agent == nil || r.Agent.ID != "venv/laatmux/%2") {
			t.Errorf("the line's agent changed with activity: %+v", r.Agent)
		}
	}
	// The same records through a view that has no attribution for the
	// host, a merging daemon older than it say: by session name.
	in.Hosts[0].Attribution = false
	nodes = Tree(in)
	byID = nodesByID(nodes)
	if a := byID["venv/worktree//w/c"].Agent; a == nil || a.ID != "venv/laatmux/%5" {
		t.Errorf("fallback: /w/c agent %+v", a)
	}
	if a := byID["venv/worktree//w/a"].Agent; a != nil {
		t.Errorf("fallback: /w/a agent %+v", a)
	}
	if held := under(nodes); held["venv/worktree//w/a"] != "" || held["venv/worktree//w/c"] != "venv/laatmux/%5" {
		t.Errorf("fallback: /w/a holds %q, /w/c holds %q", held["venv/worktree//w/a"], held["venv/worktree//w/c"])
	}
}

// Two agents in one home session: the worktree line holds both, each
// once, in one order and jumped to through the same one whatever order
// the records come in. Build showed one on the worktree's row and the
// other on a row of its own, and without attribution paired them through
// a last-wins map fed in the records' order; the tree sorts the agents
// first.
func TestTwoAgentsOneSession(t *testing.T) {
	now := time.Now()
	a := protocol.Agent{ID: "venv/laatmux/%1", EnvironmentID: "venv", Session: "proj/a", Activity: protocol.Idle, Liveness: protocol.Alive, ActivityAt: now, WorktreeID: "venv/worktree//w/a"}
	b := protocol.Agent{ID: "venv/laatmux/%2", EnvironmentID: "venv", Session: "proj/a", Activity: protocol.Idle, Liveness: protocol.Alive, ActivityAt: now, WorktreeID: "venv/worktree//w/a"}
	for _, attribution := range []bool{true, false} {
		for _, agents := range [][]protocol.Agent{{a, b}, {b, a}} {
			in := Input{
				Hosts:     []Host{{Name: "vm", EnvironmentID: "venv", Connected: true, Listed: true, Worktrees: true, Attribution: attribution}},
				Agents:    agents,
				Worktrees: []protocol.Worktree{{ID: "venv/worktree//w/a", EnvironmentID: "venv", Repo: "proj", Branch: "a", Root: "/w/a", Session: "proj/a"}},
			}
			var jump string
			var held []string
			nodes := map[string]int{}
			for _, n := range Tree(in) {
				switch n.Kind {
				case KindWorktree:
					if n.Agent != nil {
						jump = n.Agent.ID
					}
				case KindAgent:
					held = append(held, n.Agent.ID)
					nodes[n.Agent.ID]++
				}
			}
			tiles := map[string]int{}
			var suffixes []string
			rs := Agents(in)
			for _, r := range append(rs.Main, rs.Stale...) {
				if r.Agent != nil {
					tiles[r.Agent.ID]++
					suffixes = append(suffixes, r.Agent.ID+" "+r.Suffix)
				}
			}
			if nodes[a.ID] != 1 || nodes[b.ID] != 1 || tiles[a.ID] != 1 || tiles[b.ID] != 1 {
				t.Errorf("attribution %v, order %s first: agents held %v, tiles %v", attribution, agents[0].ID, nodes, tiles)
			}
			if got := jump + " | " + strings.Join(held, " "); got != a.ID+" | "+a.ID+" "+b.ID {
				t.Errorf("attribution %v, order %s first: jump | held %s", attribution, agents[0].ID, got)
			}
			if got := strings.Join(suffixes, ", "); got != a.ID+" (1), "+b.ID+" (2)" {
				t.Errorf("attribution %v, order %s first: tiles %s", attribution, agents[0].ID, got)
			}
		}
	}
	// Without a worktree the tree's own sort is what orders them: the
	// other-sessions lines, the tiles' suffixes, and which of the two a
	// loose task at their root adopts, the first in that order and it
	// alone, in either record order.
	a.Cwd, b.Cwd = "/w/a", "/w/a"
	p := protocol.Pending{ID: "add-1", Host: "vm", EnvironmentID: "venv", Repo: "proj", Branch: "a", Root: "/w/a", Session: "proj/a", Taken: true, SubmittedAt: now}
	hosts := []Host{{Name: "vm", EnvironmentID: "venv", Connected: true, Listed: true, Worktrees: true}}
	for _, agents := range [][]protocol.Agent{{a, b}, {b, a}} {
		var held, tiles []string
		for _, n := range Tree(Input{Hosts: hosts, Agents: agents}) {
			if n.Agent != nil {
				held = append(held, n.Agent.ID)
			}
		}
		for _, r := range Agents(Input{Hosts: hosts, Agents: agents}).Main {
			tiles = append(tiles, r.Agent.ID+" "+r.Suffix)
		}
		if got := strings.Join(held, " ") + " | " + strings.Join(tiles, ", "); got != a.ID+" "+b.ID+" | "+a.ID+" (1), "+b.ID+" (2)" {
			t.Errorf("no worktree, %s first: %s", agents[0].ID, got)
		}
		for _, n := range Tree(Input{Hosts: hosts, Agents: agents, Pendings: []protocol.Pending{p}}) {
			if n.ID() == "add-1" && (n.Agent == nil || n.Agent.ID != a.ID || n.Children != 1) {
				t.Errorf("task, %s first: adopted %+v, %d children", agents[0].ID, n.Agent, n.Children)
			}
		}
	}
}

// A worktree line with no home session whose agent is on this machine's
// default server stands for that session: the viewer in it is on the
// line, and on the agent's tile.
func TestWorktreeLineTakesAgentSession(t *testing.T) {
	in := Input{
		Hosts: []Host{{Name: "mac", Local: true, EnvironmentID: "menv", Connected: true, Listed: true, Worktrees: true, Attribution: true}},
		Agents: []protocol.Agent{{ID: "menv/default/%1", EnvironmentID: "menv", Server: "default", Session: "notes", Activity: protocol.Idle,
			Liveness: protocol.Alive, WorktreeID: "menv/worktree//w/a"}},
		Worktrees: []protocol.Worktree{{ID: "menv/worktree//w/a", EnvironmentID: "menv", Repo: "proj", Branch: "a", Root: "/w/a"}},
		Locals:    []workspace.Local{{Name: "notes"}},
		Current:   "notes",
	}
	lines := treeLines(Tree(in))
	if len(lines) != 1 || lines[0].Worktree == nil || lines[0].Local == nil || lines[0].Local.Name != "notes" || !lines[0].Current {
		t.Fatalf("lines %+v", lines)
	}
	rs := Agents(in)
	if len(rs.Main) != 1 || len(rs.Stale) != 0 || rs.Main[0].Worktree == nil || rs.Main[0].Local == nil || rs.Main[0].Local.Name != "notes" || !rs.Main[0].Current {
		t.Fatalf("tiles %+v", rs)
	}
}

// A homeless worktree line stands for the session its jump goes to: the
// worktree's workspace session when its agent is the one laatmux made at
// the root, never a plain attachment to that session; the agent's own
// session when the agent is on this machine's default server, whatever
// workspace session is left.
func TestHomelessLineLocal(t *testing.T) {
	hosts := []Host{{Name: "mac", Local: true, EnvironmentID: "menv", Connected: true, Listed: true, Worktrees: true, Attribution: true}}
	w := protocol.Worktree{ID: "menv/worktree//w/a", EnvironmentID: "menv", Repo: "proj", Branch: "a", Root: "/w/a"}
	locals := []workspace.Local{{Name: "mac/proj/a", Key: "menv//w/a", Host: "mac"}, {Name: "mac/proj/a-old", Attach: "mac/proj/a", Host: "mac"}, {Name: "notes"}}
	managed := protocol.Agent{ID: "menv/laatmux/%1", EnvironmentID: "menv", Session: "proj/a", Managed: true, Cwd: "/w/a", Liveness: protocol.Alive, WorktreeID: w.ID}
	first := Input{Hosts: hosts, Agents: []protocol.Agent{managed}, Worktrees: []protocol.Worktree{w}, Locals: locals, Current: "mac/proj/a"}
	got := treeLines(Tree(first))
	if len(got) != 1 || got[0].Local == nil || got[0].Local.Name != "mac/proj/a" || !got[0].Current {
		t.Fatalf("managed agent: %+v", got)
	}
	// The agent's tile stands for the plain attachment to its session,
	// not the workspace session the line has: only an agent in the home
	// session takes that. Build's row, in the dashboard's one list before
	// the two views, had the workspace session; whether the tile should
	// is #85.
	if rs := Agents(first); len(rs.Main) != 1 || !rs.Main[0].Current || rs.Main[0].Local == nil || rs.Main[0].Local.Name != "mac/proj/a-old" {
		t.Fatalf("managed agent's tile: %+v", rs)
	}
	// With no workspace session yet, the plain attachment to the same
	// managed session is not the line's either. Build left the viewer in
	// the attachment off the row; the tree has the line the viewer's
	// through its agent, whose own session the attachment is, as
	// following wants it.
	nodes := Tree(Input{Hosts: hosts, Agents: []protocol.Agent{managed}, Worktrees: []protocol.Worktree{w}, Locals: locals[1:], Current: "mac/proj/a-old"})
	if got = treeLines(nodes); len(got) != 1 || got[0].Local != nil || !got[0].Current {
		t.Fatalf("managed agent without a workspace session: %+v", got)
	}
	if c := nodes[2]; c.Kind != KindAgent || c.Local == nil || c.Local.Name != "mac/proj/a-old" {
		t.Fatalf("the agent's own session: %+v", c)
	}
	// An agent on the default server: its session, not the workspace
	// session left, which, settled, does not settle the line.
	locals[0].Settled = true
	notes := protocol.Agent{ID: "menv/default/%2", EnvironmentID: "menv", Server: "default", Session: "notes", Liveness: protocol.Alive, WorktreeID: w.ID}
	in := Input{Hosts: hosts, Agents: []protocol.Agent{notes}, Worktrees: []protocol.Worktree{w}, Locals: locals, Current: "notes"}
	if got = treeLines(Tree(in)); len(got) != 1 || got[0].Worktree == nil || got[0].Local == nil || got[0].Local.Name != "notes" || !got[0].Current || got[0].Settled {
		t.Fatalf("default-server agent: %+v", got)
	}
	rs := Agents(in)
	if len(rs.Main) != 1 || rs.Main[0].Worktree == nil || rs.Main[0].Local == nil || rs.Main[0].Local.Name != "notes" || !rs.Main[0].Current || rs.Main[0].Settled {
		t.Fatalf("default-server agent's tile: %+v", rs)
	}
	// The agent in the worktree's workspace session itself: the line is
	// settled as that session is, and its tile in the Stale fold.
	inWorkspace := notes
	inWorkspace.Session = "mac/proj/a"
	in = Input{Hosts: hosts, Agents: []protocol.Agent{inWorkspace}, Worktrees: []protocol.Worktree{w}, Locals: locals}
	if got = treeLines(Tree(in)); len(got) != 1 || got[0].Local == nil || got[0].Local.Name != "mac/proj/a" || !got[0].Settled {
		t.Fatalf("agent in the workspace session: %+v", got)
	}
	rs = Agents(in)
	if len(rs.Main) != 0 || len(rs.Stale) != 1 || rs.Stale[0].Local == nil || rs.Stale[0].Local.Name != "mac/proj/a" || !rs.Stale[0].Settled {
		t.Fatalf("agent in the workspace session's tile: %+v", rs)
	}
	// On a remote host's default server the agent's session is not
	// this machine's: the line keeps the workspace session.
	remote := []Host{{Name: "vm", EnvironmentID: "venv", Connected: true, Listed: true, Worktrees: true, Attribution: true}}
	rw := protocol.Worktree{ID: "venv/worktree//w/a", EnvironmentID: "venv", Repo: "proj", Branch: "a", Root: "/w/a"}
	ra := protocol.Agent{ID: "venv/default/%3", EnvironmentID: "venv", Server: "default", Session: "notes", Liveness: protocol.Alive, WorktreeID: rw.ID}
	remoteIn := Input{Hosts: remote, Agents: []protocol.Agent{ra}, Worktrees: []protocol.Worktree{rw},
		Locals: []workspace.Local{{Name: "vm/proj/a", Key: "venv//w/a", Host: "vm"}}, Current: "vm/proj/a"}
	got = treeLines(Tree(remoteIn))
	if len(got) != 1 || got[0].Local == nil || got[0].Local.Name != "vm/proj/a" || !got[0].Current {
		t.Fatalf("remote default-server agent: %+v", got)
	}
	// Its tile too, through the child's fallback to the line's session.
	rs = Agents(remoteIn)
	if len(rs.Main) != 1 || rs.Main[0].Local == nil || rs.Main[0].Local.Name != "vm/proj/a" || !rs.Main[0].Current {
		t.Fatalf("remote default-server agent's tile: %+v", rs)
	}
}

// The labels: the branch primary and the repository secondary; on main
// or master the repository primary; a detached worktree by its root; a
// task by its branch; a row that is no worktree's by its session alone.
func TestLabels(t *testing.T) {
	for _, c := range []struct {
		r    Row
		p, s string
	}{
		{Row{Worktree: &protocol.Worktree{Repo: "laatmux", Branch: "fix-ls", Root: "/w/fix-ls"}}, "fix-ls", "laatmux"},
		{Row{Worktree: &protocol.Worktree{Repo: "laatmux", Branch: "main", Root: "/w/main"}}, "laatmux", "main"},
		{Row{Worktree: &protocol.Worktree{Repo: "laatmux", Branch: "master", Root: "/w/m"}}, "laatmux", "master"},
		{Row{Worktree: &protocol.Worktree{Repo: "laatmux", Root: "/w/probe"}}, "probe", "laatmux detached"},
		{Row{Pending: &protocol.Pending{Repo: "proj", Branch: "task"}}, "task", "proj"},
		{Row{Name: "scratch", Agent: &protocol.Agent{Session: "scratch"}}, "scratch", ""},
		{Row{Name: "vm/proj/gone", Orphaned: true}, "vm/proj/gone", ""},
	} {
		if p, s := c.r.Labels(); p != c.p || s != c.s {
			t.Errorf("%+v: %q %q, want %q %q", c.r, p, s, c.p, c.s)
		}
	}
}

// Precedence: done beats stale, blocked is never stale, a settled
// workspace's blocked or done agent stays in place and is not dim, and a
// settled one that does not want the user folds away; a stale agent is
// dim when stale tiles are, and folds when they fold.
func TestPrecedence(t *testing.T) {
	now := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
	agent := func(pane, session string, act protocol.Activity, ago time.Duration) protocol.Agent {
		return protocol.Agent{ID: "venv/laatmux/" + pane, EnvironmentID: "venv", Session: session, Agent: "claude", Activity: act,
			ActivityAt: now.Add(-ago), Liveness: protocol.Alive, Managed: true}
	}
	in := Input{
		Hosts: []Host{{Name: "vm", EnvironmentID: "venv", Connected: true, Listed: true}},
		Agents: []protocol.Agent{
			agent("%1", "done-old", protocol.Idle, 5*time.Hour),
			agent("%2", "blocked-old", protocol.Blocked, 5*time.Hour),
			agent("%3", "idle-old", protocol.Idle, 5*time.Hour),
			agent("%4", "idle-new", protocol.Idle, time.Minute),
			agent("%5", "working", protocol.Working, 5*time.Hour),
			agent("%6", "seen", protocol.Idle, time.Minute),
		},
		Attention: map[string]protocol.Attention{
			"venv/laatmux/%1": {AgentID: "venv/laatmux/%1", FinishedAt: now.Add(-5 * time.Hour)},
			"venv/laatmux/%6": {AgentID: "venv/laatmux/%6", FinishedAt: now.Add(-time.Minute), SeenAt: now},
		},
		Now: now, StaleAfter: time.Hour, DimStale: true, CollapseStale: true,
	}
	rs := Agents(in)
	names := func(rs []Row) string {
		var out []string
		for _, r := range rs {
			flags := ""
			if r.Done {
				flags += "+done"
			}
			if r.Stale {
				flags += "+stale"
			}
			if r.Dim {
				flags += "+dim"
			}
			out = append(out, r.Name+flags)
		}
		return strings.Join(out, " ")
	}
	if got, want := names(rs.Main), "blocked-old done-old+done working idle-new seen"; got != want {
		t.Errorf("main: %s, want %s", got, want)
	}
	if got, want := names(rs.Stale), "idle-old+stale+dim"; got != want {
		t.Errorf("stale: %s, want %s", got, want)
	}
	in.DimStale, in.CollapseStale = false, false
	rs = Agents(in)
	if got, want := names(rs.Main), "blocked-old done-old+done working idle-new seen idle-old+stale"; got != want || len(rs.Stale) != 0 {
		t.Errorf("stale kept in place and not dim: %s, want %s", got, want)
	}

	// Settled workspaces: the blocked and the done agents stay; the other
	// folds into the Stale fold, where the settled group was.
	in = Input{
		Hosts: []Host{{Name: "vm", EnvironmentID: "venv", Connected: true, Listed: true, Worktrees: true, Attribution: true}},
		Agents: []protocol.Agent{
			agent("%1", "a", protocol.Blocked, time.Minute),
			agent("%2", "b", protocol.Idle, time.Minute),
			agent("%3", "c", protocol.Working, time.Minute),
		},
		Attention: map[string]protocol.Attention{"venv/laatmux/%2": {AgentID: "venv/laatmux/%2", FinishedAt: now}},
		Now:       now, StaleAfter: time.Hour, DimStale: true, CollapseStale: true,
	}
	for i, s := range []string{"a", "b", "c"} {
		root := "/w/" + s
		in.Agents[i].WorktreeID = "venv/worktree/" + root
		in.Worktrees = append(in.Worktrees, protocol.Worktree{ID: "venv/worktree/" + root, EnvironmentID: "venv", Repo: "proj", Branch: s, Root: root, Session: s})
		in.Locals = append(in.Locals, workspace.Local{Name: "vm/proj/" + s, Key: workspace.Key("venv", root), Host: "vm", Settled: true})
	}
	rs = Agents(in)
	if got, want := names(rs.Main), "a b+done"; got != want {
		t.Errorf("settled, main: %s, want %s", got, want)
	}
	if got, want := names(rs.Stale), "c+dim"; got != want {
		t.Errorf("settled: %s, want %s", got, want)
	}
}

// The sort orders: priority by status, recency by the last change,
// window by session then window; tasks first in all three.
func TestSortOrders(t *testing.T) {
	now := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
	agent := func(pane, session string, window int, act protocol.Activity, ago time.Duration) protocol.Agent {
		return protocol.Agent{ID: "venv/laatmux/" + pane, EnvironmentID: "venv", Session: session, Window: window, Agent: "claude",
			Activity: act, ActivityAt: now.Add(-ago), Liveness: protocol.Alive, Managed: true}
	}
	in := Input{
		Hosts: []Host{{Name: "vm", EnvironmentID: "venv", Connected: true, Listed: true}},
		Agents: []protocol.Agent{
			agent("%1", "b", 1, protocol.Idle, time.Second),
			agent("%2", "a", 2, protocol.Blocked, time.Hour),
			agent("%3", "b", 0, protocol.Working, time.Minute),
			agent("%4", "a", 1, protocol.Idle, 2*time.Hour),
		},
		Pendings: []protocol.Pending{{ID: "t1", Host: "vm", Repo: "proj", Branch: "task", SubmittedAt: now}},
		Now:      now,
	}
	for order, want := range map[string]string{
		"":             "proj/task a b b a",
		SortPriority:   "proj/task a b b a",
		SortRecency:    "proj/task b b a a",
		SortWindow:     "proj/task a a b b",
		"not-an-order": "proj/task a b b a",
	} {
		in.Sort = order
		var got []string
		var panes []string
		for _, r := range Agents(in).Main {
			got = append(got, r.Name)
			if r.Agent != nil {
				panes = append(panes, r.Agent.ID[len("venv/laatmux/"):])
			}
		}
		if strings.Join(got, " ") != want {
			t.Errorf("%q: %v, want %s", order, got, want)
		}
		wantPanes := map[string]string{"": "%2 %3 %1 %4", SortPriority: "%2 %3 %1 %4", SortRecency: "%1 %3 %2 %4", SortWindow: "%4 %2 %3 %1", "not-an-order": "%2 %3 %1 %4"}[order]
		if strings.Join(panes, " ") != wantPanes {
			t.Errorf("%q: panes %v, want %s", order, panes, wantPanes)
		}
	}
}

// The viewer's own tile is never folded away, stale or settled: the pane
// always shows the session it sits in.
func TestCurrentNotFolded(t *testing.T) {
	now := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
	in := Input{
		Hosts: []Host{{Name: "vm", EnvironmentID: "venv", Connected: true, Listed: true}},
		Agents: []protocol.Agent{{ID: "venv/laatmux/%1", EnvironmentID: "venv", Session: "s", Agent: "claude", Activity: protocol.Idle,
			ActivityAt: now.Add(-5 * time.Hour), Liveness: protocol.Alive, Managed: true}},
		Locals:  []workspace.Local{{Name: "vm/s", Attach: "vm/s", Host: "vm"}},
		Current: "vm/s",
		Now:     now, StaleAfter: time.Hour, DimStale: true, CollapseStale: true,
	}
	rs := Agents(in)
	if len(rs.Main) != 1 || !rs.Main[0].Stale || !rs.Main[0].Current || len(rs.Stale) != 0 {
		t.Errorf("main %+v, stale %+v", rs.Main, rs.Stale)
	}
	// A workspace settled from the sidebar with z: still there, dim, to
	// unsettle.
	in.Agents[0].ActivityAt = now
	in.Agents[0].Session = "proj/w"
	in.Worktrees = []protocol.Worktree{{ID: "venv/worktree//w", EnvironmentID: "venv", Repo: "proj", Branch: "w", Root: "/w", Session: "proj/w"}}
	in.Locals = []workspace.Local{{Name: "vm/proj/w", Key: workspace.Key("venv", "/w"), Host: "vm", Settled: true}}
	in.Current = "vm/proj/w"
	rs = Agents(in)
	if len(rs.Main) != 1 || !rs.Main[0].Settled || !rs.Main[0].Dim || !rs.Main[0].Current || len(rs.Stale) != 0 {
		t.Errorf("settled: main %+v, stale %+v", rs.Main, rs.Stale)
	}
	// It sorts with the stale ones, below an idle agent, though working.
	in.Agents[0].Activity = protocol.Working
	in.Agents = append(in.Agents, protocol.Agent{ID: "venv/laatmux/%2", EnvironmentID: "venv", Session: "other", Agent: "claude",
		Activity: protocol.Idle, ActivityAt: now, Liveness: protocol.Alive, Managed: true})
	rs = Agents(in)
	if len(rs.Main) != 2 || rs.Main[0].Name != "other" || !rs.Main[1].Current {
		t.Errorf("settled own tile sorts: %+v", rs.Main)
	}
}

// A worktree line takes its branch's PR record by the source key, so the
// ssh and https forms of one repository find the same record.
func TestBranchAttached(t *testing.T) {
	in := Input{
		Hosts:     []Host{{Name: "vm", EnvironmentID: "venv", Connected: true, Listed: true, Worktrees: true}},
		Worktrees: []protocol.Worktree{{ID: "venv/worktree//w/a", EnvironmentID: "venv", Repo: "r", Source: "https://github.com/o/r.git", Branch: "a", Root: "/w/a"}},
		Branches: map[protocol.BranchKey]protocol.BranchStatus{
			{Source: config.SourceKey("git@github.com:o/r.git"), Branch: "a"}: {PR: &protocol.PullRequest{Number: 3}},
		},
	}
	lines := treeLines(Tree(in))
	if len(lines) != 1 || lines[0].Branch == nil || lines[0].Branch.PR.Number != 3 {
		t.Errorf("%+v", lines)
	}
}
