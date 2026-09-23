package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/laat/laatmux/internal/client"
	"github.com/laat/laatmux/internal/command"
	"github.com/laat/laatmux/internal/config"
	"github.com/laat/laatmux/internal/home"
	"github.com/laat/laatmux/internal/protocol"
	"github.com/laat/laatmux/internal/rows"
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

func dashModel(cfg config.Config) *view.Model {
	m := &view.Model{Width: 80, Height: 20}
	m.SetRows(rows.Build(rows.Input{
		Hosts: []rows.Host{
			{Name: "mac", Local: true, EnvironmentID: "menv", Connected: true, Listed: true, Worktrees: true},
			{Name: "vm", EnvironmentID: "venv", Connected: true, Listed: true, Worktrees: true},
		},
		Agents: []protocol.Agent{
			{ID: "venv/laatmux/%1", EnvironmentID: "venv", Session: "proj/task", Agent: "claude", Activity: protocol.Working, Liveness: protocol.Alive, Managed: true},
		},
		Worktrees: []protocol.Worktree{
			{ID: "venv/worktree//w/proj/task", EnvironmentID: "venv", Repo: "proj", Source: "git@github.com:laat/proj.git", Branch: "task", Root: "/w/proj/task", Session: "proj/task"},
			{ID: "venv/worktree//w/proj/spike", EnvironmentID: "venv", Repo: "proj", Source: "git@github.com:laat/proj.git", Branch: "spike", Root: "/w/proj/spike"},
			{ID: "menv/worktree//w/x/y", EnvironmentID: "menv", Repo: "x", Branch: "y", Root: "/w/x/y", Session: "x/y"},
		},
		Locals: []workspace.Local{
			{Name: "vm/proj/task", Key: "venv//w/proj/task", Host: "vm", Source: "git@github.com:laat/proj.git", Branch: "task"},
			{Name: "vm/proj/gone", Key: "venv//w/proj/gone", Host: "vm", Source: "git@github.com:laat/proj.git", Branch: "gone"},
		},
	}))
	m.ShowHidden = true
	m.Render()
	return m
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

// a on a worktree row without a session pre-fills the pickers with the
// record's repository and host, and the prompt with its branch; each
// picker in turn, then the prompt, and Esc in any step returns to the
// list with nothing done.
func TestAddFlowPrefilled(t *testing.T) {
	t.Setenv("LAATMUX_HOME", t.TempDir())
	cfg := dashConfig(t)
	d := &dash{ctx: context.Background(), cfg: cfg, st: newMerged()}
	m := dashModel(cfg)
	selectRow(t, m, "proj/spike")
	d.act(m, view.Action{Kind: view.ActionOther, Key: view.Key{Rune: 'a'}})
	p, ok := m.Overlay.(*view.Picker)
	if !ok || p.Title != "add: repository" || p.Choices[p.Selected].Label != "proj" {
		t.Fatalf("repository picker: %+v", m.Overlay)
	}
	p.Handle(view.Key{Kind: view.KeyEnter})
	d.act(m, m.Poll())
	p, ok = m.Overlay.(*view.Picker)
	if !ok || p.Title != "add: host" || p.Choices[p.Selected].Label != "vm" || len(p.Choices) != 2 {
		t.Fatalf("host picker: %+v", m.Overlay)
	}
	p.Handle(view.Key{Kind: view.KeyEnter})
	d.act(m, m.Poll())
	p, ok = m.Overlay.(*view.Picker)
	if !ok || p.Title != "add: agent" || len(p.Choices) != 2 {
		t.Fatalf("agent picker: %+v", m.Overlay)
	}
	p.Handle(view.Key{Kind: view.KeyDown})
	p.Handle(view.Key{Kind: view.KeyEnter})
	d.act(m, m.Poll())
	pr, ok := m.Overlay.(*view.Prompt)
	if !ok || pr.Text != "spike" || !strings.HasPrefix(pr.Title, "add proj on vm with codex") {
		t.Fatalf("prompt: %+v", m.Overlay)
	}
	if err := pr.Validate("bad..name"); err == nil {
		t.Error("prompt accepted a name git refuses")
	}
	pr.Handle(view.Key{Kind: view.KeyEsc})
	d.act(m, m.Poll())
	if m.Overlay != nil || d.add != nil || d.run != nil {
		t.Errorf("esc did not return to the list: overlay=%v add=%v run=%v", m.Overlay, d.add, d.run)
	}
}

// The last-used host and agent for the repository are preselected, and
// a step with one candidate is skipped.
func TestAddFlowDefaults(t *testing.T) {
	t.Setenv("LAATMUX_HOME", t.TempDir())
	cfg := dashConfig(t)
	if err := home.UpdateLast(func(l *home.Last) {
		l.Set("git@github.com:laat/laatmux.git", home.LastRepo{Host: "vm", Agent: "codex"})
	}); err != nil {
		t.Fatal(err)
	}
	d := &dash{ctx: context.Background(), cfg: cfg, st: newMerged()}
	m := dashModel(cfg)
	selectRow(t, m, "proj/task") // a row with a session pre-fills nothing
	d.act(m, view.Action{Kind: view.ActionOther, Key: view.Key{Rune: 'a'}})
	p := m.Overlay.(*view.Picker)
	if p.Title != "add: repository" || p.Choices[p.Selected].Label != "laatmux" {
		t.Fatalf("repository picker preselected %q", p.Choices[p.Selected].Label)
	}
	p.Handle(view.Key{Kind: view.KeyEnter})
	d.act(m, m.Poll())
	p = m.Overlay.(*view.Picker)
	if p.Title != "add: host" || p.Choices[p.Selected].Label != "vm" {
		t.Fatalf("host picker preselected %q", p.Choices[p.Selected].Label)
	}
	p.Handle(view.Key{Kind: view.KeyEnter})
	d.act(m, m.Poll())
	p = m.Overlay.(*view.Picker)
	if p.Title != "add: agent" || p.Choices[p.Selected].Label != "codex" {
		t.Fatalf("agent picker preselected %q", p.Choices[p.Selected].Label)
	}

	// One agent and one able host: both pickers are skipped.
	cfg.Agents = map[string]config.Agent{"claude": {Cmd: []string{"claude"}}}
	cfg.Hosts = cfg.Hosts[1:2]
	d = &dash{ctx: context.Background(), cfg: cfg, st: newMerged()}
	m = dashModel(cfg)
	d.act(m, view.Action{Kind: view.ActionOther, Key: view.Key{Rune: 'a'}})
	p = m.Overlay.(*view.Picker)
	p.Handle(view.Key{Kind: view.KeyEnter})
	d.act(m, m.Poll())
	pr, ok := m.Overlay.(*view.Prompt)
	if !ok || pr.Title != "add laatmux on vm with claude: branch" || pr.Text != "" {
		t.Fatalf("after the only repository picker: %+v", m.Overlay)
	}

	// No agents: refused with a message, nothing up.
	cfg.Agents = nil
	d = &dash{ctx: context.Background(), cfg: cfg, st: newMerged()}
	m = dashModel(cfg)
	d.act(m, view.Action{Kind: view.ActionOther, Key: view.Key{Rune: 'a'}})
	if m.Overlay != nil || d.add != nil || m.Message != "no agents configured" {
		t.Errorf("no agents: overlay=%v message=%q", m.Overlay, m.Message)
	}
}

// x asks about the selected worktree, naming it and its root, with the
// request built from the record: by source and branch when this
// machine knows the repository, by root alone when it does not, and
// from a stale session's tags and key; X asks with force.
func TestRmFor(t *testing.T) {
	cfg := dashConfig(t)
	d := &dash{ctx: context.Background(), cfg: cfg, st: newMerged()}
	m := dashModel(cfg)
	selectRow(t, m, "proj/task")
	d.act(m, view.Action{Kind: view.ActionOther, Key: view.Key{Rune: 'x'}})
	want := command.Rm{Host: config.Host{Host: client.Host{Name: "vm", SSH: "vm"}, Repos: "/r", Worktrees: "/w"},
		Repo: cfg.Repos[1], Branch: "task", Root: "/w/proj/task"}
	if d.rm.Host.Name != want.Host.Name || d.rm.Repo != want.Repo || d.rm.Branch != want.Branch || d.rm.Root != want.Root || d.rm.Force {
		t.Errorf("rm = %+v", d.rm)
	}
	if m.Confirm != "remove proj/task on vm (/w/proj/task)? y/n" || m.ConfirmTag != "rm" {
		t.Errorf("confirm = %q tag %q", m.Confirm, m.ConfirmTag)
	}
	m.Handle(view.Key{Rune: 'n'})

	selectRow(t, m, "x/y")
	d.act(m, view.Action{Kind: view.ActionOther, Key: view.Key{Rune: 'X'}})
	if d.rm.Repo.Source != "" || d.rm.Branch != "" || d.rm.Root != "/w/x/y" || !d.rm.Force || d.rm.Host.Name != "mac" {
		t.Errorf("unknown repository: rm = %+v", d.rm)
	}
	if m.Confirm != "force-remove /w/x/y on mac (/w/x/y)? y/n" {
		t.Errorf("confirm = %q", m.Confirm)
	}
	m.Handle(view.Key{Rune: 'n'})

	selectRow(t, m, "vm/proj/gone")
	d.act(m, view.Action{Kind: view.ActionOther, Key: view.Key{Rune: 'x'}})
	if d.rm.Repo != cfg.Repos[1] || d.rm.Branch != "gone" || d.rm.Root != "/w/proj/gone" || d.rm.Host.Name != "vm" {
		t.Errorf("stale: rm = %+v", d.rm)
	}
	m.Handle(view.Key{Rune: 'n'})

	// An agent with no worktree is not rm's.
	m.SetRows(rows.Build(rows.Input{
		Hosts:  []rows.Host{{Name: "mac", Local: true, EnvironmentID: "menv", Connected: true, Listed: true, Worktrees: true}},
		Agents: []protocol.Agent{{ID: "menv/laatmux/%9", EnvironmentID: "menv", Session: "scratch", Activity: protocol.Idle, Liveness: protocol.Alive, Managed: true}},
	}))
	m.Render()
	selectRow(t, m, "scratch")
	d.act(m, view.Action{Kind: view.ActionOther, Key: view.Key{Rune: 'x'}})
	if m.Confirm != "" || !strings.Contains(m.Message, "not a worktree") {
		t.Errorf("agent row: confirm=%q message=%q", m.Confirm, m.Message)
	}
}

// A confirmed rm runs under a log overlay; a refusal without force
// carries the hint to use X and stays until a key; then the list
// returns with the message.
func TestRmRefusalHint(t *testing.T) {
	d := &dash{ctx: context.Background(), cfg: dashConfig(t), st: newMerged()}
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
	log.Handle(view.Key{Rune: ' '})
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
