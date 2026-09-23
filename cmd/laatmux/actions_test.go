package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
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

	// No last-used agent for the repository: the configured default is
	// preselected; without one, the first agent.
	p.Handle(view.Key{Kind: view.KeyEsc})
	d.act(m, m.Poll())
	if err := home.UpdateLast(func(l *home.Last) { l.Set("git@github.com:laat/laatmux.git", home.LastRepo{Host: "vm"}) }); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ def, want string }{{"codex", "codex"}, {"", "claude"}} {
		cfg.DefaultAgentName = c.def
		d.cfg = cfg
		d.act(m, view.Action{Kind: view.ActionOther, Key: view.Key{Rune: 'a'}})
		for i := 0; i < 2; i++ {
			m.Overlay.(*view.Picker).Handle(view.Key{Kind: view.KeyEnter})
			d.act(m, m.Poll())
		}
		p = m.Overlay.(*view.Picker)
		if p.Title != "add: agent" || p.Choices[p.Selected].Label != c.want {
			t.Fatalf("default_agent %q: agent picker preselected %q", c.def, p.Choices[p.Selected].Label)
		}
		p.Handle(view.Key{Kind: view.KeyEsc})
		d.act(m, m.Poll())
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
	d := &dash{ctx: context.Background(), cfg: cfg, st: newMerged()}
	m := dashModel(cfg)
	d.act(m, view.Action{Kind: view.ActionOther, Key: view.Key{Rune: 'a'}})
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
	d := &dash{ctx: context.Background(), cfg: cfg, st: newMerged()}
	d.st.hosts["mac"] = hostState{Local: true, EnvID: "menv"}
	d.st.hosts["vm"] = hostState{EnvID: "venv"}
	r := rows.Row{Host: "vm", Name: "proj/task", Local: &workspace.Local{Name: "oldvm/proj/task", Key: "venv//w/proj/task", Host: "oldvm"}}
	l, err := d.localFor(r)
	if err != nil || l.Host != "vm" || l.Name != "oldvm/proj/task" {
		t.Errorf("renamed host: localFor = %+v, %v", l, err)
	}
	rs := rows.Build(rows.Input{
		Hosts:  []rows.Host{{Name: "mac", Local: true, EnvironmentID: "menv", Connected: true, Listed: true}, {Name: "vm", EnvironmentID: "venv", Connected: true, Listed: true}},
		Agents: []protocol.Agent{{ID: "menv/default/%6", EnvironmentID: "menv", Server: "default", Session: "vm/proj/task", Agent: "claude", Activity: protocol.Idle, Liveness: protocol.Alive}},
		Locals: []workspace.Local{{Name: "vm/proj/task", Key: "venv//w/proj/task", Host: "vm"}},
	})
	if len(rs.Main) != 1 || rs.Main[0].Host != "mac" {
		t.Fatalf("rows = %+v", rs.Main)
	}
	l, err = d.localFor(rs.Main[0])
	if err != nil || l.Host != "vm" {
		t.Errorf("observed agent in a workspace window: localFor = %+v, %v", l, err)
	}
	if _, err := d.localFor(rows.Row{Name: "s", Stale: true, Local: &workspace.Local{Name: "s", Key: "venv//gone"}}); err == nil {
		t.Error("stale row accepted")
	}
	if _, err := d.localFor(rows.Row{Name: "scratch", Local: &workspace.Local{Name: "mac/scratch", Attach: "mac/scratch"}}); err == nil {
		t.Error("plain attachment accepted")
	}
}

// An rm whose host side succeeded and whose local cleanup then failed
// returns the root with the error, so the CLI prints the removal before
// the error and the dashboard says what was removed.
func TestRmPartialSuccess(t *testing.T) {
	startFakeDaemon(t, []string{protocol.CapStatus, protocol.CapRm}, func(pc *protocol.Conn, m protocol.Message) bool {
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
	rm := command.Rm{Host: config.Host{Host: client.Host{Name: "lab"}}, Root: "/w/proj/task"}
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
	startFakeDaemon(t, []string{protocol.CapStatus, protocol.CapAdd}, func(pc *protocol.Conn, m protocol.Message) bool {
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
	add := command.Add{Host: config.Host{Host: client.Host{Name: "lab"}}, Repo: config.Repo{Source: "git@x:o/proj.git", Name: "proj"}, Branch: "x", Agent: "claude"}
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
