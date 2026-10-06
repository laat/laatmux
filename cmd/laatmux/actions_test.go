package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/laat/laatmux/internal/command"
	"github.com/laat/laatmux/internal/config"
	"github.com/laat/laatmux/internal/home"
	"github.com/laat/laatmux/internal/merged"
	"github.com/laat/laatmux/internal/peer"
	"github.com/laat/laatmux/internal/protocol"
	"github.com/laat/laatmux/internal/rows"
	"github.com/laat/laatmux/internal/term"
	"github.com/laat/laatmux/internal/tmux"
	"github.com/laat/laatmux/internal/view"
)

func dashConfig(t *testing.T) config.Config {
	t.Helper()
	cfg, err := config.Parse([]byte(`
hosts:
  - name: mac
    repos: /r
    worktrees: /w
  - name: vm
    ssh: vm
    repos: /r
    worktrees: /w
  - name: box
    ssh: box
agents:
  claude: {cmd: [claude]}
  codex: {cmd: [codex]}
repos:
  - git@github.com:laat/laatmux.git
  - git@github.com:laat/proj.git
`))
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

// dashModel is the dashboard's model filled as fill fills it, the tree
// then the agent view, which it shows: proj/task's agent is its one
// tile; the worktrees with no agent, proj/spike and x/y, and the
// orphaned session vm/proj/gone are the tree's lines.
func dashModel(cfg config.Config) *view.Model {
	m := &view.Model{Width: 80, Height: 20}
	in := rows.Input{
		Hosts: []rows.Host{
			{Name: "mac", Local: true, EnvironmentID: "menv", Connected: true, Listed: true, Worktrees: true},
			{Name: "vm", EnvironmentID: "venv", Connected: true, Listed: true, Worktrees: true},
		},
		Agents: []protocol.Agent{
			{ID: "venv/laatmux/%1", EnvironmentID: "venv", Server: "laatmux", Session: "proj/task", Agent: "claude", Activity: protocol.Working, Liveness: protocol.Alive, Managed: true},
		},
		Worktrees: []protocol.Worktree{
			{ID: "venv/worktree//w/proj/task", EnvironmentID: "venv", Repo: "proj", Source: "git@github.com:laat/proj.git", Branch: "task", Root: "/w/proj/task", Session: "proj/task"},
			{ID: "venv/worktree//w/proj/spike", EnvironmentID: "venv", Repo: "proj", Source: "git@github.com:laat/proj.git", Branch: "spike", Root: "/w/proj/spike"},
			{ID: "menv/worktree//w/x/y", EnvironmentID: "menv", Repo: "x", Branch: "y", Root: "/w/x/y", Session: "x/y"},
		},
		Locals: []protocol.Session{
			{Name: "vm/proj/task", Key: "venv//w/proj/task", Host: "vm", Source: "git@github.com:laat/proj.git", Branch: "task"},
			{Name: "vm/proj/gone", Key: "venv//w/proj/gone", Host: "vm", Source: "git@github.com:laat/proj.git", Branch: "gone"},
		},
	}
	m.SetTree(rows.Tree(in))
	m.SetRows(rows.Agents(in, rows.Tree(in)))
	m.ShowHidden = true
	m.Render()
	return m
}

// treeView shows the model's tree, where the worktrees with no agent and
// the orphaned sessions are.
func treeView(m *view.Model) {
	m.View = view.ViewTree
	m.Render()
}

func selectRow(t *testing.T, m *view.Model, name string) *rows.Row {
	t.Helper()
	for _, it := range m.Visible() {
		if it.Row.Name == name {
			m.Selected = it.Index
			return m.Selection()
		}
	}
	t.Fatalf("no row %q", name)
	return nil
}

// visibleRow is the first visible row with the name, left unselected.
func visibleRow(t *testing.T, m *view.Model, name string) *rows.Row {
	t.Helper()
	for _, it := range m.Visible() {
		if it.Row.Name == name {
			return it.Row
		}
	}
	t.Fatalf("no row %q", name)
	return nil
}

// a on a worktree row without a session opens the form pre-filled with
// the record's repository and host, and its branch explicit; Enter on
// a chip opens the picker inside the form, and Esc anywhere returns to
// the list with nothing done. The worktree, with no agent, is a line
// in the tree.
func TestAddFlowPrefilled(t *testing.T) {
	t.Setenv("LAATMUX_HOME", t.TempDir())
	cfg := dashConfig(t)
	d := &dash{ctx: context.Background(), cfg: cfg, st: merged.New()}
	m := dashModel(cfg)
	treeView(m)
	selectRow(t, m, "proj/spike")
	d.act(m, view.Action{Kind: view.ActionOther, Key: term.Key{Rune: 'a'}})
	f, ok := m.Overlay.(*view.Form)
	if !ok || f.Chips[0].Label() != "proj" || f.Chips[1].Label() != "vm" || len(f.Chips[1].Choices) != 2 || len(f.Chips[2].Choices) != 2 {
		t.Fatalf("form: %+v", m.Overlay)
	}
	if f.Branch() != "spike" || f.Generated() {
		t.Fatalf("branch %q generated %v", f.Branch(), f.Generated())
	}
	if err := f.Validate("bad..name"); err == nil {
		t.Error("form accepted a name git refuses")
	}
	// The picker opens from a chip and is drawn in the form's place.
	f.Handle(term.Key{Kind: term.KeyTab})
	f.Handle(term.Key{Kind: term.KeyTab})
	f.Handle(term.Key{Kind: term.KeyEnter})
	if !strings.Contains(view.Text(f.Render(60, 12)), "add a task: repository") {
		t.Fatalf("no picker:\n%s", view.Text(f.Render(60, 12)))
	}
	f.Handle(term.Key{Kind: term.KeyEsc})
	d.act(m, m.Poll())
	if m.Overlay == nil {
		t.Fatal("esc in the picker ended the form")
	}
	f.Handle(term.Key{Kind: term.KeyEsc})
	d.act(m, m.Poll())
	if m.Overlay != nil || d.add != nil || d.run != nil {
		t.Errorf("esc did not return to the list: overlay=%v add=%v run=%v", m.Overlay, d.add, d.run)
	}
}

// The last-used host and agent for the repository are preselected, the
// configured default agent else the first without one, and a field
// with one candidate is shown, not skipped.
func TestAddFlowDefaults(t *testing.T) {
	t.Setenv("LAATMUX_HOME", t.TempDir())
	cfg := dashConfig(t)
	if err := home.UpdateLast(func(l *home.Last) {
		l.Set("git@github.com:laat/laatmux.git", home.LastRepo{Host: "vm", Agent: "codex"})
	}); err != nil {
		t.Fatal(err)
	}
	d := &dash{ctx: context.Background(), cfg: cfg, st: merged.New()}
	m := dashModel(cfg)
	selectRow(t, m, "proj/task") // a row with a session pre-fills its repository and host, not the branch
	d.act(m, view.Action{Kind: view.ActionOther, Key: term.Key{Rune: 'a'}})
	f := m.Overlay.(*view.Form)
	if f.Chips[0].Label() != "proj" || f.Chips[1].Label() != "vm" || f.Chips[2].Label() != "claude" || f.Branch() != "" {
		t.Fatalf("preselected %q %q %q branch %q", f.Chips[0].Label(), f.Chips[1].Label(), f.Chips[2].Label(), f.Branch())
	}
	f.Handle(term.Key{Kind: term.KeyEsc})
	d.act(m, m.Poll())

	// A repository line preselects by its source, not the host's label
	// for it: vm calls laatmux "proj", another repository's name here;
	// one holding an orphaned session alone, whose source tag ends in
	// another repository's name but is not configured here, preselects
	// none, the first configured, not that one; one an older host
	// names by a label alone goes by the label.
	m.View = view.ViewTree
	m.SetTree(rows.Tree(rows.Input{
		Hosts: []rows.Host{{Name: "vm", EnvironmentID: "venv", Connected: true, Listed: true, Worktrees: true}},
		Worktrees: []protocol.Worktree{
			{ID: "venv/worktree//w/proj/x", EnvironmentID: "venv", Repo: "proj", Source: "git@github.com:laat/laatmux.git", Branch: "x", Root: "/w/proj/x", Session: "proj/x"},
			{ID: "venv/worktree//w/old/y", EnvironmentID: "venv", Repo: "proj", Branch: "y", Root: "/w/old/y", Session: "proj/y"},
		},
		Locals: []protocol.Session{{Name: "vm/proj/gone", Key: "venv//w/proj/gone", Host: "vm", Source: "https://github.com/other/proj"}},
	}))
	m.Render()
	for _, c := range []struct{ source, want string }{{"git@github.com:laat/laatmux.git", "laatmux"}, {"https://github.com/other/proj", "laatmux"}, {"", "proj"}} {
		id := rows.RepoNode(c.source)
		if c.source == "" {
			id = rows.LabelRepoNode("proj")
		}
		if !m.Select(id) {
			t.Fatalf("no repository line for %q", c.source)
		}
		d.act(m, view.Action{Kind: view.ActionOther, Key: term.Key{Rune: 'a'}})
		f = m.Overlay.(*view.Form)
		if f.Chips[0].Label() != c.want {
			t.Fatalf("the repository line for %s preselected %q", c.source, f.Chips[0].Label())
		}
		f.Handle(term.Key{Kind: term.KeyEsc})
		d.act(m, m.Poll())
	}

	// No last-used agent for the repository: the configured default is
	// preselected; without one, the first agent. On laatmux's own line,
	// the repository whose last entry has the host and no agent.
	if err := home.UpdateLast(func(l *home.Last) { l.Set("git@github.com:laat/laatmux.git", home.LastRepo{Host: "vm"}) }); err != nil {
		t.Fatal(err)
	}
	if !m.Select(rows.RepoNode("git@github.com:laat/laatmux.git")) {
		t.Fatal("no laatmux line")
	}
	for _, c := range []struct{ def, want string }{{"codex", "codex"}, {"", "claude"}} {
		cfg.DefaultAgentName = c.def
		d.cfg = cfg
		d.act(m, view.Action{Kind: view.ActionOther, Key: term.Key{Rune: 'a'}})
		f := m.Overlay.(*view.Form)
		if f.Chips[0].Label() != "laatmux" || f.Chips[2].Label() != c.want {
			t.Fatalf("default_agent %q: preselected %q, agent %q", c.def, f.Chips[0].Label(), f.Chips[2].Label())
		}
		f.Handle(term.Key{Kind: term.KeyEsc})
		d.act(m, m.Poll())
	}

	// One agent and one able host: both chips are shown with their one
	// candidate.
	cfg.Agents = map[string]config.Agent{"claude": {Cmd: []string{"claude"}}}
	cfg.Hosts = cfg.Hosts[1:2]
	d = &dash{ctx: context.Background(), cfg: cfg, st: merged.New()}
	m = dashModel(cfg)
	d.act(m, view.Action{Kind: view.ActionOther, Key: term.Key{Rune: 'a'}})
	f = m.Overlay.(*view.Form)
	if len(f.Chips[1].Choices) != 1 || len(f.Chips[2].Choices) != 1 || f.Chips[1].Label() != "vm" || f.Chips[2].Label() != "claude" {
		t.Fatalf("single candidates: %+v", f.Chips)
	}

	// No agents: refused with a message, nothing up.
	cfg.Agents = nil
	d = &dash{ctx: context.Background(), cfg: cfg, st: merged.New()}
	m = dashModel(cfg)
	d.act(m, view.Action{Kind: view.ActionOther, Key: term.Key{Rune: 'a'}})
	if m.Overlay != nil || d.add != nil || m.Message != "no agents configured" {
		t.Errorf("no agents: overlay=%v message=%q", m.Overlay, m.Message)
	}
}

// last.json that cannot be read refuses the add before any picker,
// since the add would otherwise fail on it after the host's side is
// done.
func TestAddFlowRefusesBadLast(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("LAATMUX_HOME", dir)
	if err := os.WriteFile(filepath.Join(dir, "last.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := dashConfig(t)
	d := &dash{ctx: context.Background(), cfg: cfg, st: merged.New()}
	m := dashModel(cfg)
	d.act(m, view.Action{Kind: view.ActionOther, Key: term.Key{Rune: 'a'}})
	if m.Overlay != nil || d.add != nil || !strings.HasPrefix(m.Message, "last.json: ") {
		t.Errorf("overlay=%v add=%v message=%q", m.Overlay, d.add, m.Message)
	}
}

// S routes an existing session by the host that answers for its key's
// environment id: not the tag a renamed host left on it, and not the
// row's host, which for an agent observed in a local window of the
// workspace is this machine while the worktree is on the other one.
func TestShellRoutesByKeyEnvironment(t *testing.T) {
	cfg := dashConfig(t)
	d := &dash{ctx: context.Background(), cfg: cfg, st: merged.New()}
	d.st.Apply(protocol.Message{Type: protocol.TypeSnapshot, Hosts: []protocol.HostStatus{{Name: "mac", EnvironmentID: "menv"}, {Name: "vm", SSH: "vm", EnvironmentID: "venv"}}})
	r := rows.Row{Host: "vm", Name: "proj/task", Local: &protocol.Session{Name: "oldvm/proj/task", Key: "venv//w/proj/task", Host: "oldvm"}}
	l, err := d.localFor(r)
	if err != nil || l.Host != "vm" || l.Name != "oldvm/proj/task" {
		t.Errorf("renamed host: localFor = %+v, %v", l, err)
	}
	in := rows.Input{
		Hosts:  []rows.Host{{Name: "mac", Local: true, EnvironmentID: "menv", Connected: true, Listed: true}, {Name: "vm", EnvironmentID: "venv", Connected: true, Listed: true}},
		Agents: []protocol.Agent{{ID: "menv/default/%6", EnvironmentID: "menv", Server: "default", Session: "vm/proj/task", Agent: "claude", Activity: protocol.Idle, Liveness: protocol.Alive}},
		Locals: []protocol.Session{{Name: "vm/proj/task", Key: "venv//w/proj/task", Host: "vm"}},
	}
	rs := rows.Agents(in, rows.Tree(in))
	if len(rs.Main) != 1 || rs.Main[0].Host != "mac" {
		t.Fatalf("rows = %+v", rs.Main)
	}
	l, err = d.localFor(rs.Main[0])
	if err != nil || l.Host != "vm" {
		t.Errorf("observed agent in a workspace window: localFor = %+v, %v", l, err)
	}
	if _, err := d.localFor(rows.Row{Name: "s", Orphaned: true, Local: &protocol.Session{Name: "s", Key: "venv//gone"}}); err == nil {
		t.Error("orphaned row accepted")
	}
	if _, err := d.localFor(rows.Row{Name: "scratch", Local: &protocol.Session{Name: "mac/scratch", Attach: "mac/scratch"}}); err == nil {
		t.Error("plain attachment accepted")
	}
	// A row without a local session gets the one its own jump makes: a
	// worktree with a home; one with the home lost, through its root
	// agent, as a task line standing for it after pendingTarget; none
	// for a worktree with neither, or whose agent is on a default
	// server.
	w := protocol.Worktree{ID: "venv/worktree//w/proj/z", EnvironmentID: "venv", Repo: "proj", Source: "git@github.com:laat/proj.git", Branch: "z", Root: "/w/proj/z", Session: "proj/z"}
	lost := w
	lost.Session = ""
	root := protocol.Agent{ID: "venv/laatmux/%2", EnvironmentID: "venv", Server: "laatmux", Session: "proj/z-2", Managed: true, Cwd: "/w/proj/z"}
	deflt := protocol.Agent{ID: "venv/default/%3", EnvironmentID: "venv", Server: "default", Session: "notes"}
	task := rows.Row{Kind: rows.KindTask, Host: "vm", Name: "proj/z", Worktree: &lost, Agent: &root, Pending: &protocol.Pending{ID: "add-z", Host: "vm", EnvironmentID: "venv", Source: w.Source, Repo: "proj", Branch: "z", Root: "/w/proj/z", Session: "proj/z", Taken: true, Done: true, OK: true, Prompt: protocol.DeliveryNone}}
	target, err := pendingTarget(task)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		row     rows.Row
		managed string
	}{
		{rows.Row{Kind: rows.KindWorktree, Host: "vm", Name: "proj/z", Worktree: &w}, "proj/z"},
		{rows.Row{Kind: rows.KindWorktree, Host: "vm", Name: "proj/z", Worktree: &lost, Agent: &root}, "proj/z-2"},
		{target, "proj/z-2"},
		{rows.Row{Kind: rows.KindWorktree, Host: "vm", Name: "proj/z", Worktree: &lost}, ""},
		{rows.Row{Kind: rows.KindWorktree, Host: "vm", Name: "proj/z", Worktree: &lost, Agent: &deflt}, ""},
	} {
		spec, err := localSpec(cfg, c.row)
		switch {
		case c.managed == "" && (err == nil || !strings.Contains(err.Error(), "not a workspace")):
			t.Errorf("%+v: spec %+v, %v", c.row.Agent, spec, err)
		case c.managed != "" && (err != nil || spec.Managed != c.managed || spec.Key != "venv//w/proj/z"):
			t.Errorf("%+v: spec %+v, %v", c.row.Agent, spec, err)
		}
	}
	// S on a second agent's node, a tile, a pane or a run goes by the
	// line: the root agent's session, not the second agent's.
	line := rows.Row{Kind: rows.KindWorktree, Depth: 1, Host: "vm", Node: lost.ID, Name: "proj/z", Worktree: &lost, Agent: &root, Children: 2}
	second := protocol.Agent{ID: "venv/laatmux/%4", EnvironmentID: "venv", Server: "laatmux", Session: "scratch", Managed: true, Cwd: "/w/proj/z/sub", WorktreeID: lost.ID}
	tm := &view.Model{Tree: []rows.Row{{Kind: rows.KindRepo, Node: "repo/x"}, line,
		{Kind: rows.KindAgent, Depth: 2, Host: "vm", Node: second.ID, Worktree: &lost, Agent: &second},
		{Kind: rows.KindPane, Depth: 2, Host: "vm", Node: "venv/pane/%8", Worktree: &lost, Pane: &protocol.Pane{PaneID: "%8", Session: "scratch"}}}}
	for _, r := range []rows.Row{tm.Tree[2], tm.Tree[3], {Kind: rows.KindTile, Host: "vm", Node: second.ID, Worktree: &lost, Agent: &second, Local: &protocol.Session{Name: "vm/scratch", Attach: "vm/scratch"}}} {
		row, err := shellRow(tm, r)
		if err != nil || row.ID() != lost.ID {
			t.Errorf("%v: shell row %+v, %v", r.Kind, row, err)
			continue
		}
		if spec, err := localSpec(cfg, row); err != nil || spec.Managed != "proj/z-2" {
			t.Errorf("%v: spec %+v, %v", r.Kind, spec, err)
		}
	}
	// So does a tile with a workspace session of its own, which may be
	// another worktree's (TestShellGoesByLine).
	own := rows.Row{Kind: rows.KindTile, Host: "vm", Node: second.ID, Worktree: &lost, Agent: &second, Local: &protocol.Session{Name: "vm/proj/z", Key: "venv//w/proj/z"}}
	if row, err := shellRow(tm, own); err != nil || row.ID() != lost.ID {
		t.Errorf("a tile with its own session: %+v, %v", row, err)
	}
	// The add's agent before the host lists the worktree, as a node
	// under its task and as a tile: the task's session.
	add := protocol.Agent{ID: "venv/laatmux/%9", EnvironmentID: "venv", Server: "laatmux", Session: "proj/y", Managed: true, Cwd: "/w/proj/y"}
	taskLine := rows.Row{Kind: rows.KindTask, Depth: 1, Host: "vm", Name: "proj/y", Agent: &add, Children: 1, Pending: &protocol.Pending{ID: "add-y", Host: "vm", EnvironmentID: "venv", Source: w.Source, Repo: "proj", Branch: "y", Root: "/w/proj/y", Session: "proj/y", Taken: true, Done: true, OK: true, Prompt: protocol.DeliveryNone}}
	tm.Tree = append(tm.Tree, taskLine, rows.Row{Kind: rows.KindAgent, Depth: 2, Host: "vm", Node: add.ID, Name: "proj/y", Agent: &add})
	for _, r := range []rows.Row{tm.Tree[len(tm.Tree)-1], {Kind: rows.KindTile, Host: "vm", Node: add.ID, Name: "proj/y", Agent: &add}} {
		row, err := shellRow(tm, r)
		if err != nil || row.Worktree == nil || row.Worktree.Session != "proj/y" {
			t.Errorf("%v under a loose task: shell row %+v, %v", r.Kind, row, err)
			continue
		}
		if spec, err := localSpec(cfg, row); err != nil || spec.Managed != "proj/y" || spec.Key != "venv//w/proj/y" {
			t.Errorf("%v under a loose task: spec %+v, %v", r.Kind, spec, err)
		}
	}
	// An observed session of the task's name is not the task's.
	observed := protocol.Agent{ID: "venv/default/%10", EnvironmentID: "venv", Server: "default", Session: "proj/y"}
	if row, err := shellRow(tm, rows.Row{Kind: rows.KindTile, Host: "vm", Node: observed.ID, Name: "proj/y", Agent: &observed}); err != nil || row.ID() != observed.ID {
		t.Errorf("an observed agent named as the task: %+v, %v", row, err)
	}
}

// S on an agent of worktree B observed on this machine's default server
// in a window of worktree A's workspace session, whose row has A's
// session as its own, opens the shell in B's workspace session at B's
// root, as its tile and as its node, whether or not B has a home: the
// session z toggles from the same row. With no workspace session of B's
// it makes one from B's home, as enter on the line does, or refuses
// without a home, as z does; it opens nothing in A's. A child in a
// window of B's own workspace session, under a line holding a plain
// session, does what S on the line does.
func TestShellGoesByLine(t *testing.T) {
	dir := t.TempDir()
	log := filepath.Join(dir, "tmux.log")
	if err := os.WriteFile(filepath.Join(dir, "tmux"), []byte("#!/bin/sh\necho \"$*\" >> "+log+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	cfg := dashConfig(t)
	d := &dash{ctx: context.Background(), cfg: cfg, st: merged.New()}
	a := protocol.Worktree{ID: "menv/worktree//w/a", EnvironmentID: "menv", Repo: "proj", Source: "git@github.com:laat/proj.git", Branch: "a", Root: "/w/a", Session: "proj/a"}
	b := protocol.Worktree{ID: "menv/worktree//w/b", EnvironmentID: "menv", Repo: "proj", Source: "git@github.com:laat/proj.git", Branch: "b", Root: "/w/b"}
	agent := protocol.Agent{ID: "menv/default/%5", EnvironmentID: "menv", Server: "default", Session: "mac/proj/a", Agent: "claude", Activity: protocol.Working, Liveness: protocol.Alive, Cwd: "/w/b/src", WorktreeID: b.ID}
	// The key on the row with the id: the message and the tmux commands
	// run.
	press := func(m *view.Model, id string, key rune) (string, string) {
		t.Helper()
		os.Remove(log)
		if !m.Select(id) {
			t.Fatalf("no row %s", id)
		}
		d.act(m, view.Action{Kind: view.ActionOther, Key: term.Key{Rune: key}})
		got, _ := os.ReadFile(log)
		return m.Message, string(got)
	}
	host := rows.Host{Name: "mac", Local: true, EnvironmentID: "menv", Connected: true, Listed: true, Worktrees: true, Attribution: true}
	show := func(in rows.Input, tree bool) *view.Model {
		m := &view.Model{Width: 100, Height: 20, ShowHidden: true}
		m.SetTree(rows.Tree(in))
		m.SetRows(rows.Agents(in, rows.Tree(in)))
		if tree {
			m.View = view.ViewTree
		}
		m.Render()
		return m
	}
	for _, c := range []struct {
		home      string
		workspace bool // B's workspace session exists
	}{{"", true}, {"proj/b", true}, {"", false}, {"proj/b", false}} {
		b.Session = c.home
		in := rows.Input{
			Hosts:     []rows.Host{host},
			Agents:    []protocol.Agent{agent},
			Worktrees: []protocol.Worktree{a, b},
			Locals:    []protocol.Session{{Name: "mac/proj/a", Key: "menv//w/a", Host: "mac"}},
		}
		if c.workspace {
			in.Locals = append(in.Locals, protocol.Session{Name: "mac/proj/b", Key: "menv//w/b", Host: "mac"})
		}
		for _, tree := range []bool{false, true} {
			m := show(in, tree)
			if !m.Select(agent.ID) {
				t.Fatalf("%+v tree %v: no row %s", c, tree, agent.ID)
			}
			if r := m.Selection(); r.Worktree == nil || r.Worktree.ID != b.ID || r.Local == nil || r.Local.Name != "mac/proj/a" {
				t.Fatalf("%+v tree %v: the agent's row is %+v", c, tree, r)
			}
			msg, cmds := press(m, agent.ID, 'S')
			opened := strings.Contains(cmds, "-L default new-window -t =mac/proj/b: -n shell -c /w/b ;") && !strings.Contains(cmds, "proj/a") && strings.HasPrefix(msg, "mac/proj/b is on the default tmux server")
			switch {
			case c.workspace && (!opened || strings.Contains(cmds, "new-session")):
				t.Errorf("%+v tree %v: S on B's agent: message %q, tmux %q", c, tree, msg, cmds)
			case !c.workspace && c.home != "" && (!opened || !strings.Contains(cmds, "-L default new-session -d -s mac/proj/b ")):
				t.Errorf("%+v tree %v: S on B's agent: message %q, tmux %q", c, tree, msg, cmds)
			case !c.workspace && c.home == "" && (msg != "proj/b: not a workspace" || cmds != ""):
				// B's line, whose jump is the agent's window in A's
				// session, has none to make: S refuses, as on the line.
				t.Errorf("%+v tree %v: S on B's agent: message %q, tmux %q", c, tree, msg, cmds)
			}
			msg, cmds = press(m, agent.ID, 'z')
			switch {
			case c.workspace && (msg != "settled mac/proj/b" || cmds != "-u -L default set-option -t =mac/proj/b: @laatmux_settled 1\n"):
				t.Errorf("%+v tree %v: z on B's agent: message %q, tmux %q", c, tree, msg, cmds)
			case !c.workspace && (!strings.HasPrefix(msg, "proj/b: no local workspace session;") || cmds != ""):
				t.Errorf("%+v tree %v: z on B's agent: message %q, tmux %q", c, tree, msg, cmds)
			}
		}
	}
	// B without a home whose oldest agent, its line's jump agent, sits in
	// a plain session on this machine's default server: the line holds
	// that session. Another agent of B in a window of B's own workspace
	// session has that session as its own; S on it does what S on the
	// line does.
	b.Session = ""
	notes := protocol.Agent{ID: "menv/default/%1", EnvironmentID: "menv", Server: "default", Session: "notes", Agent: "claude", Activity: protocol.Working, Liveness: protocol.Alive, Cwd: "/w/b", WorktreeID: b.ID, Identity: &protocol.Identity{PID: 100, StartUnix: 1}}
	inB := agent
	inB.Session = "mac/proj/b"
	in := rows.Input{
		Hosts:     []rows.Host{host},
		Agents:    []protocol.Agent{notes, inB},
		Worktrees: []protocol.Worktree{a, b},
		Locals:    []protocol.Session{{Name: "mac/proj/a", Key: "menv//w/a", Host: "mac"}, {Name: "mac/proj/b", Key: "menv//w/b", Host: "mac"}},
	}
	m := show(in, true)
	if l := m.OwnerLine(b.ID); l == nil || l.Local == nil || l.Local.Name != "notes" || l.Local.Workspace() {
		t.Fatalf("B's line is %+v", l)
	}
	lineMsg, lineCmds := press(m, b.ID, 'S')
	for _, tree := range []bool{false, true} {
		m := show(in, tree)
		if !m.Select(inB.ID) {
			t.Fatalf("tree %v: no row %s", tree, inB.ID)
		}
		if r := m.Selection(); r.Worktree == nil || r.Worktree.ID != b.ID || r.Local == nil || r.Local.Name != "mac/proj/b" {
			t.Fatalf("tree %v: the agent's row is %+v", tree, r)
		}
		if msg, cmds := press(m, inB.ID, 'S'); msg != lineMsg || cmds != lineCmds {
			t.Errorf("tree %v: S on B's agent in B's session: message %q, tmux %q; on the line: message %q, tmux %q", tree, msg, cmds, lineMsg, lineCmds)
		}
	}
}

// z toggles the line's settled state from any row the line holds, not
// by a child's own local session: a second agent at the root in a
// session of its own settles the workspace session whether or not a
// plain attachment to its session is left, as its tile, as its node,
// and as a pane or a run, which have none; so it does under a standing
// task holding the line, whose own row refuses. The add's agent before
// the host lists the worktree is the task's and refuses as well. A
// worktree line and an orphaned line toggle their own session. An
// observed agent in a window of the workspace session, a row no line
// holds, toggles the session from the session's own state, also when
// no line holds the session either.
func TestSettleGoesByLine(t *testing.T) {
	dir := t.TempDir()
	log := filepath.Join(dir, "tmux.log")
	if err := os.WriteFile(filepath.Join(dir, "tmux"), []byte("#!/bin/sh\necho \"$*\" >> "+log+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	cfg := dashConfig(t)
	d := &dash{ctx: context.Background(), cfg: cfg, st: merged.New()}
	w := protocol.Worktree{ID: "venv/worktree//w/proj/z", EnvironmentID: "venv", Repo: "proj", Source: "git@github.com:laat/proj.git", Branch: "z", Root: "/w/proj/z", Session: "proj/z"}
	agents := []protocol.Agent{
		{ID: "venv/laatmux/%1", EnvironmentID: "venv", Server: "laatmux", Session: "proj/z", Agent: "claude", Activity: protocol.Working, Liveness: protocol.Alive, Managed: true, Cwd: w.Root, WorktreeID: w.ID},
		{ID: "venv/laatmux/%2", EnvironmentID: "venv", Server: "laatmux", Session: "proj/z-2", Agent: "claude", Activity: protocol.Working, Liveness: protocol.Alive, Managed: true, Cwd: w.Root, WorktreeID: w.ID},
	}
	pane := protocol.Pane{ID: "venv/pane/%3", EnvironmentID: "venv", PaneID: "%3", Session: "proj/z-2", Command: "vim", WorktreeID: w.ID}
	run := protocol.Run{ID: "venv/run/r1", EnvironmentID: "venv", Root: w.Root, WorktreeID: w.ID, Cmd: []string{"make"}}
	task := protocol.Pending{ID: "add-z", Host: "vm", EnvironmentID: "venv", Source: w.Source, Repo: "proj", Branch: "z", Root: w.Root, Session: "proj/z", Sent: true, Taken: true, Done: true, OK: true, Prompt: protocol.DeliveryNotDelivered, SubmittedAt: time.Now()}
	host := rows.Host{Name: "vm", EnvironmentID: "venv", Connected: true, Listed: true, Worktrees: true, Attribution: true}
	show := func(in rows.Input, tree bool) *view.Model {
		m := &view.Model{Width: 100, Height: 20, ShowHidden: true}
		m.SetTree(rows.Tree(in))
		m.SetRows(rows.Agents(in, rows.Tree(in)))
		if tree {
			m.View = view.ViewTree
		}
		m.Render()
		return m
	}
	// An agent on this machine's default server in a window of the
	// workspace session vm/proj/z.
	observed := protocol.Agent{ID: "menv/default/%6", EnvironmentID: "menv", Server: "default", Session: "vm/proj/z", Agent: "claude", Activity: protocol.Working, Liveness: protocol.Alive, Cwd: "/Users/u"}
	// down: vm is down and lists nothing, so no line holds vm/proj/z.
	type setup struct{ attached, settled, workspace, task, observed, down bool }
	model := func(s setup, tree bool) *view.Model {
		in := rows.Input{Hosts: []rows.Host{host}, Agents: agents, Panes: []protocol.Pane{pane}, Runs: []protocol.Run{run}, Worktrees: []protocol.Worktree{w}}
		if s.observed {
			in.Hosts = append(in.Hosts, rows.Host{Name: "mac", Local: true, EnvironmentID: "menv", Connected: true, Listed: true, Worktrees: true, Attribution: true})
			in.Agents = append(append([]protocol.Agent{}, agents...), observed)
		}
		if s.down {
			in.Hosts[0] = rows.Host{Name: "vm", EnvironmentID: "venv"}
			in.Agents, in.Panes, in.Runs, in.Worktrees = []protocol.Agent{observed}, nil, nil, nil
		}
		if s.workspace {
			// And vm/proj/q, whose worktree is gone: an orphaned line.
			in.Locals = append(in.Locals,
				protocol.Session{Name: "vm/proj/z", Key: "venv//w/proj/z", Host: "vm", Settled: s.settled},
				protocol.Session{Name: "vm/proj/q", Key: "venv//w/proj/q", Host: "vm", Source: w.Source, Settled: s.settled})
		}
		if s.attached {
			in.Locals = append(in.Locals, protocol.Session{Name: "vm/proj/z-2", Attach: "vm/proj/z-2"})
		}
		if s.task {
			in.Pendings = []protocol.Pending{task}
		}
		return show(in, tree)
	}
	// z on the row with the id: the message and the tmux commands run.
	press := func(m *view.Model, id string) (string, string) {
		t.Helper()
		os.Remove(log)
		if !m.Select(id) {
			t.Fatalf("no row %s", id)
		}
		d.act(m, view.Action{Kind: view.ActionOther, Key: term.Key{Rune: 'z'}})
		got, _ := os.ReadFile(log)
		return m.Message, string(got)
	}
	// The tmux command and the message of z toggling the session away
	// from the state it has.
	expect := func(name string, settled bool) (string, string) {
		if settled {
			return "-u -L default set-option -u -t =" + name + ": @laatmux_settled\n", "unsettled " + name
		}
		return "-u -L default set-option -t =" + name + ": @laatmux_settled 1\n", "settled " + name
	}
	children := []struct {
		name string
		tree bool
		id   string
	}{{"tile", false, agents[1].ID}, {"node", true, agents[1].ID}, {"pane", true, pane.ID}, {"run", true, run.ID}}
	for _, s := range []setup{
		{settled: false, workspace: true}, {settled: true, workspace: true},
		{attached: true, settled: false, workspace: true}, {attached: true, settled: true, workspace: true},
		{attached: true, settled: false, workspace: true, task: true}, {attached: true, settled: true, workspace: true, task: true},
	} {
		want, msg := expect("vm/proj/z", s.settled)
		for _, c := range children {
			if got, cmds := press(model(s, c.tree), c.id); got != msg || cmds != want {
				t.Errorf("%+v: z on the %s: message %q, tmux %q", s, c.name, got, cmds)
			}
		}
		// The line itself, which a standing task takes the place of.
		if !s.task {
			if got, cmds := press(model(s, true), w.ID); got != msg || cmds != want {
				t.Errorf("%+v: z on the line: message %q, tmux %q", s, got, cmds)
			}
		}
		want, msg = expect("vm/proj/q", s.settled)
		if got, cmds := press(model(s, true), "session/vm/proj/q"); got != msg || cmds != want {
			t.Errorf("%+v: z on the orphaned line: message %q, tmux %q", s, got, cmds)
		}
	}
	// The observed agent stands in other sessions, not under the
	// worktree's line, with the workspace session as its own and that
	// session's state: as its tile and as its node it unsettles a
	// settled session and settles an unsettled one, whether the
	// worktree's line holds the session or no line does.
	for _, s := range []setup{
		{settled: false, workspace: true, observed: true}, {settled: true, workspace: true, observed: true},
		{settled: false, workspace: true, observed: true, down: true}, {settled: true, workspace: true, observed: true, down: true},
	} {
		want, msg := expect("vm/proj/z", s.settled)
		for _, tree := range []bool{false, true} {
			m := model(s, tree)
			if !m.Select(observed.ID) {
				t.Fatalf("no row %s", observed.ID)
			}
			if r := m.Selection(); r.Worktree != nil || r.Settled != s.settled || r.Local == nil || r.Local.Name != "vm/proj/z" {
				t.Fatalf("%+v tree %v: the observed agent's row is %+v", s, tree, r)
			}
			held := false
			for _, n := range m.Tree {
				held = held || n.Kind == rows.KindWorktree && n.Local != nil && n.Local.Name == "vm/proj/z"
			}
			if held == s.down {
				t.Fatalf("%+v tree %v: a line holds vm/proj/z: %v, want %v", s, tree, held, !s.down)
			}
			if got, cmds := press(m, observed.ID); got != msg || cmds != want {
				t.Errorf("%+v tree %v: z on the observed agent: message %q, tmux %q", s, tree, got, cmds)
			}
		}
	}
	// The standing task's own row, its line and its tile, refuses.
	for _, tree := range []bool{false, true} {
		if got, cmds := press(model(setup{settled: true, workspace: true, task: true}, tree), task.ID); !strings.HasPrefix(got, "proj/z: a pending task") || cmds != "" {
			t.Errorf("tree %v: z on the task: message %q, tmux %q", tree, got, cmds)
		}
	}
	// No workspace session: a child says so of its line, whose enter
	// makes one, not of its own attachment.
	if got, cmds := press(model(setup{attached: true}, false), agents[1].ID); got != "proj/z: no local workspace session; enter on the line creates one" || cmds != "" {
		t.Errorf("no workspace session, z on the tile: message %q, tmux %q", got, cmds)
	}
	if got, cmds := press(model(setup{attached: true}, true), w.ID); got != "proj/z: no local workspace session; enter creates one" || cmds != "" {
		t.Errorf("no workspace session, z on the line: message %q, tmux %q", got, cmds)
	}
	// A row of no worktree that no line holds, with no workspace session:
	// the repository line, the stale fold, an agent on this machine's
	// default server in the plain session notes, and one laatmux new made
	// in the managed session scratch, with a plain attachment or without,
	// as its tile and as its node. None is a workspace, and z says so, as
	// S does, whatever enter on it does.
	notes := protocol.Agent{ID: "menv/default/%7", EnvironmentID: "menv", Server: "default", Session: "notes", Agent: "claude", Activity: protocol.Working, Liveness: protocol.Alive, Cwd: "/Users/u"}
	scratch := protocol.Agent{ID: "venv/laatmux/%8", EnvironmentID: "venv", Server: "laatmux", Session: "scratch", Agent: "claude", Activity: protocol.Working, Liveness: protocol.Alive, Managed: true, Cwd: "/Users/u"}
	for _, attached := range []bool{false, true} {
		// The workspace session settled, so that w's agents are in the
		// stale fold.
		in := rows.Input{
			Hosts:     []rows.Host{host, {Name: "mac", Local: true, EnvironmentID: "menv", Connected: true, Listed: true, Worktrees: true, Attribution: true}},
			Agents:    append(append([]protocol.Agent{}, agents...), notes, scratch),
			Worktrees: []protocol.Worktree{w},
			Locals:    []protocol.Session{{Name: "notes"}, {Name: "vm/proj/z", Key: "venv//w/proj/z", Host: "vm", Settled: true}},
		}
		attachment := ""
		if attached {
			attachment = "vm/scratch"
			in.Locals = append(in.Locals, protocol.Session{Name: attachment, Attach: attachment})
		}
		for _, c := range []struct {
			id, name string
			tree     bool
			local    string // the row's own local session
		}{
			{rows.RepoNode(w.Source), "proj", true, ""}, {rows.NodeStale, "2 stale", false, ""},
			{notes.ID, "notes", false, "notes"}, {notes.ID, "notes", true, "notes"},
			{scratch.ID, "scratch", false, attachment}, {scratch.ID, "scratch", true, attachment},
		} {
			m := show(in, c.tree)
			if !m.Select(c.id) {
				t.Fatalf("attached %v tree %v: no row %s", attached, c.tree, c.id)
			}
			if r := m.Selection(); r.Worktree != nil || c.local == "" && r.Local != nil || c.local != "" && (r.Local == nil || r.Local.Name != c.local) {
				t.Fatalf("attached %v tree %v: the row of %s is %+v", attached, c.tree, c.id, r)
			}
			if got, cmds := press(m, c.id); got != c.name+": not a workspace" || cmds != "" {
				t.Errorf("attached %v tree %v: z on %s: message %q, tmux %q", attached, c.tree, c.id, got, cmds)
			}
		}
	}
	// The add's agent before the host lists the worktree, as its node
	// and as its tile, with the task's workspace session settled: the
	// task's, refused.
	add := protocol.Agent{ID: "venv/laatmux/%9", EnvironmentID: "venv", Server: "laatmux", Session: "proj/y", Agent: "claude", Activity: protocol.Working, Liveness: protocol.Alive, Managed: true, Cwd: "/w/proj/y"}
	loose := protocol.Pending{ID: "add-y", Host: "vm", EnvironmentID: "venv", Source: w.Source, Repo: "proj", Branch: "y", Root: "/w/proj/y", Session: "proj/y", Sent: true, Taken: true, Done: true, OK: true, Prompt: protocol.DeliveryDelivered, SubmittedAt: time.Now()}
	in := rows.Input{
		Hosts:    []rows.Host{host},
		Agents:   []protocol.Agent{add},
		Locals:   []protocol.Session{{Name: "vm/proj/y", Key: "venv//w/proj/y", Host: "vm", Settled: true}},
		Pendings: []protocol.Pending{loose},
	}
	for _, tree := range []bool{false, true} {
		if got, cmds := press(show(in, tree), add.ID); !strings.HasPrefix(got, "proj/y: a pending task") || cmds != "" {
			t.Errorf("tree %v: z on the add's agent: message %q, tmux %q", tree, got, cmds)
		}
	}
}

// z on a homeless worktree's line with no local workspace session, or
// on its agent, says what enter on the line does about one, as enter
// then does it: with the agent laatmux made at the root, enter makes
// the session; with the agent on this machine's default server, in a
// plain session or in a window of another worktree's workspace session,
// enter switches there, and the add line, or for a detached worktree
// the branch add needs, follows; with the agent on another host's
// default server, enter refuses, and the add line follows, or for a
// host whose entry here has no directories, that add needs them; with
// no agent, enter refuses with the add line. A done task standing for
// the worktree goes by the task's target, as enter does; under a task
// still running, enter on the line waits for it.
func TestSettleHintGoesByEnter(t *testing.T) {
	dir := t.TempDir()
	log := filepath.Join(dir, "tmux.log")
	// A tmux that answers as the default server whichever server is
	// asked: the view is inside it, and enter switches the client.
	script := "#!/bin/sh\necho \"$*\" >> " + log + "\ncase \"$*\" in *display-message*) echo /tmp/lmx-fake/default ;; esac\n"
	if err := os.WriteFile(filepath.Join(dir, "tmux"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	t.Setenv("TMUX", "/tmp/lmx-fake/default,1,0")
	d := &dash{ctx: context.Background(), cfg: dashConfig(t), st: merged.New()}
	mac := rows.Host{Name: "mac", Local: true, EnvironmentID: "menv", Connected: true, Listed: true, Worktrees: true, Attribution: true}
	vm := rows.Host{Name: "vm", EnvironmentID: "venv", Connected: true, Listed: true, Worktrees: true, Attribution: true}
	a := protocol.Worktree{ID: "menv/worktree//w/a", EnvironmentID: "menv", Repo: "proj", Source: "git@github.com:laat/proj.git", Branch: "a", Root: "/w/a", Session: "proj/a"}
	b := protocol.Worktree{ID: "menv/worktree//w/b", EnvironmentID: "menv", Repo: "proj", Source: "git@github.com:laat/proj.git", Branch: "b", Root: "/w/b"}
	// B on vm, B on box, whose entry here has no directories for add,
	// and a detached worktree on mac.
	box := rows.Host{Name: "box", EnvironmentID: "benv", Connected: true, Listed: true, Worktrees: true, Attribution: true}
	bv, bb, det := b, b, b
	bv.ID, bv.EnvironmentID = "venv/worktree//w/b", "venv"
	bb.ID, bb.EnvironmentID = "benv/worktree//w/b", "benv"
	det.ID, det.Branch, det.Root = "menv/worktree//w/det", "", "/w/det"
	agent := func(w protocol.Worktree, id, server, session string) protocol.Agent {
		return protocol.Agent{ID: id, EnvironmentID: w.EnvironmentID, Server: server, Session: session, Agent: "claude", Activity: protocol.Working, Liveness: protocol.Alive, Managed: server == "laatmux", Cwd: w.Root, WorktreeID: w.ID}
	}
	locals := []protocol.Session{{Name: "mac/proj/a", Key: "menv//w/a", Host: "mac"}, {Name: "notes"}}
	show := func(in rows.Input, tree bool) *view.Model {
		m := &view.Model{Width: 100, Height: 20, ShowHidden: true}
		m.SetTree(rows.Tree(in))
		m.SetRows(rows.Agents(in, rows.Tree(in)))
		if tree {
			m.View = view.ViewTree
		}
		m.Render()
		return m
	}
	// z, or enter, on the row with the id: the message and the tmux
	// commands run.
	press := func(m *view.Model, id string, enter bool) (string, string) {
		t.Helper()
		os.Remove(log)
		if !m.Select(id) {
			t.Fatalf("no row %s", id)
		}
		if enter {
			d.jumpAction(m, view.Action{Kind: view.ActionJump})
		} else {
			d.act(m, view.Action{Kind: view.ActionOther, Key: term.Key{Rune: 'z'}})
		}
		got, _ := os.ReadFile(log)
		return m.Message, string(got)
	}
	inNotes := agent(b, "menv/default/%1", "default", "notes")
	onVM := agent(bv, "venv/default/%5", "default", "notes")
	onBox := agent(bb, "benv/default/%8", "default", "notes")
	detNotes := agent(det, "menv/default/%6", "default", "notes")
	// A task for B before the add has a session to report, and the same
	// done, its session reported, standing for B while its prompt waits.
	running := protocol.Pending{ID: "add-b", Host: "mac", EnvironmentID: "menv", Source: b.Source, Repo: "proj", Branch: "b", Root: b.Root, Sent: true, Taken: true, Stage: protocol.StageSetup, SubmittedAt: time.Now()}
	done := running
	done.Done, done.OK, done.Session, done.Prompt = true, true, "proj/b", protocol.DeliveryNotDelivered
	add := "laatmux add b --repo proj --host mac makes one"
	for _, c := range []struct {
		name  string
		in    rows.Input
		line  string // the line's id, where enter is pressed
		agent string // the line's agent's id, "" for none
		enter string // in what enter on the line runs in tmux; "" when it runs nothing
		// After "<line>: no local workspace session; ", with ENTER for
		// "enter" or "enter on the line" and REFUSED for what enter on
		// the line said.
		hint string
	}{
		{"no agent", rows.Input{Hosts: []rows.Host{mac}, Worktrees: []protocol.Worktree{a, b}, Locals: locals},
			b.ID, "", "", "REFUSED"},
		{"agent in notes", rows.Input{Hosts: []rows.Host{mac}, Worktrees: []protocol.Worktree{a, b}, Agents: []protocol.Agent{inNotes}, Locals: locals},
			b.ID, inNotes.ID, "switch-client -t =notes:", "ENTER jumps to notes, its agent's session; " + add},
		{"agent in A's workspace session", rows.Input{Hosts: []rows.Host{mac}, Worktrees: []protocol.Worktree{a, b}, Agents: []protocol.Agent{agent(b, "menv/default/%2", "default", "mac/proj/a")}, Locals: locals},
			b.ID, "menv/default/%2", "switch-client -t =mac/proj/a:", "ENTER jumps to mac/proj/a, its agent's session; " + add},
		{"root agent laatmux made", rows.Input{Hosts: []rows.Host{mac}, Worktrees: []protocol.Worktree{a, b}, Agents: []protocol.Agent{agent(b, "menv/laatmux/%3", "laatmux", "proj/b")}, Locals: locals},
			b.ID, "menv/laatmux/%3", "new-session -d -s mac/proj/b ", "ENTER creates one"},
		{"agent on vm's default server", rows.Input{Hosts: []rows.Host{vm}, Worktrees: []protocol.Worktree{bv}, Agents: []protocol.Agent{onVM}},
			bv.ID, onVM.ID, "", "REFUSED; laatmux add b --repo proj --host vm makes one"},
		{"agent on box's default server", rows.Input{Hosts: []rows.Host{box}, Worktrees: []protocol.Worktree{bb}, Agents: []protocol.Agent{onBox}},
			bb.ID, onBox.ID, "", "REFUSED; laatmux add makes one once host box has repos and worktrees directories in the config"},
		{"detached, agent in notes", rows.Input{Hosts: []rows.Host{mac}, Worktrees: []protocol.Worktree{a, det}, Agents: []protocol.Agent{detNotes}, Locals: locals},
			det.ID, detNotes.ID, "switch-client -t =notes:", "ENTER jumps to notes, its agent's session; laatmux add makes one once a branch is checked out in /w/det"},
		{"done task", rows.Input{Hosts: []rows.Host{mac}, Worktrees: []protocol.Worktree{a, b}, Agents: []protocol.Agent{inNotes}, Pendings: []protocol.Pending{done}, Locals: locals},
			done.ID, inNotes.ID, "new-session -d -s mac/proj/b ", "ENTER creates one"},
		{"running task", rows.Input{Hosts: []rows.Host{mac}, Worktrees: []protocol.Worktree{a, b}, Agents: []protocol.Agent{inNotes}, Pendings: []protocol.Pending{running}, Locals: locals},
			running.ID, inNotes.ID, "", "ENTER creates one once the task is done"},
	} {
		m := show(c.in, true)
		if !m.Select(c.line) {
			t.Fatalf("%s: no line %s", c.name, c.line)
		}
		prefix := m.Selection().Name + ": no local workspace session; "
		said, cmds := press(m, c.line, true)
		if c.enter == "" && cmds != "" || !strings.Contains(cmds, c.enter) {
			t.Errorf("%s: enter on the line: message %q, tmux %q, want %q run", c.name, said, cmds, c.enter)
		}
		hint := func(enter string) string {
			return prefix + strings.NewReplacer("ENTER", enter, "REFUSED", said).Replace(c.hint)
		}
		// A task's own line is the task's, which z refuses.
		if c.in.Pendings == nil {
			if got, cmds := press(show(c.in, true), c.line, false); got != hint("enter") || cmds != "" {
				t.Errorf("%s: z on the line: message %q, tmux %q, want %q", c.name, got, cmds, hint("enter"))
			}
		}
		if c.agent == "" {
			continue
		}
		// The agent, as its node and as its tile, says it of its line.
		for _, tree := range []bool{false, true} {
			if got, cmds := press(show(c.in, tree), c.agent, false); got != hint("enter on the line") || cmds != "" {
				t.Errorf("%s tree %v: z on the agent: message %q, tmux %q, want %q", c.name, tree, got, cmds, hint("enter on the line"))
			}
		}
	}
	// A line no configured host claims, and one on a host this machine's
	// config lacks: enter's refusal, and no add line for a host the line
	// is not on or that add does not know.
	unclaimed := rows.Row{Kind: rows.KindWorktree, Name: "proj/b", Worktree: &b, Agent: &inNotes}
	if got := noWorkspaceHint(d.cfg, unclaimed, false); got != "proj/b: no configured host claims this record" {
		t.Errorf("a line no host claims: %q", got)
	}
	unclaimed.Host = "ghost"
	if got := noWorkspaceHint(d.cfg, unclaimed, false); got != `unknown host "ghost"` {
		t.Errorf("a line on a host not configured: %q", got)
	}
	// An agent of no worktree, in notes, has no line to go by. What z
	// says of it is not enter's (#192), but z runs nothing.
	loose := protocol.Agent{ID: "menv/default/%7", EnvironmentID: "menv", Server: "default", Session: "notes", Agent: "claude", Activity: protocol.Working, Liveness: protocol.Alive, Cwd: "/Users/u"}
	in := rows.Input{Hosts: []rows.Host{mac}, Agents: []protocol.Agent{loose}, Locals: locals}
	for _, tree := range []bool{false, true} {
		if got, cmds := press(show(in, tree), loose.ID, false); !strings.HasPrefix(got, "notes: no local workspace session; ") || cmds != "" {
			t.Errorf("tree %v: z on an agent of no worktree: message %q, tmux %q", tree, got, cmds)
		}
	}
}

// An rm whose host side succeeded and whose local cleanup then failed
// returns the root with the error, so the CLI prints the removal before
// the error and the dashboard says what was removed.
func TestRmPartialSuccess(t *testing.T) {
	startFakeDaemon(t, []string{protocol.CapStatus, protocol.CapRm, protocol.CapFollow}, func(pc *protocol.Conn, m protocol.Message) bool {
		if m.Type == protocol.TypeRm {
			pc.Write(protocol.Message{Type: protocol.TypeResult, ID: m.ID, OK: true, Root: "/w/proj/task"})
		}
		return true
	})
	// A tmux that fails: the local session cannot be listed.
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "tmux"), []byte("#!/bin/sh\necho 'tmux: boom' >&2\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	rm := command.Rm{Host: config.Host{Host: peer.Host{Name: "lab"}}, Root: "/w/proj/task"}
	res, err := rm.Run(context.Background(), command.Discard{})
	if err == nil || res.Root != "/w/proj/task" || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("res=%+v err=%v", res, err)
	}
}

// An add whose host side succeeded and whose local session then
// failed returns the root and managed session with the error, so the
// CLI prints the ready line before the error and the dashboard says
// what exists.
func TestAddPartialSuccess(t *testing.T) {
	startFakeDaemon(t, []string{protocol.CapStatus, protocol.CapAdd, protocol.CapFollow}, func(pc *protocol.Conn, m protocol.Message) bool {
		if m.Type == protocol.TypeAdd {
			pc.Write(protocol.Message{Type: protocol.TypeResult, ID: m.ID, OK: true, Root: "/w/proj/x", Session: "proj/x"})
		}
		return true
	})
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "tmux"), []byte("#!/bin/sh\necho 'tmux: boom' >&2\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	add := command.Add{Host: config.Host{Host: peer.Host{Name: "lab"}}, Repo: config.Repo{Source: "git@x:o/proj.git", Name: "proj"}, Branch: "x", Agent: "claude"}
	res, err := add.Run(context.Background(), command.Discard{})
	if err == nil || res.Root != "/w/proj/x" || res.Managed != "proj/x" || res.Session != "" || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	last, err := home.ReadLast()
	if err != nil || last.Get("git@x:o/proj.git").Host != "lab" {
		t.Errorf("last.json not written before the local failure: %+v %v", last, err)
	}
}

// x asks about the selected worktree, naming it and its root, with the
// request built from the record: by source and branch when this
// machine knows the repository, by root alone when it does not, and
// from an orphaned session's tags and key; X asks with force. The
// worktree with an agent is asked about from the agent's tile, with
// the agent the tree joins to it named; the one with no agent and the
// orphaned session from their lines in the tree.
func TestRmFor(t *testing.T) {
	cfg := dashConfig(t)
	d := &dash{ctx: context.Background(), cfg: cfg, st: merged.New()}
	m := dashModel(cfg)
	selectRow(t, m, "proj/task")
	d.act(m, view.Action{Kind: view.ActionOther, Key: term.Key{Rune: 'x'}})
	want := command.Rm{Host: config.Host{Host: peer.Host{Name: "vm", SSH: "vm"}, Repos: "/r", Worktrees: "/w"},
		Repo: cfg.Repos[1], Branch: "task", Root: "/w/proj/task"}
	if d.rm.Host.Name != want.Host.Name || d.rm.Repo.Source != want.Repo.Source || d.rm.Branch != want.Branch || d.rm.Root != want.Root || d.rm.Force {
		t.Errorf("rm = %+v", d.rm)
	}
	if m.Confirm != "remove proj/task on vm (/w/proj/task) with its agent? y/n" || m.ConfirmTag != "rm" {
		t.Errorf("confirm = %q tag %q", m.Confirm, m.ConfirmTag)
	}
	m.Handle(term.Key{Rune: 'n'})

	treeView(m)
	selectRow(t, m, "x/y")
	d.act(m, view.Action{Kind: view.ActionOther, Key: term.Key{Rune: 'X'}})
	if d.rm.Repo.Source != "" || d.rm.Branch != "" || d.rm.Root != "/w/x/y" || !d.rm.Force || d.rm.Host.Name != "mac" {
		t.Errorf("unknown repository: rm = %+v", d.rm)
	}
	if m.Confirm != "force-remove /w/x/y on mac (/w/x/y)? y/n" {
		t.Errorf("confirm = %q", m.Confirm)
	}
	m.Handle(term.Key{Rune: 'n'})

	selectRow(t, m, "vm/proj/gone")
	d.act(m, view.Action{Kind: view.ActionOther, Key: term.Key{Rune: 'x'}})
	if d.rm.Repo.Source != cfg.Repos[1].Source || d.rm.Branch != "gone" || d.rm.Root != "/w/proj/gone" || d.rm.Host.Name != "vm" {
		t.Errorf("orphaned: rm = %+v", d.rm)
	}
	m.Handle(term.Key{Rune: 'n'})

	// An agent with no worktree is not rm's.
	scratch := rows.Input{
		Hosts:  []rows.Host{{Name: "mac", Local: true, EnvironmentID: "menv", Connected: true, Listed: true, Worktrees: true}},
		Agents: []protocol.Agent{{ID: "menv/laatmux/%9", EnvironmentID: "menv", Server: "laatmux", Session: "scratch", Activity: protocol.Idle, Liveness: protocol.Alive, Managed: true}},
	}
	m.View = view.ViewAgents
	m.SetTree(rows.Tree(scratch))
	m.SetRows(rows.Agents(scratch, rows.Tree(scratch)))
	m.Render()
	selectRow(t, m, "scratch")
	d.act(m, view.Action{Kind: view.ActionOther, Key: term.Key{Rune: 'x'}})
	if m.Confirm != "" || !strings.Contains(m.Message, "not a worktree") {
		t.Errorf("agent row: confirm=%q message=%q", m.Confirm, m.Message)
	}
}

// In the tree, x on a repository line, the stale fold, a pane or a run
// says what x removes; from a worktree line or an agent under it the
// question counts the agents the tree joins to the worktree, the jump
// agent or not.
func TestRmTreeRows(t *testing.T) {
	cfg := dashConfig(t)
	d := &dash{ctx: context.Background(), cfg: cfg, st: merged.New()}
	src := "git@github.com:laat/proj.git"
	in := rows.Input{
		Hosts: []rows.Host{{Name: "vm", EnvironmentID: "venv", Connected: true, Listed: true, Worktrees: true, Attribution: true}},
		Agents: []protocol.Agent{
			{ID: "venv/laatmux/%1", EnvironmentID: "venv", Server: "laatmux", Session: "proj/task", Agent: "claude", Activity: protocol.Working, Liveness: protocol.Alive, Managed: true, WorktreeID: "venv/worktree//w/proj/task", Identity: &protocol.Identity{PID: 1, StartUnix: 1}},
			{ID: "venv/laatmux/%2", EnvironmentID: "venv", Server: "laatmux", Session: "other", Agent: "codex", Activity: protocol.Idle, Liveness: protocol.Alive, Managed: true, WorktreeID: "venv/worktree//w/proj/task", Identity: &protocol.Identity{PID: 2, StartUnix: 2}},
			{ID: "venv/laatmux/%3", EnvironmentID: "venv", Server: "laatmux", Session: "elsewhere", Agent: "claude", Activity: protocol.Idle, Liveness: protocol.Alive, Managed: true, WorktreeID: "venv/worktree//w/proj/spike", Identity: &protocol.Identity{PID: 3, StartUnix: 3}},
		},
		Worktrees: []protocol.Worktree{
			{ID: "venv/worktree//w/proj/task", EnvironmentID: "venv", Repo: "proj", Source: src, Branch: "task", Root: "/w/proj/task", Session: "proj/task"},
			{ID: "venv/worktree//w/proj/spike", EnvironmentID: "venv", Repo: "proj", Source: src, Branch: "spike", Root: "/w/proj/spike", Session: "proj/spike"},
		},
		Panes: []protocol.Pane{{ID: "venv/pane/laatmux/%7", EnvironmentID: "venv", Session: "proj/task", PaneID: "%7", Command: "zsh", WorktreeID: "venv/worktree//w/proj/task"}},
		Runs:  []protocol.Run{{ID: "venv/run/r1", EnvironmentID: "venv", Root: "/w/proj/task", WorktreeID: "venv/worktree//w/proj/task", Cmd: []string{"make"}}},
	}
	m := &view.Model{Width: 80, Height: 30, View: view.ViewTree}
	m.SetTree(rows.Tree(in))
	m.SetRows(rows.Agents(in, rows.Tree(in)))
	m.Render()
	m.Handle(term.Key{Rune: 'f'}) // every fold open
	x := func(id string) {
		t.Helper()
		m.Message, m.Confirm = "", ""
		if !m.Select(id) {
			t.Fatalf("%s is not visible", id)
		}
		d.act(m, view.Action{Kind: view.ActionOther, Key: term.Key{Rune: 'x'}})
	}
	for _, id := range []string{rows.RepoNode(src), "venv/pane/laatmux/%7", "venv/run/r1"} {
		x(id)
		if m.Confirm != "" || !strings.Contains(m.Message, "x removes worktrees") {
			t.Errorf("%s: confirm=%q message=%q", id, m.Confirm, m.Message)
		}
	}
	// The worktree line, whose one child agent in another session is
	// not its jump agent, and the agent's own row.
	for _, id := range []string{"venv/worktree//w/proj/spike", "venv/laatmux/%3"} {
		x(id)
		if m.Confirm != "remove proj/spike on vm (/w/proj/spike) with its agent? y/n" {
			t.Errorf("%s: confirm=%q message=%q", id, m.Confirm, m.Message)
		}
		m.Handle(term.Key{Rune: 'n'})
	}
	x("venv/laatmux/%2")
	if m.Confirm != "remove proj/task on vm (/w/proj/task) with its 2 agents? y/n" {
		t.Errorf("two agents: confirm=%q message=%q", m.Confirm, m.Message)
	}
	m.Handle(term.Key{Rune: 'n'})
	// The agent view's stale fold.
	m.View = view.ViewAgents
	in.Agents[2].ActivityAt = time.Time{}
	in.Now, in.StaleAfter, in.CollapseStale = time.Now(), time.Hour, true
	m.SetTree(rows.Tree(in))
	m.SetRows(rows.Agents(in, rows.Tree(in)))
	m.Render()
	x(rows.NodeStale)
	if m.Confirm != "" || !strings.Contains(m.Message, "x removes worktrees") {
		t.Errorf("the stale fold: confirm=%q message=%q", m.Confirm, m.Message)
	}
}

// A confirmed rm runs under a log overlay; a refusal without force
// carries the hint to use X and stays until a key; then the list
// returns with the message.
func TestRmRefusalHint(t *testing.T) {
	d := &dash{ctx: context.Background(), cfg: dashConfig(t), st: merged.New()}
	m := dashModel(d.cfg)
	d.rm = command.Rm{Host: d.cfg.Hosts[1], Root: "/w/proj/task"}
	// Start with a run that fails the way git refuses a dirty worktree.
	d.start(m, "rm", func(command.Reporter) error {
		return forceHint(&command.StageError{Command: "rm", Msg: "git worktree remove /w/proj/task: fatal: '/w/proj/task' contains modified or untracked files, use --force to delete it"}, false)
	}, func(*view.Model) bool { return false })
	log := m.Overlay.(*view.Log)
	for i := 0; !log.Ended() && i < 500; i++ {
		time.Sleep(10 * time.Millisecond)
	}
	if d.act(m, m.Poll()) || m.Overlay != log {
		t.Fatal("a failed log did not stay on screen")
	}
	log.Handle(term.Key{Rune: ' '})
	if d.act(m, m.Poll()) || m.Overlay != nil {
		t.Fatal("a key did not return to the list")
	}
	if !strings.Contains(m.Message, "use --force") || !strings.HasSuffix(m.Message, "(X force-removes)") {
		t.Errorf("message = %q", m.Message)
	}
	if err := forceHint(errors.New("unknown repository"), false); strings.Contains(err.Error(), "X force") {
		t.Errorf("hint on a refusal that is not about force: %v", err)
	}
	if err := forceHint(errors.New("use --force"), true); strings.Contains(err.Error(), "X force") {
		t.Errorf("hint on a forced rm: %v", err)
	}
}

// The task form is built over the config's candidates with the
// defaults preselected: the repository named, the host and agent last
// used for it, and a note when the host's cached daemon capabilities
// lack tasks; a branch given is the user's.
func TestBuildForm(t *testing.T) {
	cfg := config.Config{
		Hosts:  []config.Host{{Host: peer.Host{Name: "mac"}, Repos: "/r", Worktrees: "/w"}, {Host: peer.Host{Name: "vm", SSH: "vm"}, Repos: "/r", Worktrees: "/w"}},
		Repos:  []config.Repo{{Source: "git@x:o/proj.git", Name: "proj"}, {Source: "git@x:o/other.git", Name: "other"}},
		Agents: map[string]config.Agent{"claude": {Cmd: []string{"claude"}}, "codex": {Cmd: []string{"codex"}}},
	}
	f := &addForm{repos: cfg.Repos, hosts: cfg.Hosts, agents: cfg.AgentNames()}
	var last home.Last
	last.Set("git@x:o/other.git", home.LastRepo{Host: "vm", Agent: "codex"})
	caps := func(host string) ([]string, bool) {
		if host == "vm" {
			return []string{protocol.CapAdd}, true
		}
		return nil, false
	}
	form := buildForm(cfg, f, last, "other", "", "", caps)
	if form.Chips[0].Label() != "other" || form.Chips[1].Label() != "vm" || form.Chips[2].Label() != "codex" {
		t.Fatalf("chips %q %q %q", form.Chips[0].Label(), form.Chips[1].Label(), form.Chips[2].Label())
	}
	if !form.Generated() || form.Branch() != "" {
		t.Fatalf("branch %q generated %v", form.Branch(), form.Generated())
	}
	if n := form.Note(form); !strings.Contains(n, "tasks not supported by vm") {
		t.Fatalf("note %q", n)
	}
	form.Chips[1].Selected = 0
	if n := form.Note(form); n != "" {
		t.Fatalf("note for an unknown host %q", n)
	}
	form.SetPrompt("Fix the thing")
	if form.Branch() != "fix-the-thing" {
		t.Fatalf("proposal %q", form.Branch())
	}
	// A repository chosen later brings its own host and agent; a host
	// the user set stays.
	form = buildForm(cfg, f, last, "proj", "", "", caps)
	if form.Chips[1].Label() != "mac" || form.Chips[2].Label() != "claude" {
		t.Fatalf("proj defaults %q %q", form.Chips[1].Label(), form.Chips[2].Label())
	}
	form.Handle(term.Key{Kind: term.KeyShiftTab})
	form.Handle(term.Key{Kind: term.KeyShiftTab})
	form.Handle(term.Key{Kind: term.KeyShiftTab}) // the repository chip
	form.Handle(term.Key{Kind: term.KeyRight})
	if form.Chips[0].Label() != "other" || form.Chips[1].Label() != "vm" || form.Chips[2].Label() != "codex" {
		t.Fatalf("after choosing other: %q %q %q", form.Chips[0].Label(), form.Chips[1].Label(), form.Chips[2].Label())
	}
	form.Handle(term.Key{Kind: term.KeyTab}) // the host chip
	form.Handle(term.Key{Kind: term.KeyLeft})
	form.Handle(term.Key{Kind: term.KeyShiftTab})
	form.Handle(term.Key{Kind: term.KeyLeft}) // back to proj
	if form.Chips[0].Label() != "proj" || form.Chips[1].Label() != "mac" || form.Chips[2].Label() != "claude" {
		t.Fatalf("after the user's host: %q %q %q", form.Chips[0].Label(), form.Chips[1].Label(), form.Chips[2].Label())
	}
	// Picking the value already shown is the user's choice too.
	form = buildForm(cfg, f, last, "proj", "", "", caps)
	form.Handle(term.Key{Kind: term.KeyShiftTab}) // the agent chip
	form.Handle(term.Key{Kind: term.KeyEnter})    // the picker on claude
	form.Handle(term.Key{Kind: term.KeyEnter})    // accept claude
	form.Handle(term.Key{Kind: term.KeyShiftTab})
	form.Handle(term.Key{Kind: term.KeyShiftTab}) // the repository chip
	form.Handle(term.Key{Kind: term.KeyRight})    // other: last agent codex
	if form.Chips[0].Label() != "other" || form.Chips[2].Label() != "claude" || form.Chips[1].Label() != "vm" {
		t.Fatalf("picked agent kept: %q %q %q", form.Chips[0].Label(), form.Chips[1].Label(), form.Chips[2].Label())
	}
	// A worktree row without a session: repository, host and branch
	// from the record, the branch explicit.
	form = buildForm(cfg, f, last, "proj", "mac", "existing", caps)
	if form.Chips[0].Label() != "proj" || form.Chips[1].Label() != "mac" || form.Branch() != "existing" || form.Generated() {
		t.Fatalf("prefilled %q %q %q %v", form.Chips[0].Label(), form.Chips[1].Label(), form.Branch(), form.Generated())
	}
	// The record's host is as good as the user's: picking the
	// repository again does not replace it.
	form.Handle(term.Key{Kind: term.KeyShiftTab})
	form.Handle(term.Key{Kind: term.KeyShiftTab})
	form.Handle(term.Key{Kind: term.KeyShiftTab})
	form.Handle(term.Key{Kind: term.KeyRight}) // other, whose last host is vm
	form.Handle(term.Key{Kind: term.KeyRight}) // back to proj
	if form.Chips[1].Label() != "mac" {
		t.Fatalf("the record's host replaced: %q", form.Chips[1].Label())
	}
}

// A submit through the relay: a refusal puts the form back up with the
// error and the text intact; an error after the daemon may hold the
// task drops the form and keeps the view with the id in the message;
// acceptance ends it with the id.
func TestSubmitFormOutcomes(t *testing.T) {
	t.Setenv("LAATMUX_HOME", t.TempDir())
	cfg := dashConfig(t)
	d := &dash{ctx: context.Background(), cfg: cfg, st: merged.New()}
	m := dashModel(cfg)
	d.act(m, view.Action{Kind: view.ActionOther, Key: term.Key{Rune: 'a'}})
	f := m.Overlay.(*view.Form)
	f.SetPrompt("Fix it")
	var got command.Add
	d.submit = func(a command.Add) (string, error) {
		got = a
		return "", errors.New("tasks not supported by vm's daemon")
	}
	f.Handle(term.Key{Kind: term.KeyEnter})
	if d.act(m, m.Poll()) {
		t.Fatal("a refusal ended the view")
	}
	back, ok := m.Overlay.(*view.Form)
	if !ok || back != f || back.Error != "tasks not supported by vm's daemon" || back.Prompt() != "Fix it" || back.Done() {
		t.Fatalf("form after a refusal: %+v", m.Overlay)
	}
	if got.Prompt != "Fix it" || got.Branch != "fix-it" || !got.Generated {
		t.Fatalf("submitted %+v", got)
	}
	// An answer the daemon may have taken: the form goes, the view
	// stays with the message.
	d.submit = func(a command.Add) (string, error) { return "add-1", errors.New("the answer was lost") }
	f.Handle(term.Key{Kind: term.KeyEnter})
	if d.act(m, m.Poll()) || !strings.Contains(m.Message, "submitted add-1") || m.Overlay != nil || d.add != nil {
		t.Fatalf("uncertain submit: message %q overlay %v add %v", m.Message, m.Overlay, d.add)
	}
	d.act(m, view.Action{Kind: view.ActionOther, Key: term.Key{Rune: 'a'}})
	f = m.Overlay.(*view.Form)
	f.SetPrompt("Fix it")
	d.submit = func(a command.Add) (string, error) { return "add-2", nil }
	f.Handle(term.Key{Kind: term.KeyEnter})
	if !d.act(m, m.Poll()) || m.Message != "accepted add-2" || d.add != nil {
		t.Fatalf("accepted: message %q add %v", m.Message, d.add)
	}
}

// compose's host: a refusal puts the form back, an answer the daemon
// may have taken waits in an ended log and then ends the view, Esc on
// the form and Ctrl-C on a log end it.
func TestComposeAct(t *testing.T) {
	t.Setenv("LAATMUX_HOME", t.TempDir())
	cfg := dashConfig(t)
	f := &addForm{repos: cfg.Repos, hosts: cfg.Hosts, agents: cfg.AgentNames()}
	d := &dash{ctx: context.Background(), cfg: cfg, st: merged.New(), add: f}
	c := &composer{d: d, f: f}
	var last home.Last
	form := buildForm(cfg, f, last, "proj", "", "", nil)
	form.SetPrompt("Fix it")
	m := &view.Model{Overlay: form, Width: 80, Height: 24}
	d.submit = func(command.Add) (string, error) { return "", errors.New("tasks not supported by vm's daemon") }
	form.Handle(term.Key{Kind: term.KeyEnter})
	if c.act(m, m.Poll()) || m.Overlay != form || form.Error == "" {
		t.Fatalf("refusal: overlay %v error %q", m.Overlay, form.Error)
	}
	d.submit = func(command.Add) (string, error) { return "add-1", errors.New("the answer was lost") }
	form.Handle(term.Key{Kind: term.KeyEnter})
	if c.act(m, m.Poll()) {
		t.Fatal("an uncertain answer ended the view at once")
	}
	log, ok := m.Overlay.(*view.Log)
	if !ok || log.Done() {
		t.Fatalf("no ended log waiting: %v", m.Overlay)
	}
	log.Handle(term.Key{Kind: term.KeyPaste, Text: "stray"})
	if log.Done() {
		t.Fatal("a paste dismissed the log")
	}
	log.Handle(term.Key{Rune: 'x'})
	if !c.act(m, m.Poll()) || !strings.Contains(c.outcome, "submitted add-1") {
		t.Fatalf("after the key: outcome %q", c.outcome)
	}
	fresh := buildForm(cfg, f, last, "proj", "", "", nil)
	fresh.Handle(term.Key{Kind: term.KeyEsc})
	m = &view.Model{Overlay: fresh}
	if !c.act(m, m.Poll()) {
		t.Fatal("esc on the form did not end the view")
	}
	// Ctrl-C on a log ends the view.
	log = view.NewLog("t")
	d.run = &running{done: func(*view.Model) bool { return true }}
	m = &view.Model{Overlay: log, Width: 80, Height: 24}
	m.Handle(term.Key{Kind: term.KeyCtrlC})
	if !c.act(m, m.Poll()) {
		t.Fatal("Ctrl-C on the log did not end compose")
	}
}

// A pending task's keys: x asks and dismisses one that needs the user
// and says why not on one still running; p delivers a retained prompt
// and says why not otherwise; Enter on a running one does nothing.
func TestPendingKeys(t *testing.T) {
	t.Setenv("LAATMUX_HOME", t.TempDir())
	cfg := dashConfig(t)
	now := time.Now()
	stuck := protocol.Pending{ID: "add-1", Host: "vm", EnvironmentID: "venv", Repo: "proj", Branch: "fix", Root: "/w/proj/fix", Session: "proj/fix",
		Taken: true, Reachable: true, Done: true, OK: true, Prompt: protocol.DeliveryNotDelivered, Error: "not ready", SubmittedAt: now}
	running := protocol.Pending{ID: "add-2", Host: "vm", EnvironmentID: "venv", Repo: "proj", Branch: "new", Taken: true, Reachable: true, Stage: protocol.StageFetch, SubmittedAt: now.Add(time.Minute)}
	m := &view.Model{Width: 80, Height: 20}
	in := rows.Input{
		Hosts:    []rows.Host{{Name: "vm", EnvironmentID: "venv", Connected: true, Listed: true, Worktrees: true}},
		Pendings: []protocol.Pending{stuck, running},
	}
	m.SetRows(rows.Agents(in, rows.Tree(in)))
	var dismissed, delivered string
	d := &dash{ctx: context.Background(), cfg: cfg, st: merged.New(),
		dismiss: func(id string) error { dismissed = id; return nil },
		deliver: func(id string) (string, string, error) { delivered = id; return protocol.DeliveryDelivered, "", nil }}
	finish := func() {
		t.Helper()
		log, ok := m.Overlay.(*view.Log)
		if !ok {
			t.Fatalf("no log: %v", m.Overlay)
		}
		for i := 0; i < 200 && !log.Done(); i++ {
			time.Sleep(5 * time.Millisecond)
		}
		d.act(m, m.Poll())
	}
	key := func(r rune) view.Action { return m.Handle(term.Key{Rune: r}) }

	// The running one, newest, is first.
	m.Handle(term.Key{Rune: 'g'})
	if r := m.Selection(); r == nil || r.ID() != "add-2" {
		t.Fatalf("selected %+v", r)
	}
	a := m.Handle(term.Key{Kind: term.KeyEnter})
	if exit, jumped := d.jumpRow(m, *m.Selection()); a.Kind != view.ActionJump || exit || jumped || m.Message != "" {
		t.Fatalf("enter on a running task: %+v message %q", a, m.Message)
	}
	d.act(m, key('x'))
	if m.Confirm != "" || !strings.Contains(m.Message, "still running") {
		t.Fatalf("x on a running task: confirm %q message %q", m.Confirm, m.Message)
	}
	d.act(m, key('p'))
	if !strings.Contains(m.Message, "nothing to deliver") || delivered != "" {
		t.Fatalf("p on a running task: %q", m.Message)
	}

	// The stuck one: p delivers, x asks then dismisses.
	m.Handle(term.Key{Rune: 'j'})
	d.act(m, key('p'))
	finish()
	if delivered != "add-1" || m.Message != "prompt delivered" {
		t.Fatalf("p: delivered %q message %q", delivered, m.Message)
	}
	d.act(m, key('x'))
	if !strings.Contains(m.Confirm, "dismiss proj/fix on vm (prompt not delivered)") {
		t.Fatalf("x: confirm %q", m.Confirm)
	}
	d.act(m, key('y'))
	finish()
	if dismissed != "add-1" || !strings.Contains(m.Message, "dismissed proj/fix on vm") {
		t.Fatalf("dismissed %q message %q", dismissed, m.Message)
	}
}

// The jump on a task's row: refused on a host removed or replaced,
// by the reported session when the worktree row is not listed or has
// no session yet, and by the worktree row when it has one.
func TestPendingTarget(t *testing.T) {
	p := protocol.Pending{ID: "add-1", Host: "vm", EnvironmentID: "venv", Repo: "proj", Branch: "fix", Root: "/w/proj/fix", Session: "proj/fix", Done: true, OK: true}
	row := func(p protocol.Pending, w *protocol.Worktree, removed bool) rows.Row {
		return rows.Row{Host: "vm", Name: "proj/fix", Pending: &p, Worktree: w, Removed: removed}
	}
	if _, err := pendingTarget(row(p, nil, true)); err == nil || !strings.Contains(err.Error(), "host removed") {
		t.Fatalf("removed: %v", err)
	}
	replaced := p
	replaced.Mismatch = "venv is now wenv"
	if _, err := pendingTarget(row(replaced, &protocol.Worktree{Session: "proj/fix"}, false)); err == nil || !strings.Contains(err.Error(), "host replaced") {
		t.Fatalf("replaced: %v", err)
	}
	// The host's environment changed before the relay recorded it.
	unseen := row(p, nil, false)
	unseen.Replaced = true
	if _, err := pendingTarget(unseen); err == nil || !strings.Contains(err.Error(), "host replaced") {
		t.Fatalf("replaced, unrecorded: %v", err)
	}
	r, err := pendingTarget(row(p, nil, false))
	if err != nil || r.Worktree == nil || r.Worktree.Session != "proj/fix" || r.Worktree.ID != "venv/worktree//w/proj/fix" {
		t.Fatalf("unlisted: %+v %v", r.Worktree, err)
	}
	bare := &protocol.Worktree{ID: "venv/worktree//w/proj/fix", EnvironmentID: "venv", Root: "/w/proj/fix", Source: "src"}
	r, err = pendingTarget(row(p, bare, false))
	if err != nil || r.Worktree.Session != "proj/fix" || r.Worktree.Source != "src" || bare.Session != "" {
		t.Fatalf("listed without a session: %+v %v", r.Worktree, err)
	}
	early := p
	early.Session = ""
	if _, err := pendingTarget(row(early, nil, false)); err == nil || !strings.Contains(err.Error(), "no session yet") {
		t.Fatalf("no session: %v", err)
	}
	failed := early
	failed.OK, failed.Error = false, "failed at agent: x"
	if _, err := pendingTarget(row(failed, nil, false)); err == nil || !strings.Contains(err.Error(), "proj/fix: failed; x dismisses") {
		t.Fatalf("failed: %v", err)
	}
	unknown := early
	unknown.OK, unknown.Error = false, "outcome unknown: the daemon no longer knows it"
	if _, err := pendingTarget(row(unknown, nil, false)); err == nil || !strings.Contains(err.Error(), "outcome unknown; x dismisses") {
		t.Fatalf("outcome unknown: %v", err)
	}
}

// The sidebar takes a task's p and x and what follows from them, and
// nothing else of the dashboard's. The worktree, with no agent, is a
// line in the tree.
func TestTaskAction(t *testing.T) {
	m := &view.Model{Width: 80, Height: 20}
	in := rows.Input{
		Hosts:     []rows.Host{{Name: "vm", EnvironmentID: "venv", Connected: true, Listed: true, Worktrees: true}},
		Pendings:  []protocol.Pending{{ID: "add-1", Host: "vm", Repo: "proj", Branch: "fix", SubmittedAt: time.Now()}},
		Worktrees: []protocol.Worktree{{ID: "venv/worktree//w/a", EnvironmentID: "venv", Repo: "proj", Branch: "a", Root: "/w/a"}},
	}
	m.SetTree(rows.Tree(in))
	m.SetRows(rows.Agents(in, rows.Tree(in)))
	m.Handle(term.Key{Rune: 'g'})
	other := func(r rune) view.Action { return view.Action{Kind: view.ActionOther, Key: term.Key{Rune: r}} }
	for _, r := range []rune{'p', 'x', 'X'} {
		if !taskAction(m, other(r)) {
			t.Errorf("%c on a task's row not taken", r)
		}
	}
	for _, r := range []rune{'a', 's', 'S'} {
		if taskAction(m, other(r)) {
			t.Errorf("%c taken on a task's row", r)
		}
	}
	treeView(m)
	if !m.Select("venv/worktree//w/a") { // the worktree row
		t.Fatal("no worktree line")
	}
	if taskAction(m, other('x')) {
		t.Error("x taken on a worktree row")
	}
	m.Ask("dismiss?", "dismiss")
	if !taskAction(m, view.Action{Kind: view.ActionConfirm}) {
		t.Error("the dismiss answer not taken")
	}
	m.Ask("remove?", "rm")
	if taskAction(m, view.Action{Kind: view.ActionConfirm}) {
		t.Error("the rm answer taken")
	}
	m.Overlay = view.NewLog("t")
	if !taskAction(m, view.Action{Kind: view.ActionOverlay}) {
		t.Error("a log's end not taken")
	}
}

// x is offered where the daemon takes it, and says why not elsewhere;
// p says where the prompt is when it cannot go; s refuses a task's row.
func TestPendingOffers(t *testing.T) {
	row := func(p protocol.Pending, replaced bool) rows.Row {
		return rows.Row{Host: "vm", Name: "proj/b", Pending: &p, Replaced: replaced}
	}
	for _, c := range []struct {
		name     string
		p        protocol.Pending
		replaced bool
		want     bool
	}{
		{"never sent", protocol.Pending{}, false, true},
		{"sent, not taken", protocol.Pending{Sent: true, Unreachable: "ssh: timeout"}, false, false},
		{"running", protocol.Pending{Sent: true, Taken: true}, false, false},
		{"running on a replaced machine, unrecorded", protocol.Pending{Sent: true, Taken: true}, true, false},
		{"recorded mismatch", protocol.Pending{Sent: true, Taken: true, Mismatch: "x"}, false, true},
		{"prompt not delivered", protocol.Pending{Sent: true, Taken: true, Done: true, OK: true, Prompt: protocol.DeliveryNotDelivered}, false, true},
		{"attempt open", protocol.Pending{Sent: true, Taken: true, Done: true, OK: true, Prompt: protocol.DeliveryUnknown, AttemptOpen: true}, false, false},
		{"awaiting the listing", protocol.Pending{Sent: true, Taken: true, Done: true, OK: true, Prompt: protocol.DeliveryDelivered}, false, false},
		{"done on a replaced machine", protocol.Pending{Sent: true, Taken: true, Done: true, OK: true, Prompt: protocol.DeliveryDelivered}, true, true},
	} {
		if got := Dismissable(row(c.p, c.replaced)); got != c.want {
			t.Errorf("%s: dismissable %v, want %v", c.name, got, c.want)
		}
	}
	// p goes where the relay can reach the machine and the prompt waits.
	undelivered := protocol.Pending{Sent: true, Taken: true, Done: true, OK: true, Prompt: protocol.DeliveryNotDelivered}
	mismatched := undelivered
	mismatched.Mismatch = "x"
	for _, c := range []struct {
		name string
		r    rows.Row
		want bool
	}{
		{"not delivered", row(undelivered, false), true},
		{"replaced, unrecorded", row(undelivered, true), false},
		{"recorded mismatch", row(mismatched, false), false},
		{"removed", rows.Row{Pending: &undelivered, Removed: true}, false},
	} {
		if got := Deliverable(c.r); got != c.want {
			t.Errorf("%s: deliverable %v, want %v", c.name, got, c.want)
		}
	}
	t.Setenv("LAATMUX_HOME", t.TempDir())
	cfg := dashConfig(t)
	d := &dash{ctx: context.Background(), cfg: cfg, st: merged.New()}
	m := &view.Model{Width: 100, Height: 20}
	expired := protocol.Pending{ID: "add-1", Host: "vm", Repo: "proj", Branch: "b", Sent: true, Taken: true, Done: true, OK: true, Prompt: protocol.DeliveryNotDelivered, AttemptError: protocol.ErrRecoveryExpired, SubmittedAt: time.Now()}
	listed := protocol.Pending{ID: "add-2", Host: "vm", Repo: "proj", Branch: "c", Sent: true, Taken: true, Done: true, OK: true, Prompt: protocol.DeliveryDelivered, SubmittedAt: time.Now().Add(-time.Minute)}
	in := rows.Input{Hosts: []rows.Host{{Name: "vm", Connected: true}}, Pendings: []protocol.Pending{expired, listed}}
	m.SetRows(rows.Agents(in, rows.Tree(in)))
	m.Handle(term.Key{Rune: 'g'})
	d.act(m, view.Action{Kind: view.ActionOther, Key: term.Key{Rune: 'p'}})
	if !strings.Contains(m.Message, "laatmux tasks show add-1 prints the prompt, if one was kept") {
		t.Errorf("p on an expired prompt: %q", m.Message)
	}
	d.act(m, view.Action{Kind: view.ActionOther, Key: term.Key{Rune: 'z'}})
	if !strings.Contains(m.Message, "pending task") {
		t.Errorf("z on a task: %q", m.Message)
	}
	m.Handle(term.Key{Rune: 'j'})
	d.act(m, view.Action{Kind: view.ActionOther, Key: term.Key{Rune: 'x'}})
	if m.Confirm != "" || !strings.Contains(m.Message, "hands over to its worktree row") {
		t.Errorf("x awaiting the listing: confirm %q message %q", m.Confirm, m.Message)
	}
	gone := listed
	gone.Gone, gone.Root, gone.EnvironmentID, gone.Session = true, "/r/c", "venv", "proj/c"
	// A gone task whose prompt was delivered has nothing kept to show.
	in = rows.Input{Hosts: []rows.Host{{Name: "vm", Connected: true}}, Pendings: []protocol.Pending{gone}}
	m.SetRows(rows.Agents(in, rows.Tree(in)))
	m.Handle(term.Key{Rune: 'g'})
	d.act(m, view.Action{Kind: view.ActionOther, Key: term.Key{Rune: 'p'}})
	if !strings.Contains(m.Message, "the prompt is delivered") || strings.Contains(m.Message, "tasks show") {
		t.Errorf("p on a gone, delivered task: %q", m.Message)
	}
	// p on a task whose host is removed, and x on a running task whose
	// replacement only the view has seen, say why not.
	stuck := protocol.Pending{ID: "add-9", Host: "old", Repo: "proj", Branch: "d", Sent: true, Taken: true, Done: true, OK: true, Prompt: protocol.DeliveryNotDelivered, SubmittedAt: time.Now()}
	in = rows.Input{Hosts: []rows.Host{{Name: "vm", Connected: true}}, Pendings: []protocol.Pending{stuck}}
	m.SetRows(rows.Agents(in, rows.Tree(in)))
	m.Handle(term.Key{Rune: 'g'})
	d.act(m, view.Action{Kind: view.ActionOther, Key: term.Key{Rune: 'p'}})
	if !strings.Contains(m.Message, "host removed; x dismisses the task") {
		t.Errorf("p on a removed host: %q", m.Message)
	}
	moving := protocol.Pending{ID: "add-8", Host: "vm", EnvironmentID: "venv", Repo: "proj", Branch: "e", Sent: true, Taken: true, SubmittedAt: time.Now()}
	in = rows.Input{Hosts: []rows.Host{{Name: "vm", EnvironmentID: "wenv", Connected: true}}, Pendings: []protocol.Pending{moving}}
	m.SetRows(rows.Agents(in, rows.Tree(in)))
	m.Handle(term.Key{Rune: 'g'})
	d.act(m, view.Action{Kind: view.ActionOther, Key: term.Key{Rune: 'x'}})
	if m.Confirm != "" || !strings.Contains(m.Message, "has not yet seen the machine change") {
		t.Errorf("x on an unrecorded replacement: confirm %q message %q", m.Confirm, m.Message)
	}
	d.act(m, view.Action{Kind: view.ActionOther, Key: term.Key{Rune: 'p'}})
	if m.Message != "proj/e: host replaced" {
		t.Errorf("p on an unrecorded replacement offers x: %q", m.Message)
	}
	if _, err := pendingTarget(rows.Row{Name: "proj/c", Pending: &gone}); err == nil || !strings.Contains(err.Error(), "gone") {
		t.Errorf("enter on a gone task: %v", err)
	}
}

// In the sidebar a click that jumps gives the focus back to the pane
// that had it; a key that jumps does not touch it, nor does a click in
// the dashboard's popup, which the jump closes. In the tree, whose
// worktree lines are rows to click, proj/task's and x/y's.
func TestClickJumpRefocuses(t *testing.T) {
	t.Setenv("LAATMUX_HOME", t.TempDir())
	cfg := dashConfig(t)
	m := dashModel(cfg)
	treeView(m)
	row := selectRow(t, m, "proj/task")
	refocused := 0
	var jumped []string
	jumpErr := error(nil)
	d := &dash{ctx: context.Background(), cfg: cfg, st: merged.New(), refocus: func() { refocused++ },
		jumper: func(r rows.Row) error { jumped = append(jumped, r.ID()); return jumpErr }}
	d.jumpAction(m, view.Action{Kind: view.ActionJump, Row: row, Mouse: true})
	if refocused != 1 || len(jumped) != 1 || jumped[0] != row.ID() {
		t.Fatalf("sidebar click: refocused %d jumped %v", refocused, jumped)
	}
	// A jump refused leaves the focus on the view, with the message,
	// and selects the row clicked, ending the following.
	jumpErr = errors.New("no session")
	m.Follow = true
	other := visibleRow(t, m, "x/y")
	d.jumpAction(m, view.Action{Kind: view.ActionJump, Row: other, Mouse: true})
	if refocused != 1 || m.Message != "no session" || m.Follow || m.Selection() == nil || m.Selection().ID() != other.ID() {
		t.Fatalf("refused: refocused %d message %q follow %v selected %+v", refocused, m.Message, m.Follow, m.Selection())
	}
	// A failed jump on the row already selected, while following, still
	// makes it the user's.
	first := selectRow(t, m, "proj/task")
	m.Follow = true
	d.jumpAction(m, view.Action{Kind: view.ActionJump, Row: first, Mouse: true})
	if m.Follow {
		t.Fatal("a failed jump on the selected row left the selection following")
	}
	jumpErr = nil
	// A run's jump is its worktree line's: through the line's root
	// agent when the home is lost, as the line itself jumps.
	lost := protocol.Worktree{ID: "venv/worktree//w/lost", EnvironmentID: "venv", Repo: "proj", Branch: "lost", Root: "/w/lost"}
	root := protocol.Agent{ID: "venv/laatmux/%7", EnvironmentID: "venv", Server: "laatmux", Session: "proj/lost-2", Managed: true, Cwd: "/w/lost", PaneID: "%7"}
	m.Tree = append(m.Tree, rows.Row{Kind: rows.KindWorktree, Depth: 1, Host: "vm", Node: lost.ID, Name: "proj/lost", Worktree: &lost, Agent: &root, Children: 1},
		rows.Row{Kind: rows.KindRun, Depth: 2, Host: "vm", Node: "venv/run/r9", Name: "make", Worktree: &lost, Run: &protocol.Run{ID: "venv/run/r9"}})
	jumped = nil
	d.jumpAction(m, view.Action{Kind: view.ActionJump, Row: &m.Tree[len(m.Tree)-1]})
	if len(jumped) != 1 || jumped[0] != lost.ID {
		t.Fatalf("a run's jump: %v", jumped)
	}
	// A click on a task still running jumps nowhere and keeps the focus.
	running := rows.Row{Name: "proj/new", Pending: &protocol.Pending{ID: "add-1"}}
	d.jumpAction(m, view.Action{Kind: view.ActionJump, Row: &running, Mouse: true})
	if refocused != 1 {
		t.Fatal("a click on a running task moved the focus")
	}
	d.jumpAction(m, view.Action{Kind: view.ActionJump, Row: row})
	if refocused != 1 {
		t.Fatal("a key's jump moved the focus")
	}
	d.exitOnJump = true
	d.jumpAction(m, view.Action{Kind: view.ActionJump, Row: row, Mouse: true})
	if refocused != 1 {
		t.Fatal("a click in the popup moved the focus")
	}
}

// A digit that jumps nowhere selects the row it counted, as a click
// does, and moves no focus; Enter leaves following as it was. In the
// tree, whose worktree lines are rows to jump to, proj/task's and x/y's.
func TestJumpNowhereSelects(t *testing.T) {
	t.Setenv("LAATMUX_HOME", t.TempDir())
	cfg := dashConfig(t)
	m := dashModel(cfg)
	treeView(m)
	m.Follow = true
	refocused := 0
	d := &dash{ctx: context.Background(), cfg: cfg, st: merged.New(), refocus: func() { refocused++ },
		jumper: func(rows.Row) error { return errors.New("no session") }}
	target := visibleRow(t, m, "x/y")
	d.jumpAction(m, view.Action{Kind: view.ActionJump, Row: target})
	if refocused != 0 || m.Follow || m.Selection() == nil || m.Selection().ID() != target.ID() {
		t.Fatalf("digit: refocused %d follow %v selected %+v", refocused, m.Follow, m.Selection())
	}
	// Enter that jumps nowhere was on the selection already: following
	// goes on. The proj/task line is the viewer's own here.
	visibleRow(t, m, "proj/task").Current = true
	m.Follow = true
	before := m.Selection().ID()
	a := m.Handle(term.Key{Kind: term.KeyEnter})
	if a.Kind != view.ActionJump || a.Row != nil {
		t.Fatalf("enter: %+v", a)
	}
	d.jumpAction(m, a)
	if refocused != 0 || !m.Follow || m.Selection().ID() != before {
		t.Fatalf("enter: refocused %d follow %v selected %+v", refocused, m.Follow, m.Selection())
	}
}

// lastPane gives the focus back to the pane active before the view's,
// once: with the view's pane no longer active, a second call does
// nothing. On a tmux server of the test's own, found through TMUX as
// the sidebar finds its own.
func TestLastPane(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("no tmux")
	}
	dir, err := os.MkdirTemp("/tmp", "lmxl")
	if err != nil {
		t.Fatal(err)
	}
	sock := filepath.Join(dir, "sock")
	tm := func(args ...string) string {
		t.Helper()
		out, err := tmux.Server{Path: sock}.Run(context.Background(), append([]string{"-f", "/dev/null"}, args...)...)
		if err != nil {
			t.Fatal(err)
		}
		return strings.TrimSpace(string(out))
	}
	t.Cleanup(func() {
		exec.Command("tmux", "-S", sock, "kill-server").Run()
		os.RemoveAll(dir)
	})
	main := tm("new-session", "-d", "-s", "s", "-x", "100", "-y", "20", "-P", "-F", "#{pane_id}", "sleep 100")
	side := tm("split-window", "-h", "-b", "-l", "30", "-t", main, "-P", "-F", "#{pane_id}", "sleep 100")
	tm("select-pane", "-t", main)
	tm("select-pane", "-t", side)
	pid := tm("display", "-p", "#{pid}")
	t.Setenv("TMUX", sock+","+pid+",0")
	t.Setenv("TMUX_PANE", side)
	active := func() string { return tm("display", "-p", "-t", "s", "#{pane_id}") }
	lastPane(context.Background())
	if a := active(); a != main {
		t.Fatalf("after lastPane: active %s, want %s", a, main)
	}
	lastPane(context.Background())
	if a := active(); a != main {
		t.Fatalf("a second lastPane toggled back to %s", a)
	}
}
