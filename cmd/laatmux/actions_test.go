package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/laat/laatmux/internal/command"
	"github.com/laat/laatmux/internal/config"
	"github.com/laat/laatmux/internal/home"
	"github.com/laat/laatmux/internal/merged"
	"github.com/laat/laatmux/internal/peer"
	"github.com/laat/laatmux/internal/protocol"
	"github.com/laat/laatmux/internal/rows"
	"github.com/laat/laatmux/internal/source"
	"github.com/laat/laatmux/internal/term"
	"github.com/laat/laatmux/internal/tmux"
	"github.com/laat/laatmux/internal/view"
	"github.com/laat/laatmux/internal/workspace"
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
// done. The message names the file once, as the error from reading it
// does.
func TestAddFlowRefusesBadLast(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("LAATMUX_HOME", dir)
	path := filepath.Join(dir, "last.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := dashConfig(t)
	d := &dash{ctx: context.Background(), cfg: cfg, st: merged.New()}
	m := dashModel(cfg)
	d.act(m, view.Action{Kind: view.ActionOther, Key: term.Key{Rune: 'a'}})
	want := path + ": " + json.Unmarshal([]byte("{not json"), &home.Last{}).Error()
	if m.Overlay != nil || d.add != nil || m.Message != want {
		t.Errorf("overlay=%v add=%v message=%q, want %q", m.Overlay, d.add, m.Message, want)
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
// without a home, as z does; it opens nothing in A's. A line whose
// jump agent sits in a plain session holds B's workspace session when
// that exists: S and z on the line and on every agent it holds act on
// that session, while enter on the line goes to the plain session.
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
	// the plain session notes on this machine's default server, and
	// another agent of B in a window of B's own workspace session, which
	// has that session as its own: the line holds B's workspace session.
	// S on the line and on either agent opens the shell there, and z
	// toggles it, though enter on the line goes to notes (#188). With no
	// workspace session of B's the line holds notes, and S and z refuse
	// on it and on its agent.
	b.Session = ""
	notes := protocol.Agent{ID: "menv/default/%1", EnvironmentID: "menv", Server: "default", Session: "notes", Agent: "claude", Activity: protocol.Working, Liveness: protocol.Alive, Cwd: "/w/b", WorktreeID: b.ID, Identity: &protocol.Identity{PID: 100, StartUnix: 1}}
	inB := agent
	inB.Session = "mac/proj/b"
	in := rows.Input{
		Hosts:     []rows.Host{host},
		Agents:    []protocol.Agent{notes, inB},
		Worktrees: []protocol.Worktree{a, b},
		Locals:    []protocol.Session{{Name: "mac/proj/a", Key: "menv//w/a", Host: "mac"}, {Name: "mac/proj/b", Key: "menv//w/b", Host: "mac"}, {Name: "notes"}},
	}
	for _, tree := range []bool{false, true} {
		m := show(in, tree)
		l := m.OwnerLine(b.ID)
		if l == nil || l.Agent == nil || l.Agent.ID != notes.ID || l.Local == nil || l.Local.Name != "mac/proj/b" {
			t.Fatalf("tree %v: B's line is %+v", tree, l)
		}
		if tree {
			os.Remove(log)
			d.jumpRow(m, *l)
			if got, _ := os.ReadFile(log); !strings.HasPrefix(m.Message, "notes is on the default tmux server") || len(got) != 0 {
				t.Errorf("enter on B's line: message %q, tmux %q", m.Message, got)
			}
		}
		ids := []string{notes.ID, inB.ID}
		if tree {
			ids = append(ids, b.ID)
		}
		for _, id := range ids {
			if msg, cmds := press(m, id, 'S'); !strings.Contains(cmds, "-L default new-window -t =mac/proj/b: -n shell -c /w/b ;") ||
				strings.Contains(cmds, "new-session") || strings.Contains(cmds, "notes") || !strings.HasPrefix(msg, "mac/proj/b is on the default tmux server") {
				t.Errorf("tree %v: S on %s: message %q, tmux %q", tree, id, msg, cmds)
			}
			if msg, cmds := press(m, id, 'z'); msg != "settled mac/proj/b" || cmds != "-u -L default set-option -t =mac/proj/b: @laatmux_settled 1\n" {
				t.Errorf("tree %v: z on %s: message %q, tmux %q", tree, id, msg, cmds)
			}
		}
	}
	in.Agents, in.Locals = []protocol.Agent{notes}, []protocol.Session{{Name: "mac/proj/a", Key: "menv//w/a", Host: "mac"}, {Name: "notes"}}
	for _, tree := range []bool{false, true} {
		m := show(in, tree)
		if l := m.OwnerLine(b.ID); l == nil || l.Local == nil || l.Local.Name != "notes" {
			t.Fatalf("tree %v: B's line without a workspace session is %+v", tree, l)
		}
		ids := []string{notes.ID}
		if tree {
			ids = append(ids, b.ID)
		}
		for _, id := range ids {
			if msg, cmds := press(m, id, 'S'); msg != "proj/b: not a workspace" || cmds != "" {
				t.Errorf("tree %v: S on %s without a workspace session: message %q, tmux %q", tree, id, msg, cmds)
			}
			if msg, cmds := press(m, id, 'z'); !strings.HasPrefix(msg, "proj/b: no local workspace session;") || cmds != "" {
				t.Errorf("tree %v: z on %s without a workspace session: message %q, tmux %q", tree, id, msg, cmds)
			}
		}
	}
	// vm lists worktree proj/z with the home session proj/z, and in a
	// split of it the user ran claude in their home directory: an agent
	// of no worktree, in other sessions with no local session of its own.
	// Enter on it, as its tile and as its node, lands in vm/proj/z, the
	// worktree's workspace session, and S opens the shell there, making
	// the session first when there is none, as z toggles it
	// (TestSettleGoesByLine); so with the home lost, through the root
	// agent's session.
	vm := rows.Host{Name: "vm", EnvironmentID: "venv", Connected: true, Listed: true, Worktrees: true, Attribution: true}
	z := protocol.Worktree{ID: "venv/worktree//w/proj/z", EnvironmentID: "venv", Repo: "proj", Source: "git@github.com:laat/proj.git", Branch: "z", Root: "/w/proj/z", Session: "proj/z"}
	root := protocol.Agent{ID: "venv/laatmux/%1", EnvironmentID: "venv", Server: "laatmux", Session: "proj/z", Agent: "claude", Activity: protocol.Working, Liveness: protocol.Alive, Managed: true, Cwd: z.Root, WorktreeID: z.ID}
	stray := protocol.Agent{ID: "venv/laatmux/%10", EnvironmentID: "venv", Server: "laatmux", Session: "proj/z", Agent: "claude", Activity: protocol.Working, Liveness: protocol.Alive, Managed: true, Cwd: "/home/u"}
	for _, c := range []struct {
		home      string
		workspace bool // vm/proj/z exists
	}{{"proj/z", true}, {"", true}, {"proj/z", false}, {"", false}} {
		z.Session = c.home
		in := rows.Input{Hosts: []rows.Host{vm}, Agents: []protocol.Agent{root, stray}, Worktrees: []protocol.Worktree{z}}
		if c.workspace {
			in.Locals = []protocol.Session{{Name: "vm/proj/z", Key: "venv//w/proj/z", Host: "vm"}}
		}
		for _, tree := range []bool{false, true} {
			m := show(in, tree)
			if !m.Select(stray.ID) {
				t.Fatalf("%+v tree %v: no row %s", c, tree, stray.ID)
			}
			if r := m.Selection(); r.Worktree != nil || r.Local != nil || r.Name != "proj/z" {
				t.Fatalf("%+v tree %v: the agent's row is %+v", c, tree, r)
			}
			os.Remove(log)
			d.jumpRow(m, *m.Selection())
			if !strings.HasPrefix(m.Message, "vm/proj/z is on the default tmux server") {
				got, _ := os.ReadFile(log)
				t.Errorf("%+v tree %v: enter on the agent: message %q, tmux %q", c, tree, m.Message, got)
			}
			msg, cmds := press(m, stray.ID, 'S')
			if !strings.Contains(cmds, "-L default new-window -t =vm/proj/z: -n shell ") || !strings.HasPrefix(msg, "vm/proj/z is on the default tmux server") ||
				c.workspace == strings.Contains(cmds, "-L default new-session -d -s vm/proj/z ") {
				t.Errorf("%+v tree %v: S on the agent: message %q, tmux %q", c, tree, msg, cmds)
			}
		}
	}
	// The homeless worktree proj/a, whose root agent was moved by hand
	// into proj/z, has proj/z as its home too, and comes first in the
	// tree's order. The host then gives proj/z no home either, the
	// session's panes no longer all in its root; it keeps it only with
	// proj/a's root inside proj/z's. Either way enter on proj/z's root
	// agent, a child of proj/z's line, and on the agent of no worktree
	// lands in vm/proj/z, the session's by its home or by its name, as
	// its tile and as its node, and S and z on them act on vm/proj/z;
	// enter on proj/a's moved root agent, and on a pane of proj/a in
	// proj/z, lands in vm/proj/a, their own line's workspace session
	// attaching proj/z too, where S and z on them act. An agent of proj/z
	// in a window of homeless proj/b's session, which proj/z's line does
	// not attach, lands in vm/proj/b, while S and z on it act on
	// vm/proj/z.
	pa := protocol.Worktree{ID: "venv/worktree//w/proj/a", EnvironmentID: "venv", Repo: "proj", Source: z.Source, Branch: "a", Root: "/w/proj/a"}
	moved := protocol.Agent{ID: "venv/laatmux/%2", EnvironmentID: "venv", Server: "laatmux", Session: "proj/z", Agent: "claude", Activity: protocol.Working, Liveness: protocol.Alive, Managed: true, Cwd: pa.Root, WorktreeID: pa.ID}
	pane := protocol.Pane{ID: "venv/pane/laatmux/%3", EnvironmentID: "venv", Server: "laatmux", Session: "proj/z", PaneID: "%3", Command: "zsh", Cwd: pa.Root, WorktreeID: pa.ID}
	pb := protocol.Worktree{ID: "venv/worktree//w/proj/b", EnvironmentID: "venv", Repo: "proj", Source: z.Source, Branch: "b", Root: "/w/proj/b"}
	rootB := protocol.Agent{ID: "venv/laatmux/%4", EnvironmentID: "venv", Server: "laatmux", Session: "proj/b", Agent: "claude", Activity: protocol.Working, Liveness: protocol.Alive, Managed: true, Cwd: pb.Root, WorktreeID: pb.ID}
	visitor := protocol.Agent{ID: "venv/laatmux/%11", EnvironmentID: "venv", Server: "laatmux", Session: "proj/b", Agent: "claude", Activity: protocol.Working, Liveness: protocol.Alive, Managed: true, Cwd: z.Root, WorktreeID: z.ID}
	for _, home := range []string{"", "proj/z"} {
		z.Session = home
		in = rows.Input{Hosts: []rows.Host{vm}, Agents: []protocol.Agent{root, moved, stray, rootB, visitor}, Worktrees: []protocol.Worktree{pa, pb, z}, Panes: []protocol.Pane{pane},
			Locals: []protocol.Session{{Name: "vm/proj/a", Key: "venv//w/proj/a", Host: "vm"}, {Name: "vm/proj/b", Key: "venv//w/proj/b", Host: "vm"}, {Name: "vm/proj/z", Key: "venv//w/proj/z", Host: "vm"}}}
		for _, tree := range []bool{false, true} {
			m := show(in, tree)
			if l := m.OwnerLine(pa.ID); l == nil || l.Home() != "proj/z" {
				t.Fatalf("home %q tree %v: proj/a's line is %+v", home, tree, l)
			}
			for _, c := range []struct {
				id, enter, act string
				tree           bool // a node only
			}{
				{root.ID, "vm/proj/z", "vm/proj/z", false}, {stray.ID, "vm/proj/z", "vm/proj/z", false}, {moved.ID, "vm/proj/a", "vm/proj/a", false},
				{pane.ID, "vm/proj/a", "vm/proj/a", true}, {visitor.ID, "vm/proj/b", "vm/proj/z", false},
			} {
				if c.tree && !tree {
					continue
				}
				if !m.Select(c.id) {
					t.Fatalf("home %q tree %v: no row %s", home, tree, c.id)
				}
				os.Remove(log)
				d.jumpRow(m, *m.Selection())
				if !strings.HasPrefix(m.Message, c.enter+" is on the default tmux server") {
					got, _ := os.ReadFile(log)
					t.Errorf("home %q tree %v: enter on %s: message %q, tmux %q, want %s", home, tree, c.id, m.Message, got, c.enter)
				}
				if msg, cmds := press(m, c.id, 'S'); !strings.Contains(cmds, "-L default new-window -t ="+c.act+": -n shell ") || !strings.HasPrefix(msg, c.act+" is on the default tmux server") {
					t.Errorf("home %q tree %v: S on %s: message %q, tmux %q, want %s", home, tree, c.id, msg, cmds, c.act)
				}
				if msg, cmds := press(m, c.id, 'z'); msg != "settled "+c.act || cmds != "-u -L default set-option -t ="+c.act+": @laatmux_settled 1\n" {
					t.Errorf("home %q tree %v: z on %s: message %q, tmux %q, want %s", home, tree, c.id, msg, cmds, c.act)
				}
			}
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
// no line holds the session either. A row of no worktree and no
// workspace session, which no line holds, is not a workspace.
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
	// A managed agent of no worktree in the worktree's home session, run
	// by the user in their home directory from a split of proj/z: it
	// stands in other sessions with no local session of its own and with
	// the workspace session's state, as its tile and as its node. z on it
	// goes where enter goes, to the line whose home its session is, and
	// toggles vm/proj/z, as S opens the shell there (TestShellGoesByLine);
	// so with the home lost, through the root agent's session, and with
	// no agent of the worktree left, through the session's name. Without
	// the workspace session it says what enter on the line does about
	// one, or, the line with no home, what enter on the agent does.
	stray := protocol.Agent{ID: "venv/laatmux/%10", EnvironmentID: "venv", Server: "laatmux", Session: "proj/z", Agent: "claude", Activity: protocol.Working, Liveness: protocol.Alive, Managed: true, Cwd: "/home/u"}
	lost := w
	lost.Session = ""
	for _, c := range []struct {
		w                        protocol.Worktree
		workspace, settled, bare bool
	}{
		{w, true, false, false}, {w, true, true, false}, {lost, true, false, false}, {lost, true, true, false}, {w, false, false, false}, {lost, false, false, false},
		{lost, true, false, true}, {lost, true, true, true}, {lost, false, false, true},
	} {
		in := rows.Input{Hosts: []rows.Host{host}, Agents: append(append([]protocol.Agent{}, agents...), stray), Worktrees: []protocol.Worktree{c.w}}
		if c.bare {
			in.Agents = []protocol.Agent{stray}
		}
		want, msg := expect("vm/proj/z", c.settled)
		switch {
		case c.workspace:
			in.Locals = []protocol.Session{{Name: "vm/proj/z", Key: "venv//w/proj/z", Host: "vm", Settled: c.settled}}
		case c.bare:
			want, msg = "", "proj/z: no local workspace session; enter creates one"
		default:
			want, msg = "", "proj/z: no local workspace session; enter on the line creates one"
		}
		for _, tree := range []bool{false, true} {
			m := show(in, tree)
			if !m.Select(stray.ID) {
				t.Fatalf("%+v tree %v: no row %s", c, tree, stray.ID)
			}
			if r := m.Selection(); r.Worktree != nil || r.Local != nil || r.Settled != c.settled {
				t.Fatalf("%+v tree %v: the agent's row is %+v", c, tree, r)
			}
			if got, cmds := press(m, stray.ID); got != msg || cmds != want {
				t.Errorf("%+v tree %v: z on the agent: message %q, tmux %q", c, tree, got, cmds)
			}
		}
	}
	// No configured host claims the worktree's machine nor the agent's,
	// another one: the workspace session whose home has the agent's
	// session's name is not the agent's, and z says it is not a
	// workspace, as enter refuses it.
	other := stray
	other.ID, other.EnvironmentID = "yenv/laatmux/%10", "yenv"
	xw := w
	xw.ID, xw.EnvironmentID = "xenv/worktree//w/proj/z", "xenv"
	unclaimed := rows.Input{Agents: []protocol.Agent{other}, Worktrees: []protocol.Worktree{xw}, Locals: []protocol.Session{{Name: "x/proj/z", Key: "xenv//w/proj/z"}}}
	for _, tree := range []bool{false, true} {
		if got, cmds := press(show(unclaimed, tree), other.ID); got != "proj/z: not a workspace" || cmds != "" {
			t.Errorf("unclaimed tree %v: z on the agent: message %q, tmux %q", tree, got, cmds)
		}
	}
	// The add's agent before the host lists the worktree, as its node
	// and as its tile, with the task's workspace session settled, with
	// none, as a background add leaves it, and with only a plain
	// attachment to the agent's session: the task's, refused, though its
	// row has no worktree.
	add := protocol.Agent{ID: "venv/laatmux/%9", EnvironmentID: "venv", Server: "laatmux", Session: "proj/y", Agent: "claude", Activity: protocol.Working, Liveness: protocol.Alive, Managed: true, Cwd: "/w/proj/y"}
	loose := protocol.Pending{ID: "add-y", Host: "vm", EnvironmentID: "venv", Source: w.Source, Repo: "proj", Branch: "y", Root: "/w/proj/y", Session: "proj/y", Sent: true, Taken: true, Done: true, OK: true, Prompt: protocol.DeliveryDelivered, SubmittedAt: time.Now()}
	for _, locals := range [][]protocol.Session{
		{{Name: "vm/proj/y", Key: "venv//w/proj/y", Host: "vm", Settled: true}},
		nil,
		{{Name: "vm/proj/y", Attach: "vm/proj/y"}},
	} {
		in := rows.Input{
			Hosts:    []rows.Host{host},
			Agents:   []protocol.Agent{add},
			Locals:   locals,
			Pendings: []protocol.Pending{loose},
		}
		// In the agent view the add's agent is on the task's tile.
		for _, tree := range []bool{false, true} {
			id := add.ID
			if !tree {
				id = loose.ID
			}
			if got, cmds := press(show(in, tree), id); !strings.HasPrefix(got, "proj/y: a pending task") || cmds != "" {
				t.Errorf("%v tree %v: z on the add's agent: message %q, tmux %q", locals, tree, got, cmds)
			}
		}
	}
}

// fakeDefaultTmux puts a tmux on PATH that answers as the default
// server whichever server is asked, with TMUX naming it: the view is
// inside it, and enter switches the client. It logs each command line
// to the file returned, and prints nothing but for display-message,
// which prints its format, the last argument, with the socket path for
// #{socket_path}, frame and all, as Records reads it. PATH has nothing
// else, so the shell does it alone.
func fakeDefaultTmux(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	log := filepath.Join(dir, "tmux.log")
	script := "#!/bin/sh\necho \"$*\" >> " + log + "\ncase \"$*\" in *display-message*) for a; do f=$a; done; printf '%s\\n' \"/tmp/lmx-fake/default${f#'#{socket_path}'}\" ;; esac\n"
	if err := os.WriteFile(filepath.Join(dir, "tmux"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	t.Setenv("TMUX", "/tmp/lmx-fake/default,1,0")
	return log
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
// no agent, on a host with no capabilities cached, enter refuses with
// the add line (TestEnterMakesShellSession has a host with new). A done
// task standing for the worktree goes by the task's target, as enter
// does; under a task still running, enter on the line waits for it.
func TestSettleHintGoesByEnter(t *testing.T) {
	log := fakeDefaultTmux(t)
	t.Setenv("LAATMUX_HOME", t.TempDir())
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
	add := "laatmux add b --repo proj --host mac --agent claude makes one"
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
			bv.ID, onVM.ID, "", "REFUSED; laatmux add b --repo proj --host vm --agent claude makes one"},
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
		// The agent, as its node and as its tile, says it of its line;
		// under a standing task it is on the task's tile, no tile of
		// its own.
		for _, tree := range []bool{false, true} {
			if !tree && c.in.Pendings != nil {
				continue
			}
			if got, cmds := press(show(c.in, tree), c.agent, false); got != hint("enter on the line") || cmds != "" {
				t.Errorf("%s tree %v: z on the agent: message %q, tmux %q, want %q", c.name, tree, got, cmds, hint("enter on the line"))
			}
		}
	}
	// A line no configured host claims, and one on a host this machine's
	// config lacks: enter's refusal, and no add line for a host the line
	// is not on or that add does not know.
	unclaimed := rows.Row{Kind: rows.KindWorktree, Name: "proj/b", Worktree: &b, Agent: &inNotes}
	if got := noWorkspaceHint(d.cfg, d.st, unclaimed, false); got != "proj/b: no configured host claims this record" {
		t.Errorf("a line no host claims: %q", got)
	}
	unclaimed.Host = "ghost"
	if got := noWorkspaceHint(d.cfg, d.st, unclaimed, false); got != `unknown host "ghost"` {
		t.Errorf("a line on a host not configured: %q", got)
	}
	// A line of no worktree whose jump goes by an agent, which settle
	// does not pass: enter's refusal, and no add line, which is the
	// worktree's.
	stray := rows.Row{Kind: rows.KindWorktree, Host: "vm", Name: "notes", Agent: &protocol.Agent{ID: "venv/default/%9", EnvironmentID: "venv", Server: "default", Session: "notes"}}
	if got := noWorkspaceHint(d.cfg, d.st, stray, true); got != "vm/notes: on vm's default tmux server, which laatmux only observes; attach is limited to managed sessions" {
		t.Errorf("a line of no worktree: %q", got)
	}
	// An agent of no worktree, in notes, has no line to go by: it is not
	// a workspace, whatever enter on it does, and z runs nothing.
	loose := protocol.Agent{ID: "menv/default/%7", EnvironmentID: "menv", Server: "default", Session: "notes", Agent: "claude", Activity: protocol.Working, Liveness: protocol.Alive, Cwd: "/Users/u"}
	in := rows.Input{Hosts: []rows.Host{mac}, Agents: []protocol.Agent{loose}, Locals: locals}
	for _, tree := range []bool{false, true} {
		if got, cmds := press(show(in, tree), loose.ID, false); got != "notes: not a workspace" || cmds != "" {
			t.Errorf("tree %v: z on an agent of no worktree: message %q, tmux %q", tree, got, cmds)
		}
	}
}

// fakeNew is a fake local daemon answering as environment env with the
// capabilities given that answers a subscribe with snap, when given,
// and new: each request goes on the channel returned, then the hook for
// its name runs, when there is one, and says whether tmux refuses the
// name as a duplicate; one with no hook is made.
func fakeNew(t *testing.T, env string, caps []string, snap *protocol.Message, hooks map[string]func() bool) <-chan protocol.Message {
	t.Helper()
	requests := make(chan protocol.Message, 16)
	startFakeDaemonAs(t, env, caps, func(pc *protocol.Conn, m protocol.Message) bool {
		switch m.Type {
		case protocol.TypeSubscribe:
			if snap != nil {
				pc.Write(*snap)
			}
		case protocol.TypeNew:
			requests <- m
			res := protocol.Message{Type: protocol.TypeResult, ID: m.ID, OK: true, Session: m.Name, PaneID: "%7"}
			if hook := hooks[m.Name]; hook != nil && hook() {
				res = protocol.Message{Type: protocol.TypeResult, ID: m.ID, Error: "tmux new-session -d -s " + m.Name + " -c /w: duplicate session: " + m.Name}
			}
			pc.Write(res)
		}
		return true
	})
	return requests
}

// inUse is fakeNew's hooks for names tmux refuses as duplicates.
func inUse(names ...string) map[string]func() bool {
	hooks := map[string]func() bool{}
	for _, n := range names {
		hooks[n] = func() bool { return true }
	}
	return hooks
}

// asked is the new request on the channel, as "<name> <cwd> <host>
// <cmd>", "" for none.
func asked(c <-chan protocol.Message) string {
	select {
	case m := <-c:
		return fmt.Sprintf("%s %s %s %q", m.Name, m.Cwd, m.Host, m.Cmd)
	default:
		return ""
	}
}

// Enter on a worktree line with no managed session and no agent, in the
// dashboard and in the sidebar alike, has the host's daemon make one,
// named as add names it, by the host's label, at the root, with no
// command, without the view waiting on it: the message line says a
// session is being made, and a jump meanwhile is refused with it. Then,
// on the view's goroutine, the jump makes the workspace session attached
// to it, switches there, says what it made, and ends the dashboard; a
// switch that fails says what was made before its error. z on the line
// says enter does that and add makes one with an agent. A name in use
// that no record places elsewhere, a session made since the records
// were read, is attached, and so is one whose records, arriving
// meanwhile, have the add's agent at the root; one another worktree has
// as its home, or in which an agent or a pane of another worktree runs,
// by the records once the host has answered, another clone's add's made
// meanwhile too, is refused as add refuses it, by the records at enter
// when the stream has no listing of the worktree's machine by the
// answer. A user who has moved on meanwhile, to a form or a question or
// with the client to another session, is left there, the message saying
// the session is there. A detached worktree, a host whose cached
// capabilities lack new, and a host the merged state has as down keep
// the add hint, and nothing is asked; so does a host whose hello lacks
// new. A host that answers as another machine is asked nothing.
func TestEnterMakesShellSession(t *testing.T) {
	log := fakeDefaultTmux(t)
	src, fork := "git@github.com:laat/proj.git", "git@github.com:fork/proj.git"
	wt := func(env, repo, branch, root string) protocol.Worktree {
		return protocol.Worktree{ID: env + "/worktree/" + root, EnvironmentID: env, Repo: repo, Source: src, Branch: branch, Root: root}
	}
	b, det, taken, race, slow := wt("menv", "proj", "b", "/w/b"), wt("menv", "proj", "", "/w/det"), wt("menv", "proj", "taken", "/w/taken"), wt("menv", "proj", "race", "/w/race"), wt("menv", "proj", "slow", "/w/slow")
	// The host labels c's repository otherwise, which this machine's
	// view shows by its own label.
	c := wt("menv", "proj-host", "c", "/w/c")
	// Another clone's worktrees on the branches of clone, lost, pn and
	// dc, whose sessions are named as theirs would be: its home, its
	// agent with the home lost, a pane of it, its home again.
	clone, lost, pn, dc := wt("menv", "proj", "clone", "/w/clone"), wt("menv", "proj", "lost", "/w/lost"), wt("menv", "proj", "pn", "/w/pn"), wt("menv", "proj", "dc", "/w/dc")
	other, otherLost, otherPn, otherDc := wt("menv", "proj", "clone", "/w2/clone"), wt("menv", "proj", "lost", "/w2/lost"), wt("menv", "proj", "pn", "/w2/pn"), wt("menv", "proj", "dc", "/w2/dc")
	other.Source, otherLost.Source, otherPn.Source, otherDc.Source, other.Session, otherDc.Session = fork, fork, fork, fork, "proj/clone", "proj/dc"
	// And on late's, whose session an add makes meanwhile.
	late, otherLate := wt("menv", "proj", "late", "/w/late"), wt("menv", "proj", "late", "/w2/late")
	otherLate.Source = fork
	onVM, onBox := wt("venv", "proj", "b", "/w/b"), wt("benv", "proj", "b", "/w/b")
	records := []protocol.Worktree{b, det, taken, c, race, slow, clone, lost, pn, dc, late, other, otherLost, otherPn, otherDc, otherLate, onVM, onBox}
	caps := []string{protocol.CapStatus, protocol.CapWorktrees, protocol.CapAttribution, protocol.CapNew}
	snap := protocol.Message{Type: protocol.TypeSnapshot, Hosts: []protocol.HostStatus{
		{Name: "mac", EnvironmentID: "menv", Connected: true, Listed: true, Capabilities: caps},
		{Name: "vm", SSH: "vm", EnvironmentID: "venv", Connected: true, Listed: true, Capabilities: caps[:3]},
		{Name: "box", SSH: "box", EnvironmentID: "benv", Error: "ssh: connection refused", Capabilities: caps},
	}, Worktrees: records,
		Agents: []protocol.Agent{{ID: "menv/laatmux/%3", EnvironmentID: "menv", Server: "laatmux", Session: "proj/lost", Agent: "claude", Managed: true, Cwd: "/w2/lost", WorktreeID: otherLost.ID}},
		Panes:  []protocol.Pane{{ID: "menv/pane/laatmux/%4", EnvironmentID: "menv", Server: "laatmux", Session: "proj/pn", PaneID: "%4", Cwd: "/w2/pn/src", WorktreeID: otherPn.ID}}}
	// The records as the stream has them by the answer, which the
	// records at enter lack: the add's agent in proj/race at its root;
	// another clone's add's agent in proj/late at its own; mac down.
	raced, lated := snap, snap
	raced.Agents = append(slices.Clip(snap.Agents), protocol.Agent{ID: "menv/laatmux/%5", EnvironmentID: "menv", Server: "laatmux", Session: "proj/race", Agent: "claude", Managed: true, Cwd: race.Root, WorktreeID: race.ID})
	lated.Agents = append(slices.Clip(snap.Agents), protocol.Agent{ID: "menv/laatmux/%6", EnvironmentID: "menv", Server: "laatmux", Session: "proj/late", Agent: "claude", Managed: true, Cwd: otherLate.Root, WorktreeID: otherLate.ID})
	down := snap
	down.Hosts = slices.Clone(snap.Hosts)
	down.Hosts[0].Error, down.Hosts[0].Connected = "ssh: connection reset", false
	var d *dash
	release := make(chan struct{})
	hooks := inUse("proj/taken", "proj/clone", "proj/lost", "proj/pn")
	hooks["proj/race"] = func() bool { d.st.Apply(raced); return true }
	hooks["proj/late"] = func() bool { d.st.Apply(lated); return true }
	// The records the stream has by dc's answer, nil for those at enter.
	var dcAnswer atomic.Pointer[protocol.Message]
	hooks["proj/dc"] = func() bool {
		if s := dcAnswer.Load(); s != nil {
			d.st.Apply(*s)
		}
		return true
	}
	hooks["proj/slow"] = func() bool { <-release; return false }
	requests := fakeNew(t, "menv", []string{protocol.CapStatus, protocol.CapNew}, nil, hooks)
	ends := make(chan func(*view.Model) view.Action, 1)
	where := "work"
	d = &dash{ctx: context.Background(), cfg: dashConfig(t), st: merged.New(), cmds: ends, clientAt: func(context.Context) string { return where }}
	d.st.Apply(snap)
	viewed := slices.Clone(records)
	viewed[3].Repo = "proj" // merged.State's relabelling
	in := rows.Input{Hosts: []rows.Host{
		{Name: "mac", Local: true, EnvironmentID: "menv", Connected: true, Listed: true, Worktrees: true, Attribution: true},
		{Name: "vm", EnvironmentID: "venv", Connected: true, Listed: true, Worktrees: true, Attribution: true},
		{Name: "box", EnvironmentID: "benv", Listed: true, Worktrees: true, Attribution: true},
	}, Worktrees: viewed, Agents: snap.Agents, Panes: snap.Panes, HostRepos: map[string]string{c.ID: "proj-host"}}
	model := func(id string) *view.Model {
		t.Helper()
		m := &view.Model{Width: 100, Height: 40, ShowHidden: true, View: view.ViewTree}
		m.SetTree(rows.Tree(in))
		m.SetRows(rows.Agents(in, rows.Tree(in)))
		m.Render()
		if !m.Select(id) {
			t.Fatalf("no line %s", id)
		}
		return m
	}
	type pressed struct {
		exit                    bool
		waiting, msg, cmds, req string
		end                     view.Action
	}
	// enterThen is enter on the line, what the user does meanwhile, and
	// the end of a jump that waits on the host run as the view runs it:
	// the message while it waits, and after the end, the end's action,
	// the message, the tmux commands run and the new request.
	enterThen := func(exitOnJump bool, id string, meanwhile func(m *view.Model)) pressed {
		t.Helper()
		m := model(id)
		os.Remove(log)
		d.exitOnJump = exitOnJump
		var p pressed
		p.exit = d.jumpAction(m, view.Action{Kind: view.ActionJump})
		p.waiting = m.Message
		if d.making != "" {
			meanwhile(m)
			select {
			case end := <-ends:
				p.end = end(m)
			case <-time.After(10 * time.Second):
				t.Fatalf("enter on %s: the jump did not end", id)
			}
		}
		got, _ := os.ReadFile(log)
		p.msg, p.cmds, p.req = m.Message, string(got), asked(requests)
		return p
	}
	enter := func(exitOnJump bool, id string) pressed {
		t.Helper()
		return enterThen(exitOnJump, id, func(*view.Model) {})
	}
	z := func(id string) (msg, cmds, req string) {
		t.Helper()
		m := model(id)
		os.Remove(log)
		d.act(m, view.Action{Kind: view.ActionOther, Key: term.Key{Rune: 'z'}})
		got, _ := os.ReadFile(log)
		return m.Message, string(got), asked(requests)
	}
	for _, sidebar := range []bool{false, true} {
		p := enter(!sidebar, b.ID)
		if p.waiting != "making a session on mac…" || p.exit || p.req != `proj/b /w/b mac []` || p.msg != "made session proj/b on mac, a shell at /w/b" || (p.end.Kind == view.ActionQuit) == sidebar {
			t.Errorf("sidebar %v: enter: %+v", sidebar, p)
		}
		for _, want := range []string{"new-session -d -s mac/proj/b ", "@laatmux_attach_target proj/b ", "switch-client -t =mac/proj/b:"} {
			if !strings.Contains(p.cmds, want) {
				t.Errorf("sidebar %v: enter ran %q, want %q in it", sidebar, p.cmds, want)
			}
		}
	}
	// A jump while one waits on the host is refused with its message.
	m := model(b.ID)
	d.jumpAction(m, view.Action{Kind: view.ActionJump})
	if again := model(c.ID); d.jumpAction(again, view.Action{Kind: view.ActionJump}) || again.Message != "making a session on mac…" {
		t.Errorf("a jump meanwhile: message %q", again.Message)
	}
	select {
	case end := <-ends:
		end(m)
	case <-time.After(10 * time.Second):
		t.Fatal("the jump did not end")
	}
	if req := asked(requests); req != `proj/b /w/b mac []` || asked(requests) != "" {
		t.Errorf("a jump meanwhile asked: %q", req)
	}
	if msg, cmds, req := z(b.ID); msg != "proj/b: no local workspace session; enter creates one with a shell; laatmux add b --repo proj --host mac --agent claude makes one with an agent" || cmds != "" || req != "" {
		t.Errorf("z: message %q, tmux %q, asked %q", msg, cmds, req)
	}
	if p := enter(true, c.ID); p.req != `proj-host/c /w/c mac []` || !strings.Contains(p.cmds, "switch-client -t =mac/proj-host/c:") {
		t.Errorf("by the host's label: %+v", p)
	}
	for _, w := range []protocol.Worktree{taken, race} {
		name := "proj/" + w.Branch
		if p := enter(true, w.ID); p.req != name+" "+w.Root+" mac []" || p.msg != "" || p.end.Kind != view.ActionQuit || !strings.Contains(p.cmds, "@laatmux_attach_target "+name+" ") || !strings.Contains(p.cmds, "switch-client -t =mac/"+name+":") {
			t.Errorf("a name in use, no record elsewhere: %+v", p)
		}
	}
	// The view does not wait on the host: enter returns with the host's
	// answer held back, and the jump ends once it comes.
	m = model(slow.ID)
	os.Remove(log)
	if d.jumpAction(m, view.Action{Kind: view.ActionJump}) || m.Message != "making a session on mac…" {
		t.Errorf("enter while the host holds the answer: message %q", m.Message)
	}
	select {
	case req := <-requests:
		if req.Name != "proj/slow" {
			t.Errorf("asked %+v", req)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the host was not asked")
	}
	select {
	case <-ends:
		t.Fatal("the jump ended before the host answered")
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	select {
	case end := <-ends:
		if a := end(m); a.Kind != view.ActionQuit || m.Message != "made session proj/slow on mac, a shell at /w/slow" {
			t.Errorf("the end once the host answered: %+v, message %q", a, m.Message)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the jump did not end")
	}
	// A user who has moved on meanwhile is left there.
	for what, meanwhile := range map[string]func(m *view.Model){
		"a form":          func(m *view.Model) { m.Overlay = view.NewHelp("help", false) },
		"a question":      func(m *view.Model) { m.Ask("remove proj/b?", "rm") },
		"another session": func(*view.Model) { where = "elsewhere" },
	} {
		p := enterThen(true, b.ID, meanwhile)
		where = "work"
		if p.req != `proj/b /w/b mac []` || p.msg != "made session proj/b on mac, a shell at /w/b; enter on the line goes there" || p.end.Kind != view.ActionNone || p.cmds != "" {
			t.Errorf("moved on to %s: %+v", what, p)
		}
	}
	if p := enterThen(true, taken.ID, func(*view.Model) { where = "elsewhere" }); p.msg != "session proj/taken on mac is there; enter on the line goes there" || p.cmds != "" {
		t.Errorf("moved on, a name in use: %+v", p)
	}
	where = "work"
	for _, k := range []struct {
		w    protocol.Worktree
		name string
		in   string
	}{
		{clone, "proj/clone", "/w2/clone"},
		{lost, "proj/lost", "/w2/lost"},
		{pn, "proj/pn", "/w2/pn/src"},
		{late, "proj/late", "/w2/late"},
	} {
		want := "mac: session " + k.name + " runs in " + k.in + ", not " + k.w.Root + "; name in use"
		if p := enter(true, k.w.ID); p.req != k.name+" "+k.w.Root+` mac []` || p.msg != want || p.end.Kind != view.ActionNone || p.cmds != "" {
			t.Errorf("a name in use elsewhere: %+v, want %q", p, want)
		}
	}
	// A switch that fails, outside tmux, says what was made first.
	t.Setenv("TMUX", "")
	if p := enter(true, b.ID); !strings.HasPrefix(p.msg, "made session proj/b on mac, a shell at /w/b; mac/proj/b is on the default tmux server; attach with: ") || p.end.Kind != view.ActionNone {
		t.Errorf("a switch that fails: %+v", p)
	}
	t.Setenv("TMUX", "/tmp/lmx-fake/default,1,0")
	for _, k := range []struct {
		w    protocol.Worktree
		hint string
	}{
		{det, "/w/det on mac has no managed session; laatmux add makes one once a branch is checked out in /w/det"},
		{onVM, "vm/proj/b has no managed session; laatmux add b --repo proj --host vm --agent claude makes one"},
		{onBox, "box/proj/b has no managed session; laatmux add makes one once host box has repos and worktrees directories in the config"},
	} {
		if p := enter(true, k.w.ID); p.waiting != k.hint || p.msg != k.hint || p.cmds != "" || p.req != "" {
			t.Errorf("enter on %s: %+v", k.w.ID, p)
		}
		if msg, cmds, req := z(k.w.ID); !strings.HasSuffix(msg, ": no local workspace session; "+k.hint) || cmds != "" || req != "" {
			t.Errorf("z on %s: message %q, tmux %q, asked %q", k.w.ID, msg, cmds, req)
		}
	}
	// The stream has no listing of the worktree's machine by the
	// answer: the host down, already by the look at the client at enter
	// or only by the answer; listing again after its entry changed; or
	// the entry reaching another machine. The records shellable read
	// place the session elsewhere.
	relisting, moved := snap, snap
	relisting.Hosts, moved.Hosts = slices.Clone(snap.Hosts), slices.Clone(snap.Hosts)
	relisting.Hosts[0].Listed = false
	relisting.Worktrees, relisting.Agents, relisting.Panes = nil, nil, nil
	moved.Hosts[0].EnvironmentID = "menv2"
	for _, c := range []struct {
		name   string
		answer *protocol.Message
		look   bool
	}{
		{"down by the look", nil, true},
		{"down by the answer", &down, false},
		{"listing again", &relisting, false},
		{"another machine", &moved, false},
	} {
		dcAnswer.Store(c.answer)
		if c.look {
			d.clientAt = func(context.Context) string {
				d.st.Apply(down)
				return where
			}
		}
		if p := enter(true, dc.ID); p.req != `proj/dc /w/dc mac []` || p.msg != "mac: session proj/dc runs in /w2/dc, not /w/dc; name in use" || p.cmds != "" {
			t.Errorf("%s: %+v", c.name, p)
		}
		d.st.Apply(snap)
		d.clientAt = func(context.Context) string { return where }
	}
	// A daemon that answers as another machine than the records', the
	// host entry moved since: nothing is asked of it.
	requests = fakeNew(t, "other", []string{protocol.CapStatus, protocol.CapNew}, nil, nil)
	if p := enter(true, b.ID); p.msg != "mac: new proj/b: answers as environment other, not menv the request was resolved for" || p.cmds != "" || p.req != "" || p.end.Kind != view.ActionNone {
		t.Errorf("another machine: %+v", p)
	}
	// A daemon whose hello lacks new, the cached capabilities
	// notwithstanding: an older build answering since.
	requests = fakeNew(t, "menv", []string{protocol.CapStatus}, nil, nil)
	if p := enter(true, b.ID); p.msg != "mac/proj/b has no managed session; laatmux add b --repo proj --host mac --agent claude makes one" || p.cmds != "" || p.req != "" || p.end.Kind != view.ActionNone {
		t.Errorf("no new in the hello: %+v", p)
	}
}

// Enter on a main checkout's line with no home and no agent makes it a
// session with a shell at its root as enter makes one for a worktree,
// named as add would name a worktree's on the branch, and the workspace
// session keyed by its root; z says enter does, with no add line. S on
// it, and on a worktree with no home and no agent, makes the session
// the same way, then opens the shell window at the root in the
// workspace session and switches there; a user who has moved on is
// left there, the message saying S goes there. A jump or an S while one
// waits on the host is refused with its message. On a host whose cached
// capabilities, or whose hello, lack new, enter keeps its refusal, that
// no agent runs in the checkout, and S its own, that the row is no
// workspace.
func TestShellMakesSession(t *testing.T) {
	log := fakeDefaultTmux(t)
	src := "git@github.com:laat/proj.git"
	b := protocol.Worktree{ID: "menv/worktree//w/b", EnvironmentID: "menv", Repo: "proj", Source: src, Branch: "b", Root: "/w/b"}
	main := protocol.Worktree{ID: "menv/checkout//r/proj", EnvironmentID: "menv", Repo: "proj", Source: src, Branch: "main", Root: "/r/proj", Main: true}
	onVM, mainVM := b, main
	onVM.ID, onVM.EnvironmentID, mainVM.ID, mainVM.EnvironmentID = "venv/worktree//w/b", "venv", "venv/checkout//r/proj", "venv"
	// A worktree and another clone's main checkout whose managed sessions
	// are gone, their workspace sessions left.
	c, clone := b, main
	c.ID, c.Branch, c.Root, clone.ID, clone.Branch, clone.Root = "menv/worktree//w/c", "c", "/w/c", "menv/checkout//r/proj2", "dev", "/r/proj2"
	caps := []string{protocol.CapStatus, protocol.CapWorktrees, protocol.CapAttribution, protocol.CapCheckouts, protocol.CapNew}
	snap := protocol.Message{Type: protocol.TypeSnapshot, Hosts: []protocol.HostStatus{
		{Name: "mac", EnvironmentID: "menv", Connected: true, Listed: true, Capabilities: caps},
		{Name: "vm", SSH: "vm", EnvironmentID: "venv", Connected: true, Listed: true, Capabilities: caps[:4]},
	}, Worktrees: []protocol.Worktree{b, main, onVM, mainVM, c, clone}}
	release := make(chan struct{})
	requests := fakeNew(t, "menv", []string{protocol.CapStatus, protocol.CapNew}, nil, map[string]func() bool{"proj/b": func() bool { <-release; return false }})
	ends := make(chan func(*view.Model) view.Action, 1)
	where := "work"
	d := &dash{ctx: context.Background(), cfg: dashConfig(t), st: merged.New(), cmds: ends, clientAt: func(context.Context) string { return where }}
	d.st.Apply(snap)
	in := rows.Input{Hosts: []rows.Host{
		{Name: "mac", Local: true, EnvironmentID: "menv", Connected: true, Listed: true, Worktrees: true, Attribution: true},
		{Name: "vm", EnvironmentID: "venv", Connected: true, Listed: true, Worktrees: true, Attribution: true},
	}, Worktrees: snap.Worktrees, Locals: []protocol.Session{
		{Name: "mac/proj/c", Key: "menv//w/c", Host: "mac", Source: src, Branch: "c"},
		{Name: "mac/proj/old", Key: "menv//r/proj2", Host: "mac", Source: src, Branch: "old"},
	}}
	model := func(id string) *view.Model {
		t.Helper()
		m := &view.Model{Width: 100, Height: 40, ShowHidden: true, View: view.ViewTree}
		m.SetTree(rows.Tree(in))
		m.SetRows(rows.Agents(in, rows.Tree(in)))
		m.Render()
		if !m.Select(id) {
			t.Fatalf("no line %s", id)
		}
		return m
	}
	type pressed struct {
		waiting, msg, cmds, req string
		end                     view.Action
	}
	// pressThen is the key on the line, what the user does meanwhile,
	// and the end of what waits on the host, as the view runs it.
	pressThen := func(key rune, id string, meanwhile func()) pressed {
		t.Helper()
		m := model(id)
		os.Remove(log)
		d.exitOnJump = true
		if key == '\r' {
			d.jumpAction(m, view.Action{Kind: view.ActionJump})
		} else {
			d.act(m, view.Action{Kind: view.ActionOther, Key: term.Key{Rune: key}})
		}
		var p pressed
		p.waiting = m.Message
		if d.making != "" {
			meanwhile()
			select {
			case end := <-ends:
				p.end = end(m)
			case <-time.After(10 * time.Second):
				t.Fatalf("%c on %s: it did not end", key, id)
			}
		}
		got, _ := os.ReadFile(log)
		p.msg, p.cmds, p.req = m.Message, string(got), asked(requests)
		return p
	}
	press := func(key rune, id string) pressed {
		t.Helper()
		return pressThen(key, id, func() {})
	}
	// Enter and S on the main line, S on the worktree's.
	for _, k := range []struct {
		key        rune
		id         string
		req, made  string
		managed    string
		root       string
		shellOpens bool
	}{
		{'\r', main.ID, `proj/main /r/proj mac []`, "made session proj/main on mac, a shell at /r/proj", "proj/main", "/r/proj", false},
		{'S', main.ID, `proj/main /r/proj mac []`, "made session proj/main on mac, a shell at /r/proj", "proj/main", "/r/proj", true},
	} {
		p := press(k.key, k.id)
		if p.waiting != "making a session on mac…" || p.req != k.req || p.msg != k.made || p.end.Kind != view.ActionQuit {
			t.Errorf("%c on %s: %+v", k.key, k.id, p)
		}
		wants := []string{"new-session -d -s mac/" + k.managed + " ", "@laatmux_attach_target " + k.managed + " ", "@laatmux_workspace menv/" + k.root + " ", "switch-client -t =mac/" + k.managed + ":"}
		if k.shellOpens {
			wants = append(wants, "new-window -t =mac/"+k.managed+": -n shell -c "+k.root)
		} else if strings.Contains(p.cmds, "new-window") {
			t.Errorf("%c on %s opened a shell window: %q", k.key, k.id, p.cmds)
		}
		for _, want := range wants {
			if !strings.Contains(p.cmds, want) {
				t.Errorf("%c on %s ran %q, want %q in it", k.key, k.id, p.cmds, want)
			}
		}
	}
	// S where the line has a workspace session at the root, its managed
	// session gone, on a worktree and on a main checkout: the session is
	// made first, as enter makes it, the workspace session for the root
	// attached to it, and the shell window opened there. In production
	// Ensure finds the session the line had by the root's key and takes
	// it up; the fake tmux lists none, so Ensure makes one.
	for _, k := range []struct{ id, ws, req, managed, root string }{
		{c.ID, "mac/proj/c", `proj/c /w/c mac []`, "proj/c", "/w/c"},
		{clone.ID, "mac/proj/dev", `proj/dev /r/proj2 mac []`, "proj/dev", "/r/proj2"},
	} {
		p := press('S', k.id)
		if p.req != k.req || p.end.Kind != view.ActionQuit {
			t.Errorf("S on %s with a workspace session left: %+v", k.id, p)
		}
		for _, want := range []string{"@laatmux_attach_target " + k.managed + " ", "new-window -t =" + k.ws + ": -n shell -c " + k.root, "switch-client -t =" + k.ws + ":"} {
			if !strings.Contains(p.cmds, want) {
				t.Errorf("S on %s ran %q, want %q in it", k.id, p.cmds, want)
			}
		}
	}
	// S on the worktree's line, the host holding its answer back: a jump
	// and an S meanwhile are refused with the message.
	m := model(b.ID)
	d.act(m, view.Action{Kind: view.ActionOther, Key: term.Key{Rune: 'S'}})
	for _, other := range []func(*view.Model){
		func(o *view.Model) { d.jumpAction(o, view.Action{Kind: view.ActionJump}) },
		func(o *view.Model) { d.act(o, view.Action{Kind: view.ActionOther, Key: term.Key{Rune: 'S'}}) },
	} {
		o := model(main.ID)
		if other(o); o.Message != "making a session on mac…" {
			t.Errorf("meanwhile: %q", o.Message)
		}
	}
	os.Remove(log)
	close(release)
	select {
	case end := <-ends:
		if a := end(m); a.Kind != view.ActionQuit || m.Message != "made session proj/b on mac, a shell at /w/b" {
			t.Errorf("S on the worktree: %+v, message %q", a, m.Message)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("S on the worktree did not end")
	}
	got, _ := os.ReadFile(log)
	for _, want := range []string{"new-session -d -s mac/proj/b ", "@laatmux_attach_target proj/b ", "new-window -t =mac/proj/b: -n shell -c /w/b", "switch-client -t =mac/proj/b:"} {
		if !strings.Contains(string(got), want) {
			t.Errorf("S on the worktree ran %q, want %q in it", got, want)
		}
	}
	if req := asked(requests); req != `proj/b /w/b mac []` || asked(requests) != "" {
		t.Errorf("S on the worktree asked %q", req)
	}
	// Moved on meanwhile: left there.
	if p := pressThen('S', main.ID, func() { where = "elsewhere" }); p.msg != "made session proj/main on mac, a shell at /r/proj; S on the line opens the shell there" || p.cmds != "" || p.end.Kind != view.ActionNone {
		t.Errorf("S, moved on: %+v", p)
	}
	where = "work"
	m = model(main.ID)
	d.settle(m)
	if m.Message != "proj/main: no local workspace session; enter creates one with a shell" {
		t.Errorf("z on the main line: %q", m.Message)
	}
	// A host whose cached capabilities lack new.
	for _, k := range []struct {
		key  rune
		id   string
		want string
	}{
		{'\r', mainVM.ID, "vm/proj/main is the main checkout, and no agent runs in it"},
		{'S', mainVM.ID, "vm/proj/main is the main checkout, which has no workspace session"},
		{'S', onVM.ID, "proj/b: not a workspace"},
	} {
		if p := press(k.key, k.id); p.waiting != k.want || p.msg != k.want || p.cmds != "" || p.req != "" {
			t.Errorf("%c on %s: %+v, want %q", k.key, k.id, p, k.want)
		}
	}
	m = model(mainVM.ID)
	d.settle(m)
	if m.Message != "proj/main: no local workspace session; vm/proj/main is the main checkout, and no agent runs in it" {
		t.Errorf("z on the main line on vm: %q", m.Message)
	}
	// A hello that lacks new, the cached capabilities notwithstanding.
	requests = fakeNew(t, "menv", []string{protocol.CapStatus}, nil, nil)
	for _, k := range []struct {
		key  rune
		id   string
		want string
	}{
		{'\r', main.ID, "mac/proj/main is the main checkout, and no agent runs in it"},
		{'S', main.ID, "mac/proj/main is the main checkout, which has no workspace session"},
		{'S', b.ID, "proj/b: not a workspace"},
	} {
		if p := press(k.key, k.id); p.msg != k.want || p.cmds != "" || p.req != "" || p.end.Kind != view.ActionNone {
			t.Errorf("%c on %s with no new in the hello: %+v, want %q", k.key, k.id, p, k.want)
		}
	}
}

// A main checkout whose home a split gone elsewhere took, with the agent
// laatmux made at its root still its own and an agent of no checkout in
// a split of that session: enter on the line, while the root agent is
// the most recently active, attaches the workspace session named and
// keyed after the checkout to the root agent's session, and asks the
// host for nothing; enter on the other agent's row lands in that
// workspace session, as does S on it, which opens the shell window at
// the root there. With an agent in a plain session the most recently
// active, enter on the line goes to its session, and the other agent's
// row still lands in the checkout's workspace session.
func TestMainCheckoutLostHome(t *testing.T) {
	log := fakeDefaultTmux(t)
	src := "git@github.com:laat/proj.git"
	main := protocol.Worktree{ID: "menv/checkout//r/proj", EnvironmentID: "menv", Repo: "proj", Source: src, Branch: "main", Root: "/r/proj", Main: true}
	now := time.Now()
	made := protocol.Agent{ID: "menv/laatmux/%5", EnvironmentID: "menv", Server: "laatmux", Session: "proj/main", PaneID: "%5", Agent: "codex", Managed: true, Cwd: main.Root,
		WorktreeID: main.ID, Activity: protocol.Working, ActivityAt: now, Liveness: protocol.Alive, Identity: &protocol.Identity{PID: 5, StartUnix: 5}}
	stray := protocol.Agent{ID: "menv/laatmux/%6", EnvironmentID: "menv", Server: "laatmux", Session: "proj/main", PaneID: "%6", Agent: "claude", Cwd: "/tmp",
		Activity: protocol.Idle, ActivityAt: now, Liveness: protocol.Alive, Identity: &protocol.Identity{PID: 6, StartUnix: 6}}
	plain := protocol.Agent{ID: "menv/default/%2", EnvironmentID: "menv", Server: "default", Session: "notes", PaneID: "%2", Agent: "claude",
		WorktreeID: main.ID, Activity: protocol.Idle, ActivityAt: now.Add(-time.Hour), Liveness: protocol.Alive, Identity: &protocol.Identity{PID: 2, StartUnix: 2}}
	caps := []string{protocol.CapStatus, protocol.CapWorktrees, protocol.CapAttribution, protocol.CapCheckouts, protocol.CapNew}
	snap := protocol.Message{Type: protocol.TypeSnapshot, Hosts: []protocol.HostStatus{{Name: "mac", EnvironmentID: "menv", Connected: true, Listed: true, Capabilities: caps}},
		Worktrees: []protocol.Worktree{main}, Agents: []protocol.Agent{made, stray, plain}}
	requests := fakeNew(t, "menv", []string{protocol.CapStatus, protocol.CapNew}, nil, nil)
	d := &dash{ctx: context.Background(), cfg: dashConfig(t), st: merged.New(), cmds: make(chan func(*view.Model) view.Action, 1), clientAt: func(context.Context) string { return "work" }}
	d.st.Apply(snap)
	in := rows.Input{Hosts: []rows.Host{{Name: "mac", Local: true, EnvironmentID: "menv", Connected: true, Listed: true, Worktrees: true, Attribution: true}},
		Worktrees: snap.Worktrees, Agents: snap.Agents}
	press := func(key rune, id string) (msg, cmds string) {
		t.Helper()
		m := &view.Model{Width: 100, Height: 40, ShowHidden: true, View: view.ViewTree}
		m.SetTree(rows.Tree(in))
		m.SetRows(rows.Agents(in, rows.Tree(in)))
		m.Render()
		if !m.Select(id) {
			t.Fatalf("no row %s", id)
		}
		os.Remove(log)
		if key == '\r' {
			d.jumpAction(m, view.Action{Kind: view.ActionJump})
		} else {
			d.act(m, view.Action{Kind: view.ActionOther, Key: term.Key{Rune: key}})
		}
		got, _ := os.ReadFile(log)
		return m.Message, string(got)
	}
	attached := []string{"new-session -d -s mac/proj/main ", "@laatmux_workspace menv//r/proj ", "@laatmux_attach_target proj/main ", "switch-client -t =mac/proj/main:"}
	for _, k := range []struct {
		key  rune
		id   string
		want []string
	}{
		{'\r', main.ID, attached},
		{'\r', stray.ID, attached},
		{'S', stray.ID, append(slices.Clone(attached), "new-window -t =mac/proj/main: -n shell -c /r/proj")},
	} {
		msg, cmds := press(k.key, k.id)
		for _, want := range k.want {
			if !strings.Contains(cmds, want) {
				t.Errorf("%c on %s: message %q, ran %q, want %q in it", k.key, k.id, msg, cmds, want)
			}
		}
		if d.making != "" || asked(requests) != "" {
			t.Errorf("%c on %s asked the host for a session", k.key, k.id)
		}
	}
	// The agent in the plain session the most recently active.
	in.Agents[0].Activity, in.Agents[2].Activity = protocol.Idle, protocol.Working
	if msg, cmds := press('\r', main.ID); !strings.Contains(cmds, "switch-client -t =notes:") || strings.Contains(cmds, "new-session") {
		t.Errorf("enter on the line, the plain session's agent working: message %q, ran %q", msg, cmds)
	}
	if msg, cmds := press('\r', stray.ID); !strings.Contains(cmds, "@laatmux_workspace menv//r/proj ") || !strings.Contains(cmds, "@laatmux_attach_target proj/main ") || !strings.Contains(cmds, "switch-client -t =mac/proj/main:") {
		t.Errorf("enter on the other agent, the plain session's agent working: message %q, ran %q", msg, cmds)
	}
	// The home named otherwise, by hand or on another branch: the root
	// agent's session is the line's all the same, and either agent's row
	// lands in the workspace session keyed by the root.
	in.Agents[0].Session, in.Agents[1].Session = "scratch", "scratch"
	for _, id := range []string{made.ID, stray.ID} {
		msg, cmds := press('\r', id)
		for _, want := range []string{"@laatmux_workspace menv//r/proj ", "@laatmux_attach_target scratch ", "switch-client -t =mac/proj/main:"} {
			if !strings.Contains(cmds, want) {
				t.Errorf("enter on %s, the home named scratch: message %q, ran %q, want %q in it", id, msg, cmds, want)
			}
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

// noteList is a Reporter that keeps the notes.
type noteList []string

func (*noteList) Progress(protocol.Message) {}
func (n *noteList) Note(s string)           { *n = append(*n, s) }

// An rm and an add whose local listing a user's after-list-sessions
// hook failed after: the rm kills the workspace session the listing
// has, and the add makes its session, each with the hook's error as a
// note through the Reporter, not as a failure.
func TestRmAddHookFails(t *testing.T) {
	isolatedDefault(t)
	startFakeDaemon(t, []string{protocol.CapStatus, protocol.CapRm, protocol.CapAdd, protocol.CapFollow}, func(pc *protocol.Conn, m protocol.Message) bool {
		switch m.Type {
		case protocol.TypeRm:
			pc.Write(protocol.Message{Type: protocol.TypeResult, ID: m.ID, OK: true, Root: "/w/proj/task"})
		case protocol.TypeAdd:
			pc.Write(protocol.Message{Type: protocol.TypeResult, ID: m.ID, OK: true, Root: "/w/proj/x", Session: "proj/x"})
		}
		return true
	})
	ctx := context.Background()
	if _, err := workspace.Server.Run(ctx, "new-session", "-d", "-s", "lab/proj/task",
		tmux.Next, "set-option", "-t", "=lab/proj/task:", "@laatmux_workspace", protocol.SessionKey("lenv", "/w/proj/task"),
		tmux.Next, "set-hook", "-g", "after-list-sessions", "select-window -t nosuch:9"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { workspace.Server.Run(context.Background(), "set-hook", "-gu", "after-list-sessions") })
	hooked := func(notes noteList) bool {
		n := 0
		for _, s := range notes {
			if strings.HasPrefix(s, "tmux list-sessions ") && strings.HasSuffix(s, "(after the listing printed its records: a hook's error)") {
				n++
			}
		}
		return n == 1
	}
	var notes noteList
	rm := command.Rm{Host: config.Host{Host: peer.Host{Name: "lab"}}, Root: "/w/proj/task"}
	if res, err := rm.Run(ctx, &notes); err != nil || len(res.Killed) != 1 || res.Killed[0] != "lab/proj/task" || !hooked(notes) {
		t.Errorf("rm: %+v %v, notes %q; want lab/proj/task killed and the hook's error noted", res, err, notes)
	}
	notes = nil
	add := command.Add{Host: config.Host{Host: peer.Host{Name: "lab"}}, Repo: config.Repo{Source: "git@x:o/proj.git", Name: "proj"}, Branch: "x", Agent: "claude"}
	if res, err := add.Run(ctx, &notes); err != nil || res.Session != "lab/proj/x" || !res.Created || !hooked(notes) {
		t.Errorf("add: %+v %v, notes %q; want lab/proj/x made and the hook's error noted", res, err, notes)
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
	want := command.Rm{Host: config.Host{Host: peer.Host{Name: "vm", SSH: "vm"}, Repos: config.Paths{"/r"}, Worktrees: "/w"},
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

// x and X refuse a main checkout, from its line in the tree and from
// its agent's tile, with no question asked; a preselects its repository
// and host, not its branch. With its agent in a plain session and no
// workspace session, z says enter goes to the agent's session, with no
// add line, and S that it has no workspace session; with one at its
// root, left from a worktree there before or made by a jump, z settles
// it and S opens the shell window in it, as on a worktree's line.
func TestMainCheckoutRefused(t *testing.T) {
	log := fakeDefaultTmux(t)
	cfg := dashConfig(t)
	d := &dash{ctx: context.Background(), cfg: cfg, st: merged.New()}
	w := protocol.Worktree{ID: "menv/checkout//r/proj", EnvironmentID: "menv", Repo: "proj", Source: "git@github.com:laat/proj.git", Branch: "main", Root: "/r/proj", Main: true}
	in := rows.Input{
		Hosts: []rows.Host{{Name: "mac", Local: true, EnvironmentID: "menv", Connected: true, Listed: true, Worktrees: true, Attribution: true}},
		Agents: []protocol.Agent{{ID: "menv/default/%1", EnvironmentID: "menv", Server: "default", Session: "work", Agent: "claude",
			Activity: protocol.Working, Liveness: protocol.Alive, WorktreeID: w.ID}},
		Worktrees: []protocol.Worktree{w},
	}
	m := &view.Model{Width: 80, Height: 20}
	show := func() {
		m.SetTree(rows.Tree(in))
		m.SetRows(rows.Agents(in, rows.Tree(in)))
		m.Render()
	}
	press := func(r rune) string {
		t.Helper()
		m.Message, m.Confirm = "", ""
		os.Remove(log)
		d.act(m, view.Action{Kind: view.ActionOther, Key: term.Key{Rune: r}})
		got, _ := os.ReadFile(log)
		return string(got)
	}
	for _, ws := range []bool{false, true} {
		if ws {
			in.Locals = []protocol.Session{{Name: "mac/proj/old", Key: "menv//r/proj", Host: "mac"}}
		}
		show()
		for _, c := range []struct {
			show  func() bool
			kind  rows.Kind
			enter string
		}{
			{func() bool { m.View = view.ViewAgents; return m.Select("menv/default/%1") }, rows.KindTile, "enter on the line"},
			{func() bool { treeView(m); return m.Select(w.ID) }, rows.KindWorktree, "enter"},
		} {
			if !c.show() || m.Selection().Kind != c.kind {
				t.Fatalf("selection %+v, want a %v", m.Selection(), c.kind)
			}
			for _, r := range []rune{'x', 'X'} {
				press(r)
				if m.Confirm != "" || m.Message != "mac/proj/main is the main checkout; x removes worktrees" {
					t.Errorf("%c on %v: confirm=%q message=%q", r, m.Selection().Kind, m.Confirm, m.Message)
				}
			}
			z, s := press('z'), press('S')
			switch {
			case !ws && (z != "" || s != ""):
				t.Errorf("on %v with no workspace session: tmux ran %q, %q", c.kind, z, s)
			case ws && (!strings.Contains(z, "set-option -t =mac/proj/old: @laatmux_settled 1") || !strings.Contains(s, "new-window -t =mac/proj/old: -n shell -c /r/proj") || !strings.Contains(s, "switch-client -t =mac/proj/old:")):
				t.Errorf("on %v with a workspace session at its root: z ran %q, S ran %q", c.kind, z, s)
			}
			if !ws {
				press('z')
				if want := "proj/main: no local workspace session; " + c.enter + " jumps to work, its agent's session"; m.Message != want {
					t.Errorf("z on %v: %q, want %q", c.kind, m.Message, want)
				}
				press('S')
				if m.Message != "mac/proj/main is the main checkout, which has no workspace session" {
					t.Errorf("S on %v: %q", c.kind, m.Message)
				}
			}
			// a preselects the repository and host, not the branch, which
			// git keeps checked out in the checkout.
			press('a')
			f, ok := m.Overlay.(*view.Form)
			if !ok || f.Chips[0].Label() != "proj" || f.Chips[1].Label() != "mac" || f.Branch() != "" {
				t.Errorf("a on %v: form %+v, message %q", m.Selection().Kind, m.Overlay, m.Message)
			}
			m.Overlay, d.add = nil, nil
		}
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

// A worktree whose branch the host only shows (#304) is removed from the
// dashboard by its root, with the branch as shown, which the daemon
// takes for that root; a on its line leaves the form's branch for the
// user: add could not name that branch. The form refuses a branch that
// is not UTF-8 as it is typed, before anything is sent.
func TestShownBranchFromTheDashboard(t *testing.T) {
	t.Setenv("LAATMUX_HOME", t.TempDir())
	cfg := dashConfig(t)
	d := &dash{ctx: context.Background(), cfg: cfg, st: merged.New()}
	m := &view.Model{Width: 80, Height: 20, ShowHidden: true, View: view.ViewTree}
	in := rows.Input{
		Hosts: []rows.Host{{Name: "vm", EnvironmentID: "venv", Connected: true, Listed: true, Worktrees: true}},
		Worktrees: []protocol.Worktree{
			{ID: "venv/worktree//w/proj/hand", EnvironmentID: "venv", Repo: "proj", Source: "git@github.com:laat/proj.git", Branch: `"a\xffb"`, BranchDisplayOnly: true, Root: "/w/proj/hand"},
		},
	}
	m.SetTree(rows.Tree(in))
	m.SetRows(rows.Agents(in, rows.Tree(in)))
	m.Render()
	selectRow(t, m, `proj/"a\xffb"`)
	d.act(m, view.Action{Kind: view.ActionOther, Key: term.Key{Rune: 'x'}})
	if d.rm.Host.Name != "vm" || d.rm.Repo.Source != cfg.Repos[1].Source || d.rm.Branch != `"a\xffb"` || d.rm.Root != "/w/proj/hand" {
		t.Errorf("rm = %+v", d.rm)
	}
	if m.Confirm != `remove proj/"a\xffb" on vm (/w/proj/hand)? y/n` {
		t.Errorf("confirm = %q", m.Confirm)
	}
	m.Handle(term.Key{Rune: 'n'})
	d.act(m, view.Action{Kind: view.ActionOther, Key: term.Key{Rune: 'a'}})
	f, ok := m.Overlay.(*view.Form)
	if !ok || f.Chips[0].Label() != "proj" || f.Chips[1].Label() != "vm" || f.Branch() != "" {
		t.Fatalf("form: %+v", m.Overlay)
	}
	if err := f.Validate("a\xffb"); err == nil || !strings.Contains(err.Error(), "not valid UTF-8") {
		t.Errorf("form validate: %v", err)
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
// The form opened from a workspace session preselects the session's
// repository and host: the host when the form offers it, the
// repository by its source whatever this machine names it; nothing
// outside a workspace or for a session whose tags do not say.
func TestPresetForWorkspace(t *testing.T) {
	hosts := []config.Host{{Host: peer.Host{Name: "mac"}}, {Host: peer.Host{Name: "vm", SSH: "vm"}}}
	for _, c := range []struct {
		name       string
		s          protocol.Session
		repo, host string
	}{
		{"a workspace on vm", protocol.Session{Name: "vm/proj/x", Key: "venv//w/x", Host: "vm", Source: "git@x:o/proj.git", Branch: "x"}, "git@x:o/proj.git", "vm"},
		{"a host the form does not offer", protocol.Session{Name: "box/proj/x", Key: "benv//w/x", Host: "box", Source: "git@x:o/proj.git"}, "git@x:o/proj.git", ""},
		{"tags without a source", protocol.Session{Name: "vm/proj/x", Key: "venv//w/x", Host: "vm"}, "", "vm"},
		{"a plain session", protocol.Session{Name: "work", Host: "vm", Source: "git@x:o/proj.git"}, "", ""},
	} {
		if repo, host := presetFor(c.s, hosts); repo != c.repo || host != c.host {
			t.Errorf("%s: %q %q, want %q %q", c.name, repo, host, c.repo, c.host)
		}
	}
	cfg := config.Config{
		Hosts:  hosts,
		Repos:  []config.Repo{{Source: "git@x:o/proj.git", Name: "proj"}, {Source: "git@x:o/other.git", Name: "other"}},
		Agents: map[string]config.Agent{"claude": {Cmd: []string{"claude"}}},
	}
	f := &addForm{repos: cfg.Repos, hosts: cfg.Hosts, agents: cfg.AgentNames()}
	var last home.Last
	last.Set("git@x:o/proj.git", home.LastRepo{Host: "mac", Agent: "claude"})
	// The session's source selects the repository, the session's host
	// wins over the last used one.
	repo, host := presetFor(protocol.Session{Name: "vm/proj/x", Key: "venv//w/x", Host: "vm", Source: "git@x:o/proj.git"}, hosts)
	form := buildForm(cfg, f, last, repo, host, "", nil)
	if form.Chips[0].Label() != "proj" || form.Chips[1].Label() != "vm" {
		t.Errorf("chips %q %q, want proj vm", form.Chips[0].Label(), form.Chips[1].Label())
	}
}

func TestBuildForm(t *testing.T) {
	cfg := config.Config{
		Hosts:  []config.Host{{Host: peer.Host{Name: "mac"}, Repos: config.Paths{"/r"}, Worktrees: "/w"}, {Host: peer.Host{Name: "vm", SSH: "vm"}, Repos: config.Paths{"/r"}, Worktrees: "/w"}},
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

// x's question on a worktree and on a task, and the message a dismiss
// ends with, name a branch with a C1 control character, which git
// takes, by its <repo>/<branch> as tmux.Printable shows it.
func TestConfirmsQuoteBranch(t *testing.T) {
	t.Setenv("LAATMUX_HOME", t.TempDir())
	cfg := dashConfig(t)
	wb, pb := "w\u009b31m", "p\u009b31m"
	in := rows.Input{
		Hosts:     []rows.Host{{Name: "vm", EnvironmentID: "venv", Connected: true, Listed: true, Worktrees: true}},
		Worktrees: []protocol.Worktree{{ID: "venv/worktree//w/proj/w", EnvironmentID: "venv", Repo: "proj", Source: "git@github.com:laat/proj.git", Branch: wb, Root: "/w/proj/w"}},
		Pendings: []protocol.Pending{{ID: "add-1", Host: "vm", EnvironmentID: "venv", Repo: "proj", Branch: pb, Root: "/w/proj/p",
			Taken: true, Reachable: true, Done: true, OK: true, Prompt: protocol.DeliveryNotDelivered, Error: "not ready", SubmittedAt: time.Now()}},
	}
	m := &view.Model{Width: 80, Height: 20}
	m.SetTree(rows.Tree(in))
	m.SetRows(rows.Agents(in, rows.Tree(in)))
	treeView(m)
	d := &dash{ctx: context.Background(), cfg: cfg, st: merged.New(), dismiss: func(string) error { return nil }}

	selectRow(t, m, "proj/"+wb)
	d.act(m, view.Action{Kind: view.ActionOther, Key: term.Key{Rune: 'x'}})
	if want := "remove " + strconv.Quote("proj/"+wb) + " on vm (/w/proj/w)? y/n"; m.Confirm != want {
		t.Errorf("rm: confirm %q, want %q", m.Confirm, want)
	}
	m.Handle(term.Key{Rune: 'n'})

	selectRow(t, m, "proj/"+pb)
	d.act(m, view.Action{Kind: view.ActionOther, Key: term.Key{Rune: 'x'}})
	if want := "dismiss " + strconv.Quote("proj/"+pb) + " on vm (prompt not delivered)? y/n"; m.Confirm != want {
		t.Errorf("dismiss: confirm %q, want %q", m.Confirm, want)
	}
	d.act(m, m.Handle(term.Key{Rune: 'y'}))
	log, ok := m.Overlay.(*view.Log)
	if !ok {
		t.Fatalf("no log: %v", m.Overlay)
	}
	// The dismiss is a stub that returns at once; ten seconds is room
	// for a loaded machine, and a log not done by then is said as such.
	for deadline := time.Now().Add(10 * time.Second); !log.Done() && time.Now().Before(deadline); {
		time.Sleep(5 * time.Millisecond)
	}
	if !log.Done() {
		t.Fatal("the dismiss did not finish")
	}
	d.act(m, m.Poll())
	if want := "dismissed " + strconv.Quote("proj/"+pb) + " on vm"; m.Message != want {
		t.Errorf("dismiss: message %q, want %q", m.Message, want)
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

// The dashboard's keys on the row following takes. vm lists proj/z
// homed in proj/z, and the viewer is in vm/proj/z. The root agent is
// idle, last active ten minutes ago; a `cd ~ && claude` in a split of
// proj/z, a managed agent of no worktree in other sessions, is working,
// active a minute ago. Its tile sorts first, yet following is on the
// root agent's, so o opens proj/z's PR, O its checks, x asks to remove
// proj/z, and a pre-fills its repository and host. With the root agent
// no longer listed, following is on the split's tile, which x says is
// no worktree.
func TestFollowedKeysFindWorktree(t *testing.T) {
	t.Setenv("LAATMUX_HOME", t.TempDir())
	cfg := dashConfig(t)
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	src := "git@github.com:laat/proj.git"
	wt := protocol.Worktree{ID: "venv/worktree//w/proj/z", EnvironmentID: "venv", Repo: "proj", Source: src, Branch: "z", Root: "/w/proj/z", Session: "proj/z"}
	root := protocol.Agent{ID: "venv/laatmux/%1", EnvironmentID: "venv", Server: "laatmux", Session: "proj/z", Agent: "claude", Activity: protocol.Idle, ActivityAt: now.Add(-10 * time.Minute), Liveness: protocol.Alive, Managed: true, Cwd: "/w/proj/z", WorktreeID: wt.ID}
	stray := protocol.Agent{ID: "venv/laatmux/%5", EnvironmentID: "venv", Server: "laatmux", Session: "proj/z", Agent: "claude", Activity: protocol.Working, ActivityAt: now.Add(-time.Minute), Liveness: protocol.Alive, Managed: true, Cwd: "/home/u"}
	key := protocol.BranchKey{Source: source.Key(src), Branch: "z"}
	pr, checks := "https://github.com/laat/proj/pull/7", "https://github.com/laat/proj/commit/abc/checks"
	in := rows.Input{
		Hosts: []rows.Host{
			{Name: "mac", Local: true, EnvironmentID: "menv", Connected: true, Listed: true, Worktrees: true, Attribution: true},
			{Name: "vm", EnvironmentID: "venv", Connected: true, Listed: true, Worktrees: true, Attribution: true},
		},
		Agents:    []protocol.Agent{root, stray},
		Worktrees: []protocol.Worktree{wt},
		Locals:    []protocol.Session{{Name: "vm/proj/z", Key: "venv//w/proj/z", Host: "vm"}},
		Branches:  map[protocol.BranchKey]protocol.BranchStatus{key: {BranchKey: key, ChecksURL: checks, PR: &protocol.PullRequest{Number: 7, State: "open", URL: pr}}},
		Current:   "vm/proj/z",
		Now:       now,
	}
	m := &view.Model{Now: now, Width: 80, Height: 20, View: view.ViewAgents, Follow: true}
	m.Set(rows.Tree(in), rows.Agents(in, rows.Tree(in)), nil)
	m.Render()
	if vis := m.Visible(); len(vis) != 2 || vis[0].Row.ID() != stray.ID || !vis[0].Row.Current {
		t.Fatalf("the split's tile is not first and the viewer's: %+v", vis)
	}
	var opened []string
	defer func(was func(string) error) { openURL = was }(openURL)
	openURL = func(url string) error { opened = append(opened, url); return nil }
	d := &dash{ctx: context.Background(), cfg: cfg, st: merged.New()}
	d.act(m, view.Action{Kind: view.ActionOther, Key: term.Key{Rune: 'o'}})
	d.act(m, view.Action{Kind: view.ActionOther, Key: term.Key{Rune: 'O'}})
	if len(opened) != 2 || opened[0] != pr || opened[1] != checks {
		t.Errorf("o and O: opened %v, message %q", opened, m.Message)
	}
	d.act(m, view.Action{Kind: view.ActionOther, Key: term.Key{Rune: 'x'}})
	if m.Confirm != "remove proj/z on vm (/w/proj/z) with its agent? y/n" {
		t.Errorf("x: confirm %q message %q", m.Confirm, m.Message)
	}
	m.Handle(term.Key{Rune: 'n'})
	d.act(m, view.Action{Kind: view.ActionOther, Key: term.Key{Rune: 'a'}})
	if f, ok := m.Overlay.(*view.Form); !ok || f.Chips[0].Label() != "proj" || f.Chips[1].Label() != "vm" {
		t.Errorf("a: form %+v message %q", m.Overlay, m.Message)
	}
	m.Overlay, d.add = nil, nil
	if !m.Follow {
		t.Fatal("the keys ended following")
	}
	// The root agent no longer listed: the split's tile is the viewer's
	// only one.
	in.Agents = []protocol.Agent{stray}
	m.Set(rows.Tree(in), rows.Agents(in, rows.Tree(in)), nil)
	if r := m.Selection(); r == nil || r.ID() != stray.ID {
		t.Fatalf("only the split's tile: follows %+v", r)
	}
	d.act(m, view.Action{Kind: view.ActionOther, Key: term.Key{Rune: 'x'}})
	if m.Confirm != "" || !strings.Contains(m.Message, "not a worktree") {
		t.Errorf("x on the split's tile: confirm %q message %q", m.Confirm, m.Message)
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
