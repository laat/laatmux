package rows

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/laat/laatmux/internal/protocol"
	"github.com/laat/laatmux/internal/source"
)

// treeInput is two repositories on two hosts: laatmux with a worktree
// holding two agents, a shell and a run, one with no agent, and one
// only a task stands for yet; proj with a worktree and an orphaned
// session; an observed agent and a plain attachment in other sessions.
func treeInput(now time.Time) Input {
	src := "git@github.com:laat/laatmux.git"
	agent := func(id, env, session, name string, act protocol.Activity, start int64, wt string) protocol.Agent {
		return protocol.Agent{ID: id, Server: "laatmux", EnvironmentID: env, Session: session, Agent: name, Activity: act, ActivityAt: now.Add(-time.Minute),
			Liveness: protocol.Alive, Managed: true, Identity: &protocol.Identity{PID: 1, StartUnix: start}, WorktreeID: wt, Title: name + " title"}
	}
	return Input{
		Hosts: []Host{
			{Name: "mac", Local: true, EnvironmentID: "menv", Connected: true, Listed: true, Worktrees: true, Attribution: true},
			{Name: "vm", EnvironmentID: "venv", Connected: true, Listed: true, Worktrees: true, Attribution: true},
		},
		Agents: []protocol.Agent{
			agent("venv/laatmux/%2", "venv", "laatmux/agents-config", "codex", protocol.Idle, 20, "venv/worktree//r/agents-config"),
			agent("venv/laatmux/%1", "venv", "laatmux/agents-config", "claude", protocol.Blocked, 10, "venv/worktree//r/agents-config"),
			agent("menv/laatmux/%3", "menv", "proj/batch", "claude", protocol.Working, 30, "menv/worktree//w/batch"),
			withCwd(agent("venv/laatmux/%4", "venv", "laatmux/new-one", "claude", protocol.Working, 40, ""), "/r/new-one"),
			{ID: "venv/default/%5", EnvironmentID: "venv", Server: "default", Session: "scratch", Agent: "claude", Activity: protocol.Blocked, ActivityAt: now, Liveness: protocol.Alive},
			{ID: "menv/laatmux/%6", EnvironmentID: "menv", Server: "laatmux", Session: "new", Agent: "claude", Activity: protocol.Idle, ActivityAt: now, Liveness: protocol.Alive, Managed: true},
		},
		Worktrees: []protocol.Worktree{
			{ID: "venv/worktree//r/agents-config", EnvironmentID: "venv", Repo: "laatmux", Source: src, Branch: "agents-config", Root: "/r/agents-config", Session: "laatmux/agents-config"},
			{ID: "venv/worktree//r/auto-layout", EnvironmentID: "venv", Repo: "laatmux", Source: src, Branch: "auto-layout", Root: "/r/auto-layout"},
			{ID: "menv/worktree//w/batch", EnvironmentID: "menv", Repo: "proj", Source: "https://github.com/laat/proj", Branch: "batch", Root: "/w/batch", Session: "proj/batch"},
		},
		Panes: []protocol.Pane{{ID: "venv/pane/laatmux/%7", EnvironmentID: "venv", Session: "laatmux/agents-config", Window: 1, PaneID: "%7", Command: "zsh", WorktreeID: "venv/worktree//r/agents-config"}},
		Runs:  []protocol.Run{{ID: "venv/run/r1", EnvironmentID: "venv", Root: "/r/agents-config", WorktreeID: "venv/worktree//r/agents-config", Cmd: []string{"make", "test"}, StartedAt: now.Add(-42 * time.Second)}},
		Locals: []protocol.Session{
			{Name: "vm/laatmux/agents-config", Key: "venv//r/agents-config", Host: "vm"},
			{Name: "mac/proj/batch", Key: "menv//w/batch", Host: "mac"},
			{Name: "mac/proj/gone", Key: "menv//w/gone", Host: "mac", Source: "https://github.com/laat/proj"},
			{Name: "vm/lost", Key: "venv//r/lost", Host: "vm"},
			{Name: "mac/new", Attach: "mac/new", Host: "mac"},
		},
		Pendings: []protocol.Pending{{ID: "add-1", Host: "vm", EnvironmentID: "venv", Source: src, Repo: "laatmux", Branch: "new-one", Root: "/r/new-one", Session: "laatmux/new-one", Taken: true, SubmittedAt: now}},
		Current:  "vm/laatmux/agents-config",
		Now:      now, StaleAfter: time.Hour, DimStale: true, CollapseStale: true,
	}
}

func withCwd(a protocol.Agent, cwd string) protocol.Agent {
	a.Cwd = cwd
	return a
}

// outline is one line per node: its label, a child by its name.
func outline(nodes []Row) string {
	var b strings.Builder
	for _, n := range nodes {
		p, _ := n.Labels()
		if n.Depth == 2 {
			p = n.Name
		}
		b.WriteString(strings.Repeat("  ", n.Depth) + p)
		if n.Suffix != "" {
			b.WriteString(" " + n.Suffix)
		}
		if n.Current {
			b.WriteString(" *")
		}
		b.WriteString("\n")
	}
	return b.String()
}

// The tree: repositories by name, worktrees by branch with agents by
// start, panes and runs under them, a task's line before the listing
// holding its agent, orphaned sessions under their repository or in
// other sessions, and other sessions last.
func TestTree(t *testing.T) {
	now := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	nodes := Tree(treeInput(now))
	want := `laatmux
  agents-config *
    laatmux/agents-config
    laatmux/agents-config
    zsh
    make test
  auto-layout
  new-one
    laatmux/new-one
proj
  batch
    proj/batch
  mac/proj/gone
other sessions
  new
  scratch
  vm/lost
`
	if got := outline(nodes); got != want {
		t.Errorf("tree:\n%s\nwant:\n%s", got, want)
	}
	var wt Row
	for _, n := range nodes {
		if n.Node == "venv/worktree//r/agents-config" {
			wt = n
		}
	}
	if wt.Children != 4 || wt.Agent == nil || wt.Agent.Activity != protocol.Blocked || !wt.Foldable() {
		t.Errorf("worktree line: children %d, pressing %+v", wt.Children, wt.Agent)
	}
}

// A record naming no server is on none the tree knows: attributed to
// its worktree it is a line under it, but not the agent the worktree's
// own line shows and jumps through, with no local session of its own;
// unattributed, it is listed among the observed sessions after the
// managed agents.
func TestTreeServerlessAgent(t *testing.T) {
	now := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	in := treeInput(now)
	in.Worktrees[1].Session = "laatmux/auto-layout"
	a := protocol.Agent{ID: "venv//%8", EnvironmentID: "venv", Session: "laatmux/auto-layout", Agent: "claude", Activity: protocol.Blocked, ActivityAt: now, Liveness: protocol.Alive, Managed: true, WorktreeID: "venv/worktree//r/auto-layout"}
	in.Agents = append(in.Agents, a)
	find := func(nodes []Row) (wt, agent *Row) {
		for i := range nodes {
			switch nodes[i].Node {
			case "venv/worktree//r/auto-layout":
				wt = &nodes[i]
			case "venv//%8":
				agent = &nodes[i]
			}
		}
		return wt, agent
	}
	wt, agent := find(Tree(in))
	if wt == nil || wt.Agent != nil || wt.Children != 1 {
		t.Fatalf("worktree line took the serverless agent: %+v", wt)
	}
	if agent == nil || agent.Depth != 2 || agent.Local != nil {
		t.Fatalf("serverless agent's line: %+v", agent)
	}
	// Without attribution the worktree's agents are found by server and
	// session, which a serverless record never is.
	in.Hosts[1].Attribution = false
	in.Agents[len(in.Agents)-1].WorktreeID = ""
	wt, agent = find(Tree(in))
	if wt == nil || wt.Agent != nil || wt.Children != 0 {
		t.Fatalf("worktree line matched the serverless agent by session: %+v", wt)
	}
	if agent == nil || agent.Depth != 1 || agent.Local != nil {
		t.Fatalf("serverless agent's row: %+v", agent)
	}
}

// The agent view: one tile per agent, the task first, agents of one
// worktree numbered in the tree's order, the viewer's agents current.
func TestAgents(t *testing.T) {
	now := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	in := treeInput(now)
	rs := Agents(in, Tree(in))
	// The task, then blocked (the newest activity first), working, idle.
	want := `new-one
scratch
agents-config (1) *
batch
laatmux/new-one
new
agents-config (2) *
`
	if got := outline(rs.Main); got != want || len(rs.Stale) != 0 {
		t.Errorf("agents:\n%s\nwant:\n%s\nstale %d", got, want, len(rs.Stale))
	}
	// A task's tile and its worktree's agent, the moment both are
	// listed, are not numbered as a pair.
	in = treeInput(now)
	in.Pendings = append(in.Pendings, protocol.Pending{ID: "add-2", Host: "vm", EnvironmentID: "venv", Source: "git@github.com:laat/laatmux.git", Repo: "laatmux", Branch: "auto-layout", Root: "/r/auto-layout", Session: "laatmux/auto-layout", Taken: true, SubmittedAt: now})
	for _, r := range Agents(in, Tree(in)).Main {
		if r.Suffix != "" && (r.Pending != nil || r.Name == "auto-layout") {
			t.Errorf("%s numbered %q beside its task", r.Name, r.Suffix)
		}
	}
}

// Tree contents the note lists: an older host's agents placed by
// session; an orphaned session with no source tag in other sessions;
// two tasks for one worktree, the newest owning the children and the
// other a line without; a task's line holding the add's agent before the
// listing; a task that failed after the worktree was made, a line of its
// own beside the worktree's.
func TestTreeContents(t *testing.T) {
	now := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	src := "git@github.com:laat/proj.git"
	in := Input{
		Hosts: []Host{
			{Name: "old", EnvironmentID: "oenv", Connected: true, Listed: true, Worktrees: true}, // no attribution
			{Name: "vm", EnvironmentID: "venv", Connected: true, Listed: true, Worktrees: true, Attribution: true},
		},
		Agents: []protocol.Agent{
			{ID: "oenv/laatmux/%1", EnvironmentID: "oenv", Server: "laatmux", Session: "proj/a", Agent: "claude", Activity: protocol.Working, ActivityAt: now, Liveness: protocol.Alive, Managed: true},
			{ID: "oenv/laatmux/%2", EnvironmentID: "oenv", Server: "laatmux", Session: "proj/other", Agent: "claude", Activity: protocol.Idle, ActivityAt: now, Liveness: protocol.Alive, Managed: true},
			{ID: "venv/laatmux/%3", EnvironmentID: "venv", Server: "laatmux", Session: "proj/b", Agent: "claude", Activity: protocol.Working, ActivityAt: now, Liveness: protocol.Alive, Managed: true, WorktreeID: "venv/worktree//w/b", Cwd: "/w/b"},
			{ID: "venv/laatmux/%4", EnvironmentID: "venv", Server: "laatmux", Session: "proj/c", Agent: "claude", Activity: protocol.Working, ActivityAt: now, Liveness: protocol.Alive, Managed: true, Cwd: "/w/c"},
		},
		Worktrees: []protocol.Worktree{
			{ID: "oenv/worktree//w/a", EnvironmentID: "oenv", Repo: "proj", Source: src, Branch: "a", Root: "/w/a", Session: "proj/a"},
			{ID: "venv/worktree//w/b", EnvironmentID: "venv", Repo: "proj", Source: src, Branch: "b", Root: "/w/b", Session: "proj/b"},
			{ID: "venv/worktree//w/d", EnvironmentID: "venv", Repo: "proj", Source: src, Branch: "d", Root: "/w/d"},
		},
		Locals: []protocol.Session{{Name: "vm/proj/lost", Key: "venv//w/lost", Host: "vm"}},
		Pendings: []protocol.Pending{
			{ID: "t-old", Host: "vm", EnvironmentID: "venv", Source: src, Repo: "proj", Branch: "b", Root: "/w/b", Taken: true, SubmittedAt: now.Add(-time.Minute)},
			// Done with the prompt undelivered: it needs the user, and
			// stands for the worktree still.
			{ID: "t-new", Host: "vm", EnvironmentID: "venv", Source: src, Repo: "proj", Branch: "b", Root: "/w/b", Taken: true, Done: true, OK: true, Prompt: protocol.DeliveryNotDelivered, SubmittedAt: now},
			{ID: "t-c", Host: "vm", EnvironmentID: "venv", Source: src, Repo: "proj", Branch: "c", Root: "/w/c", Session: "proj/c", Taken: true, SubmittedAt: now},
			{ID: "t-d", Host: "vm", EnvironmentID: "venv", Source: src, Repo: "proj", Branch: "d", Root: "/w/d", Taken: true, Done: true, Error: "failed at setup: boom", Stage: "setup", SubmittedAt: now},
		},
		Now: now,
	}
	var got []string
	for _, n := range Tree(in) {
		id := n.ID()
		if n.Kind == KindAgent {
			id = "agent " + n.Agent.ID
		}
		got = append(got, strings.Repeat("  ", n.Depth)+id)
	}
	want := []string{
		"repo/" + source.Key(src),
		"  oenv/worktree//w/a",
		"    agent oenv/laatmux/%1",
		"  t-new", // the newest task stands for b, with its agent
		"    agent venv/laatmux/%3",
		"  t-old", // the other, a line of its own
		"  t-c",   // before the listing, holding the add's agent
		"    agent venv/laatmux/%4",
		"  venv/worktree//w/d",
		"  t-d", // failed after the worktree was made: beside it
		NodeOther,
		"  agent oenv/laatmux/%2",
		"  session/vm/proj/lost",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("tree:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	for _, n := range Tree(in) {
		if n.ID() == "t-new" && (!n.NeedsUser() || n.Children != 1 || n.Worst == nil) {
			t.Errorf("the task needing the user: %+v", n)
		}
	}
	// The repository's name and place do not follow the worktrees'
	// order: swapped, the tree is the same.
	in.Worktrees[0], in.Worktrees[1] = in.Worktrees[1], in.Worktrees[0]
	in.Worktrees[1].Repo = "zproj"
	var again []string
	for _, n := range Tree(in) {
		again = append(again, n.ID()+" "+n.Name)
	}
	var first []string
	in.Worktrees[0], in.Worktrees[1] = in.Worktrees[1], in.Worktrees[0]
	for _, n := range Tree(in) {
		first = append(first, n.ID()+" "+n.Name)
	}
	if strings.Join(again, "\n") != strings.Join(first, "\n") {
		t.Errorf("the tree follows the worktrees' order:\n%s\nagainst:\n%s", strings.Join(again, "\n"), strings.Join(first, "\n"))
	}
}

// A settled worktree with an idle agent first and a working one after:
// the working one is the most pressing, and a gone agent never is.
func TestPressing(t *testing.T) {
	now := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	src := "git@github.com:laat/proj.git"
	in := Input{
		Hosts: []Host{{Name: "vm", EnvironmentID: "venv", Connected: true, Listed: true, Worktrees: true, Attribution: true}},
		Agents: []protocol.Agent{
			{ID: "venv/laatmux/%1", EnvironmentID: "venv", Server: "laatmux", Session: "proj/a", Agent: "claude", Activity: protocol.Idle, ActivityAt: now, Liveness: protocol.Alive, Managed: true, WorktreeID: "venv/worktree//w/a", Identity: &protocol.Identity{PID: 1, StartUnix: 1}},
			{ID: "venv/laatmux/%2", EnvironmentID: "venv", Server: "laatmux", Session: "proj/a", Agent: "claude", Activity: protocol.Working, ActivityAt: now, Liveness: protocol.Alive, Managed: true, WorktreeID: "venv/worktree//w/a", Identity: &protocol.Identity{PID: 2, StartUnix: 2}},
			{ID: "venv/laatmux/%3", EnvironmentID: "venv", Server: "laatmux", Session: "proj/a", Agent: "claude", Activity: protocol.Blocked, ActivityAt: now, Liveness: protocol.Gone, Managed: true, WorktreeID: "venv/worktree//w/a", Identity: &protocol.Identity{PID: 3, StartUnix: 3}},
		},
		Worktrees: []protocol.Worktree{{ID: "venv/worktree//w/a", EnvironmentID: "venv", Repo: "proj", Source: src, Branch: "a", Root: "/w/a", Session: "proj/a"}},
		Locals:    []protocol.Session{{Name: "vm/proj/a", Key: "venv//w/a", Host: "vm", Settled: true}},
		Now:       now,
	}
	for _, n := range Tree(in) {
		if n.Kind != KindWorktree {
			continue
		}
		if !n.Settled || n.Worst == nil || n.Worst.Agent.ID != "venv/laatmux/%2" {
			t.Errorf("worst: %+v settled %v", n.Worst, n.Settled)
		}
		if !n.Worst.Wants() {
			t.Error("a settled working agent does not open the fold")
		}
	}
}

// An agent observed on this machine's default server in a window of a
// workspace session whose worktree is on another host stands in other
// sessions, and is one of that workspace's agents all the same: settled
// as the session is, dim unless pressing, and in the Stale fold unless
// pressing or the viewer's own; not settled with the session unsettled.
// One in a plain attachment is not settled by an option set there by
// hand.
func TestObservedAgentSettled(t *testing.T) {
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	observed := func(id, session string, act protocol.Activity) protocol.Agent {
		return protocol.Agent{ID: id, EnvironmentID: "menv", Server: "default", Session: session, Agent: "claude", Activity: act, ActivityAt: now, Liveness: protocol.Alive, Cwd: "/Users/u"}
	}
	idle := observed("menv/default/%5", "vm/proj/task", protocol.Idle)
	working := observed("menv/default/%6", "vm/proj/task", protocol.Working)
	blocked := observed("menv/default/%7", "vm/proj/task", protocol.Blocked)
	notes := observed("menv/default/%8", "notes", protocol.Idle)
	input := func(settled bool, current string) Input {
		return Input{
			Hosts: []Host{
				{Name: "mac", Local: true, EnvironmentID: "menv", Connected: true, Listed: true, Worktrees: true, Attribution: true},
				{Name: "vm", EnvironmentID: "venv", Connected: true, Listed: true, Worktrees: true, Attribution: true},
			},
			Agents: []protocol.Agent{
				{ID: "venv/laatmux/%1", EnvironmentID: "venv", Server: "laatmux", Session: "proj/task", Agent: "claude", Activity: protocol.Idle, ActivityAt: now, Liveness: protocol.Alive, Managed: true, Cwd: "/w/task", WorktreeID: "venv/worktree//w/task"},
				idle, working, blocked, notes,
			},
			Worktrees: []protocol.Worktree{{ID: "venv/worktree//w/task", EnvironmentID: "venv", Repo: "proj", Branch: "task", Root: "/w/task", Session: "proj/task"}},
			Locals: []protocol.Session{
				{Name: "vm/proj/task", Key: "venv//w/task", Host: "vm", Settled: settled},
				{Name: "notes", Attach: "notes", Settled: true},
			},
			Current: current,
			Now:     now,
		}
	}
	type state struct{ settled, dim, stale bool }
	for _, c := range []struct {
		settled bool
		current string
		want    map[string]state
	}{
		{true, "", map[string]state{idle.ID: {true, true, true}, working.ID: {true, true, true}, blocked.ID: {true, false, false}, notes.ID: {}}},
		{true, "vm/proj/task", map[string]state{idle.ID: {true, true, false}, working.ID: {true, true, false}, blocked.ID: {true, false, false}, notes.ID: {}}},
		{false, "", map[string]state{idle.ID: {}, working.ID: {}, blocked.ID: {}, notes.ID: {}}},
	} {
		in := input(c.settled, c.current)
		tree := Tree(in)
		// The nodes in other sessions, and the tiles of the agents there,
		// which are in the Stale fold or not.
		nodes, tiles, want := map[string]state{}, map[string]state{}, map[string]state{}
		for _, n := range tree {
			if n.Kind == KindAgent && n.Depth == 1 {
				nodes[n.Agent.ID] = state{settled: n.Settled, dim: n.Dim}
				// The viewer's by its own session, the one it is in.
				if mine := c.current != "" && n.Agent.Session == c.current; n.Own != mine || n.Current != mine {
					t.Errorf("settled %v current %q: %s Own %v Current %v", c.settled, c.current, n.Agent.ID, n.Own, n.Current)
				}
			}
		}
		rs := Agents(in, tree)
		for _, g := range []struct {
			tiles []Row
			stale bool
		}{{rs.Main, false}, {rs.Stale, true}} {
			for _, r := range g.tiles {
				if r.Agent.Server == protocol.ServerDefault {
					tiles[r.Agent.ID] = state{r.Settled, r.Dim, g.stale}
				}
				// Own in the viewer's session: the observed ones and the
				// worktree's own agent through its workspace session.
				if mine := c.current != "" && r.Local != nil && r.Local.Name == c.current; r.Own != mine {
					t.Errorf("settled %v current %q: the tile %s Own %v", c.settled, c.current, r.ID(), r.Own)
				}
			}
		}
		for id, s := range c.want {
			s.stale = false
			want[id] = s
		}
		if !reflect.DeepEqual(nodes, want) {
			t.Errorf("settled %v current %q: the nodes %+v, want %+v", c.settled, c.current, nodes, want)
		}
		if !reflect.DeepEqual(tiles, c.want) {
			t.Errorf("settled %v current %q: the tiles %+v, want %+v", c.settled, c.current, tiles, c.want)
		}
	}
}

// A managed agent of no worktree in a worktree's home session, started
// in another directory from a split there, stands in other sessions
// with the settled state of the worktree's workspace session, where its
// pane jump lands and which z on it toggles: settled as the session is,
// dim unless pressing, and in the Stale fold unless pressing or the
// viewer's own, which it is with the viewer in that session; so with
// the home lost, through the session of the agent laatmux made at the
// root. Not one in a managed session that is no line's home, by an
// option set by hand on its plain attachment, nor one in a standing
// task's session, which z refuses as the task's, nor an observed agent
// in a session of the home's name on vm's default server. The agent of
// the same split on box, where a worktree has the same home, takes
// box's state.
func TestHomeAgentSettled(t *testing.T) {
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	managed := func(id, session string, act protocol.Activity) protocol.Agent {
		return protocol.Agent{ID: id, EnvironmentID: "venv", Server: "laatmux", Session: session, Agent: "claude", Activity: act, ActivityAt: now, Liveness: protocol.Alive, Managed: true, Cwd: "/home/u"}
	}
	root := managed("venv/laatmux/%1", "proj/z", protocol.Idle)
	root.Cwd, root.WorktreeID = "/w/proj/z", "venv/worktree//w/proj/z"
	idle := managed("venv/laatmux/%5", "proj/z", protocol.Idle)
	working := managed("venv/laatmux/%6", "proj/z", protocol.Working)
	blocked := managed("venv/laatmux/%7", "proj/z", protocol.Blocked)
	scratch := managed("venv/laatmux/%8", "scratch", protocol.Idle)
	inTask := managed("venv/laatmux/%9", "proj/y", protocol.Idle)
	observed := managed("venv/default/%10", "proj/z", protocol.Idle)
	observed.Server, observed.Managed = "default", false
	onBox := managed("benv/laatmux/%5", "proj/z", protocol.Idle)
	onBox.EnvironmentID = "benv"
	input := func(home string, settled bool, current string) Input {
		return Input{
			Hosts: []Host{
				{Name: "vm", EnvironmentID: "venv", Connected: true, Listed: true, Worktrees: true, Attribution: true},
				{Name: "box", EnvironmentID: "benv", Connected: true, Listed: true, Worktrees: true, Attribution: true},
			},
			Agents: []protocol.Agent{root, idle, working, blocked, scratch, inTask, observed, onBox},
			Worktrees: []protocol.Worktree{
				{ID: root.WorktreeID, EnvironmentID: "venv", Repo: "proj", Branch: "z", Root: "/w/proj/z", Session: home},
				{ID: "benv/worktree//w/proj/z", EnvironmentID: "benv", Repo: "proj", Branch: "z", Root: "/w/proj/z", Session: "proj/z"},
			},
			Pendings: []protocol.Pending{{ID: "add-y", Host: "vm", EnvironmentID: "venv", Repo: "proj", Branch: "y", Root: "/w/proj/y", Session: "proj/y",
				Sent: true, Taken: true, Done: true, OK: true, Prompt: protocol.DeliveryNotDelivered, SubmittedAt: now}},
			Locals: []protocol.Session{
				{Name: "vm/proj/z", Key: "venv//w/proj/z", Host: "vm", Settled: settled},
				{Name: "vm/proj/y", Key: "venv//w/proj/y", Host: "vm", Settled: settled},
				{Name: "vm/scratch", Attach: "vm/scratch", Settled: true},
				{Name: "box/proj/z", Key: "benv//w/proj/z", Host: "box", Settled: !settled},
			},
			Current: current,
			Now:     now,
		}
	}
	type state struct{ settled, dim, stale, current bool }
	// vm's session settled, with the viewer elsewhere and in it, and box's
	// settled alone.
	settled := map[string]state{idle.ID: {true, true, true, false}, working.ID: {true, true, true, false}, blocked.ID: {true, false, false, false}, scratch.ID: {}, inTask.ID: {}, observed.ID: {}, onBox.ID: {}}
	viewed := map[string]state{idle.ID: {true, true, false, true}, working.ID: {true, true, false, true}, blocked.ID: {true, false, false, true}, scratch.ID: {}, inTask.ID: {}, observed.ID: {}, onBox.ID: {}}
	none := map[string]state{idle.ID: {}, working.ID: {}, blocked.ID: {}, scratch.ID: {}, inTask.ID: {}, observed.ID: {}, onBox.ID: {true, true, true, false}}
	for _, c := range []struct {
		home    string
		settled bool
		current string
		want    map[string]state
	}{
		{"proj/z", true, "", settled}, {"", true, "", settled},
		{"proj/z", true, "vm/proj/z", viewed}, {"", true, "vm/proj/z", viewed},
		{"proj/z", false, "", none}, {"proj/z", true, "vm/proj/y", settled},
	} {
		in := input(c.home, c.settled, c.current)
		tree := Tree(in)
		// The nodes in other sessions, and the tiles of the agents there,
		// which are in the Stale fold or not.
		nodes, tiles, want := map[string]state{}, map[string]state{}, map[string]state{}
		for _, n := range tree {
			if n.Kind == KindAgent && n.Depth == 1 {
				nodes[n.Agent.ID] = state{settled: n.Settled, dim: n.Dim, current: n.Current}
				// The viewer's through the line's workspace session, not
				// by a session of its own.
				if n.Own {
					t.Errorf("home %q current %q: %s Own", c.home, c.current, n.Agent.ID)
				}
			}
		}
		rs := Agents(in, tree)
		for _, g := range []struct {
			tiles []Row
			stale bool
		}{{rs.Main, false}, {rs.Stale, true}} {
			for _, r := range g.tiles {
				if r.Agent != nil && r.Pending == nil && r.Worktree == nil {
					tiles[r.Agent.ID] = state{r.Settled, r.Dim, g.stale, r.Current}
					// The viewer's tile is in the viewer's session, through
					// the line's workspace session: Own.
					if r.Own != r.Current {
						t.Errorf("home %q current %q: the tile %s Own %v Current %v", c.home, c.current, r.ID(), r.Own, r.Current)
					}
				}
			}
		}
		for id, s := range c.want {
			s.stale = false
			want[id] = s
		}
		if !reflect.DeepEqual(nodes, want) {
			t.Errorf("home %q settled %v current %q: the nodes %+v, want %+v", c.home, c.settled, c.current, nodes, want)
		}
		if !reflect.DeepEqual(tiles, c.want) {
			t.Errorf("home %q settled %v current %q: the tiles %+v, want %+v", c.home, c.settled, c.current, tiles, c.want)
		}
	}
	// claude gone from the root, with the agents of no worktree in proj/z
	// taking the host's home away: proj/z's line has no home at all, and
	// the session, named after it, is still its home for the viewer, as
	// with its root agent there.
	for _, c := range []struct {
		current string
		want    map[string]state
	}{{"", settled}, {"vm/proj/z", viewed}} {
		in := input("", true, c.current)
		in.Agents = in.Agents[1:]
		tree := Tree(in)
		for _, n := range tree {
			if n.Kind == KindAgent && n.Depth == 1 {
				if want, ok := c.want[n.Agent.ID]; ok && (n.Settled != want.settled || n.Current != want.current) {
					t.Errorf("no root agent, current %q: the node %s settled %v current %v, want %+v", c.current, n.Agent.ID, n.Settled, n.Current, want)
				}
			}
		}
	}
	// On mac, homeless proj/z with no workspace session, its agent
	// observed in the plain session notes on this machine's default
	// server, which its line holds: an agent of no worktree in mac's
	// managed session proj/z, the line's by its name, is not in notes,
	// and is not the viewer's there.
	mz := protocol.Worktree{ID: "menv/worktree//w/proj/z", EnvironmentID: "menv", Repo: "proj", Branch: "z", Root: "/w/proj/z"}
	inNotes := protocol.Agent{ID: "menv/default/%1", EnvironmentID: "menv", Server: "default", Session: "notes", Agent: "claude", Activity: protocol.Idle, ActivityAt: now,
		Liveness: protocol.Alive, Cwd: mz.Root, WorktreeID: mz.ID}
	onMac := managed("menv/laatmux/%5", "proj/z", protocol.Idle)
	onMac.EnvironmentID = "menv"
	notes := Input{
		Hosts:     []Host{{Name: "mac", Local: true, EnvironmentID: "menv", Connected: true, Listed: true, Worktrees: true, Attribution: true}},
		Agents:    []protocol.Agent{inNotes, onMac},
		Worktrees: []protocol.Worktree{mz},
		Locals:    []protocol.Session{{Name: "notes"}},
		Current:   "notes",
		Now:       now,
	}
	tree := Tree(notes)
	if l := HomeLine(tree, "mac", "proj/z"); l < 0 || !tree[l].Current || tree[l].Local == nil || tree[l].Local.Name != "notes" {
		t.Fatalf("notes: proj/z's line, the viewer's, holding notes: %d", l)
	}
	for _, n := range tree {
		if n.Kind == KindAgent && n.Agent.ID == onMac.ID && (n.Current || n.Settled) {
			t.Errorf("notes: the agent's node %+v", n)
		}
	}
	for _, r := range Agents(notes, tree).Main {
		if r.Agent != nil && r.Agent.ID == onMac.ID && (r.Current || r.Own) {
			t.Errorf("notes: the agent's tile %+v", r)
		}
	}
	// No configured host claims the worktree's machine nor the agent's,
	// another one: the agent takes neither the state nor the viewer of a
	// workspace session whose home has its session's name.
	other := managed("yenv/laatmux/%5", "proj/z", protocol.Idle)
	other.EnvironmentID = "yenv"
	unclaimed := Input{
		Agents:    []protocol.Agent{other},
		Worktrees: []protocol.Worktree{{ID: "xenv/worktree//w/proj/z", EnvironmentID: "xenv", Repo: "proj", Branch: "z", Root: "/w/proj/z", Session: "proj/z"}},
		Locals:    []protocol.Session{{Name: "x/proj/z", Key: "xenv//w/proj/z", Settled: true}},
		Current:   "x/proj/z",
		Now:       now,
	}
	seen := false
	for _, n := range Tree(unclaimed) {
		if n.Kind == KindAgent && n.Agent.ID == other.ID {
			seen = true
			if n.Settled || n.Current {
				t.Errorf("unclaimed: the agent's node %+v", n)
			}
		}
	}
	if !seen {
		t.Errorf("unclaimed: no node %s", other.ID)
	}
	// Two lines with the home: proj/z and the homeless worktree at
	// /w/proj/a, branch z of the repository other, whose root agent was
	// moved into proj/z and which comes first in the tree's order. The
	// host then gives proj/z no home either, the session's panes no
	// longer all in its root, and both lines have it through their root
	// agents; it keeps the home only with /w/proj/a inside proj/z's
	// root. Either way the session is proj/z's, by its name, the host's
	// label proj and its branch, also where this machine labels proj
	// zed, or by its home, as LineFor finds it: the agent shows the
	// settled state of proj/z's workspace session, not the other's, and
	// is the viewer's with the viewer in vm/proj/z, not in vm/proj/a,
	// whichever of the two is settled.
	a := protocol.Worktree{ID: "venv/worktree//w/proj/a", EnvironmentID: "venv", Repo: "other", Branch: "z", Root: "/w/proj/a"}
	moved := managed("venv/laatmux/%2", "proj/z", protocol.Idle)
	moved.Cwd, moved.WorktreeID = a.Root, a.ID
	for _, c := range []struct {
		home    string
		relabel bool
		settled bool // vm/proj/z; vm/proj/a is the other way
		current string
	}{
		{"", false, true, "vm/proj/z"}, {"", false, false, "vm/proj/a"}, {"", true, true, "vm/proj/z"}, {"", true, false, "vm/proj/a"},
		{"proj/z", false, true, "vm/proj/z"}, {"proj/z", false, false, "vm/proj/a"},
	} {
		in := input(c.home, c.settled, c.current)
		if c.relabel {
			in.Worktrees[0].Repo, in.HostRepos = "zed", map[string]string{root.WorktreeID: "proj"}
		}
		in.Worktrees = append(in.Worktrees, a)
		in.Agents = append(in.Agents, moved)
		in.Locals = append(in.Locals, protocol.Session{Name: "vm/proj/a", Key: "venv//w/proj/a", Host: "vm", Settled: !c.settled})
		tree := Tree(in)
		first := -1
		for i := range tree {
			if first < 0 && tree[i].Depth == 1 && tree[i].Home() == "proj/z" {
				first = i
			}
		}
		if first < 0 || tree[first].ID() != a.ID {
			t.Fatalf("%+v: the first line with the home is %d, not proj/a's", c, first)
		}
		if l := HomeLine(tree, "vm", "proj/z"); l < 0 || tree[l].ID() != root.WorktreeID {
			t.Errorf("%+v: the home's line is %d, not proj/z's", c, l)
		}
		found := false
		for _, n := range tree {
			if n.Kind == KindAgent && n.Depth == 1 && n.Agent.ID == idle.ID {
				found = true
				if n.Settled != c.settled || n.Current != c.settled {
					t.Errorf("two lines with the home %+v: the agent's node %+v, want proj/z's state and viewer", c, n)
				}
			}
		}
		if !found {
			t.Errorf("two lines with the home %+v: no node %s in other sessions", c, idle.ID)
		}
		found = false
		for _, r := range Agents(in, tree).Main {
			found = found || r.Agent != nil && r.Agent.ID == idle.ID && r.Current == c.settled
		}
		if !found {
			t.Errorf("two lines with the home %+v: the agent's tile is not in the main group with the viewer's state", c)
		}
	}
}

// A managed agent of worktree proj/y started from a split of proj/z's
// home session, `cd ../y && claude`, sits under proj/y's line with
// proj/z's workspace session as its own, where its pane jump lands: the
// viewer in vm/proj/z is on proj/y's line through it, and on its tile;
// so with proj/z's home lost, through the session of the agent laatmux
// made at its root, and in a standing task's session, through the
// task's workspace session. The viewer in a plain attachment to proj/z
// is on proj/y's line all the same, and on proj/z's by its own session,
// with or without an agent of proj/z in its home, and with only shells
// there; the visitor's tile is in the viewer's session. With a plain
// attachment there the viewer elsewhere is not; the viewer in vm/proj/y
// is not on proj/z's, nor, with no host claiming proj/z, the viewer in
// an attachment tagged for none. Without vm/proj/z the agent keeps
// proj/y's workspace session; so does one in a session of proj/z's name
// on vm's default server, and one in the session of a task whose line
// holds a plain session. With claude gone from proj/z's root, the
// visitor's pane in a sibling directory takes the host's home away and
// proj/z's line has no home at all; the session, named after proj/z,
// is still proj/z's home for the viewer: the visitor takes vm/proj/z,
// and a plain attachment to proj/z marks proj/z's line, also without
// vm/proj/z, unless the session is another worktree's own home. The
// root agent of homeless worktree proj/a, moved by hand into proj/z, is
// in its own line's home and keeps proj/a's workspace session, where
// its jump goes. A line the viewer is on through the visitor is not
// Own; the line whose session the viewer is in is.
func TestVisitorTakesHomeSession(t *testing.T) {
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	managed := func(id, session, root string) protocol.Agent {
		return protocol.Agent{ID: id, EnvironmentID: "venv", Server: "laatmux", Session: session, Agent: "claude", Activity: protocol.Idle, ActivityAt: now,
			Liveness: protocol.Alive, Managed: true, Cwd: root, WorktreeID: "venv/worktree/" + root}
	}
	z := protocol.Worktree{ID: "venv/worktree//w/proj/z", EnvironmentID: "venv", Repo: "proj", Branch: "z", Root: "/w/proj/z", Session: "proj/z"}
	y := protocol.Worktree{ID: "venv/worktree//w/proj/y", EnvironmentID: "venv", Repo: "proj", Branch: "y", Root: "/w/proj/y", Session: "proj/y"}
	// A split's pane is not one laatmux new made: not Managed.
	visitor := managed("venv/laatmux/%3", "proj/z", y.Root)
	visitor.Managed = false
	base := Input{
		Hosts:     []Host{{Name: "vm", EnvironmentID: "venv", Connected: true, Listed: true, Worktrees: true, Attribution: true}},
		Agents:    []protocol.Agent{managed("venv/laatmux/%1", "proj/z", z.Root), managed("venv/laatmux/%2", "proj/y", y.Root), visitor},
		Worktrees: []protocol.Worktree{z, y},
		Locals: []protocol.Session{
			{Name: "vm/proj/z", Key: "venv//w/proj/z", Host: "vm"},
			{Name: "vm/proj/y", Key: "venv//w/proj/y", Host: "vm"},
		},
		Now: now,
	}
	// seen is an agent's node's own session, the lines marked as the
	// viewer's and those of them Own, and whether its tile is the
	// viewer's, and Own.
	type seen struct {
		local         string
		marked, own   []string
		tile, tileOwn bool
	}
	look := func(in Input, id string) seen {
		var s seen
		tree := Tree(in)
		for _, n := range tree {
			switch {
			case n.Kind == KindAgent && n.Agent.ID == id && n.Local != nil:
				s.local = n.Local.Name
			case n.Depth == 1 && n.Current:
				s.marked = append(s.marked, n.ID())
				if n.Own {
					s.own = append(s.own, n.ID())
				}
			case n.Own:
				t.Errorf("%s: Own and not the viewer's", n.ID())
			}
		}
		rs := Agents(in, tree)
		for _, r := range append(rs.Main, rs.Stale...) {
			if r.Agent != nil && r.Agent.ID == id {
				s.tile, s.tileOwn = r.Current, r.Own
				continue
			}
			// Another agent's tile is Own in its own session alone, not
			// through its line.
			if mine := in.Current != "" && r.Local != nil && r.Local.Name == in.Current; r.Pending == nil && r.Own != mine {
				t.Errorf("the tile %s Own %v", r.ID(), r.Own)
			}
		}
		return s
	}
	att := protocol.Session{Name: "vm/proj/z-att", Attach: "vm/proj/z", Host: "vm"}
	task := protocol.Pending{ID: "add-t", Host: "vm", EnvironmentID: "venv", Repo: "proj", Branch: "t", Root: "/w/proj/t", Session: "proj/t", Taken: true, Stage: protocol.StageSetup, SubmittedAt: now}
	lostZ := func(in *Input) {
		in.Worktrees[0].Session = ""
		in.Agents = in.Agents[1:]
	}
	q := protocol.Worktree{ID: "venv/worktree//w/proj/q", EnvironmentID: "venv", Repo: "proj", Branch: "q", Root: "/w/proj/q", Session: "proj/z"}
	both, onY, onZ := []string{y.ID, z.ID}, []string{y.ID}, []string{z.ID}
	for _, c := range []struct {
		name    string
		edit    func(in *Input)
		current string
		want    seen
	}{
		{"the viewer in vm/proj/z", func(*Input) {}, "vm/proj/z", seen{"vm/proj/z", both, onZ, true, true}},
		{"proj/z's home lost", func(in *Input) { in.Worktrees[0].Session = "" }, "vm/proj/z", seen{"vm/proj/z", both, onZ, true, true}},
		{"the viewer in vm/proj/y", func(*Input) {}, "vm/proj/y", seen{"vm/proj/z", onY, onY, true, false}},
		{"the viewer elsewhere", func(*Input) {}, "", seen{"vm/proj/z", nil, nil, false, false}},
		{"the viewer in an attachment to proj/z", func(in *Input) { in.Locals = append(in.Locals, att) }, att.Name, seen{"vm/proj/z", both, onZ, true, true}},
		{"the viewer in an attachment to proj/z, no agent of proj/z", func(in *Input) {
			in.Agents = in.Agents[1:]
			in.Locals = append(in.Locals, att)
		}, att.Name, seen{"vm/proj/z", both, onZ, true, true}},
		{"the viewer in an attachment to proj/z, only shells there", func(in *Input) {
			in.Agents = in.Agents[1:2]
			in.Locals = append(in.Locals, att)
		}, att.Name, seen{"", onZ, onZ, false, false}},
		{"proj/z's home lost, the viewer in an attachment to proj/z", func(in *Input) {
			in.Worktrees[0].Session = ""
			in.Locals = append(in.Locals, att)
		}, att.Name, seen{"vm/proj/z", both, onZ, true, true}},
		{"an attachment to proj/z, the viewer elsewhere", func(in *Input) { in.Locals = append(in.Locals, att) }, "", seen{"vm/proj/z", nil, nil, false, false}},
		{"in a standing task's session", func(in *Input) {
			in.Agents[2].Session = task.Session
			in.Pendings = []protocol.Pending{task}
			in.Locals = append(in.Locals, protocol.Session{Name: "vm/proj/t", Key: "venv//w/proj/t", Host: "vm"})
		}, "vm/proj/t", seen{"vm/proj/t", []string{task.ID, y.ID}, []string{task.ID}, true, true}},
		{"no vm/proj/z", func(in *Input) { in.Locals = in.Locals[1:2] }, "vm/proj/y", seen{"vm/proj/y", onY, onY, true, true}},
		{"on vm's default server", func(in *Input) { in.Agents[2].Server = "default" }, "vm/proj/z", seen{"vm/proj/y", onZ, onZ, false, false}},
		{"no host claims proj/z, an attachment tagged for none", func(in *Input) {
			in.Hosts, in.Agents = nil, in.Agents[1:2]
			in.Locals = append(in.Locals, protocol.Session{Name: "proj/z-att", Attach: "/proj/z"})
		}, "proj/z-att", seen{"", nil, nil, false, false}},
		// claude gone from proj/z's root, the visitor's pane in the sibling
		// /w/proj/y: the host gives proj/z no home, and its line has none,
		// no root agent left. The session is the one add made for proj/z,
		// by its name, and still its home for the viewer.
		{"proj/z's home gone with its agent", lostZ, "vm/proj/z", seen{"vm/proj/z", both, onZ, true, true}},
		{"proj/z's home gone with its agent, the viewer in an attachment to proj/z", func(in *Input) {
			lostZ(in)
			in.Locals = append(in.Locals, att)
		}, att.Name, seen{"vm/proj/z", both, onZ, true, true}},
		{"proj/z's home gone with its agent, an attachment to proj/z, the viewer in vm/proj/z", func(in *Input) {
			lostZ(in)
			in.Locals = append(in.Locals, att)
		}, "vm/proj/z", seen{"vm/proj/z", both, onZ, true, true}},
		{"proj/z's home gone with its agent, no vm/proj/z, the viewer in an attachment to proj/z", func(in *Input) {
			lostZ(in)
			in.Locals = append(in.Locals[1:2], att)
		}, att.Name, seen{att.Name, both, onZ, true, true}},
		{"proj/z's home gone with its agent, the viewer elsewhere", lostZ, "", seen{"vm/proj/z", nil, nil, false, false}},
		{"proj/z's home gone with its agent, an attachment to proj/z, the viewer in vm/proj/y", func(in *Input) {
			lostZ(in)
			in.Locals = append(in.Locals, att)
		}, "vm/proj/y", seen{"vm/proj/z", onY, onY, true, false}},
		// The session is the home of worktree q, its panes all in q's root:
		// q's own session, which proj/z's name does not take from it.
		{"proj/z's home gone with its agent, proj/z q's home", func(in *Input) {
			lostZ(in)
			in.Worktrees = append(in.Worktrees, q)
			in.Locals = append(in.Locals, att, protocol.Session{Name: "vm/proj/q", Key: "venv//w/proj/q", Host: "vm"})
		}, att.Name, seen{"vm/proj/q", []string{q.ID, y.ID}, []string{q.ID}, true, true}},
	} {
		in := base
		in.Agents, in.Worktrees, in.Locals = append([]protocol.Agent(nil), base.Agents...), append([]protocol.Worktree(nil), base.Worktrees...), append([]protocol.Session(nil), base.Locals...)
		c.edit(&in)
		in.Current = c.current
		if got := look(in, visitor.ID); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: the agent's session, the viewer's lines, those Own, its tile the viewer's: %+v, want %+v", c.name, got, c.want)
		}
	}
	// Homeless proj/a, branch z of the repository other, whose root agent
	// was moved by hand into proj/z: its line's home is proj/z, the
	// session's line proj/z's by its own home or, homeless too, by its
	// name, also with no root agent of proj/z left there, and the moved
	// agent keeps vm/proj/a beside the visitor's vm/proj/z.
	a := protocol.Worktree{ID: "venv/worktree//w/proj/a", EnvironmentID: "venv", Repo: "other", Branch: "z", Root: "/w/proj/a"}
	moved := managed("venv/laatmux/%4", "proj/z", a.Root)
	for _, home := range []string{"proj/z", "", "gone"} {
		in := base
		in.Worktrees = []protocol.Worktree{z, y, a}
		in.Worktrees[0].Session = home
		in.Agents = append(append([]protocol.Agent(nil), base.Agents...), moved)
		if home == "gone" {
			in.Worktrees[0].Session, in.Agents = "", in.Agents[1:]
		}
		in.Locals = append(append([]protocol.Session(nil), base.Locals...), protocol.Session{Name: "vm/proj/a", Key: "venv//w/proj/a", Host: "vm"})
		in.Current = "vm/proj/z"
		if got := look(in, moved.ID); got.local != "vm/proj/a" {
			t.Errorf("proj/z's home %q: the moved root agent's session %q, want vm/proj/a", home, got.local)
		}
		if got, want := look(in, visitor.ID), (seen{"vm/proj/z", both, onZ, true, true}); !reflect.DeepEqual(got, want) {
			t.Errorf("proj/z's home %q beside proj/a: %+v, want %+v", home, got, want)
		}
	}
	// claude gone from proj/z's root, another agent of proj/z in a split
	// of the session, in proj/z's directory sub, and a shell in a split in
	// proj/y's taking the host's home away: no visitor, the agent sits in
	// proj/z's home for the viewer as one in the home does, with vm/proj/z
	// as its own, its tile Own in vm/proj/z alone and the viewer's
	// through proj/z's line in vm/proj/z-att.
	sub := managed("venv/laatmux/%5", "proj/z", "/w/proj/z/sub")
	sub.Managed, sub.WorktreeID = false, z.ID
	for _, c := range []struct {
		current string
		want    seen
	}{{"vm/proj/z", seen{"vm/proj/z", onZ, onZ, true, true}}, {att.Name, seen{"vm/proj/z", onZ, onZ, true, false}}} {
		in := base
		in.Worktrees = []protocol.Worktree{z, y}
		in.Worktrees[0].Session = ""
		in.Agents = []protocol.Agent{base.Agents[1], sub}
		in.Locals = append(append([]protocol.Session(nil), base.Locals...), att)
		in.Current = c.current
		if got := look(in, sub.ID); !reflect.DeepEqual(got, c.want) {
			t.Errorf("proj/z's own agent in its session, the viewer in %s: %+v, want %+v", c.current, got, c.want)
		}
	}
	// On mac, a task standing for homeless proj/z, whose worktree's agent
	// sits in the plain session notes on this machine's default server:
	// the task's line holds notes, and the task's session proj/z is its
	// home. The visitor's pane is not in notes: it keeps mac/proj/y, and
	// the viewer in notes is not on proj/y's line.
	mz := protocol.Worktree{ID: "menv/worktree//w/proj/z", EnvironmentID: "menv", Repo: "proj", Branch: "z", Root: "/w/proj/z"}
	my := protocol.Worktree{ID: "menv/worktree//w/proj/y", EnvironmentID: "menv", Repo: "proj", Branch: "y", Root: "/w/proj/y", Session: "proj/y"}
	inNotes := protocol.Agent{ID: "menv/default/%1", EnvironmentID: "menv", Server: "default", Session: "notes", Agent: "claude", Activity: protocol.Idle, ActivityAt: now,
		Liveness: protocol.Alive, Cwd: mz.Root, WorktreeID: mz.ID}
	onMac := visitor
	onMac.ID, onMac.EnvironmentID, onMac.WorktreeID = "menv/laatmux/%3", "menv", my.ID
	in := Input{
		Hosts:     []Host{{Name: "mac", Local: true, EnvironmentID: "menv", Connected: true, Listed: true, Worktrees: true, Attribution: true}},
		Agents:    []protocol.Agent{inNotes, onMac},
		Worktrees: []protocol.Worktree{mz, my},
		Pendings:  []protocol.Pending{{ID: "add-z", Host: "mac", EnvironmentID: "menv", Repo: "proj", Branch: "z", Root: mz.Root, Session: "proj/z", Taken: true, Stage: protocol.StageSetup, SubmittedAt: now}},
		Locals:    []protocol.Session{{Name: "notes"}, {Name: "mac/proj/y", Key: "menv//w/proj/y", Host: "mac"}},
		Current:   "notes",
		Now:       now,
	}
	tree := Tree(in)
	if l := HomeLine(tree, "mac", "proj/z"); l < 0 || tree[l].Pending == nil || tree[l].Local == nil || tree[l].Local.Name != "notes" {
		t.Fatalf("the task's line, home proj/z, holding notes: %d", l)
	}
	if got, want := look(in, onMac.ID), (seen{"mac/proj/y", []string{"add-z"}, []string{"add-z"}, false, false}); !reflect.DeepEqual(got, want) {
		t.Errorf("the task's line holding notes: %+v, want %+v", got, want)
	}
}

// The viewer in a plain attachment to a line's home is on the line by
// its own session whatever runs there, and of several lines with that
// home only the one HomeLine picks is Own; the others are the viewer's
// through it. On vm:
//   - a task add-t for proj/t before the listing, its session proj/t:
//     the viewer in vm/proj/t-att is on the task's line, Own, with the
//     add's agent at the root in proj/t, with no agent there, and with
//     claude started there after a cd elsewhere, with and without the
//     task's workspace session; the viewer elsewhere is not (#298).
//     The older add-o for proj/t beside it is the viewer's with it, not
//     Own: add-t, the newest, holds the add's agent and comes first;
//   - add-t standing for proj/t listed with no home and no agent, whose
//     home is the task's session: the same, and the older add-o for it,
//     beside it, is the viewer's with it, not Own (#298). With add-t's
//     session not reported yet and the agent laatmux made at the root
//     in proj/t, add-t's home is that agent's session, which HomeLine
//     counts add-o's own: add-t, which holds the children and which
//     following goes to, is Own all the same, add-o the viewer's with it;
//     so with no agent, add-t then having no home, the viewer on the
//     group through add-o's session alone. With neither session
//     reported, the viewer in an attachment to proj/t, the session add
//     made, is on both by its name, add-t Own; so with add-t's session
//     proj/t-1 and add-o's unreported, though HomeLine picks add-o by the
//     name. Tasks standing for proj/y after proj/x's line, which the
//     viewer is on, are not the viewer's;
//   - add-t submitted through vm2, another name of vm's machine, which
//     lists its worktrees under vm: the viewer in an attachment through
//     either name is on the task's line, Own, standing for proj/t with
//     its home or before the listing. With vm2 another machine, which
//     has a proj/z homed in proj/z as vm has, the viewer in vm2's
//     attachment is on vm2's line alone. A task for proj/z submitted
//     through vm2 before the listing, beside proj/a's root agent moved
//     into proj/z, leaves one line Own through an attachment by either
//     name: proj/a's, the one HomeLine picks under vm, the name the
//     view's LineFor asks with, which does not see the task (#322). A
//     task standing for proj/t with no home, submitted through vm2, is
//     the viewer's and Own through vm's attachment to the session add
//     made;
//   - proj/z with its home lost, its root agent in proj/z, and homeless
//     proj/a, branch z of the repository other, whose root agent was
//     moved by hand into proj/z: both lines' home is proj/z, and proj/a's
//     line is first in the tree. The viewer in vm/proj/z-att is on both,
//     and Own on proj/z's alone, the session's line by its name; so with
//     proj/z's home still reported, the line's by its own home, and with
//     no agent of proj/z there, proj/z having no home at all (#303).
func TestHomeAttachment(t *testing.T) {
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	hosts := []Host{{Name: "vm", EnvironmentID: "venv", Connected: true, Listed: true, Worktrees: true, Attribution: true}}
	managed := func(id, session, cwd, wt string) protocol.Agent {
		return protocol.Agent{ID: id, EnvironmentID: "venv", Server: "laatmux", Session: session, Agent: "claude", Activity: protocol.Idle, ActivityAt: now,
			Liveness: protocol.Alive, Managed: true, Cwd: cwd, WorktreeID: wt}
	}
	task := protocol.Pending{ID: "add-t", Host: "vm", EnvironmentID: "venv", Repo: "proj", Branch: "t", Root: "/w/proj/t", Session: "proj/t", Taken: true, Stage: protocol.StageSetup, SubmittedAt: now}
	older := task
	older.ID, older.SubmittedAt = "add-o", now.Add(-time.Minute)
	unreported := task
	unreported.Session = ""
	olderUnreported := older
	olderUnreported.Session = ""
	ownSession := task
	ownSession.Session = "proj/t-1"
	wt := protocol.Worktree{ID: "venv/worktree//w/proj/t", EnvironmentID: "venv", Repo: "proj", Branch: "t", Root: "/w/proj/t"}
	tAtt := protocol.Session{Name: "vm/proj/t-att", Attach: "vm/proj/t", Host: "vm"}
	tLocal := protocol.Session{Name: "vm/proj/t", Key: "venv//w/proj/t", Host: "vm"}
	z := protocol.Worktree{ID: "venv/worktree//w/proj/z", EnvironmentID: "venv", Repo: "proj", Branch: "z", Root: "/w/proj/z"}
	a := protocol.Worktree{ID: "venv/worktree//w/proj/a", EnvironmentID: "venv", Repo: "other", Branch: "z", Root: "/w/proj/a"}
	wtHomed := wt
	wtHomed.Session = "proj/t"
	viaVM2 := task
	viaVM2.Host = "vm2"
	aliases := []Host{hosts[0], hosts[0]}
	aliases[1].Name = "vm2"
	vm2Att := protocol.Session{Name: "vm2/proj/t", Attach: "vm2/proj/t", Host: "vm2"}
	x := protocol.Worktree{ID: "venv/worktree//w/proj/x", EnvironmentID: "venv", Repo: "proj", Branch: "x", Root: "/w/proj/x", Session: "proj/x"}
	y := protocol.Worktree{ID: "venv/worktree//w/proj/y", EnvironmentID: "venv", Repo: "proj", Branch: "y", Root: "/w/proj/y", Session: "proj/y"}
	yNew := protocol.Pending{ID: "add-y2", Host: "vm", EnvironmentID: "venv", Repo: "proj", Branch: "y", Root: y.Root, Session: y.Session, Taken: true, Stage: protocol.StageSetup, SubmittedAt: now}
	yOld := yNew
	yOld.ID, yOld.SubmittedAt = "add-y1", now.Add(-time.Minute)
	xAtt := protocol.Session{Name: "vm/proj/x-att", Attach: "vm/proj/x", Host: "vm"}
	zHomed := z
	zHomed.Session = "proj/z"
	twoMachines := []Host{hosts[0], hosts[0]}
	twoMachines[1].Name, twoMachines[1].EnvironmentID = "vm2", "v2env"
	z2 := zHomed
	z2.ID, z2.EnvironmentID = "v2env/worktree//w/proj/z", "v2env"
	z2Att := protocol.Session{Name: "vm2/proj/z-att", Attach: "vm2/proj/z", Host: "vm2"}
	zAtt := protocol.Session{Name: "vm/proj/z-att", Attach: "vm/proj/z", Host: "vm"}
	zTask := protocol.Pending{ID: "add-z", Host: "vm2", EnvironmentID: "venv", Repo: "proj", Branch: "z", Root: z.Root, Session: "proj/z", Taken: true, Stage: protocol.StageSetup, SubmittedAt: now}
	vm2zAtt := protocol.Session{Name: "vm2/proj/z", Attach: "vm2/proj/z", Host: "vm2"}
	unreportedVM2 := unreported
	unreportedVM2.Host = "vm2"
	zLocals := []protocol.Session{{Name: "vm/proj/z", Key: "venv//w/proj/z", Host: "vm"}, {Name: "vm/proj/a", Key: "venv//w/proj/a", Host: "vm"}, zAtt}
	zAgents := []protocol.Agent{managed("venv/laatmux/%1", "proj/z", z.Root, z.ID), managed("venv/laatmux/%2", "proj/z", a.Root, a.ID)}
	onT := []string{task.ID}
	for _, c := range []struct {
		name        string
		in          Input
		marked, own []string // the worktree and task lines in the tree's order
	}{
		{"the add's agent there", Input{Agents: []protocol.Agent{managed("venv/laatmux/%7", "proj/t", task.Root, "")}, Pendings: []protocol.Pending{task}, Locals: []protocol.Session{tAtt}, Current: tAtt.Name}, onT, onT},
		{"no agent there", Input{Pendings: []protocol.Pending{task}, Locals: []protocol.Session{tAtt}, Current: tAtt.Name}, onT, onT},
		{"no agent there, the task's workspace session", Input{Pendings: []protocol.Pending{task}, Locals: []protocol.Session{tLocal, tAtt}, Current: tAtt.Name}, onT, onT},
		{"claude after a cd there", Input{Agents: []protocol.Agent{managed("venv/laatmux/%8", "proj/t", "/home/u", "")}, Pendings: []protocol.Pending{task}, Locals: []protocol.Session{tAtt}, Current: tAtt.Name}, onT, onT},
		{"claude after a cd there, the task's workspace session", Input{Agents: []protocol.Agent{managed("venv/laatmux/%8", "proj/t", "/home/u", "")}, Pendings: []protocol.Pending{task}, Locals: []protocol.Session{tLocal, tAtt}, Current: tAtt.Name}, onT, onT},
		{"the viewer elsewhere", Input{Pendings: []protocol.Pending{task}, Locals: []protocol.Session{tAtt}}, nil, nil},
		{"beside add-o, the add's agent there", Input{Agents: []protocol.Agent{managed("venv/laatmux/%7", "proj/t", task.Root, "")}, Pendings: []protocol.Pending{older, task}, Locals: []protocol.Session{tAtt}, Current: tAtt.Name}, []string{task.ID, older.ID}, onT},
		{"beside add-o, no agent there", Input{Pendings: []protocol.Pending{older, task}, Locals: []protocol.Session{tAtt}, Current: tAtt.Name}, []string{task.ID, older.ID}, onT},
		{"standing for proj/t, no home, no agent", Input{Worktrees: []protocol.Worktree{wt}, Pendings: []protocol.Pending{task}, Locals: []protocol.Session{tAtt}, Current: tAtt.Name}, onT, onT},
		{"standing for proj/t beside add-o", Input{Worktrees: []protocol.Worktree{wt}, Pendings: []protocol.Pending{older, task}, Locals: []protocol.Session{tLocal, tAtt}, Current: tAtt.Name}, []string{task.ID, older.ID}, onT},
		{"add-t's session unreported, beside add-o", Input{Agents: []protocol.Agent{managed("venv/laatmux/%9", "proj/t", wt.Root, wt.ID)}, Worktrees: []protocol.Worktree{wt}, Pendings: []protocol.Pending{older, unreported}, Locals: []protocol.Session{tLocal, tAtt}, Current: tAtt.Name}, []string{task.ID, older.ID}, onT},
		{"add-t's session unreported, no agent, beside add-o", Input{Worktrees: []protocol.Worktree{wt}, Pendings: []protocol.Pending{older, unreported}, Locals: []protocol.Session{tAtt}, Current: tAtt.Name}, []string{task.ID, older.ID}, onT},
		{"neither session reported, no agent", Input{Worktrees: []protocol.Worktree{wt}, Pendings: []protocol.Pending{olderUnreported, unreported}, Locals: []protocol.Session{tAtt}, Current: tAtt.Name}, []string{task.ID, older.ID}, onT},
		{"add-t's session proj/t-1, add-o's unreported", Input{Worktrees: []protocol.Worktree{wt}, Pendings: []protocol.Pending{olderUnreported, ownSession}, Locals: []protocol.Session{tAtt}, Current: tAtt.Name}, []string{task.ID, older.ID}, onT},
		{"proj/x, then tasks standing for proj/y", Input{Worktrees: []protocol.Worktree{x, y}, Pendings: []protocol.Pending{yOld, yNew}, Locals: []protocol.Session{xAtt}, Current: xAtt.Name}, []string{x.ID}, []string{x.ID}},
		{"standing for proj/t, through vm2, attached through vm", Input{Hosts: aliases, Worktrees: []protocol.Worktree{wtHomed}, Pendings: []protocol.Pending{viaVM2}, Locals: []protocol.Session{tAtt}, Current: tAtt.Name}, onT, onT},
		{"standing for proj/t, through vm2, attached through vm2", Input{Hosts: aliases, Worktrees: []protocol.Worktree{wtHomed}, Pendings: []protocol.Pending{viaVM2}, Locals: []protocol.Session{vm2Att}, Current: vm2Att.Name}, onT, onT},
		{"through vm2, attached through vm", Input{Hosts: aliases, Pendings: []protocol.Pending{viaVM2}, Locals: []protocol.Session{tAtt}, Current: tAtt.Name}, onT, onT},
		{"proj/z's task through vm2 beside proj/a, attached through vm", Input{Hosts: aliases, Agents: zAgents[1:], Worktrees: []protocol.Worktree{a}, Pendings: []protocol.Pending{zTask}, Locals: []protocol.Session{zLocals[1], zAtt}, Current: zAtt.Name}, []string{a.ID, zTask.ID}, []string{a.ID}},
		{"proj/z's task through vm2 beside proj/a, attached through vm2", Input{Hosts: aliases, Agents: zAgents[1:], Worktrees: []protocol.Worktree{a}, Pendings: []protocol.Pending{zTask}, Locals: []protocol.Session{zLocals[1], vm2zAtt}, Current: vm2zAtt.Name}, []string{a.ID, zTask.ID}, []string{a.ID}},
		{"standing for proj/t with no home, through vm2, attached through vm", Input{Hosts: aliases, Worktrees: []protocol.Worktree{wt}, Pendings: []protocol.Pending{unreportedVM2}, Locals: []protocol.Session{tAtt}, Current: tAtt.Name}, onT, onT},
		{"proj/z on vm and on vm2, attached through vm2", Input{Hosts: twoMachines, Worktrees: []protocol.Worktree{zHomed, z2}, Locals: []protocol.Session{zAtt, z2Att}, Current: z2Att.Name}, []string{z2.ID}, []string{z2.ID}},
		{"proj/z's home lost, beside proj/a", Input{Agents: zAgents, Worktrees: []protocol.Worktree{z, a}, Locals: zLocals, Current: zAtt.Name}, []string{a.ID, z.ID}, []string{z.ID}},
		{"proj/z's home reported, beside proj/a", Input{Agents: zAgents, Worktrees: []protocol.Worktree{zHomed, a}, Locals: zLocals, Current: zAtt.Name}, []string{a.ID, z.ID}, []string{z.ID}},
		{"proj/z with no home, beside proj/a", Input{Agents: zAgents[1:], Worktrees: []protocol.Worktree{z, a}, Locals: zLocals, Current: zAtt.Name}, []string{a.ID, z.ID}, []string{z.ID}},
		{"beside proj/a, the viewer elsewhere", Input{Agents: zAgents, Worktrees: []protocol.Worktree{z, a}, Locals: zLocals}, nil, nil},
	} {
		in := c.in
		in.Now = now
		if in.Hosts == nil {
			in.Hosts = hosts
		}
		tree := Tree(in)
		if len(in.Worktrees) == 2 && in.Worktrees[1].ID == a.ID {
			// The precondition: proj/a's line first with the home, proj/z's
			// the one HomeLine picks.
			if l := HomeLine(tree, "vm", "proj/z"); l < 0 || tree[l].ID() != z.ID {
				t.Fatalf("%s: the home's line is %d, not proj/z's", c.name, l)
			}
			for _, n := range tree {
				if n.Depth == 1 && n.Home() == "proj/z" {
					if n.ID() != a.ID {
						t.Fatalf("%s: the first line with the home is %s, not proj/a's", c.name, n.ID())
					}
					break
				}
			}
		}
		var marked, own []string
		for _, n := range tree {
			if (n.Kind == KindWorktree || n.Kind == KindTask) && n.Current {
				marked = append(marked, n.ID())
				if n.Own {
					own = append(own, n.ID())
				}
			}
		}
		if !reflect.DeepEqual(marked, c.marked) || !reflect.DeepEqual(own, c.own) {
			t.Errorf("%s: the viewer's lines %v, Own %v; want %v, Own %v", c.name, marked, own, c.marked, c.own)
		}
		// proj/a's root agent in proj/z is the viewer's with its line, and
		// not Own: its own session is vm/proj/a, where its jump goes.
		rs := Agents(in, tree)
		for _, r := range append(rs.Main, rs.Stale...) {
			if r.Agent != nil && r.Agent.ID == zAgents[1].ID && (r.Current != (in.Current != "") || r.Own) {
				t.Errorf("%s: proj/a's root agent's tile Current %v Own %v", c.name, r.Current, r.Own)
			}
		}
	}
}

// Two plain attachments with one tag, set by hand or by an older build,
// attach to one managed session: the viewer in either is in an
// attachment to the session, the first listed as well as the last, the
// one the join keeps by the tag (#320). On vm, each session with two:
//   - proj/t, the home of proj/t with nothing running there: proj/t's
//     line is the viewer's and Own (attachedHome);
//   - proj/z, the home of proj/z, with its agent and a visitor from
//     proj/y: proj/z's line is the viewer's and Own, proj/y's the
//     viewer's through the visitor, whose tile is Own (visitors);
//   - other, which holds an agent of proj/a, whose home is proj/a: the
//     line is the viewer's through it, its tile Own;
//   - scratch, which holds a managed agent of no worktree: its node in
//     other sessions is the viewer's and Own, as is its tile;
//   - proj/n, the session of task add-n before the listing, with the
//     add's agent at the root: the task's line and tile are the
//     viewer's and Own, as is the agent's tile.
//
// A workspace session tagged by hand with proj/t's tag is no attachment:
// the viewer in it is on its orphaned line alone. The viewer in no
// session, or in one that is not laatmux's, is on none.
func TestTwoAttachmentsOneTag(t *testing.T) {
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	agent := func(id, session, cwd, wt string, managed bool) protocol.Agent {
		return protocol.Agent{ID: id, EnvironmentID: "venv", Server: "laatmux", Session: session, Agent: "claude", Activity: protocol.Idle, ActivityAt: now,
			Liveness: protocol.Alive, Managed: managed, Cwd: cwd, WorktreeID: wt}
	}
	tree := func(branch string) protocol.Worktree {
		return protocol.Worktree{ID: "venv/worktree//w/proj/" + branch, EnvironmentID: "venv", Repo: "proj", Branch: branch, Root: "/w/proj/" + branch, Session: "proj/" + branch}
	}
	pt, pz, py, pa := tree("t"), tree("z"), tree("y"), tree("a")
	inZ := agent("venv/laatmux/%1", "proj/z", pz.Root, pz.ID, true)
	inY := agent("venv/laatmux/%2", "proj/y", py.Root, py.ID, true)
	visitor := agent("venv/laatmux/%3", "proj/z", py.Root, py.ID, false)
	inOther := agent("venv/laatmux/%4", "other", pa.Root, pa.ID, false)
	inScratch := agent("venv/laatmux/%5", "scratch", "/home/u", "", true)
	task := protocol.Pending{ID: "add-n", Host: "vm", EnvironmentID: "venv", Repo: "proj", Branch: "n", Root: "/w/proj/n", Session: "proj/n", Taken: true, Stage: protocol.StageSetup, SubmittedAt: now}
	inTask := agent("venv/laatmux/%6", "proj/n", task.Root, "", true)
	tagged := protocol.Session{Name: "vm/proj/w", Key: "venv//w/proj/w", Host: "vm", Attach: "vm/proj/t"}
	in := Input{
		Hosts:     []Host{{Name: "vm", EnvironmentID: "venv", Connected: true, Listed: true, Worktrees: true, Attribution: true}},
		Agents:    []protocol.Agent{inZ, inY, visitor, inOther, inScratch, inTask},
		Worktrees: []protocol.Worktree{pt, pz, py, pa},
		Pendings:  []protocol.Pending{task},
		Locals:    []protocol.Session{{Name: "vm/proj/z", Key: "venv//w/proj/z", Host: "vm"}, {Name: "vm/proj/y", Key: "venv//w/proj/y", Host: "vm"}, tagged},
		Now:       now,
	}
	pair := func(session string) []string { return []string{"vm/" + session + "-a", "vm/" + session + "-b"} }
	for _, s := range []string{"proj/t", "proj/z", "other", "scratch", "proj/n"} {
		for _, name := range pair(s) {
			in.Locals = append(in.Locals, protocol.Session{Name: name, Attach: "vm/" + s, Host: "vm"})
		}
	}
	// seen is the lines and nodes in other sessions marked as the
	// viewer's, those of them Own, and the tiles the viewer's and Own.
	type seen struct{ marked, own, tiles, tilesOwn []string }
	orphan := "session/" + tagged.Name
	for _, c := range []struct {
		viewers []string
		want    seen
	}{
		{pair("proj/t"), seen{[]string{pt.ID}, []string{pt.ID}, nil, nil}},
		{pair("proj/z"), seen{[]string{py.ID, pz.ID}, []string{pz.ID}, []string{inY.ID, visitor.ID, inZ.ID}, []string{visitor.ID}}},
		{pair("other"), seen{[]string{pa.ID}, nil, []string{inOther.ID}, []string{inOther.ID}}},
		{pair("scratch"), seen{[]string{inScratch.ID}, []string{inScratch.ID}, []string{inScratch.ID}, []string{inScratch.ID}}},
		{pair("proj/n"), seen{[]string{task.ID}, []string{task.ID}, []string{task.ID, inTask.ID}, []string{task.ID, inTask.ID}}},
		{[]string{tagged.Name}, seen{[]string{orphan}, []string{orphan}, nil, nil}},
		{[]string{"", "main"}, seen{}},
	} {
		for _, cur := range c.viewers {
			in.Current = cur
			var got seen
			tr := Tree(in)
			for _, n := range tr {
				if n.Depth == 1 && n.Current {
					got.marked = append(got.marked, n.ID())
					if n.Own {
						got.own = append(got.own, n.ID())
					}
				}
			}
			rs := Agents(in, tr)
			for _, r := range rs.Main {
				if r.Current {
					got.tiles = append(got.tiles, r.ID())
				}
				if r.Own {
					got.tilesOwn = append(got.tilesOwn, r.ID())
				}
			}
			if !reflect.DeepEqual(got, c.want) {
				t.Errorf("the viewer in %q: %+v, want %+v", cur, got, c.want)
			}
		}
	}
}

// A worktree line's agent is the one its jump goes
// through, and the most pressing one is kept apart for the icon; the
// viewer in an attachment to another managed session holding the
// worktree's agent has that worktree's line and that agent's tile;
// agents come in one order whatever the map they came from.
func TestTreeJumpAgentAndViewer(t *testing.T) {
	now := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	src := "git@github.com:laat/proj.git"
	in := Input{
		Hosts: []Host{{Name: "vm", EnvironmentID: "venv", Connected: true, Listed: true, Worktrees: true, Attribution: true}},
		Agents: []protocol.Agent{
			{ID: "venv/laatmux/%2", EnvironmentID: "venv", Server: "laatmux", Session: "other", Agent: "claude", Activity: protocol.Blocked, ActivityAt: now, Liveness: protocol.Alive, Managed: true, WorktreeID: "venv/worktree//w/a", Cwd: "/w/a", Identity: &protocol.Identity{PID: 2, StartUnix: 2}},
			{ID: "venv/laatmux/%1", EnvironmentID: "venv", Server: "laatmux", Session: "proj/a", Agent: "claude", Activity: protocol.Idle, ActivityAt: now, Liveness: protocol.Alive, Managed: true, WorktreeID: "venv/worktree//w/a", Cwd: "/w/a", Identity: &protocol.Identity{PID: 1, StartUnix: 1}},
		},
		Worktrees: []protocol.Worktree{{ID: "venv/worktree//w/a", EnvironmentID: "venv", Repo: "proj", Source: src, Branch: "a", Root: "/w/a"}},
		Locals:    []protocol.Session{{Name: "vm/other", Attach: "vm/other", Host: "vm"}},
		Current:   "vm/other",
		Now:       now,
	}
	for i := 0; i < 2; i++ {
		nodes := Tree(in)
		var line Row
		for _, n := range nodes {
			if n.Kind == KindWorktree {
				line = n
			}
		}
		// No home session: the jump agent is the one laatmux made at
		// the root, %1, while the blocked visitor %2 is the worst.
		// The viewer's line through %2, not by a session of its own.
		if line.Agent == nil || line.Agent.ID != "venv/laatmux/%1" || line.Worst == nil || line.Worst.Agent.ID != "venv/laatmux/%2" || !line.Current || line.Own {
			t.Errorf("line: agent %+v worst %+v current %v own %v", line.Agent, line.Worst, line.Current, line.Own)
		}
		if nodes[2].Agent.ID != "venv/laatmux/%1" || nodes[3].Agent.ID != "venv/laatmux/%2" {
			t.Errorf("children out of order: %s %s", nodes[2].Agent.ID, nodes[3].Agent.ID)
		}
		// Both are the viewer's worktree's agents: both current, the
		// blocked visitor first in sort order, so following lands on it.
		tiles := Agents(in, Tree(in)).Main
		if len(tiles) != 2 || !tiles[0].Current || tiles[0].Agent.ID != "venv/laatmux/%2" || !tiles[1].Current {
			t.Errorf("tiles: %+v", tiles)
		}
		in.Agents[0], in.Agents[1] = in.Agents[1], in.Agents[0]
	}
}

// A repository known by an orphaned session's source tag alone, no
// worktree naming it, is named by the source's last element: the forge
// path's for a forge form (the trailing slash and .git gone with it),
// the source's own otherwise, without .git; a .git left after the
// forge's own strip goes too.
func TestRepoNamedBySourceAlone(t *testing.T) {
	in := Input{
		Hosts: []Host{{Name: "mac", Local: true, EnvironmentID: "menv", Connected: true, Listed: true, Worktrees: true}},
		Locals: []protocol.Session{
			{Name: "mac/one/x", Key: "menv//w/one/x", Host: "mac", Source: "https://GitHub.com/Laat/One.git/"},
			{Name: "mac/two/y", Key: "menv//w/two/y", Host: "mac", Source: "alice@box:projects/two.git"},
			{Name: "mac/three/z", Key: "menv//w/three/z", Host: "mac", Source: "git@github.com:o/three.git.git"},
		},
	}
	want := "One\n  mac/one/x\nthree\n  mac/three/z\ntwo\n  mac/two/y\n"
	if got := outline(Tree(in)); got != want {
		t.Fatalf("tree:\n%s\nwant:\n%s", got, want)
	}
}

// Tasks are newest first by submission, the instant compared whatever
// its zone, and by id for two submitted in one instant.
func TestNewer(t *testing.T) {
	t0 := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	a := &protocol.Pending{ID: "add-1", SubmittedAt: t0}
	b := &protocol.Pending{ID: "add-2", SubmittedAt: t0.In(time.FixedZone("x", 3600))}
	c := &protocol.Pending{ID: "add-0", SubmittedAt: t0.Add(time.Second)}
	if !newer(a, b) || newer(b, a) || newer(a, a) || !newer(c, a) || newer(a, c) {
		t.Fatal("newer: wrong order")
	}
}

// mainInput is a repository on the local host with a worktree and its
// main checkout, two agents in plain sessions on the default server in
// the checkout, attributed to its record by the host, and one in a
// clone outside the repos directory, in none.
func mainInput(now time.Time) Input {
	src := "https://github.com/laat/laatmux"
	main := "menv/checkout//code/laatmux"
	plain := func(id, session string, act protocol.Activity, at time.Duration, wt string) protocol.Agent {
		return protocol.Agent{ID: id, EnvironmentID: "menv", Server: "default", Session: session, Agent: "claude", Activity: act,
			ActivityAt: now.Add(-at), Liveness: protocol.Alive, Identity: &protocol.Identity{PID: 1, StartUnix: 1}, WorktreeID: wt}
	}
	return Input{
		Hosts: []Host{{Name: "mac", Local: true, EnvironmentID: "menv", Connected: true, Listed: true, Worktrees: true, Attribution: true}},
		Agents: []protocol.Agent{
			plain("menv/default/%1", "laatmux", protocol.Idle, 10*time.Minute, main),
			plain("menv/default/%2", "notes", protocol.Working, 30*time.Minute, main),
			plain("menv/default/%4", "dots", protocol.Idle, time.Minute, ""),
			{ID: "menv/laatmux/%3", EnvironmentID: "menv", Server: "laatmux", Session: "laatmux/fix", Agent: "claude", Activity: protocol.Idle,
				ActivityAt: now, Liveness: protocol.Alive, Managed: true, WorktreeID: "menv/worktree//w/fix"},
		},
		Worktrees: []protocol.Worktree{
			{ID: "menv/worktree//w/fix", EnvironmentID: "menv", Repo: "laatmux", Source: src, Branch: "fix", Root: "/w/fix", Session: "laatmux/fix"},
			{ID: main, EnvironmentID: "menv", Repo: "laatmux", Source: src, Branch: "main", Root: "/code/laatmux", Main: true,
				Git: &protocol.GitStatus{Base: "origin/main", Uncommitted: [2]int{3, 1}, Dirty: true}},
		},
		Locals:  []protocol.Session{{Name: "mac/laatmux/fix", Key: "menv//w/fix", Host: "mac"}},
		Current: "laatmux",
		Now:     now,
	}
}

// An agent of a main checkout in a window of a workspace session, or of
// a plain attachment's session, does not make the checkout's line the
// viewer's there, whichever of its agents the line jumps through: those
// sessions are other lines'.
func TestMainCheckoutLineInLaatmuxSessions(t *testing.T) {
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	in := mainInput(now)
	in.Agents[0].Session = "mac/laatmux/fix" // a window of fix's workspace session
	in.Agents[1].Session = "att"             // a plain attachment's session
	in.Locals = append(in.Locals, protocol.Session{Name: "att", Attach: "mac/other", Host: "mac"})
	line := func(nodes []Row, id string) Row {
		for _, n := range nodes {
			if n.Depth == 1 && n.ID() == id {
				return n
			}
		}
		t.Fatalf("no line %s", id)
		return Row{}
	}
	for _, c := range []struct {
		viewer  string
		working int // the agent working, the line's jump agent
	}{{"mac/laatmux/fix", 0}, {"mac/laatmux/fix", 1}, {"att", 0}, {"att", 1}} {
		in.Current = c.viewer
		for i := range in.Agents[:2] {
			in.Agents[i].Activity = protocol.Idle
		}
		in.Agents[c.working].Activity = protocol.Working
		nodes := Tree(in)
		if m := line(nodes, "menv/checkout//code/laatmux"); m.Own {
			t.Errorf("viewer in %s, %s working: the main line is the viewer's", c.viewer, in.Agents[c.working].ID)
		}
		if f := line(nodes, "menv/worktree//w/fix"); f.Own != (c.viewer == "mac/laatmux/fix") {
			t.Errorf("viewer in %s: fix's line Own %v", c.viewer, f.Own)
		}
	}
}

// A workspace session left at a main checkout's root, from a worktree
// there before, is no session of the checkout's: it stays an orphaned
// line, and the viewer in it is not on the checkout's line.
func TestMainCheckoutLeftWorkspace(t *testing.T) {
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	in := mainInput(now)
	left := protocol.Session{Name: "mac/laatmux/old", Key: "menv//code/laatmux", Host: "mac", Source: "https://github.com/laat/laatmux", Settled: true}
	in.Locals = append(in.Locals, left)
	in.Current = left.Name
	nodes := Tree(in)
	var main, orphan *Row
	for i := range nodes {
		switch n := &nodes[i]; {
		case n.mainCheckout():
			main = n
		case n.Orphaned && n.Local != nil && n.Local.Name == left.Name:
			orphan = n
		}
	}
	if main == nil || main.Current || main.Own || main.Settled || main.Local != nil && main.Local.Name == left.Name {
		t.Fatalf("the main line took the session left: %+v", main)
	}
	for _, n := range nodes {
		if n.Depth == 2 && n.Worktree != nil && n.Worktree.Main && n.Local != nil && n.Local.Name == left.Name {
			t.Fatalf("an agent of the checkout took the session left: %+v", n)
		}
	}
	if orphan == nil || !orphan.Current {
		t.Fatalf("the session left is no orphaned line, the viewer's: %+v", orphan)
	}
}

// A main checkout is a line under its repository, first, with the agents
// the host attributed to it, from plain sessions on the default server;
// the line jumps through the most recently active, one working before
// one idle, and is the viewer's by its own session when the viewer sits
// with any of them, whichever the jump goes through. Its agents are
// tiles titled by the repository with the branch under it. It has no
// session add named, and with no agent it says so.
// An agent in a clone the host does not publish stays in other sessions.
func TestMainCheckoutLine(t *testing.T) {
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	in := mainInput(now)
	nodes := Tree(in)
	want := `laatmux
  laatmux *
    laatmux
    notes
  fix
    laatmux/fix
other sessions
  dots
`
	if got := outline(nodes); got != want {
		t.Fatalf("tree:\n%s\nwant:\n%s", got, want)
	}
	line := nodes[1]
	if line.Agent == nil || line.Agent.ID != "menv/default/%2" || line.Local == nil || line.Local.Name != "notes" || !line.Own {
		t.Fatalf("the line jumps through the working agent, in notes, and is the viewer's, in laatmux: %+v", line)
	}
	if line.Children != 2 || line.Worktree.Git == nil {
		t.Fatalf("line %+v", line)
	}
	if HomeLine(nodes, "mac", "laatmux/main") != -1 {
		t.Error("a managed session named as add would name the main checkout's is its")
	}
	// The working one goes idle before the other did: the other is the
	// most recently active, the viewer's.
	in.Agents[1].Activity = protocol.Idle
	nodes = Tree(in)
	if line := nodes[1]; line.Agent == nil || line.Agent.ID != "menv/default/%1" || line.Local.Name != "laatmux" || !line.Own {
		t.Fatalf("the line jumps through the latest active, the viewer's: %+v", line)
	}
	tiles := Agents(in, nodes)
	var mains []Row
	for _, r := range append(tiles.Main, tiles.Stale...) {
		if r.Worktree != nil && r.Worktree.Main {
			mains = append(mains, r)
		}
	}
	if len(mains) != 2 {
		t.Fatalf("tiles %+v", tiles)
	}
	for _, r := range mains {
		if title, sub := r.Titles(); title != "laatmux" || sub != "main" || r.Suffix == "" {
			t.Errorf("tile %s: %q %q %q", r.Agent.ID, title, sub, r.Suffix)
		}
	}
	// No agent: the line says so, as a worktree line with a session does.
	in.Agents = in.Agents[2:]
	nodes = Tree(in)
	if line := nodes[1]; !line.Worktree.Main || line.Agent != nil || line.State() != "no agent" || line.Children != 0 {
		t.Fatalf("the line with no agent: %+v", line)
	}
}
