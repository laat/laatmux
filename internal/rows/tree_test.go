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
// is on proj/y's line all the same, and with a plain attachment there
// the viewer elsewhere is not; the viewer in vm/proj/y is not on
// proj/z's. Without vm/proj/z the agent keeps proj/y's workspace
// session; so does one in a session of proj/z's name on vm's default
// server, and one in the session of a task whose line holds a plain
// session. The root agent of homeless worktree proj/a, moved by hand
// into proj/z, is in its own line's home and keeps proj/a's workspace
// session, where its jump goes. A line the viewer is on through the
// visitor is not Own; the line whose session the viewer is in is.
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
	// viewer's.
	type seen struct {
		local       string
		marked, own []string
		tile        bool
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
			s.tile = s.tile || r.Agent != nil && r.Agent.ID == id && r.Current
			// An agent's tile is Own in the viewer's session alone, not
			// through its line.
			if mine := in.Current != "" && r.Local != nil && r.Local.Name == in.Current; r.Pending == nil && r.Own != mine {
				t.Errorf("the tile %s Own %v", r.ID(), r.Own)
			}
		}
		return s
	}
	att := protocol.Session{Name: "vm/proj/z-att", Attach: "vm/proj/z", Host: "vm"}
	task := protocol.Pending{ID: "add-t", Host: "vm", EnvironmentID: "venv", Repo: "proj", Branch: "t", Root: "/w/proj/t", Session: "proj/t", Taken: true, Stage: protocol.StageSetup, SubmittedAt: now}
	both, onY, onZ := []string{y.ID, z.ID}, []string{y.ID}, []string{z.ID}
	for _, c := range []struct {
		name    string
		edit    func(in *Input)
		current string
		want    seen
	}{
		{"the viewer in vm/proj/z", func(*Input) {}, "vm/proj/z", seen{"vm/proj/z", both, onZ, true}},
		{"proj/z's home lost", func(in *Input) { in.Worktrees[0].Session = "" }, "vm/proj/z", seen{"vm/proj/z", both, onZ, true}},
		{"the viewer in vm/proj/y", func(*Input) {}, "vm/proj/y", seen{"vm/proj/z", onY, onY, true}},
		{"the viewer elsewhere", func(*Input) {}, "", seen{"vm/proj/z", nil, nil, false}},
		{"the viewer in an attachment to proj/z", func(in *Input) { in.Locals = append(in.Locals, att) }, att.Name, seen{"vm/proj/z", both, onZ, true}},
		{"an attachment to proj/z, the viewer elsewhere", func(in *Input) { in.Locals = append(in.Locals, att) }, "", seen{"vm/proj/z", nil, nil, false}},
		{"in a standing task's session", func(in *Input) {
			in.Agents[2].Session = task.Session
			in.Pendings = []protocol.Pending{task}
			in.Locals = append(in.Locals, protocol.Session{Name: "vm/proj/t", Key: "venv//w/proj/t", Host: "vm"})
		}, "vm/proj/t", seen{"vm/proj/t", []string{task.ID, y.ID}, []string{task.ID}, true}},
		{"no vm/proj/z", func(in *Input) { in.Locals = in.Locals[1:2] }, "vm/proj/y", seen{"vm/proj/y", onY, onY, true}},
		{"on vm's default server", func(in *Input) { in.Agents[2].Server = "default" }, "vm/proj/z", seen{"vm/proj/y", onZ, onZ, false}},
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
	// name, and the moved agent keeps vm/proj/a beside the visitor's
	// vm/proj/z.
	a := protocol.Worktree{ID: "venv/worktree//w/proj/a", EnvironmentID: "venv", Repo: "other", Branch: "z", Root: "/w/proj/a"}
	moved := managed("venv/laatmux/%4", "proj/z", a.Root)
	for _, home := range []string{"proj/z", ""} {
		in := base
		in.Worktrees = []protocol.Worktree{z, y, a}
		in.Worktrees[0].Session = home
		in.Agents = append(append([]protocol.Agent(nil), base.Agents...), moved)
		in.Locals = append(append([]protocol.Session(nil), base.Locals...), protocol.Session{Name: "vm/proj/a", Key: "venv//w/proj/a", Host: "vm"})
		in.Current = "vm/proj/z"
		if got := look(in, moved.ID); got.local != "vm/proj/a" {
			t.Errorf("proj/z's home %q: the moved root agent's session %q, want vm/proj/a", home, got.local)
		}
		if got, want := look(in, visitor.ID), (seen{"vm/proj/z", both, onZ, true}); !reflect.DeepEqual(got, want) {
			t.Errorf("proj/z's home %q beside proj/a: %+v, want %+v", home, got, want)
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
	if got, want := look(in, onMac.ID), (seen{"mac/proj/y", []string{"add-z"}, []string{"add-z"}, false}); !reflect.DeepEqual(got, want) {
		t.Errorf("the task's line holding notes: %+v, want %+v", got, want)
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
