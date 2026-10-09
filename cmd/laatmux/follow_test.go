package main

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/laat/laatmux/internal/config"
	"github.com/laat/laatmux/internal/merged"
	"github.com/laat/laatmux/internal/palette"
	"github.com/laat/laatmux/internal/protocol"
	"github.com/laat/laatmux/internal/view"
)

// A sidebar pane running when the config file changes takes the file
// read again on its next look: the hosts, agents and default agent a
// task form made from then on offers, the copy rules a submit sends,
// this machine's name for a repository in the rows, the line templates,
// the theme, the agent and kind icons, the jump key labels, the
// strip's chip width and the hosts line. A file that changes and does
// not load is said once in the footer, the view keeping what it had,
// and the file put right is taken.
func TestViewFollowsConfig(t *testing.T) {
	t.Setenv("NO_COLOR", "")
	p := filepath.Join(t.TempDir(), "config.yaml")
	t.Setenv("LAATMUX_CONFIG", p)
	t.Setenv("LAATMUX_HOME", t.TempDir())
	// Each write a rename over the file, so a look never reads one half
	// written, which would be a change of its own.
	write := func(s string) {
		t.Helper()
		if err := os.WriteFile(p+".new", []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(p+".new", p); err != nil {
			t.Fatal(err)
		}
	}
	const before = `hosts:
  - name: mac
    repos: /r
    worktrees: /w
agents:
  claude: {cmd: [claude]}
repos:
  - git@github.com:laat/proj.git
theme: {mode: dark}
`
	write(before)
	var w config.Watch
	cfg, _, err := w.Changed()
	if err != nil {
		t.Fatal(err)
	}
	st := merged.New()
	st.Apply(protocol.Message{Type: protocol.TypeSnapshot,
		Hosts:     []protocol.HostStatus{{Name: "mac", EnvironmentID: "menv", Connected: true, Listed: true, Capabilities: []string{"status", "worktrees"}}},
		Worktrees: []protocol.Worktree{{ID: "menv/worktree//w/proj/task", EnvironmentID: "menv", Repo: "proj", Source: "git@github.com:laat/proj.git", Branch: "task", Root: "/w/proj/task"}},
	})
	d := &dash{ctx: context.Background(), st: st}
	var theme palette.Theme
	taker := &configTaker{d: d, st: st, o: viewOptions{listen: true}, bg: &background{}, theme: func(th palette.Theme) { theme = th }}
	m := &view.Model{View: view.ViewTree, Width: 60, Height: 20}
	taker.take(m, cfg, false)
	fill(m, st.Status(""))
	dark := theme
	if m.JumpKeys || m.ItemWidth != config.DefaultSidebarItemWidth || m.LocalHost != "mac" {
		t.Fatalf("at start: jump keys %v item width %d local %q", m.JumpKeys, m.ItemWidth, m.LocalHost)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cmds := make(chan func(*view.Model) view.Action, 1)
	watchConfig(ctx, &w, 10*time.Millisecond, cmds, func(m *view.Model, cfg config.Config) { taker.take(m, cfg, true) }, taker.failed)
	next := func() {
		t.Helper()
		select {
		case f := <-cmds:
			f(m)
		case <-time.After(5 * time.Second):
			t.Fatal("no look at the changed file")
		}
	}
	write(`hosts:
  - name: mac
    repos: /r
    worktrees: /w
  - name: vm
    ssh: vm
    repos: /r
    worktrees: /w
agents:
  claude: {cmd: [claude]}
  codex: {cmd: [codex]}
default_agent: codex
repos:
  - source: git@github.com:laat/proj.git
    name: renamed
copy: ["*.local"]
theme: {mode: light}
agent_icons: {claude: {icon: CC}}
kind_icons: {worktree: W}
sidebar:
  jump_keys: true
  horizontal: {item_width: 30}
  templates:
    tree: {worktree: "WT {kind_icon} {branch}"}
`)
	next()
	if _, ok := d.cfg.Find("vm"); !ok || strings.Join(d.cfg.Copy, ",") != "*.local" {
		t.Fatalf("the dash's config: %+v", d.cfg)
	}
	if reflect.DeepEqual(theme, dark) {
		t.Error("the theme stayed dark")
	}
	if !m.JumpKeys || m.ItemWidth != 30 || m.AgentIcons["claude"].Icon != "CC" {
		t.Errorf("jump keys %v item width %d agent icons %v", m.JumpKeys, m.ItemWidth, m.AgentIcons)
	}
	// The hosts line has the host added, which the stream has yet to
	// carry.
	if want := []view.HostEntry{{Name: "vm", Connecting: true}}; !reflect.DeepEqual(m.Hosts, want) {
		t.Errorf("the hosts line %+v", m.Hosts)
	}
	var b strings.Builder
	for _, l := range m.Render() {
		for _, s := range l.Spans {
			b.WriteString(s.Text)
		}
		b.WriteString("\n")
	}
	if out := b.String(); !strings.Contains(out, "renamed") || !strings.Contains(out, "WT W task") {
		t.Errorf("the rows after the change:\n%s", out)
	}
	d.startAdd(m)
	form, ok := m.Overlay.(*view.Form)
	if !ok {
		t.Fatalf("no form: %q", m.Message)
	}
	if len(form.Chips[1].Choices) != 2 || form.Chips[2].Label() != "codex" {
		t.Fatalf("the form: hosts %+v agent %q", form.Chips[1].Choices, form.Chips[2].Label())
	}

	// A bad file with the form up: the footer, and the form's note,
	// which covers it.
	write("hosts: [\n")
	next()
	if !strings.HasPrefix(m.Message, "config: ") || !strings.HasSuffix(m.Message, "; the view keeps the config it had") {
		t.Fatalf("message %q", m.Message)
	}
	if n := form.Note(form); !strings.HasPrefix(n, "config: ") || !strings.HasSuffix(n, "; the form keeps what it offers") {
		t.Fatalf("the form's note %q", n)
	}
	if _, ok := d.cfg.Find("vm"); !ok {
		t.Fatal("a file that does not load replaced the config")
	}
	select {
	case <-cmds:
		t.Fatal("the bad file said twice")
	case <-time.After(100 * time.Millisecond):
	}
	// A form opened while the file does not load says so too.
	m.Overlay, d.add = nil, nil
	d.startAdd(m)
	later, ok := m.Overlay.(*view.Form)
	if !ok || !strings.HasPrefix(later.Note(later), "config: ") {
		t.Fatalf("a form opened after the failure: %v", m.Overlay)
	}
	write(before)
	next()
	if _, ok := d.cfg.Find("vm"); ok || !reflect.DeepEqual(theme, dark) || len(m.Hosts) != 0 {
		t.Fatalf("the file put right was not taken: hosts line %+v", m.Hosts)
	}
	if m.Message != "" || strings.HasPrefix(later.Note(later), "config: ") {
		t.Fatalf("after the file was put right: footer %q note %q", m.Message, later.Note(later))
	}
	// A footer of another's, a dismiss's say, is left to its next key.
	m.Message = "dismissed proj/b on vm"
	write(strings.Replace(before, "mode: dark", "mode: light", 1))
	next()
	if m.Message != "dismissed proj/b on vm" {
		t.Fatalf("a take cleared another's footer: %q", m.Message)
	}
}

// The terminal is asked for its background once, the first time the
// theme needs it; a theme turned to auto once the view reads the keys
// takes that answer, or none, and never asks.
func TestBackgroundAskedOnce(t *testing.T) {
	asks := 0
	b := &background{ask: func() (bool, bool) { asks++; return false, true }}
	auto := config.Config{Theme: config.Theme{Mode: palette.ModeAuto}}
	light, _ := lookWith(auto, b.get)
	lookWith(auto, b.get)
	if asks != 1 {
		t.Fatalf("asked %d times", asks)
	}
	if want, _ := palette.New(false, nil); !reflect.DeepEqual(light, want) {
		t.Error("the answer was not taken")
	}
	if dark, ok := (&background{}).get(); dark || ok {
		t.Fatal("an answer with none asked")
	}
}
