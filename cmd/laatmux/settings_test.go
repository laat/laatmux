package main

import (
	"context"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/laat/laatmux/internal/config"
	"github.com/laat/laatmux/internal/home"
	"github.com/laat/laatmux/internal/protocol"
	"github.com/laat/laatmux/internal/rows"
	"github.com/laat/laatmux/internal/term"
	"github.com/laat/laatmux/internal/view"
	"github.com/laat/laatmux/internal/workspace"
)

// The settings: a pane starts from the file's view, layout and scope
// over the config's, unless a flag fixed the layout or the strip the
// view; a change writes the view, the layout and the toggled folds,
// never the scope; another pane's write reaches the view through the
// poll; a strip writes no view or layout.
func TestSettings(t *testing.T) {
	t.Setenv("LAATMUX_HOME", t.TempDir())
	cfg := config.Config{}
	now := time.Now()
	if err := home.UpdateSidebar(now, func(s *home.Sidebar) {
		s.View, s.Layout, s.Scope = "tree", "compact", "project"
		s.SetFolds(map[string]bool{"repo/x": true}, nil, nil, now)
	}); err != nil {
		t.Fatal(err)
	}
	m := &view.Model{Layout: view.Tiles, View: view.ViewAgents}
	seen := startSettings(cfg, m, settingsHost{})
	if m.View != view.ViewTree || m.Layout != view.Compact || m.Scope != view.ScopeProject || seen.IsZero() {
		t.Fatalf("start: %s %s %s %v", m.View, m.Layout, m.Scope, seen)
	}
	folds := m.ToggledFolds()
	if len(folds) != 1 || !folds["repo/x"] {
		t.Errorf("folds at start: %v", folds)
	}
	// The strip: its view and layout its own; the dashboard: at all,
	// with keys of its own for the view and layout, compact until
	// chosen otherwise.
	fixed := &view.Model{Layout: view.Tiles, View: view.ViewAgents}
	startSettings(cfg, fixed, settingsHost{fixedLayout: true, fixedView: true, fixedScope: true})
	if fixed.View != view.ViewAgents || fixed.Layout != view.Tiles || fixed.Scope != "" {
		t.Errorf("fixed: %s %s %s", fixed.View, fixed.Layout, fixed.Scope)
	}
	dash := &view.Model{Layout: view.Compact, View: view.ViewAgents}
	startSettings(cfg, dash, settingsHost{dashboard: true, fixedScope: true})
	if dash.View != view.ViewAgents || dash.Layout != view.Compact || dash.Scope != "" {
		t.Errorf("the dashboard from the sidebar's keys: %s %s %s", dash.View, dash.Layout, dash.Scope)
	}
	// The config's scope, with none in the file.
	cfg.Sidebar.Scope = "session"
	os.Remove(home.SidebarPath())
	fromCfg := &view.Model{}
	startSettings(cfg, fromCfg, settingsHost{})
	if fromCfg.Scope != view.ScopeSession {
		t.Errorf("the config's scope: %s", fromCfg.Scope)
	}
	if err := home.UpdateSidebar(now, func(s *home.Sidebar) {
		s.View, s.Layout, s.Scope = "tree", "compact", "project"
		s.SetFolds(map[string]bool{"repo/x": true}, nil, nil, now)
	}); err != nil {
		t.Fatal(err)
	}
	// A change: the folds this pane set written with their sighting,
	// one taken from the file not written back, the scope left as the
	// CLI set it; the view and layout only after Tab and v, never a
	// layout a flag fixed, never a strip's.
	m.View, m.Layout, m.Scope = view.ViewTree, view.Tiles, view.ScopeAll
	m.ApplyFolds(map[string]bool{"repo/x": true, "repo/y": false}) // the file's, whole
	m.Tree = []rows.Row{{Kind: rows.KindRepo, Node: "repo/x", Children: 1}}
	m.Handle(term.Key{Rune: 'f'}) // repo/x was closed from the file: f opens it here
	if err := saveSettings(m, now.Add(time.Minute), settingsHost{}); err != nil {
		t.Fatal(err)
	}
	s, _, _ := home.ReadSidebar()
	if s.View != "tree" || s.Layout != "compact" || s.Scope != "project" || s.Folds["repo/x"].Closed || !s.Folds["repo/x"].Seen.Equal(now.Add(time.Minute)) {
		t.Errorf("saved after a fold: %+v", s)
	}
	if _, ok := s.Folds["repo/y"]; ok {
		t.Errorf("a fold taken from the file written back: %+v", s.Folds)
	}
	// Written once: another pane closes it, and a second save here
	// changes nothing of the folds.
	if err := home.UpdateSidebar(now, func(s *home.Sidebar) { s.SetFolds(map[string]bool{"repo/x": true}, nil, nil, now) }); err != nil {
		t.Fatal(err)
	}
	if err := saveSettings(m, now.Add(2*time.Minute), settingsHost{}); err != nil {
		t.Fatal(err)
	}
	if s, _, _ := home.ReadSidebar(); !s.Folds["repo/x"].Closed {
		t.Errorf("a fold written twice over another pane's change: %+v", s.Folds)
	}
	// Tab writes the view; v the layout, unless a flag fixed it: the
	// file holds compact, v makes the model compact from tiles, and a
	// fixed layout leaves the file's tiles alone once set so.
	if err := home.UpdateSidebar(now, func(s *home.Sidebar) { s.Layout = "tiles" }); err != nil {
		t.Fatal(err)
	}
	m.Handle(term.Key{Kind: term.KeyTab}) // agents
	m.Handle(term.Key{Rune: 'v'})         // compact
	if err := saveSettings(m, now.Add(3*time.Minute), settingsHost{fixedLayout: true}); err != nil {
		t.Fatal(err)
	}
	if s, _, _ := home.ReadSidebar(); s.View != "agents" || s.Layout != "tiles" {
		t.Errorf("after Tab and v with the layout fixed: %+v", s)
	}
	m.Handle(term.Key{Rune: 'v'}) // tiles
	m.Handle(term.Key{Rune: 'v'}) // compact
	if err := saveSettings(m, now.Add(4*time.Minute), settingsHost{}); err != nil {
		t.Fatal(err)
	}
	if s, _, _ := home.ReadSidebar(); s.Layout != "compact" {
		t.Errorf("after v: %+v", s)
	}
	strip := &view.Model{Layout: view.Strip, View: view.ViewAgents}
	strip.Handle(term.Key{Kind: term.KeyTab})
	if err := saveSettings(strip, now.Add(5*time.Minute), settingsHost{fixedView: true, fixedLayout: true}); err != nil {
		t.Fatal(err)
	}
	if s, _, _ := home.ReadSidebar(); s.View != "agents" || s.Layout != "compact" {
		t.Errorf("a strip wrote the defaults: %+v", s)
	}
	// The dashboard's Tab and v write its own keys, not the sidebar's.
	dash.Handle(term.Key{Kind: term.KeyTab})
	dash.Handle(term.Key{Rune: 'v'})
	if err := saveSettings(dash, now.Add(6*time.Minute), settingsHost{dashboard: true}); err != nil {
		t.Fatal(err)
	}
	if s, _, _ := home.ReadSidebar(); s.View != "agents" || s.Layout != "compact" || s.DashboardView != "tree" || s.DashboardLayout != "tiles" {
		t.Errorf("the dashboard's keys: %+v", s)
	}
	dash2 := &view.Model{Layout: view.Compact, View: view.ViewAgents}
	startSettings(cfg, dash2, settingsHost{dashboard: true, fixedScope: true})
	if dash2.View != view.ViewTree || dash2.Layout != view.Tiles {
		t.Errorf("the dashboard from its keys: %s %s", dash2.View, dash2.Layout)
	}
	// touchSettings refreshes the sighting of folds whose nodes the
	// model has, values untouched.
	toucher := &view.Model{View: view.ViewTree, Tree: []rows.Row{{Kind: rows.KindRepo, Node: "repo/x", Children: 1}}}
	toucher.ApplyFolds(map[string]bool{"repo/x": true, "repo/gone": true})
	if err := home.UpdateSidebar(now, func(s *home.Sidebar) {
		s.SetFolds(map[string]bool{"repo/x": true, "repo/gone": true}, nil, nil, now.Add(-2*time.Hour))
	}); err != nil {
		t.Fatal(err)
	}
	if err := touchSettings(toucher, now.Add(8*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if s, _, _ := home.ReadSidebar(); !s.Folds["repo/x"].Seen.Equal(now.Add(8*time.Minute)) || !s.Folds["repo/gone"].Seen.Equal(now.Add(-2*time.Hour)) || !s.Folds["repo/x"].Closed {
		t.Errorf("touched: %+v", s.Folds)
	}
	// A carried fold takes only where the file has none.
	carrier := &view.Model{View: view.ViewTree, Width: 60, Height: 20}
	carrier.Tree = []rows.Row{{Kind: rows.KindRepo, Node: "repo/x", Children: 1}, {Kind: rows.KindTask, Depth: 1, Host: "vm", Children: 1, Pending: &protocol.Pending{ID: "add-z", Host: "vm", EnvironmentID: "venv", Root: "/r/z", Session: "z", Taken: true}}}
	carrier.Select("add-z")
	carrier.Handle(term.Key{Rune: 'h'})
	carrier.DirtyFolds()
	carrier.Handoffs = map[string]string{"add-z": "venv/worktree//r/z"}
	carrier.SetTree([]rows.Row{{Kind: rows.KindRepo, Node: "repo/x", Children: 1}, {Kind: rows.KindWorktree, Depth: 1, Host: "vm", Node: "venv/worktree//r/z", Children: 1, Worktree: &protocol.Worktree{ID: "venv/worktree//r/z"}}})
	if err := home.UpdateSidebar(now, func(s *home.Sidebar) { s.SetFolds(map[string]bool{"venv/worktree//r/z": false}, nil, nil, now) }); err != nil {
		t.Fatal(err)
	}
	if err := saveSettings(carrier, now.Add(7*time.Minute), settingsHost{}); err != nil {
		t.Fatal(err)
	}
	if s, _, _ := home.ReadSidebar(); s.Folds["venv/worktree//r/z"].Closed {
		t.Errorf("a carried fold written over a pane ahead: %+v", s.Folds)
	}
	// Another pane's write reaches the view through the poll.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cmds := make(chan func(*view.Model) view.Action, 1)
	watchSettings(ctx, home.SidebarMtime(), cmds)
	time.Sleep(50 * time.Millisecond)
	// mtime has second resolution on some file systems: a write a
	// second on.
	time.Sleep(1100 * time.Millisecond)
	if err := home.UpdateSidebar(now, func(s *home.Sidebar) { s.SetFolds(map[string]bool{"repo/z": true}, nil, nil, now.Add(time.Hour)) }); err != nil {
		t.Fatal(err)
	}
	select {
	case f := <-cmds:
		f(m)
	case <-time.After(5 * time.Second):
		t.Fatal("no poll after another pane's write")
	}
	if folds := m.ToggledFolds(); !folds["repo/z"] {
		t.Errorf("the other pane's fold not taken: %v", folds)
	}
}

// The socket: a pane listens under the state directory, tagged with
// the path; the CLI finds it by the window and sends next, prev, jump
// with the client, view and scope; view and scope write the default
// whether or not a pane answers; a window with no sidebar, a leftover
// socket and a refused connection exit quietly; --all reaches every
// pane; reap removes the socket of a pane gone.
func TestSidebarControl(t *testing.T) {
	isolatedDefault(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	run := func(args ...string) string {
		t.Helper()
		out, err := workspace.Server.Run(ctx, args...)
		if err != nil {
			t.Fatalf("%v: %v", args, err)
		}
		return strings.TrimSpace(string(out))
	}
	window := run("display", "-p", "-t", "boot", "#{window_id}")
	pane := run("split-window", "-d", "-h", "-t", window, "-P", "-F", "#{pane_id}", "sleep 1000")
	run("set-option", "-p", "-t", pane, sidebarTag, "1")
	t.Setenv("TMUX_PANE", pane)
	got := make(chan view.Command, 8)
	cmds := make(chan func(*view.Model) view.Action, 8)
	stop, err := listenPane(ctx, cmds, func(c view.Command) func(*view.Model) view.Action {
		got <- c
		return func(m *view.Model) view.Action { return view.Action{} }
	})
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	if path := run("display", "-p", "-t", pane, "#{"+socketTag+"}"); !strings.HasPrefix(path, socketDir()) {
		t.Fatalf("the pane's socket tag: %q", path)
	}
	next := func() view.Command {
		t.Helper()
		select {
		case c := <-got:
			<-cmds
			return c
		case <-time.After(5 * time.Second):
			t.Fatal("no command reached the pane")
		}
		return view.Command{}
	}
	for _, c := range []struct {
		name string
		args []string
		want view.Command
	}{
		{"next", []string{"-t", window}, view.Command{Name: "next"}},
		{"prev", []string{"-t", window}, view.Command{Name: "prev"}},
		{"jump", []string{"3", "-t", window, "-c", "/dev/ttys004"}, view.Command{Name: "jump", N: 3, Client: "/dev/ttys004"}},
		{"view", []string{"tree", "-t", window}, view.Command{Name: "view", Arg: "tree"}},
		{"scope", []string{"session", "--all"}, view.Command{Name: "scope", Arg: "session"}},
	} {
		if err := sidebarControl(ctx, c.name, c.args); err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if g := next(); g != c.want {
			t.Errorf("%s: got %+v, want %+v", c.name, g, c.want)
		}
	}
	if s, _, err := home.ReadSidebar(); err != nil || s.View != "tree" || s.Scope != "session" {
		t.Errorf("the defaults written: %+v %v", s, err)
	}
	for _, bad := range [][]string{{"next", "--all"}, {"jump", "x"}, {"view", "tree", "-c", "c"}, {"scope", "me"}} {
		if err := sidebarControl(ctx, bad[0], bad[1:]); err == nil {
			t.Errorf("%v accepted", bad)
		}
	}
	// A window with no sidebar, and a leftover socket of a pane gone:
	// quiet; reap removes the leftover.
	other := run("new-window", "-d", "-t", "boot:", "-P", "-F", "#{window_id}", "sleep 1000")
	if err := sidebarControl(ctx, "next", []string{"-t", other}); err != nil {
		t.Errorf("a window with no sidebar: %v", err)
	}
	pid, _ := serverPID(ctx)
	leftover := socketPath(pid, "%999")
	if err := os.WriteFile(leftover, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	reapSockets(ctx)
	if _, err := os.Stat(leftover); err == nil {
		t.Error("the leftover socket not reaped")
	}
	if _, err := os.Stat(run("display", "-p", "-t", pane, "#{"+socketTag+"}")); err != nil {
		t.Error("the live pane's socket reaped")
	}
	stop()
	if err := sidebarControl(ctx, "next", []string{"-t", window}); err != nil {
		t.Errorf("a socket refusing: %v", err)
	}
	// view and scope write the default whether or not a pane answers:
	// a window with no sidebar, and none listening anywhere.
	if err := sidebarControl(ctx, "view", []string{"agents", "-t", other}); err != nil {
		t.Errorf("view with no sidebar: %v", err)
	}
	if err := sidebarControl(ctx, "scope", []string{"project", "--all"}); err != nil {
		t.Errorf("scope with no pane listening: %v", err)
	}
	if s, _, err := home.ReadSidebar(); err != nil || s.View != "agents" || s.Scope != "project" {
		t.Errorf("the defaults written with no pane answering: %+v %v", s, err)
	}
	// A socket of another server's pane, refusing: reaped by the dial,
	// since the listing cannot say whether its pane is gone.
	foreign := socketPath(pid+100000, "%1")
	if err := os.WriteFile(foreign, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	reapSockets(ctx)
	if _, err := os.Stat(foreign); err == nil {
		t.Error("another server's socket refusing not reaped")
	}
	// Another server's socket listening: not the listing's to judge,
	// kept.
	alive := socketPath(pid+100000, "%998")
	ln, err := net.Listen("unix", alive)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	reapSockets(ctx)
	if _, err := os.Stat(alive); err != nil {
		t.Error("another server's live socket reaped")
	}
	// A command that is no jump leaves no client on the dash; a jump's
	// client is switched by the pane's jump, once.
	d := &dash{ctx: ctx}
	m0 := dashModel(dashConfig(t))
	m0.Ask("Quit sidebar? y/n", "quit")
	d.paneCommand(view.Command{Name: "jump", N: 1, Client: "/dev/ttys004"})(m0)
	if d.client != "" {
		t.Errorf("a jump ignored kept the client %q", d.client)
	}
	m0.Confirm = ""
	if a := d.paneCommand(view.Command{Name: "jump", N: 1, Client: "/dev/ttys004"})(m0); a.Kind != view.ActionJump || d.client != "/dev/ttys004" {
		t.Errorf("a jump's client: %+v %q", a, d.client)
	}
	seen := ""
	d.jumper = func(r rows.Row) error {
		seen, _ = d.ctx.Value(clientKey{}).(string)
		return nil
	}
	m := dashModel(dashConfig(t))
	d.st, d.cfg = newMerged(), dashConfig(t)
	d.jumpAction(m, view.Action{Kind: view.ActionJump, Row: m.Selection()})
	if seen != "/dev/ttys004" || d.client != "" {
		t.Errorf("the client through the jump: %q, kept %q", seen, d.client)
	}
}
