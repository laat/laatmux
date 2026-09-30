package rows

import (
	"strings"
	"testing"
	"time"

	"github.com/laat/laatmux/internal/config"
	"github.com/laat/laatmux/internal/protocol"
	"github.com/laat/laatmux/internal/workspace"
)

// treeInput is two repositories on two hosts: laatmux with a worktree
// holding two agents, a shell and a run, one with no agent, and one
// only a task stands for yet; proj with a worktree and an orphaned
// session; an observed agent and a plain attachment in other sessions.
func treeInput(now time.Time) Input {
	src := "git@github.com:laat/laatmux.git"
	agent := func(id, env, session, name string, act protocol.Activity, start int64, wt string) protocol.Agent {
		return protocol.Agent{ID: id, EnvironmentID: env, Session: session, Agent: name, Activity: act, ActivityAt: now.Add(-time.Minute),
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
			{ID: "menv/laatmux/%6", EnvironmentID: "menv", Session: "new", Agent: "claude", Activity: protocol.Idle, ActivityAt: now, Liveness: protocol.Alive, Managed: true},
		},
		Worktrees: []protocol.Worktree{
			{ID: "venv/worktree//r/agents-config", EnvironmentID: "venv", Repo: "laatmux", Source: src, Branch: "agents-config", Root: "/r/agents-config", Session: "laatmux/agents-config"},
			{ID: "venv/worktree//r/auto-layout", EnvironmentID: "venv", Repo: "laatmux", Source: src, Branch: "auto-layout", Root: "/r/auto-layout"},
			{ID: "menv/worktree//w/batch", EnvironmentID: "menv", Repo: "proj", Source: "https://github.com/laat/proj", Branch: "batch", Root: "/w/batch", Session: "proj/batch"},
		},
		Panes: []protocol.Pane{{ID: "venv/pane/laatmux/%7", EnvironmentID: "venv", Session: "laatmux/agents-config", Window: 1, PaneID: "%7", Command: "zsh", WorktreeID: "venv/worktree//r/agents-config"}},
		Runs:  []protocol.Run{{ID: "venv/run/r1", EnvironmentID: "venv", Root: "/r/agents-config", WorktreeID: "venv/worktree//r/agents-config", Cmd: []string{"make", "test"}, StartedAt: now.Add(-42 * time.Second)}},
		Locals: []workspace.Local{
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

// The agent view: one tile per agent, the task first, agents of one
// worktree numbered in the tree's order, the viewer's agents current.
func TestAgents(t *testing.T) {
	now := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	rs := Agents(treeInput(now))
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
			{ID: "oenv/laatmux/%1", EnvironmentID: "oenv", Session: "proj/a", Agent: "claude", Activity: protocol.Working, ActivityAt: now, Liveness: protocol.Alive, Managed: true},
			{ID: "oenv/laatmux/%2", EnvironmentID: "oenv", Session: "proj/other", Agent: "claude", Activity: protocol.Idle, ActivityAt: now, Liveness: protocol.Alive, Managed: true},
			{ID: "venv/laatmux/%3", EnvironmentID: "venv", Session: "proj/b", Agent: "claude", Activity: protocol.Working, ActivityAt: now, Liveness: protocol.Alive, Managed: true, WorktreeID: "venv/worktree//w/b", Cwd: "/w/b"},
			{ID: "venv/laatmux/%4", EnvironmentID: "venv", Session: "proj/c", Agent: "claude", Activity: protocol.Working, ActivityAt: now, Liveness: protocol.Alive, Managed: true, Cwd: "/w/c"},
		},
		Worktrees: []protocol.Worktree{
			{ID: "oenv/worktree//w/a", EnvironmentID: "oenv", Repo: "proj", Source: src, Branch: "a", Root: "/w/a", Session: "proj/a"},
			{ID: "venv/worktree//w/b", EnvironmentID: "venv", Repo: "proj", Source: src, Branch: "b", Root: "/w/b", Session: "proj/b"},
			{ID: "venv/worktree//w/d", EnvironmentID: "venv", Repo: "proj", Source: src, Branch: "d", Root: "/w/d"},
		},
		Locals: []workspace.Local{{Name: "vm/proj/lost", Key: "venv//w/lost", Host: "vm"}},
		Pendings: []protocol.Pending{
			{ID: "t-old", Host: "vm", EnvironmentID: "venv", Source: src, Repo: "proj", Branch: "b", Root: "/w/b", Taken: true, SubmittedAt: now.Add(-time.Minute)},
			{ID: "t-new", Host: "vm", EnvironmentID: "venv", Source: src, Repo: "proj", Branch: "b", Root: "/w/b", Taken: true, SubmittedAt: now},
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
		"repo/" + config.SourceKey(src),
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
}

// Review round 1: a worktree line's agent is the one its jump goes
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
			{ID: "venv/laatmux/%2", EnvironmentID: "venv", Session: "other", Agent: "claude", Activity: protocol.Blocked, ActivityAt: now, Liveness: protocol.Alive, Managed: true, WorktreeID: "venv/worktree//w/a", Cwd: "/w/a", Identity: &protocol.Identity{PID: 2, StartUnix: 2}},
			{ID: "venv/laatmux/%1", EnvironmentID: "venv", Session: "proj/a", Agent: "claude", Activity: protocol.Idle, ActivityAt: now, Liveness: protocol.Alive, Managed: true, WorktreeID: "venv/worktree//w/a", Cwd: "/w/a", Identity: &protocol.Identity{PID: 1, StartUnix: 1}},
		},
		Worktrees: []protocol.Worktree{{ID: "venv/worktree//w/a", EnvironmentID: "venv", Repo: "proj", Source: src, Branch: "a", Root: "/w/a"}},
		Locals:    []workspace.Local{{Name: "vm/other", Attach: "vm/other", Host: "vm"}},
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
		if line.Agent == nil || line.Agent.ID != "venv/laatmux/%1" || line.Worst == nil || line.Worst.Agent.ID != "venv/laatmux/%2" || !line.Current {
			t.Errorf("line: agent %+v worst %+v current %v", line.Agent, line.Worst, line.Current)
		}
		if nodes[2].Agent.ID != "venv/laatmux/%1" || nodes[3].Agent.ID != "venv/laatmux/%2" {
			t.Errorf("children out of order: %s %s", nodes[2].Agent.ID, nodes[3].Agent.ID)
		}
		// Both are the viewer's worktree's agents: both current, the
		// blocked visitor first in sort order, so following lands on it.
		tiles := Agents(in).Main
		if len(tiles) != 2 || !tiles[0].Current || tiles[0].Agent.ID != "venv/laatmux/%2" || !tiles[1].Current {
			t.Errorf("tiles: %+v", tiles)
		}
		in.Agents[0], in.Agents[1] = in.Agents[1], in.Agents[0]
	}
}
