package view

import (
	"strings"
	"testing"
	"time"

	"github.com/laat/laatmux/internal/protocol"
	"github.com/laat/laatmux/internal/rows"
	"github.com/laat/laatmux/internal/workspace"
)

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
	in.Agents = append(in.Agents, protocol.Agent{ID: "venv/laatmux/%11", EnvironmentID: "venv", Session: "elsewhere", Agent: "codex", Activity: protocol.Working, ActivityAt: now, Liveness: protocol.Alive, Managed: true, WorktreeID: "venv/worktree//r/agents-config", Identity: &protocol.Identity{PID: 11, StartUnix: 40}})
	in.Pendings = []protocol.Pending{
		{ID: "add-ac", Host: "vm", EnvironmentID: "venv", Source: src, Repo: "laatmux", Branch: "agents-config", Root: "/r/agents-config", Session: "laatmux/agents-config", Taken: true, Done: true, OK: true, Prompt: protocol.DeliveryNotDelivered, SubmittedAt: now},
		{ID: "add-new", Host: "vm", EnvironmentID: "venv", Source: src, Repo: "laatmux", Branch: "new-one", Root: "/r/new-one", Session: "laatmux/new-one", Taken: true, SubmittedAt: now},
		{ID: "add-anki", Host: "mac", EnvironmentID: "menv", Source: "https://github.com/laat/anki-llm", Repo: "anki-llm", Branch: "x", Root: "/w/x", Session: "anki-llm/x", Taken: true, SubmittedAt: now},
	}
	m := &Model{Now: now, LocalHost: "mac", View: ViewAgents, Width: 80, Height: 40, Follow: true}
	set := func() {
		m.SetTree(rows.Tree(in))
		m.SetRows(rows.Agents(in))
		m.Render()
	}
	set()
	all := ids(m)
	if !strings.Contains(all, "venv/laatmux/%11") || !strings.Contains(all, "add-anki") {
		t.Fatalf("all:\n%s", all)
	}
	// F: to session. The viewer is in agents-config's home session.
	m.Handle(Key{Rune: 'F'})
	if !m.SettingsChanged() {
		t.Error("F changed no setting")
	}
	want := "add-ac\nvenv/laatmux/%11\nvenv/laatmux/%1\nvenv/laatmux/%2"
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
	m.Handle(Key{Rune: 'F'})
	if m.Scope != ScopeSession {
		t.Errorf("F from project: %s", m.Scope)
	}
	m.Handle(Key{Rune: 'F'})
	if m.Scope != ScopeProject {
		t.Errorf("F back: %s", m.Scope)
	}
	m.Scope, m.prevScope = ScopeSession, ""
	m.Handle(Key{Rune: 'F'})
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
	in.Agents = append(in.Agents, protocol.Agent{ID: "venv/laatmux/%9", EnvironmentID: "venv", Session: "laatmux/new-one", Agent: "claude", Activity: protocol.Idle, ActivityAt: now, Liveness: protocol.Alive, Managed: true, Cwd: "/r/new-one", Identity: &protocol.Identity{PID: 9, StartUnix: 9}})
	in.Locals = append(in.Locals, workspace.Local{Name: "vm/laatmux/new-one", Attach: "vm/laatmux/new-one", Host: "vm"})
	in.Current = "vm/laatmux/new-one"
	m.Scope = ScopeSession
	set()
	if got := ids(m); got != rows.RepoNode(src)+"\nadd-new" {
		t.Errorf("a task's session:\n%s", got)
	}
	m.Select("add-new")
	m.Handle(Key{Rune: 'l'})
	if got := ids(m); got != rows.RepoNode(src)+"\nadd-new\nvenv/laatmux/%9" {
		t.Errorf("a task's session, unfolded:\n%s", got)
	}
	// A session with no worktree: the viewer's line alone; one that is
	// no row's: the empty state.
	in.Current = "mac/scratch"
	in.Locals = append(in.Locals, workspace.Local{Name: "mac/scratch", Attach: "mac/scratch", Host: "mac"})
	in.Agents = append(in.Agents, protocol.Agent{ID: "menv/laatmux/%12", EnvironmentID: "menv", Session: "scratch", Agent: "claude", Activity: protocol.Idle, ActivityAt: now, Liveness: protocol.Alive, Managed: true, Identity: &protocol.Identity{PID: 12, StartUnix: 12}})
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
	// An orphaned session under its repository is the viewer's line.
	in.Current = "mac/laatmux/gone"
	m.View = ViewTree
	set()
	if got := ids(m); got != rows.RepoNode(src)+"\nsession/mac/laatmux/gone" {
		t.Errorf("an orphaned session:\n%s", got)
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
	m.SetRows(rows.Agents(in))
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
	m.Ask("Quit sidebar? y/n", "quit")
	m.Command(Command{Name: "view", Arg: "agents"})
	if m.View != ViewTree {
		t.Error("a command acted while a question was up")
	}
	m.Confirm, m.ConfirmTag = "", ""
	m.Overlay = NewHelp("h")
	was := m.Selected
	m.Command(Command{Name: "next"})
	if m.Selected != was {
		t.Error("a command acted while an overlay was up")
	}
	m.Overlay = nil
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
	m.SetRows(rows.Agents(in))
	m.Render()
	m.Handle(Key{Rune: '?'})
	if m.Overlay == nil {
		t.Fatal("no help overlay")
	}
	text := Text(m.Render())
	if !strings.HasPrefix(text, "keys\n") || !strings.Contains(text, "Tab          switch view") || !strings.Contains(text, "z            settle") {
		t.Errorf("help:\n%s", text)
	}
	m.Handle(Key{Rune: 'x'})
	if m.Overlay != nil {
		t.Error("a key did not close the help")
	}
	if a := m.Handle(Key{Rune: 'q'}); a.Kind != ActionNone || m.Confirm != "Quit sidebar? y/n" {
		t.Errorf("q: %+v %q", a, m.Confirm)
	}
	if a := m.Handle(Key{Rune: 'n'}); a.Kind != ActionNone || m.Confirm != "" {
		t.Errorf("n: %+v %q", a, m.Confirm)
	}
	m.Handle(Key{Rune: '/'})
	m.Handle(Key{Kind: KeyCtrlC})
	if m.Confirm == "" {
		t.Error("Ctrl-C while filtering did not ask")
	}
	if a := m.Handle(Key{Rune: 'y'}); a.Kind != ActionQuit {
		t.Errorf("y: %+v", a)
	}
	m.Filtering = false
	m.AskQuit = false
	if a := m.Handle(Key{Rune: 'q'}); a.Kind != ActionQuit {
		t.Errorf("q without the question: %+v", a)
	}
	// Settings: a fold toggled, the view switched, the layout, F; each
	// reported once; the toggled folds given and taken.
	m.SettingsChanged()
	m.Select("venv/worktree//r/agents-config")
	m.Handle(Key{Rune: 's'})
	if !m.SettingsChanged() || m.SettingsChanged() {
		t.Error("a fold toggled: not reported once")
	}
	folds := m.ToggledFolds()
	if len(folds) != 1 || !folds["venv/worktree//r/agents-config"] {
		t.Errorf("toggled folds: %v", folds)
	}
	m.Handle(Key{Kind: KeyTab})
	if !m.SettingsChanged() {
		t.Error("the view switched: not reported")
	}
	m.Handle(Key{Rune: 'v'})
	if !m.SettingsChanged() {
		t.Error("the layout: not reported")
	}
	m.Handle(Key{Kind: KeyTab})
	m.SettingsChanged()
	m.Handle(Key{Rune: 'f'})
	if !m.SettingsChanged() || len(m.ToggledFolds()) < 3 {
		t.Errorf("f: %v", m.ToggledFolds())
	}
	other := &Model{Now: now, LocalHost: "mac", View: ViewTree, Width: 60, Height: 20}
	other.SetTree(rows.Tree(in))
	other.SetRows(rows.Agents(in))
	other.ApplyFolds(map[string]bool{"venv/worktree//r/auto-layout": true})
	other.Render()
	if !other.closed(&other.Tree[other.indexOf("venv/worktree//r/auto-layout")]) || other.SettingsChanged() {
		t.Error("a fold applied from outside: not closed, or reported as a change")
	}
	if !other.HasNode("venv/worktree//r/auto-layout") || other.HasNode("nope") {
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
	m.Handle(Key{Kind: KeyRight})
	m.Handle(Key{Kind: KeyRight})
	m.Handle(Key{Kind: KeyRight})
	golden(t, "strip-scrolled", Debug(m.Render()))
	if m.Selected != 3 {
		t.Errorf("right thrice: %d", m.Selected)
	}
	m.Handle(Key{Rune: 'h'})
	if m.Selected != 2 {
		t.Errorf("h: %d", m.Selected)
	}
	m.Handle(Key{Kind: KeyTab})
	if m.View != ViewAgents {
		t.Error("Tab switched the strip's view")
	}
	m.Render()
	if a := m.Handle(Key{Kind: KeyMouse, X: 2, Y: 1}); a.Kind != ActionJump || a.Row == nil || a.Row.ID() != m.Visible()[m.hscroll].Row.ID() {
		t.Errorf("a click on the first chip: %+v", a)
	}
	m.Handle(Key{Rune: '/'})
	m.Handle(Key{Rune: 'f'})
	out := m.Render()
	if len(out) != 3 || Text(out[2:]) != "/f_\n" {
		t.Errorf("the footer while filtering:\n%s", Debug(out))
	}
	m.Handle(Key{Kind: KeyEsc})
	if out := m.Render(); strings.HasPrefix(Text(out[2:]), "/") {
		t.Error("the footer without something to say")
	}
	m.Rows = rows.Rows{}
	m.Loading = true
	if !strings.Contains(Text(m.Render()), "Loading") {
		t.Error("no loading state")
	}
}
