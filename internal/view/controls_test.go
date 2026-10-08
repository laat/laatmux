package view

import (
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/laat/laatmux/internal/palette"
	"github.com/laat/laatmux/internal/protocol"
	"github.com/laat/laatmux/internal/rows"
	"github.com/laat/laatmux/internal/term"
)

// has is whether the folds hold the id, whatever its value.
func has(d map[string]bool, id string) bool {
	_, ok := d[id]
	return ok
}

// ids is the visible rows' ids, one per line.
func ids(m *Model) string {
	var out []string
	for _, it := range m.Visible() {
		out = append(out, it.Row.ID())
	}
	return strings.Join(out, "\n")
}

// The scopes by the viewer's row: session is the viewer's worktree's
// agents in two managed sessions with the tasks at its root, in the
// agent view as tiles and in the tree as the line with its children
// under its repository; project is the repository's; all is all. F
// goes to session and back, from all, from session, and after a scope
// set from outside.
func TestScopes(t *testing.T) {
	now := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	src := "git@github.com:laat/laatmux.git"
	in := treeInput(now)
	// A second agent of the viewer's worktree in another managed
	// session, a task at its root that needs the user, and a task at
	// another root.
	in.Agents = append(in.Agents, protocol.Agent{ID: "venv/laatmux/%11", EnvironmentID: "venv", Server: "laatmux", Session: "elsewhere", Agent: "codex", Activity: protocol.Working, ActivityAt: now, Liveness: protocol.Alive, Managed: true, WorktreeID: "venv/worktree//r/agents-config", Identity: &protocol.Identity{PID: 11, StartUnix: 40}})
	in.Pendings = []protocol.Pending{
		{ID: "add-ac", Host: "vm", EnvironmentID: "venv", Source: src, Repo: "laatmux", Branch: "agents-config", Root: "/r/agents-config", Session: "laatmux/agents-config", Taken: true, Done: true, OK: true, Prompt: protocol.DeliveryNotDelivered, SubmittedAt: now},
		{ID: "add-new", Host: "vm", EnvironmentID: "venv", Source: src, Repo: "laatmux", Branch: "new-one", Root: "/r/new-one", Session: "laatmux/new-one", Taken: true, SubmittedAt: now},
		{ID: "add-anki", Host: "mac", EnvironmentID: "menv", Source: "https://github.com/laat/anki-llm", Repo: "anki-llm", Branch: "x", Root: "/w/x", Session: "anki-llm/x", Taken: true, SubmittedAt: now},
	}
	m := &Model{Now: now, LocalHost: "mac", View: ViewAgents, Width: 80, Height: 40, Follow: true}
	set := func() {
		m.SetTree(rows.Tree(in))
		m.SetRows(rows.Agents(in, rows.Tree(in)))
		m.Render()
	}
	set()
	all := ids(m)
	if !strings.Contains(all, "venv/laatmux/%11") || !strings.Contains(all, "add-anki") {
		t.Fatalf("all:\n%s", all)
	}
	// f under session sets the folds of the lines shown, not the
	// repository line shared with the panes on all.
	m.View, m.Scope = ViewTree, ScopeSession
	m.Render()
	m.Handle(term.Key{Rune: 'f'})
	d, _ := m.DirtyFolds()
	if _, repo := d[rows.RepoNode(src)]; repo || len(d) == 0 {
		t.Errorf("f under session: %v", d)
	}
	if _, other := d["venv/worktree//r/auto-layout"]; other {
		t.Errorf("f under session set another worktree's fold: %v", d)
	}
	m.Handle(term.Key{Rune: 'f'}) // open again
	// With the repository line folded by a pane on all, f opens it here
	// alone, a reveal, and the lines under it decide the rest.
	m.ApplyFolds(map[string]bool{rows.RepoNode(src): true})
	m.DirtyFolds()
	m.Handle(term.Key{Rune: 'f'})
	if m.closed(&m.Tree[m.indexOf(rows.RepoNode(src))]) {
		t.Error("f under session left the repository line folded")
	}
	if d, _ := m.DirtyFolds(); has(d, rows.RepoNode(src)) || len(d) == 0 {
		t.Errorf("f under session with the repository folded: %v", d)
	}
	// A folded repository line is a closed fold shown: f opens, the
	// lines under it too, whether they were open or closed, and writes
	// them open; the next f closes them.
	for _, closed := range []bool{false, true} {
		m.ApplyFolds(map[string]bool{}) // the reveal's value forgotten, so the file's takes again
		m.ApplyFolds(map[string]bool{rows.RepoNode(src): true, "add-ac": closed})
		m.DirtyFolds()
		m.Handle(term.Key{Rune: 'f'})
		if m.closed(&m.Tree[m.indexOf("add-ac")]) {
			t.Errorf("f with the repository folded and add-ac closed=%v did not open it", closed)
		}
		if d, _ := m.DirtyFolds(); !has(d, "add-ac") || d["add-ac"] || has(d, rows.RepoNode(src)) {
			t.Errorf("f with the repository folded wrote %v", d)
		}
		m.Handle(term.Key{Rune: 'f'})
		if !m.closed(&m.Tree[m.indexOf("add-ac")]) {
			t.Error("f again with everything open did not close")
		}
		m.DirtyFolds() // written
	}
	m.ApplyFolds(map[string]bool{})
	m.View, m.Scope = ViewAgents, ScopeAll
	m.ApplyFolds(map[string]bool{})
	// F: to session. The viewer is in agents-config's home session.
	m.Handle(term.Key{Rune: 'F'})
	if !m.SettingsChanged() {
		t.Error("F changed no setting")
	}
	// add-ac's tile carries the worktree's agent %1; no tile of its own.
	want := "add-ac\nvenv/laatmux/%11\nvenv/laatmux/%2"
	if got := ids(m); got != want || m.Scope != ScopeSession {
		t.Errorf("session tiles:\n%s\nwant:\n%s", got, want)
	}
	if !strings.Contains(Text(m.Render()), "[session]") {
		t.Error("the footer does not name the scope")
	}
	// Project: the repository's tiles, the task for another branch
	// and the stale worktree among them, not anki-llm's.
	m.Scope = ScopeProject
	m.ShowHidden = true
	got := ids(m)
	if !strings.Contains(got, "add-new") || !strings.Contains(got, "venv/laatmux/%8") || strings.Contains(got, "add-anki") || strings.Contains(got, "menv/laatmux/%3") {
		t.Errorf("project tiles:\n%s", got)
	}
	// F from project goes to session and back to project; from
	// session alone, to all.
	m.Handle(term.Key{Rune: 'F'})
	if m.Scope != ScopeSession {
		t.Errorf("F from project: %s", m.Scope)
	}
	m.Handle(term.Key{Rune: 'F'})
	if m.Scope != ScopeProject {
		t.Errorf("F back: %s", m.Scope)
	}
	m.Scope, m.prevScope = ScopeSession, ""
	m.Handle(term.Key{Rune: 'F'})
	if m.Scope != ScopeAll {
		t.Errorf("F from session alone: %s", m.Scope)
	}
	// The tree under session: the repository, the task standing for
	// the viewer's worktree with its children, nothing else.
	m.View, m.Scope = ViewTree, ScopeSession
	want = rows.RepoNode(src) + "\nadd-ac\nvenv/laatmux/%1\nvenv/laatmux/%2\nvenv/laatmux/%11\nvenv/pane/laatmux/%7\nvenv/run/r1"
	if got := ids(m); got != want {
		t.Errorf("session tree:\n%s\nwant:\n%s", got, want)
	}
	// Under project: every line under laatmux, none under anki-llm and
	// no other-sessions header.
	m.Scope = ScopeProject
	got = ids(m)
	if !strings.HasPrefix(got, rows.RepoNode(src)+"\n") || !strings.Contains(got, "add-new") || !strings.Contains(got, "session/mac/laatmux/gone") || strings.Contains(got, "anki") || strings.Contains(got, "venv/default/%5") {
		t.Errorf("project tree:\n%s", got)
	}
	if strings.Contains(Text(m.Render()), "other sessions") {
		t.Error("the other-sessions header under project")
	}
	// A viewer in a task's session before the listing: the task line
	// with the add's agent.
	in.Agents = append(in.Agents, protocol.Agent{ID: "venv/laatmux/%9", EnvironmentID: "venv", Server: "laatmux", Session: "laatmux/new-one", Agent: "claude", Activity: protocol.Idle, ActivityAt: now, Liveness: protocol.Alive, Managed: true, Cwd: "/r/new-one", Identity: &protocol.Identity{PID: 9, StartUnix: 9}})
	in.Locals = append(in.Locals, protocol.Session{Name: "vm/laatmux/new-one", Attach: "vm/laatmux/new-one", Host: "vm"})
	in.Current = "vm/laatmux/new-one"
	m.Scope = ScopeSession
	set()
	if got := ids(m); got != rows.RepoNode(src)+"\nadd-new" {
		t.Errorf("a task's session:\n%s", got)
	}
	m.Select("add-new")
	m.Handle(term.Key{Rune: 'l'})
	if got := ids(m); got != rows.RepoNode(src)+"\nadd-new\nvenv/laatmux/%9" {
		t.Errorf("a task's session, unfolded:\n%s", got)
	}
	// A session with no worktree: the viewer's line alone; one that is
	// no row's: the empty state.
	in.Current = "mac/scratch"
	in.Locals = append(in.Locals, protocol.Session{Name: "mac/scratch", Attach: "mac/scratch", Host: "mac"})
	in.Agents = append(in.Agents, protocol.Agent{ID: "menv/laatmux/%12", EnvironmentID: "menv", Server: "laatmux", Session: "scratch", Agent: "claude", Activity: protocol.Idle, ActivityAt: now, Liveness: protocol.Alive, Managed: true, Identity: &protocol.Identity{PID: 12, StartUnix: 12}})
	set()
	if got := ids(m); got != "menv/laatmux/%12" {
		t.Errorf("a session with no worktree:\n%s", got)
	}
	m.View = ViewAgents
	if got := ids(m); got != "menv/laatmux/%12" {
		t.Errorf("a session with no worktree, tiles:\n%s", got)
	}
	in.Current = "mac/shell"
	set()
	if got := ids(m); got != "" || !strings.Contains(Text(m.Render()), "No agents running") {
		t.Errorf("a session that is no row's:\n%s\n%s", got, Text(m.Render()))
	}
	// An orphaned session under its repository is the viewer's line,
	// under project too: no worktree, so project is session.
	in.Current = "mac/laatmux/gone"
	m.View = ViewTree
	set()
	if got := ids(m); got != rows.RepoNode(src)+"\nsession/mac/laatmux/gone" {
		t.Errorf("an orphaned session:\n%s", got)
	}
	m.Scope = ScopeProject
	if got := ids(m); got != rows.RepoNode(src)+"\nsession/mac/laatmux/gone" {
		t.Errorf("an orphaned session under project:\n%s", got)
	}
	// A failed task at the viewer's root is the viewer's under session,
	// beside the worktree's line; the add's agent before the listing is
	// in the agent view's scope through its task line.
	in = treeInput(now)
	in.Pendings = []protocol.Pending{
		{ID: "add-fail", Host: "vm", EnvironmentID: "venv", Source: src, Repo: "laatmux", Branch: "agents-config", Root: "/r/agents-config", Session: "laatmux/agents-config", Taken: true, Done: true, Error: "failed at agent: boom", SubmittedAt: now},
	}
	m.Scope = ScopeSession
	set()
	if got := ids(m); !strings.Contains(got, "\nadd-fail") || strings.Contains(got, "auto-layout") {
		t.Errorf("a failed task at the root:\n%s", got)
	}
	in = treeInput(now)
	in.Agents = append(in.Agents, protocol.Agent{ID: "venv/laatmux/%9", EnvironmentID: "venv", Server: "laatmux", Session: "laatmux/new-one", Agent: "claude", Activity: protocol.Idle, ActivityAt: now, Liveness: protocol.Alive, Managed: true, Cwd: "/r/new-one", Identity: &protocol.Identity{PID: 9, StartUnix: 9}})
	in.Pendings = []protocol.Pending{{ID: "add-new", Host: "vm", EnvironmentID: "venv", Source: src, Repo: "laatmux", Branch: "new-one", Root: "/r/new-one", Session: "laatmux/new-one", Taken: true, SubmittedAt: now}}
	in.Locals = append(in.Locals, protocol.Session{Name: "vm/laatmux/new-one", Attach: "vm/laatmux/new-one", Host: "vm"})
	in.Current = "vm/laatmux/new-one"
	m.View = ViewAgents
	set()
	// The add's agent is on the task's tile, not one of its own.
	if got := ids(m); got != "add-new" {
		t.Errorf("the add's agent under session, tiles:\n%s", got)
	}
	m.Scope = ScopeProject
	if got := ids(m); !strings.HasPrefix(got, "add-new\n") || strings.Contains(got, "venv/laatmux/%9") || !strings.Contains(got, "venv/laatmux/%1") || strings.Contains(got, "menv/laatmux/%3") {
		t.Errorf("the add's agent under project, tiles:\n%s", got)
	}
}

// The viewer's own row is in the scope whatever its worktree. vm lists
// proj/z homed in proj/z; the user runs `cd ~ && claude` in a split of
// proj/z, a managed agent of no worktree, and claude in a window of the
// workspace session vm/proj/z on this machine's default server, an
// observed one; both stand in other sessions with no worktree, and with
// the viewer in vm/proj/z both are the viewer's. Under session the
// agent view shows them beside proj/z's own agent, and the tree shows
// them under the other-sessions header after proj/z's line; under
// project beside the repository's. Other agents of no worktree, the
// split of proj/y's home, one in another managed session and one
// observed in another session, stay hidden, as do the other worktrees;
// with the viewer in vm/proj/y the split of proj/y is the viewer's and
// proj/z's two are not.
func TestScopeViewerRow(t *testing.T) {
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	src, other := "git@github.com:u/proj.git", "git@github.com:u/other.git"
	agent := func(id, session, cwd, wt string) protocol.Agent {
		return protocol.Agent{ID: id, EnvironmentID: "venv", Server: "laatmux", Session: session, Agent: "claude", Activity: protocol.Working, ActivityAt: now, Liveness: protocol.Alive, Managed: true, Cwd: cwd, WorktreeID: wt}
	}
	observed := func(id, session string) protocol.Agent {
		return protocol.Agent{ID: id, EnvironmentID: "menv", Server: "default", Session: session, Agent: "claude", Activity: protocol.Working, ActivityAt: now, Liveness: protocol.Alive, Cwd: "/Users/u"}
	}
	in := rows.Input{
		Hosts: []rows.Host{
			{Name: "mac", Local: true, EnvironmentID: "menv", Connected: true, Listed: true, Worktrees: true, Attribution: true},
			{Name: "vm", EnvironmentID: "venv", Connected: true, Listed: true, Worktrees: true, Attribution: true},
		},
		Agents: []protocol.Agent{
			agent("venv/laatmux/%1", "proj/z", "/w/proj/z", "venv/worktree//w/proj/z"),
			agent("venv/laatmux/%5", "proj/z", "/home/u", ""),
			agent("venv/laatmux/%2", "proj/y", "/w/proj/y", "venv/worktree//w/proj/y"),
			agent("venv/laatmux/%9", "proj/y", "/home/u", ""),
			agent("venv/laatmux/%3", "other/x", "/w/other/x", "venv/worktree//w/other/x"),
			agent("venv/laatmux/%8", "scratch", "/home/u", ""),
			observed("menv/default/%6", "vm/proj/z"),
			observed("menv/default/%7", "notes"),
		},
		Worktrees: []protocol.Worktree{
			{ID: "venv/worktree//w/proj/z", EnvironmentID: "venv", Repo: "proj", Source: src, Branch: "z", Root: "/w/proj/z", Session: "proj/z"},
			{ID: "venv/worktree//w/proj/y", EnvironmentID: "venv", Repo: "proj", Source: src, Branch: "y", Root: "/w/proj/y", Session: "proj/y"},
			{ID: "venv/worktree//w/other/x", EnvironmentID: "venv", Repo: "other", Source: other, Branch: "x", Root: "/w/other/x", Session: "other/x"},
		},
		Locals: []protocol.Session{
			{Name: "vm/proj/z", Key: "venv//w/proj/z", Host: "vm"},
			{Name: "vm/proj/y", Key: "venv//w/proj/y", Host: "vm"},
			{Name: "vm/other/x", Key: "venv//w/other/x", Host: "vm"},
		},
		Current: "vm/proj/z",
		Now:     now,
	}
	m := &Model{Now: now, LocalHost: "mac", View: ViewAgents, Width: 80, Height: 40, Follow: true, Scope: ScopeSession}
	set := func() {
		m.SetTree(rows.Tree(in))
		m.SetRows(rows.Agents(in, rows.Tree(in)))
		m.Render()
	}
	sortedIDs := func() string {
		got := strings.Split(ids(m), "\n")
		slices.Sort(got)
		return strings.Join(got, " ")
	}
	set()
	if got, want := sortedIDs(), "menv/default/%6 venv/laatmux/%1 venv/laatmux/%5"; got != want {
		t.Errorf("session tiles: %s, want %s", got, want)
	}
	m.View = ViewTree
	if got, want := ids(m), rows.RepoNode(src)+"\nvenv/worktree//w/proj/z\nvenv/laatmux/%1\nvenv/laatmux/%5\nmenv/default/%6"; got != want {
		t.Errorf("session tree:\n%s\nwant:\n%s", got, want)
	}
	if !strings.Contains(Text(m.Render()), "other sessions") {
		t.Errorf("session tree without the other-sessions header:\n%s", Text(m.Render()))
	}
	m.Scope = ScopeProject
	if got, want := ids(m), rows.RepoNode(src)+"\nvenv/worktree//w/proj/y\nvenv/laatmux/%2\nvenv/worktree//w/proj/z\nvenv/laatmux/%1\nvenv/laatmux/%5\nmenv/default/%6"; got != want {
		t.Errorf("project tree:\n%s\nwant:\n%s", got, want)
	}
	if !strings.Contains(Text(m.Render()), "other sessions") {
		t.Errorf("project tree without the other-sessions header:\n%s", Text(m.Render()))
	}
	m.View = ViewAgents
	if got, want := sortedIDs(), "menv/default/%6 venv/laatmux/%1 venv/laatmux/%2 venv/laatmux/%5"; got != want {
		t.Errorf("project tiles: %s, want %s", got, want)
	}
	// The viewer in proj/y's workspace session.
	in.Current = "vm/proj/y"
	m.Scope = ScopeSession
	set()
	if got, want := sortedIDs(), "venv/laatmux/%2 venv/laatmux/%9"; got != want {
		t.Errorf("session tiles in vm/proj/y: %s, want %s", got, want)
	}
	m.View = ViewTree
	if got, want := ids(m), rows.RepoNode(src)+"\nvenv/worktree//w/proj/y\nvenv/laatmux/%2\nvenv/laatmux/%9"; got != want {
		t.Errorf("session tree in vm/proj/y:\n%s\nwant:\n%s", got, want)
	}
	// Back in vm/proj/z, claude in another window there in a worktree on
	// mac, beside its own agent in its home session: that line is the
	// viewer's too, so the scope keeps it with its children, and in the
	// agent view its agents, beside proj/z's: a worktree of proj that
	// sorts before proj/z or after; and one of another repository, after
	// proj or before it, which project keeps with its repository line
	// beside proj's lines. The scope's worktree is proj/z's whichever
	// sorts first, the viewer's by its own session, and its repository
	// proj's. So for claude started in a split of proj/z after a cd into
	// a worktree on vm: an agent of that worktree on the managed server
	// in proj/z's home session, whose pane jump lands in vm/proj/z.
	base := in
	third, aaa := "git@github.com:u/third.git", "git@github.com:u/aaa.git"
	for _, managed := range []bool{false, true} {
		for _, c := range []struct{ repo, source, branch string }{{"proj", src, "a"}, {"proj", src, "zz"}, {"third", third, "b"}, {"aaa", aaa, "c"}} {
			in = base
			session := c.repo + "/" + c.branch
			root := "/m/" + session
			wt := "menv/worktree/" + root
			visiting := observed("menv/default/%10", "vm/proj/z")
			visiting.Cwd, visiting.WorktreeID = root, wt
			home := protocol.Agent{ID: "menv/laatmux/%4", EnvironmentID: "menv", Server: "laatmux", Session: session, Agent: "claude", Activity: protocol.Working, ActivityAt: now, Liveness: protocol.Alive, Managed: true, Cwd: root, WorktreeID: wt}
			w := protocol.Worktree{ID: wt, EnvironmentID: "menv", Repo: c.repo, Source: c.source, Branch: c.branch, Root: root, Session: session}
			local := protocol.Session{Name: "mac/" + session, Key: "menv/" + root, Host: "mac"}
			if managed {
				root = "/w/" + session
				wt = "venv/worktree/" + root
				visiting, home = agent("venv/laatmux/%10", "proj/z", root, wt), agent("venv/laatmux/%4", session, root, wt)
				// A split's pane is not one laatmux new made.
				visiting.Managed = false
				w.ID, w.EnvironmentID, w.Root = wt, "venv", root
				local = protocol.Session{Name: "vm/" + session, Key: "venv/" + root, Host: "vm"}
			}
			name := c.branch + ", managed " + strconv.FormatBool(managed)
			in.Agents = append(slices.Clone(base.Agents), visiting, home)
			in.Worktrees = append(slices.Clone(base.Worktrees), w)
			in.Locals = append(slices.Clone(base.Locals), local)
			in.Current = "vm/proj/z"
			tiles := func(more ...string) string {
				want := append([]string{visiting.ID, home.ID, "menv/default/%6", "venv/laatmux/%1", "venv/laatmux/%5"}, more...)
				slices.Sort(want)
				return strings.Join(want, " ")
			}
			m.View, m.Scope = ViewAgents, ScopeSession
			set()
			if got, want := sortedIDs(), tiles(); got != want {
				t.Errorf("%s: session tiles: %s, want %s", name, got, want)
			}
			if w, repo, _ := m.viewerWorktree(); w != "venv/worktree//w/proj/z" || repo != rows.RepoNode(src) {
				t.Errorf("%s: the scope's worktree %s in %s, want proj/z's in proj", name, w, repo)
			}
			m.View = ViewTree
			theirs := "\n" + wt + "\n" + visiting.ID + "\n" + home.ID
			vm := "\nvenv/worktree//w/proj/z\nvenv/laatmux/%1"
			others := "\nvenv/laatmux/%5\nmenv/default/%6"
			want := map[string]string{
				"a":  rows.RepoNode(src) + theirs + vm + others,
				"zz": rows.RepoNode(src) + vm + theirs + others,
				"b":  rows.RepoNode(src) + vm + "\n" + rows.RepoNode(third) + theirs + others,
				"c":  rows.RepoNode(aaa) + theirs + "\n" + rows.RepoNode(src) + vm + others,
			}[c.branch]
			if got := ids(m); got != want {
				t.Errorf("%s: session tree:\n%s\nwant:\n%s", name, got, want)
			}
			if c.repo == "proj" {
				continue
			}
			m.Scope = ScopeProject
			projLines := "\nvenv/worktree//w/proj/y\nvenv/laatmux/%2" + vm
			want = rows.RepoNode(src) + projLines + "\n" + rows.RepoNode(third) + theirs + others
			if c.repo == "aaa" {
				want = rows.RepoNode(aaa) + theirs + "\n" + rows.RepoNode(src) + projLines + others
			}
			if got := ids(m); got != want {
				t.Errorf("%s: project tree:\n%s\nwant:\n%s", name, got, want)
			}
			m.View = ViewAgents
			if got, want := sortedIDs(), tiles("venv/laatmux/%2"); got != want {
				t.Errorf("%s: project tiles: %s, want %s", name, got, want)
			}
		}
	}
	// The example of #253 and #254: claude started in a split of proj/z
	// after a cd into proj/y, and a failed task at proj/z's root. The
	// viewer's row is proj/z's line, by its own session, though proj/y's
	// sorts first: following stays on proj/z in both views and both
	// scopes, session keeps the task at its root, and proj/y's line,
	// marked through the visitor, is shown with its agents.
	in = base
	split := agent("venv/laatmux/%11", "proj/z", "/w/proj/y", "venv/worktree//w/proj/y")
	split.Managed = false
	in.Agents = append(slices.Clone(base.Agents), split)
	in.Pendings = []protocol.Pending{{ID: "add-fail", Host: "vm", EnvironmentID: "venv", Source: src, Repo: "proj", Branch: "z", Root: "/w/proj/z", Session: "proj/z", Taken: true, Done: true, Error: "failed at agent: boom", SubmittedAt: now}}
	in.Current = "vm/proj/z"
	for _, scope := range []Scope{ScopeSession, ScopeProject} {
		m.View, m.Scope = ViewTree, scope
		set()
		if r := m.Selection(); r == nil || r.ID() != "venv/worktree//w/proj/z" {
			t.Errorf("the split's visitor, %s: the tree follows %+v", scope, r)
		}
		if got, want := ids(m), rows.RepoNode(src)+"\nvenv/worktree//w/proj/y\nvenv/laatmux/%11\nvenv/laatmux/%2\nvenv/worktree//w/proj/z\nvenv/laatmux/%1\nadd-fail\nvenv/laatmux/%5\nmenv/default/%6"; got != want {
			t.Errorf("the split's visitor, %s: tree:\n%s\nwant:\n%s", scope, got, want)
		}
		m.View = ViewAgents
		if r := m.Selection(); r == nil || r.ID() != "venv/laatmux/%1" {
			t.Errorf("the split's visitor, %s: the agent view follows %+v", scope, r)
		}
		if got, want := sortedIDs(), "add-fail menv/default/%6 venv/laatmux/%1 venv/laatmux/%11 venv/laatmux/%2 venv/laatmux/%5"; got != want {
			t.Errorf("the split's visitor, %s: tiles %s, want %s", scope, got, want)
		}
	}
}

// A chip's band: the highlight background under the span's own colour
// with the background known; reverse video and no colour without, as
// the list's band.
func TestChipANSI(t *testing.T) {
	th, _ := palette.New(true, nil)
	l := Line{Spans: []Span{{Text: "●", Fg: palette.Warning, band: true}, {Text: " x", band: true}}}
	s := ANSI(l, th)
	bandBg, warn := th.SGR(palette.HighlightRowBg, true), th.SGR(palette.Warning, false)
	if !strings.Contains(s, bandBg) || !strings.Contains(s, warn) || strings.Index(s, bandBg) > strings.Index(s, warn) {
		t.Errorf("a chip with the background known: %q", s)
	}
	// A template's background stays off the band: the selected chip
	// is told apart by the band alone.
	tinted := Line{Spans: []Span{{Text: "x", Bg: palette.Accent, band: true}}}
	if s := ANSI(tinted, th); strings.Contains(s, th.SGR(palette.Accent, true)) || !strings.Contains(s, bandBg) {
		t.Errorf("a template's background under the band: %q", s)
	}
	dim := Line{Spans: []Span{{Text: "x", Dim: true, band: true}}}
	if s := ANSI(dim, palette.Mono()); strings.Contains(s, "\x1b[2m") {
		t.Errorf("a dim span faint under a reverse band: %q", s)
	}
	if s := ANSI(l, palette.Mono()); !strings.Contains(s, "\x1b[7m") || strings.Contains(s, "\x1b[38") {
		t.Errorf("a chip without colours: %q", s)
	}
}

// A command over the socket: next and prev move whether the pane is
// filtering or not; jump N is the digit's jump; view and scope switch,
// as the CLI wrote them, without a setting to persist; while a question
// or an overlay is up a command does nothing.
func TestCommands(t *testing.T) {
	now := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	in := treeInput(now)
	m := &Model{Now: now, LocalHost: "mac", View: ViewAgents, Width: 60, Height: 30}
	m.SetTree(rows.Tree(in))
	m.SetRows(rows.Agents(in, rows.Tree(in)))
	m.Render()
	for _, c := range []struct{ line, err string }{
		{"next", ""}, {"prev", ""}, {"jump 3", ""}, {"jump 3 client=/dev/ttys004", ""}, {"view tree", ""}, {"scope session", ""},
		{"", "empty command"}, {"jump", "jump takes a number"}, {"jump 0", `jump "0": not 1 to 9`}, {"view side", `view "side" is not agents or tree`}, {"scope me", `scope "me" is not all, session or project`}, {"dance", `unknown command "dance"`}, {"next now", "next takes no argument"},
	} {
		cmd, err := ParseCommand(c.line)
		switch {
		case c.err == "" && err != nil:
			t.Errorf("%q: %v", c.line, err)
		case c.err != "" && (err == nil || err.Error() != c.err):
			t.Errorf("%q: %v, want %s", c.line, err, c.err)
		case c.line == "jump 3 client=/dev/ttys004" && (cmd.N != 3 || cmd.Client != "/dev/ttys004"):
			t.Errorf("%q: %+v", c.line, cmd)
		}
	}
	m.Filtering, m.Filter = true, "a"
	m.Command(Command{Name: "next"})
	if m.Selected != 1 || !m.Filtering {
		t.Errorf("next while filtering: selected %d filtering %v", m.Selected, m.Filtering)
	}
	m.Command(Command{Name: "prev"})
	if m.Selected != 0 {
		t.Errorf("prev: %d", m.Selected)
	}
	m.Filtering, m.Filter = false, ""
	if a := m.Command(Command{Name: "jump", N: 2}); a.Kind != ActionJump || a.Row == nil || a.Row.ID() != m.Visible()[1].Row.ID() {
		t.Errorf("jump 2: %+v", a)
	}
	m.Command(Command{Name: "view", Arg: "tree"})
	if m.View != ViewTree || m.SettingsChanged() {
		t.Errorf("view tree: %s, or a setting to persist", m.View)
	}
	m.Command(Command{Name: "scope", Arg: "session"})
	if m.Scope != ScopeSession || m.SettingsChanged() {
		t.Errorf("scope session: %s, or a setting to persist", m.Scope)
	}
	// A scope set from outside clears F's memory: F from session goes
	// to all, not back to what F left.
	m.Scope, m.prevScope = ScopeProject, ""
	m.Handle(term.Key{Rune: 'F'})
	m.Command(Command{Name: "scope", Arg: "session"})
	m.Handle(term.Key{Rune: 'F'})
	if m.Scope != ScopeAll {
		t.Errorf("F after a scope from outside: %s", m.Scope)
	}
	m.Scope = ScopeAll
	m.Ask("Quit sidebar? y/n", "quit")
	m.Command(Command{Name: "view", Arg: "agents"})
	if m.View != ViewTree {
		t.Error("a command acted while a question was up")
	}
	m.Confirm, m.ConfirmTag = "", ""
	m.Overlay = NewHelp("h", false)
	was := m.Selected
	m.Command(Command{Name: "next"})
	if m.Selected != was {
		t.Error("a command acted while an overlay was up")
	}
	m.Overlay = nil
	// A pending change is kept through a command, and a view from the
	// CLI is no view to write.
	m.settings, m.viewSet = true, false
	m.Command(Command{Name: "view", Arg: "agents"})
	if !m.settings {
		t.Error("a pending change dropped by a command")
	}
	if v, _ := m.ChangedDefaults(); v {
		t.Error("a view from the CLI to be written")
	}
	m.settings = false
	// A strip keeps the agent view.
	m.View, m.Layout = ViewAgents, Strip
	m.Command(Command{Name: "view", Arg: "tree"})
	if m.View != ViewAgents {
		t.Error("a strip switched to the tree")
	}
}

// ? shows the keys, any key closes it; q and Ctrl-C ask in a sidebar
// pane, while filtering too, and y quits; the settings' changes are
// reported once, the toggled folds given and taken.
func TestHelpQuitSettings(t *testing.T) {
	now := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	in := treeInput(now)
	m := &Model{Now: now, LocalHost: "mac", View: ViewTree, Width: 60, Height: 20, AskQuit: true, HelpTitle: "keys", Help: []string{"z            settle"}}
	m.SetTree(rows.Tree(in))
	m.SetRows(rows.Agents(in, rows.Tree(in)))
	m.Render()
	m.Handle(term.Key{Rune: '?'})
	if m.Overlay == nil {
		t.Fatal("no help overlay")
	}
	text := Text(m.Render())
	if !strings.HasPrefix(text, "keys\n") || !strings.Contains(text, "Tab          switch view") || !strings.Contains(text, "z            settle") {
		t.Errorf("help:\n%s", text)
	}
	m.Handle(term.Key{Rune: 'x'})
	if m.Overlay != nil {
		t.Error("a key did not close the help")
	}
	m.Layout = Strip
	m.Handle(term.Key{Rune: '?'})
	if text := Text(m.Render()); strings.Contains(text, "Tab") || !strings.Contains(text, "\nf            the stale chip, open or closed") {
		t.Errorf("the strip's help:\n%s", text)
	}
	m.Handle(term.Key{Rune: 'x'})
	// A one-line strip: the keys alone, one at a time, scrolled.
	m.Height = 1
	m.Handle(term.Key{Rune: '?'})
	if out := m.Render(); len(out) != 1 || !strings.HasPrefix(Text(out), "h l") {
		t.Errorf("help on one line:\n%s", Debug(out))
	}
	m.Handle(term.Key{Kind: term.KeyDown})
	if out := m.Render(); !strings.HasPrefix(Text(out), "g G") {
		t.Errorf("help on one line scrolled:\n%s", Debug(out))
	}
	for i := 0; i < 20; i++ {
		m.Handle(term.Key{Kind: term.KeyDown})
	}
	if out := m.Render(); !strings.HasPrefix(Text(out), "z            settle") {
		t.Errorf("help on one line scrolled past the end:\n%s", Debug(out))
	}
	m.Handle(term.Key{Rune: 'x'})
	m.Height = 20
	m.Layout = Tiles
	if a := m.Handle(term.Key{Rune: 'q'}); a.Kind != ActionNone || m.Confirm != "Quit sidebar? y/n" {
		t.Errorf("q: %+v %q", a, m.Confirm)
	}
	if a := m.Handle(term.Key{Rune: 'n'}); a.Kind != ActionNone || m.Confirm != "" {
		t.Errorf("n: %+v %q", a, m.Confirm)
	}
	m.Handle(term.Key{Rune: '/'})
	m.Handle(term.Key{Kind: term.KeyCtrlC})
	if m.Confirm == "" {
		t.Error("Ctrl-C while filtering did not ask")
	}
	if a := m.Handle(term.Key{Rune: 'y'}); a.Kind != ActionQuit {
		t.Errorf("y: %+v", a)
	}
	m.Filtering = false
	m.AskQuit = false
	if a := m.Handle(term.Key{Rune: 'q'}); a.Kind != ActionQuit {
		t.Errorf("q without the question: %+v", a)
	}
	// Settings: a fold toggled, the view switched, the layout, F; each
	// reported once; the toggled folds given and taken.
	m.SettingsChanged()
	m.Select("venv/worktree//r/agents-config")
	m.Handle(term.Key{Rune: 's'})
	if !m.SettingsChanged() || m.SettingsChanged() {
		t.Error("a fold toggled: not reported once")
	}
	folds := m.ToggledFolds()
	if len(folds) != 1 || !folds["venv/worktree//r/agents-config"] {
		t.Errorf("toggled folds: %v", folds)
	}
	m.Handle(term.Key{Kind: term.KeyTab})
	if !m.SettingsChanged() {
		t.Error("the view switched: not reported")
	}
	m.Handle(term.Key{Rune: 'v'})
	if !m.SettingsChanged() {
		t.Error("the layout: not reported")
	}
	m.Handle(term.Key{Kind: term.KeyTab})
	m.SettingsChanged()
	m.Handle(term.Key{Rune: 'f'})
	if !m.SettingsChanged() || len(m.ToggledFolds()) < 3 {
		t.Errorf("f: %v", m.ToggledFolds())
	}
	other := &Model{Now: now, LocalHost: "mac", View: ViewTree, Width: 60, Height: 20}
	other.SetTree(rows.Tree(in))
	other.SetRows(rows.Agents(in, rows.Tree(in)))
	other.Render()
	other.Select("venv/laatmux/%8") // under auto-layout
	other.ApplyFolds(map[string]bool{"venv/worktree//r/agents-config": true})
	other.Render()
	if !other.closed(&other.Tree[other.indexOf("venv/worktree//r/agents-config")]) || other.SettingsChanged() {
		t.Error("a fold applied from outside: not closed, or reported as a change")
	}
	if r := other.Selection(); r == nil || r.ID() != "venv/laatmux/%8" {
		t.Errorf("a fold applied from outside moved the selection: %+v", r)
	}
	// The folds taken from outside are not this pane's to write; its
	// own are, once.
	if d, _ := other.DirtyFolds(); len(d) != 0 {
		t.Errorf("folds from outside dirty: %v", d)
	}
	other.Select("venv/worktree//r/auto-layout")
	other.Handle(term.Key{Rune: 's'})
	if d, c := other.DirtyFolds(); len(d) != 1 || !d["venv/worktree//r/auto-layout"] || len(c) != 0 {
		t.Errorf("the pane's own fold not dirty: %v %v", d, c)
	}
	if d, _ := other.DirtyFolds(); len(d) != 0 {
		t.Errorf("dirty twice: %v", d)
	}
	// The stale fold is a fold like the others: toggled, dirty, taken.
	other.View = ViewAgents
	other.Handle(term.Key{Rune: 'f'})
	if d, _ := other.DirtyFolds(); !other.ShowHidden || len(d) != 1 || d[rows.NodeStale] {
		t.Errorf("the stale fold opened: shown %v dirty %v", other.ShowHidden, d)
	}
	other.ApplyFolds(map[string]bool{rows.NodeStale: true})
	if other.ShowHidden {
		t.Error("the stale fold closed from outside still open")
	}
	// A value the file held last time is not applied again: a fold
	// opened here to reveal a selection stays open when an unrelated
	// write comes round.
	other.View = ViewTree
	other.ApplyFolds(map[string]bool{"venv/worktree//r/agents-config": true})
	other.Render()
	other.setFold("venv/worktree//r/agents-config", false) // a reveal, not the user's
	other.ApplyFolds(map[string]bool{"venv/worktree//r/agents-config": true, "repo/other": true})
	if other.closed(&other.Tree[other.indexOf("venv/worktree//r/agents-config")]) {
		t.Error("a reveal undone by a value the file held before")
	}
	other.ApplyFolds(map[string]bool{"venv/worktree//r/agents-config": false})
	other.ApplyFolds(map[string]bool{"venv/worktree//r/agents-config": true})
	if !other.closed(&other.Tree[other.indexOf("venv/worktree//r/agents-config")]) {
		t.Error("a changed value not applied")
	}
	// A value this pane wrote is the baseline: another pane's change
	// back to the old value is a change.
	other.Select("venv/worktree//r/agents-config")
	other.Handle(term.Key{Rune: 'l'}) // open, the user's
	other.DirtyFolds()                // written
	other.ApplyFolds(map[string]bool{"venv/worktree//r/agents-config": true})
	if !other.closed(&other.Tree[other.indexOf("venv/worktree//r/agents-config")]) {
		t.Error("another pane's change back not applied after a write here")
	}
	// A fold the file dropped is forgotten here too, unless set here
	// since; one dropped and back gets the first fold anew.
	other.ApplyFolds(map[string]bool{"venv/worktree//r/auto-layout": true})
	other.ApplyFolds(map[string]bool{})
	if _, ok := other.toggled["venv/worktree//r/auto-layout"]; ok {
		t.Error("a fold the file dropped kept")
	}
	if other.closed(&other.Tree[other.indexOf("venv/worktree//r/auto-layout")]) {
		t.Error("a fold the file dropped still closed")
	}
	other.ApplyFolds(map[string]bool{"venv/worktree//r/auto-layout": false})
	other.Select("venv/worktree//r/auto-layout")
	other.Handle(term.Key{Rune: 'h'}) // set here, not yet written
	other.ApplyFolds(map[string]bool{})
	if !other.closed(&other.Tree[other.indexOf("venv/worktree//r/auto-layout")]) {
		t.Error("a fold set here forgotten with the file's")
	}
	// A handoff carries the user's value, the file's, not a reveal
	// this pane made over it; the reveal stays on screen.
	src := "git@github.com:laat/laatmux.git"
	hin := treeInput(now)
	hin.Agents = append(hin.Agents, protocol.Agent{ID: "venv/laatmux/%9", EnvironmentID: "venv", Server: "laatmux", Session: "laatmux/new-one", Agent: "claude", Activity: protocol.Idle, ActivityAt: now, Liveness: protocol.Alive, Managed: true, Cwd: "/r/new-one", Identity: &protocol.Identity{PID: 9, StartUnix: 9}})
	hin.Pendings = []protocol.Pending{{ID: "add-h", Host: "vm", EnvironmentID: "venv", Source: src, Repo: "laatmux", Branch: "new-one", Root: "/r/new-one", Session: "laatmux/new-one", Taken: true, SubmittedAt: now}}
	h := &Model{Now: now, LocalHost: "mac", View: ViewTree, Width: 60, Height: 30}
	h.SetTree(rows.Tree(hin))
	h.SetRows(rows.Agents(hin, rows.Tree(hin)))
	h.Render()
	h.ApplyFolds(map[string]bool{"add-h": true}) // the user's, from the file
	h.setFold("add-h", false)                    // a reveal here
	hin.Worktrees = append(hin.Worktrees, protocol.Worktree{ID: "venv/worktree//r/new-one", EnvironmentID: "venv", Repo: "laatmux", Source: src, Branch: "new-one", Root: "/r/new-one", Session: "laatmux/new-one"})
	hin.Agents[len(hin.Agents)-1].WorktreeID = "venv/worktree//r/new-one"
	hin.Pendings = nil
	h.Handoffs = map[string]string{"add-h": "venv/worktree//r/new-one"}
	h.SetTree(rows.Tree(hin))
	h.SetRows(rows.Agents(hin, rows.Tree(hin)))
	if h.closed(&h.Tree[h.indexOf("venv/worktree//r/new-one")]) {
		t.Error("the reveal not kept on the successor")
	}
	if folds, carried := h.DirtyFolds(); !folds["venv/worktree//r/new-one"] || !carried["venv/worktree//r/new-one"] {
		t.Errorf("the user's fold not carried: %v %v", folds, carried)
	}
	if !other.HasNode("venv/worktree//r/auto-layout") || other.HasNode("nope") || !other.HasNode(rows.NodeStale) {
		t.Error("HasNode")
	}
}

// The strip: chips of the top template's lines side by side, the
// selection banded, the count past the edge, the arrows and a click
// moving through them, Tab doing nothing, and the footer only with
// something to say.
func TestStrip(t *testing.T) {
	now := time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)
	m := model(now)
	m.Layout, m.View, m.Width, m.Height, m.ItemWidth = Strip, ViewAgents, 60, 3, 18
	golden(t, "strip", Debug(m.Render()))
	m.Handle(term.Key{Kind: term.KeyRight})
	m.Handle(term.Key{Kind: term.KeyRight})
	m.Handle(term.Key{Kind: term.KeyRight})
	golden(t, "strip-scrolled", Debug(m.Render()))
	if m.Selected != 3 {
		t.Errorf("right thrice: %d", m.Selected)
	}
	m.Handle(term.Key{Rune: 'h'})
	if m.Selected != 2 {
		t.Errorf("h: %d", m.Selected)
	}
	m.Handle(term.Key{Kind: term.KeyTab})
	if m.View != ViewAgents {
		t.Error("Tab switched the strip's view")
	}
	m.Render()
	if a := m.Handle(term.Key{Kind: term.KeyMouse, X: 2, Y: 1}); a.Kind != ActionJump || a.Row == nil || a.Row.ID() != m.Visible()[m.hscroll].Row.ID() {
		t.Errorf("a click on the first chip: %+v", a)
	}
	m.Handle(term.Key{Rune: '/'})
	m.Handle(term.Key{Rune: 'f'})
	out := m.Render()
	if len(out) != 3 || Text(out[2:]) != "/f_\n" {
		t.Errorf("the footer while filtering:\n%s", Debug(out))
	}
	m.Handle(term.Key{Kind: term.KeyEsc})
	if out := m.Render(); strings.HasPrefix(Text(out[2:]), "/") {
		t.Error("the footer without something to say")
	}
	m.Rows = rows.Rows{}
	m.Loading = true
	if !strings.Contains(Text(m.Render()), "Loading") {
		t.Error("no loading state")
	}
	// A one-column strip with chips past the edge, and a one-line one
	// with a question: no panic, the question shown.
	m = model(now)
	m.Layout, m.View, m.Width, m.Height, m.ItemWidth = Strip, ViewAgents, 1, 3, 18
	m.Render()
	m.Width, m.Height = 40, 1
	m.Ask("Quit sidebar? y/n", "quit")
	if out := m.Render(); len(out) != 1 || !strings.HasPrefix(Text(out), "Quit sidebar?") {
		t.Errorf("a question on a one-line strip:\n%s", Debug(out))
	}
	m.Confirm = ""
	// A one-line strip with nothing to show and a question: the
	// question alone, no panic.
	m.Rows, m.Height = rows.Rows{}, 1
	m.Ask("Quit sidebar? y/n", "quit")
	if out := m.Render(); len(out) != 1 || !strings.HasPrefix(Text(out), "Quit sidebar?") {
		t.Errorf("an empty one-line strip with a question:\n%s", Debug(out))
	}
	m.Confirm = ""
	m = model(now)
	m.Layout, m.View, m.Width, m.Height, m.ItemWidth = Strip, ViewAgents, 60, 3, 18
	// The marker keeps its room: no chip is drawn under it, and a click
	// there lands on nothing.
	out = m.Render()
	if a := m.Handle(term.Key{Kind: term.KeyMouse, X: 58, Y: 1}); a.Kind == ActionJump {
		t.Errorf("a click on the marker jumped:\n%s", Debug(out))
	}
	// A click from before the last draw is judged by that draw's
	// lines: with a footer drawn then, its line is no chip.
	m.Filtering = true
	m.Render()
	then := m.Now.Add(time.Millisecond) // after that frame, before the next
	m.Filtering = false
	m.Now = m.Now.Add(time.Second)
	m.Render()
	if a := m.Handle(term.Key{Kind: term.KeyMouse, X: 2, Y: 3, At: then}); a.Kind == ActionJump {
		t.Error("a click on the footer of the frame before jumped")
	}
	if a := m.Handle(term.Key{Kind: term.KeyMouse, X: 2, Y: 1, At: then}); a.Kind != ActionJump {
		t.Errorf("a click on a chip of the frame before: %+v", a)
	}
	// A click on the footer line is no chip: with a filter set and not
	// being typed, the footer shows it and a click there is judged.
	m.Height = 3
	m.Filter, m.Filtering = "a", false
	m.Render()
	if a := m.Handle(term.Key{Kind: term.KeyMouse, X: 2, Y: 3}); a.Kind == ActionJump {
		t.Error("a click on the footer jumped")
	}
	if a := m.Handle(term.Key{Kind: term.KeyMouse, X: 2, Y: 1}); a.Kind != ActionJump {
		t.Errorf("a click on a chip: %+v", a)
	}
	m.Filter = ""
	// A click on the stale chip folds it.
	m.Rows = fixture(now)
	m.Width, m.ItemWidth = 200, 10
	m.Render()
	stale := -1
	for _, c := range m.hitCols {
		if c.id == rows.NodeStale {
			stale = c.from + 1
		}
	}
	if stale < 0 {
		t.Fatalf("no stale chip: %+v", m.hitCols)
	}
	if a := m.Handle(term.Key{Kind: term.KeyMouse, X: stale + 1, Y: 1}); a.Kind != ActionNone || !m.ShowHidden {
		t.Errorf("a click on the stale chip: %+v shown %v", a, m.ShowHidden)
	}
	// The chips' numbers are the digits', fold rows skipped, and a dim
	// row's chip is dim, its template's colour stripped.
	m.SetTemplates(CompileTemplates(nil, "", []string{"{idx} #[fg=accent]{primary}"}, "", "", "", "", ""))
	m.ShowHidden = true
	m.Handle(term.Key{Rune: 'g'})
	text := Text(m.Render())
	first := strings.SplitN(text, "\n", 2)[0]
	var n int
	for _, it := range m.Visible() {
		if it.Row.Numbered() {
			n++
		}
	}
	// The chip is known by its {primary}: the last tile is the settled
	// worktree's agent, whose label is its branch, not its session name.
	if i, ok := m.nth(n); !ok {
		t.Errorf("no number %d:\n%s", n, text)
	} else if p, _ := m.Visible()[i].Row.Labels(); !strings.Contains(first, strconv.Itoa(n)+" "+string([]rune(p)[:4])) {
		t.Errorf("the last number %d not on its chip:\n%s", n, text)
	}
	out = m.Render()
	dimmed := false
	for _, sp := range out[0].Spans {
		if strings.Contains(sp.Text, "dead") {
			// Dim, and colourless: a colour keeps the faint off.
			dimmed = sp.Dim && sp.Fg == ""
		}
	}
	if !dimmed {
		t.Errorf("a dim row's chip not dim:\n%s", Debug(out))
	}
	// The viewer's own label keeps its colour on a dim chip.
	for _, it := range m.Visible() {
		if it.Row.Name == "proj/dead" {
			it.Row.Current = true
		}
	}
	out = m.Render()
	kept := false
	for _, sp := range out[0].Spans {
		if strings.Contains(sp.Text, "dead") {
			kept = sp.Fg == palette.CurrentWorktreeFg && !sp.Dim
		}
	}
	if !kept {
		t.Errorf("the viewer's label on a dim chip lost its colour:\n%s", Debug(out))
	}
	for _, it := range m.Visible() {
		it.Row.Current = false
	}
	m.SetTemplates(DefaultTemplates())
	m.ShowHidden = false
	// A strip too narrow for a chip beside the marker still shows one.
	m.Width, m.ItemWidth = 26, 24
	m.Render()
	if len(m.hitCols) != 1 || !strings.Contains(Text(m.Render()), "→") {
		t.Errorf("a narrow strip: %+v\n%s", m.hitCols, Text(m.Render()))
	}
	// The chip clipped short of the marker: a click on the marker is
	// not a click on the chip.
	if len(m.hitCols) == 1 && m.hitCols[0].to > m.Width-3 {
		t.Errorf("the clipped chip's columns reach the marker: %+v", m.hitCols)
	}
	m.Width, m.ItemWidth = 60, 18
	m.Scope = ScopeSession
	if !strings.Contains(Text(m.Render()), "[session]") {
		t.Error("the strip does not name the scope")
	}
}

// A dim row's chip is dim throughout, as its line is in the list: faint
// in the dimmed colour and without the template's background, which a
// fresh row's chip keeps; the viewer's own label on a dim chip keeps
// its colour, not the background, and is not faint where a template
// makes it dim.
func TestStripDimChip(t *testing.T) {
	now := time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)
	m := model(now)
	m.Layout, m.View, m.Width, m.Height, m.ItemWidth = Strip, ViewAgents, 200, 1, 14
	m.ShowHidden = true
	m.SetTemplates(CompileTemplates(nil, "", []string{"#[bg=#ffff00]{primary}"}, "", "", "", "", ""))
	dark, _ := palette.New(true, nil)
	yellow := dark.SGR("#ffff00", true)
	// chipIn is the named chip of the first line, drawn alone on a line
	// as the strip draws it, in a theme.
	chipIn := func(name string, th palette.Theme) string {
		t.Helper()
		out := m.Render()
		for _, c := range stripChips(out[0]) {
			if l := (Line{Spans: c}); strings.HasPrefix(Text([]Line{l}), name+" ") {
				return ANSI(l, th)
			}
		}
		t.Fatalf("no chip %s:\n%s", name, Debug(out))
		return ""
	}
	chip := func(name string) string { t.Helper(); return chipIn(name, dark) }
	if got := chip("remote-notes"); !strings.Contains(got, yellow+"remote-notes") {
		t.Errorf("a fresh chip lost the template's background: %q", got)
	}
	if got := chip("dead"); strings.Contains(got, yellow) || !strings.Contains(got, dark.SGR(palette.Dimmed, false)+"dead") || strings.Contains(got, "\x1b[2m") {
		t.Errorf("a dim chip not drawn as its line: %q", got)
	}
	for _, it := range m.Visible() {
		if it.Row.Name == "proj/dead" {
			it.Row.Current = true
		}
	}
	if got := chip("dead"); strings.Contains(got, yellow) || !strings.Contains(got, dark.SGR(palette.CurrentWorktreeFg, false)+"dead") {
		t.Errorf("the viewer's label on a dim chip: %q", got)
	}
	// Without colours, where faint would show.
	m.SetTemplates(CompileTemplates(nil, "", []string{"#[dim]{primary}"}, "", "", "", "", ""))
	if got := chipIn("dead", palette.Mono()); !strings.HasPrefix(got, "\x1b[1mdead") {
		t.Errorf("the viewer's label made dim on a dim chip: %q", got)
	}
}

// A dim row's chip is drawn cell for cell as the row's line in the list
// under the same template: faint in the dimmed colour in a theme with
// colours, dark or light, and faint without
// them, a token's colour and a style's dropped, bold kept, no
// background, and the viewer's own label in its colour, not faint. A
// template's text in the label's colour is not the label: dimmed as
// any other. A fresh row's dim token stays faint in the terminal's
// colour, on its chip as on its line.
func TestStripDimChipAsLine(t *testing.T) {
	now := time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)
	dark, _ := palette.New(true, nil)
	mono := palette.Mono()
	const iw = 30
	// both is the named row's line in the compact list, as wide as a
	// chip, and its chip in the strip alone on a line, without the
	// padding the list's line does not draw.
	both := func(src, name string, current bool) (Line, Line) {
		t.Helper()
		m := model(now)
		m.View, m.ItemWidth, m.ShowHidden = ViewAgents, iw, true
		m.SetTemplates(CompileTemplates(nil, src, []string{src}, "", "", "", "", ""))
		for _, it := range m.Visible() {
			if it.Row.Name == "proj/"+name {
				it.Row.Current = current
			}
		}
		named := func(l Line) bool { return slices.Contains(strings.Fields(Text([]Line{l})), name) }
		m.Layout, m.Width, m.Height = Compact, iw, 40
		var line, chip *Line
		for _, l := range m.Render() {
			if named(l) {
				line = &l
			}
		}
		m.Layout, m.Width, m.Height = Strip, 400, 1
		for _, c := range stripChips(m.Render()[0]) {
			if named(Line{Spans: c}) {
				chip = &Line{Spans: c}
			}
		}
		if line == nil || chip == nil {
			t.Fatalf("%q: no line or no chip for %s: %v %v", src, name, line, chip)
		}
		chip.Spans = clip(chip.Spans, spansWidth(line.Spans))
		return *line, *chip
	}
	light, _ := palette.New(false, nil)
	themes := map[string]palette.Theme{"dark": dark, "light": light, "mono": mono}
	const tinted = "#[fg=current_worktree_fg]x {primary}"
	// The dead agent's icon is blank; the down host's agent shows the
	// spinner standing still, in the working colour.
	for _, name := range []string{"dead", "down"} {
		for _, src := range []string{"{primary}", "#[bg=#ffff00]{status_icon} #[fg=accent]{primary} #[bold]@{host}", tinted} {
			for _, current := range []bool{false, true} {
				line, chip := both(src, name, current)
				if !line.Dim {
					t.Fatalf("%q: the %s row's line is not dim", src, name)
				}
				for tn, th := range themes {
					if got, want := drawn(t, ANSI(chip, th)), drawn(t, ANSI(line, th)); got != want {
						t.Errorf("%s %q, current %v, %s: the chip draws\n%s\nthe line\n%s", name, src, current, tn, got, want)
					}
				}
			}
		}
	}
	// The bytes: the line's as they were, and the chip's the same.
	line, chip := both("{primary}", "dead", false)
	for _, c := range []struct {
		th   palette.Theme
		want string
	}{
		{dark, dark.SGR(palette.Dimmed, false) + "dead\x1b[0m"},
		{mono, "\x1b[2mdead\x1b[0m"},
	} {
		if got := ANSI(line, c.th); got != c.want {
			t.Errorf("mono %v: the dim line %q, want %q", c.th.Mono, got, c.want)
		}
		if got := ANSI(chip, c.th); !strings.HasPrefix(got, c.want) {
			t.Errorf("mono %v: the dim chip %q, want its line's %q", c.th.Mono, got, c.want)
		}
	}
	// The x the template gives the label's colour is dimmed, on a row
	// not the viewer's and on the viewer's own, where the label alone
	// keeps the colour and is not faint. The chips draw as these lines,
	// above.
	dim, label := dark.SGR(palette.Dimmed, false), dark.SGR(palette.CurrentWorktreeFg, false)
	for _, c := range []struct {
		current bool
		th      palette.Theme
		want    string
	}{
		{false, dark, dim + "x dead\x1b[0m"},
		{false, mono, "\x1b[2mx dead\x1b[0m"},
		{true, dark, dim + "x \x1b[1m" + label + "dead\x1b[0m"},
		{true, mono, "\x1b[2mx \x1b[22m\x1b[1mdead\x1b[0m"},
	} {
		line, _ := both(tinted, "dead", c.current)
		if got := ANSI(line, c.th); !strings.HasPrefix(got, c.want) {
			t.Errorf("current %v, mono %v: the dim line %q, want %q", c.current, c.th.Mono, got, c.want)
		}
	}
	line, chip = both("{primary} @{host}", "remote-notes", false)
	for _, l := range []Line{line, chip} {
		if got := ANSI(l, dark); !strings.Contains(got, "@\x1b[2mvm/default\x1b[0m") {
			t.Errorf("a fresh row's remote host not faint in the terminal's colour: %q", got)
		}
	}
}

// stripChips is a strip line's chips: the spans between the separators.
func stripChips(l Line) [][]Span {
	chips := [][]Span{nil}
	for _, sp := range l.Spans {
		if sp.Text == stripSep && sp.Fg == palette.Border {
			chips = append(chips, nil)
			continue
		}
		chips[len(chips)-1] = append(chips[len(chips)-1], sp)
	}
	return chips
}
