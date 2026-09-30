package main

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/laat/laatmux/internal/config"
	"github.com/laat/laatmux/internal/home"
	"github.com/laat/laatmux/internal/rows"
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
		s.SetFolds(map[string]bool{"repo/x": true}, nil, now)
	}); err != nil {
		t.Fatal(err)
	}
	m := &view.Model{Layout: view.Tiles, View: view.ViewAgents}
	seen := startSettings(cfg, m, false, false)
	if m.View != view.ViewTree || m.Layout != view.Compact || m.Scope != view.ScopeProject || seen.IsZero() {
		t.Fatalf("start: %s %s %s %v", m.View, m.Layout, m.Scope, seen)
	}
	folds := m.ToggledFolds()
	if len(folds) != 1 || !folds["repo/x"] {
		t.Errorf("folds at start: %v", folds)
	}
	fixed := &view.Model{Layout: view.Tiles, View: view.ViewAgents}
	startSettings(cfg, fixed, true, true)
	if fixed.View != view.ViewAgents || fixed.Layout != view.Tiles {
		t.Errorf("fixed: %s %s", fixed.View, fixed.Layout)
	}
	// A change: the view and layout written, the folds with their
	// sighting, the scope left as the CLI set it.
	m.View, m.Layout, m.Scope = view.ViewAgents, view.Tiles, view.ScopeSession
	m.ApplyFolds(map[string]bool{"repo/y": false})
	if err := saveSettings(m, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	s, _, _ := home.ReadSidebar()
	if s.View != "agents" || s.Layout != "tiles" || s.Scope != "project" || !s.Folds["repo/x"].Closed || s.Folds["repo/y"].Closed || !s.Folds["repo/y"].Seen.Equal(now.Add(time.Minute)) {
		t.Errorf("saved: %+v", s)
	}
	strip := &view.Model{Layout: view.Strip, View: view.ViewAgents}
	if err := saveSettings(strip, now.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if s, _, _ := home.ReadSidebar(); s.View != "agents" || s.Layout != "tiles" {
		t.Errorf("a strip wrote the defaults: %+v", s)
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
	if err := home.UpdateSidebar(now, func(s *home.Sidebar) { s.SetFolds(map[string]bool{"repo/z": true}, nil, now.Add(time.Hour)) }); err != nil {
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
	// The pane's jump switches the client the command named.
	d := &dash{ctx: ctx, client: "/dev/ttys004"}
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
