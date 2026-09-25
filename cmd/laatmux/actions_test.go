package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
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

// a on a worktree row without a session opens the form pre-filled with
// the record's repository and host, and its branch explicit; Enter on
// a chip opens the picker inside the form, and Esc anywhere returns to
// the list with nothing done.
func TestAddFlowPrefilled(t *testing.T) {
	t.Setenv("LAATMUX_HOME", t.TempDir())
	cfg := dashConfig(t)
	d := &dash{ctx: context.Background(), cfg: cfg, st: newMerged()}
	m := dashModel(cfg)
	selectRow(t, m, "proj/spike")
	d.act(m, view.Action{Kind: view.ActionOther, Key: view.Key{Rune: 'a'}})
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
	f.Handle(view.Key{Kind: view.KeyTab})
	f.Handle(view.Key{Kind: view.KeyTab})
	f.Handle(view.Key{Kind: view.KeyEnter})
	if !strings.Contains(view.Text(f.Render(60, 12)), "add a task: repository") {
		t.Fatalf("no picker:\n%s", view.Text(f.Render(60, 12)))
	}
	f.Handle(view.Key{Kind: view.KeyEsc})
	d.act(m, m.Poll())
	if m.Overlay == nil {
		t.Fatal("esc in the picker ended the form")
	}
	f.Handle(view.Key{Kind: view.KeyEsc})
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
	d := &dash{ctx: context.Background(), cfg: cfg, st: newMerged()}
	m := dashModel(cfg)
	selectRow(t, m, "proj/task") // a row with a session pre-fills nothing
	d.act(m, view.Action{Kind: view.ActionOther, Key: view.Key{Rune: 'a'}})
	f := m.Overlay.(*view.Form)
	if f.Chips[0].Label() != "laatmux" || f.Chips[1].Label() != "vm" || f.Chips[2].Label() != "codex" || f.Branch() != "" {
		t.Fatalf("preselected %q %q %q branch %q", f.Chips[0].Label(), f.Chips[1].Label(), f.Chips[2].Label(), f.Branch())
	}
	f.Handle(view.Key{Kind: view.KeyEsc})
	d.act(m, m.Poll())

	// No last-used agent for the repository: the configured default is
	// preselected; without one, the first agent.
	if err := home.UpdateLast(func(l *home.Last) { l.Set("git@github.com:laat/laatmux.git", home.LastRepo{Host: "vm"}) }); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ def, want string }{{"codex", "codex"}, {"", "claude"}} {
		cfg.DefaultAgentName = c.def
		d.cfg = cfg
		d.act(m, view.Action{Kind: view.ActionOther, Key: view.Key{Rune: 'a'}})
		f := m.Overlay.(*view.Form)
		if f.Chips[2].Label() != c.want {
			t.Fatalf("default_agent %q: agent preselected %q", c.def, f.Chips[2].Label())
		}
		f.Handle(view.Key{Kind: view.KeyEsc})
		d.act(m, m.Poll())
	}

	// One agent and one able host: both chips are shown with their one
	// candidate.
	cfg.Agents = map[string]config.Agent{"claude": {Cmd: []string{"claude"}}}
	cfg.Hosts = cfg.Hosts[1:2]
	d = &dash{ctx: context.Background(), cfg: cfg, st: newMerged()}
	m = dashModel(cfg)
	d.act(m, view.Action{Kind: view.ActionOther, Key: view.Key{Rune: 'a'}})
	f = m.Overlay.(*view.Form)
	if len(f.Chips[1].Choices) != 1 || len(f.Chips[2].Choices) != 1 || f.Chips[1].Label() != "vm" || f.Chips[2].Label() != "claude" {
		t.Fatalf("single candidates: %+v", f.Chips)
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
	if d.rm.Host.Name != want.Host.Name || d.rm.Repo.Source != want.Repo.Source || d.rm.Branch != want.Branch || d.rm.Root != want.Root || d.rm.Force {
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
	if d.rm.Repo.Source != cfg.Repos[1].Source || d.rm.Branch != "gone" || d.rm.Root != "/w/proj/gone" || d.rm.Host.Name != "vm" {
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

// The task form is built over the config's candidates with the
// defaults preselected: the repository named, the host and agent last
// used for it, and a note when the host's cached daemon capabilities
// lack tasks; a branch given is the user's.
func TestBuildForm(t *testing.T) {
	cfg := config.Config{
		Hosts:  []config.Host{{Host: client.Host{Name: "mac"}, Repos: "/r", Worktrees: "/w"}, {Host: client.Host{Name: "vm", SSH: "vm"}, Repos: "/r", Worktrees: "/w"}},
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
	form.Handle(view.Key{Kind: view.KeyShiftTab})
	form.Handle(view.Key{Kind: view.KeyShiftTab})
	form.Handle(view.Key{Kind: view.KeyShiftTab}) // the repository chip
	form.Handle(view.Key{Kind: view.KeyRight})
	if form.Chips[0].Label() != "other" || form.Chips[1].Label() != "vm" || form.Chips[2].Label() != "codex" {
		t.Fatalf("after choosing other: %q %q %q", form.Chips[0].Label(), form.Chips[1].Label(), form.Chips[2].Label())
	}
	form.Handle(view.Key{Kind: view.KeyTab}) // the host chip
	form.Handle(view.Key{Kind: view.KeyLeft})
	form.Handle(view.Key{Kind: view.KeyShiftTab})
	form.Handle(view.Key{Kind: view.KeyLeft}) // back to proj
	if form.Chips[0].Label() != "proj" || form.Chips[1].Label() != "mac" || form.Chips[2].Label() != "claude" {
		t.Fatalf("after the user's host: %q %q %q", form.Chips[0].Label(), form.Chips[1].Label(), form.Chips[2].Label())
	}
	// Picking the value already shown is the user's choice too.
	form = buildForm(cfg, f, last, "proj", "", "", caps)
	form.Handle(view.Key{Kind: view.KeyShiftTab}) // the agent chip
	form.Handle(view.Key{Kind: view.KeyEnter})    // the picker on claude
	form.Handle(view.Key{Kind: view.KeyEnter})    // accept claude
	form.Handle(view.Key{Kind: view.KeyShiftTab})
	form.Handle(view.Key{Kind: view.KeyShiftTab}) // the repository chip
	form.Handle(view.Key{Kind: view.KeyRight})    // other: last agent codex
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
	form.Handle(view.Key{Kind: view.KeyShiftTab})
	form.Handle(view.Key{Kind: view.KeyShiftTab})
	form.Handle(view.Key{Kind: view.KeyShiftTab})
	form.Handle(view.Key{Kind: view.KeyRight}) // other, whose last host is vm
	form.Handle(view.Key{Kind: view.KeyRight}) // back to proj
	if form.Chips[1].Label() != "mac" {
		t.Fatalf("the record's host replaced: %q", form.Chips[1].Label())
	}
}

// A submit through the relay: a refusal puts the form back up with the
// error and the text intact; an error after the daemon may hold the
// task ends the view with the id; acceptance ends it with the id.
func TestSubmitFormOutcomes(t *testing.T) {
	t.Setenv("LAATMUX_HOME", t.TempDir())
	cfg := dashConfig(t)
	d := &dash{ctx: context.Background(), cfg: cfg, st: newMerged(), relay: true}
	m := dashModel(cfg)
	d.act(m, view.Action{Kind: view.ActionOther, Key: view.Key{Rune: 'a'}})
	f := m.Overlay.(*view.Form)
	f.SetPrompt("Fix it")
	var got command.Add
	d.submit = func(a command.Add) (string, error) {
		got = a
		return "", errors.New("tasks not supported by vm's daemon")
	}
	f.Handle(view.Key{Kind: view.KeyEnter})
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
	f.Handle(view.Key{Kind: view.KeyEnter})
	if d.act(m, m.Poll()) || !strings.Contains(m.Message, "submitted add-1") || m.Overlay != nil || d.add != nil {
		t.Fatalf("uncertain submit: message %q overlay %v add %v", m.Message, m.Overlay, d.add)
	}
	d.act(m, view.Action{Kind: view.ActionOther, Key: view.Key{Rune: 'a'}})
	f = m.Overlay.(*view.Form)
	f.SetPrompt("Fix it")
	d.submit = func(a command.Add) (string, error) { return "add-2", nil }
	f.Handle(view.Key{Kind: view.KeyEnter})
	if !d.act(m, m.Poll()) || m.Message != "accepted add-2" || d.add != nil {
		t.Fatalf("accepted: message %q add %v", m.Message, d.add)
	}
}

// The notice a foreground add leaves when its prompt did not reach the
// agent: the state and reason, the session, and the prompt's lines to
// copy, wrapped, scrollable, until dismissed; nothing when the prompt
// was delivered or there was none; shown whatever else went wrong.
func TestUndelivered(t *testing.T) {
	add := command.Add{Repo: config.Repo{Name: "proj"}, Host: config.Host{Host: client.Host{Name: "vm"}}, Branch: "b", Prompt: "one\ntwo " + strings.Repeat("long ", 30)}
	res := command.Added{Done: true, Root: "/r/b", Managed: "proj/b", Prompt: protocol.DeliveryNotDelivered, Reason: "session existed"}
	n := undelivered(add, res)
	if n == nil || n.Done() {
		t.Fatal("no notice, or done before a key")
	}
	text := view.Text(n.Render(40, 8))
	for _, want := range []string{"proj/b", "one", "prompt not delivered: session existed"} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %q in\n%s", want, text)
		}
	}
	for _, l := range strings.Split(text, "\n") {
		if len([]rune(l)) > 40 {
			t.Fatalf("line wider than the screen: %q", l)
		}
	}
	// The long line is wrapped and reachable by scrolling; a stray key
	// does not dismiss.
	n.Handle(view.Key{Rune: 'x'})
	for range 20 {
		n.Handle(view.Key{Kind: view.KeyDown})
	}
	if n.Done() || !strings.Contains(view.Text(n.Render(40, 8)), "long long") {
		t.Fatalf("scrolled:\n%s", view.Text(n.Render(40, 8)))
	}
	n.Handle(view.Key{Kind: view.KeyEnter})
	if !n.Done() {
		t.Fatal("enter did not end it")
	}
	// A launch that failed with the delivery unknown, no session: the
	// text says the agent may have it, never that it does not.
	failed := command.Added{Done: false, Sent: true, Answered: true, Stage: protocol.StageAgent, Root: "/r/b", Prompt: protocol.DeliveryUnknown, Reason: "new-session failed after the session may have been made"}
	if n := undelivered(add, failed); n == nil || !strings.Contains(view.Text(n.Render(80, 12)), "prompt unknown") || strings.Contains(view.Text(n.Render(80, 12)), "without") {
		t.Fatalf("unknown delivery on a failed add:\n%s", view.Text(n.Render(80, 12)))
	}
	// A failure before the agent stage is positively before the send;
	// no result at all is unknown.
	early := command.Added{Sent: true, Answered: true, Stage: protocol.StageFetch}
	if n := undelivered(add, early); n == nil || !strings.Contains(view.Text(n.Render(80, 12)), "failed at fetch, before the prompt was sent") {
		t.Fatalf("early failure:\n%s", view.Text(n.Render(80, 12)))
	}
	lost := command.Added{Sent: true}
	if n := undelivered(add, lost); n == nil || !strings.Contains(view.Text(n.Render(80, 12)), "outcome unknown") || strings.Contains(view.Text(n.Render(80, 12)), "not sent") {
		t.Fatalf("lost result:\n%s", view.Text(n.Render(80, 12)))
	}
	// Refused before any daemon had it: a host down, or one without
	// the capability.
	refused := command.Added{}
	if n := undelivered(add, refused); n == nil || !strings.Contains(view.Text(n.Render(80, 12)), "the prompt was not sent") || strings.Contains(view.Text(n.Render(80, 12)), "may have") {
		t.Fatalf("refused:\n%s", view.Text(n.Render(80, 12)))
	}
	// The prompt is kept in a file of the user's own.
	t.Setenv("LAATMUX_HOME", t.TempDir())
	path, err := keepPrompt("id", "p\tq\n")
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	st, _ := os.Stat(path)
	if err != nil || string(b) != "p\tq\n" || st.Mode().Perm() != 0o600 {
		t.Fatalf("kept %q %v %v", b, err, st.Mode())
	}
	// The prompt's whitespace is kept.
	tabs := command.Add{Repo: config.Repo{Name: "proj"}, Host: config.Host{Host: client.Host{Name: "vm"}}, Branch: "b", Prompt: "run:\n\tmake  all"}
	if text := view.Text(undelivered(tabs, res).Render(80, 12)); !strings.Contains(text, "    make  all") {
		t.Fatalf("whitespace:\n%s", text)
	}
	if undelivered(add, command.Added{Done: true, Prompt: protocol.DeliveryDelivered}) != nil || undelivered(command.Add{}, res) != nil {
		t.Fatal("a notice with nothing to recover")
	}
}

// compose's host: a refusal puts the form back, an answer the daemon
// may have taken waits in an ended log and then ends the view, and
// the notice of an undelivered prompt ends the view when dismissed.
func TestComposeAct(t *testing.T) {
	t.Setenv("LAATMUX_HOME", t.TempDir())
	cfg := dashConfig(t)
	f := &addForm{repos: cfg.Repos, hosts: cfg.Hosts, agents: cfg.AgentNames()}
	d := &dash{ctx: context.Background(), cfg: cfg, st: newMerged(), relay: true, add: f}
	c := &composer{d: d, f: f}
	var last home.Last
	form := buildForm(cfg, f, last, "proj", "", "", nil)
	form.SetPrompt("Fix it")
	m := &view.Model{Overlay: form, Width: 80, Height: 24}
	d.submit = func(command.Add) (string, error) { return "", errors.New("tasks not supported by vm's daemon") }
	form.Handle(view.Key{Kind: view.KeyEnter})
	if c.act(m, m.Poll()) || m.Overlay != form || form.Error == "" {
		t.Fatalf("refusal: overlay %v error %q", m.Overlay, form.Error)
	}
	d.submit = func(command.Add) (string, error) { return "add-1", errors.New("the answer was lost") }
	form.Handle(view.Key{Kind: view.KeyEnter})
	if c.act(m, m.Poll()) {
		t.Fatal("an uncertain answer ended the view at once")
	}
	log, ok := m.Overlay.(*view.Log)
	if !ok || log.Done() {
		t.Fatalf("no ended log waiting: %v", m.Overlay)
	}
	log.Handle(view.Key{Kind: view.KeyPaste, Text: "stray"})
	if log.Done() {
		t.Fatal("a paste dismissed the log")
	}
	log.Handle(view.Key{Rune: 'x'})
	if !c.act(m, m.Poll()) || !strings.Contains(c.outcome, "submitted add-1") {
		t.Fatalf("after the key: outcome %q", c.outcome)
	}
	// The notice, as the foreground path leaves it: dismissed, the
	// view ends.
	m = &view.Model{Overlay: view.NewNotice("t", []string{"the prompt"}, ""), Width: 80, Height: 24}
	m.Overlay.Handle(view.Key{Kind: view.KeyEsc})
	if !c.act(m, m.Poll()) {
		t.Fatal("the notice's dismissal did not end the view")
	}
	fresh := buildForm(cfg, f, last, "proj", "", "", nil)
	fresh.Handle(view.Key{Kind: view.KeyEsc})
	m = &view.Model{Overlay: fresh}
	if !c.act(m, m.Poll()) {
		t.Fatal("esc on the form did not end the view")
	}
	// Ctrl-C on a foreground add's log: the quit notice, then the end.
	log = view.NewLog("t")
	d.run = &running{log: log, done: func(*view.Model) bool { return true }, prompt: "the prompt", quit: new(atomic.Bool)}
	m = &view.Model{Overlay: log, Width: 80, Height: 24}
	m.Handle(view.Key{Kind: view.KeyCtrlC})
	if c.act(m, m.Poll()) {
		t.Fatal("Ctrl-C on the log ended compose before the notice")
	}
	if _, ok := m.Overlay.(*view.Notice); !ok {
		t.Fatalf("no quit notice in compose: %v", m.Overlay)
	}
	m.Handle(view.Key{Kind: view.KeyEnter})
	if !c.act(m, m.Poll()) {
		t.Fatal("the quit notice's dismissal did not end compose")
	}
}

// The dashboard: a failed log with a prompt to recover puts the notice
// over the message and gives the message back when it is dismissed.
func TestNoticeRestoresMessage(t *testing.T) {
	t.Setenv("LAATMUX_HOME", t.TempDir())
	cfg := dashConfig(t)
	d := &dash{ctx: context.Background(), cfg: cfg, st: newMerged()}
	m := dashModel(cfg)
	log := view.NewLog("t")
	d.run = &running{log: log, done: func(*view.Model) bool { return true }}
	d.recover = view.NewNotice("t", []string{"the prompt"}, "")
	m.Overlay = log
	log.End(errors.New("local session: boom"))
	log.Handle(view.Key{Rune: 'x'})
	if d.act(m, m.Poll()) {
		t.Fatal("ended the view")
	}
	n, ok := m.Overlay.(*view.Notice)
	if !ok {
		t.Fatalf("no notice: %v", m.Overlay)
	}
	m.Handle(view.Key{Kind: view.KeyEnter}) // clears the message as it dismisses
	if !n.Done() || m.Message != "" {
		t.Fatalf("dismissed: done %v message %q", n.Done(), m.Message)
	}
	d.act(m, m.Poll())
	if m.Overlay != nil || m.Message != "local session: boom" {
		t.Fatalf("after the notice: overlay %v message %q", m.Overlay, m.Message)
	}
	// With a session and no error, the notice's dismissal jumps.
	jumped := ""
	d.switcher = func(s string) error { jumped = s; return nil }
	d.last = command.Added{Session: "mac/proj/b"}
	d.recovered, m.Message = "", ""
	m.Overlay = view.NewNotice("t", []string{"the prompt"}, "")
	m.Handle(view.Key{Kind: view.KeyEnter})
	if d.act(m, m.Poll()) || jumped != "mac/proj/b" || d.last.Session != "" {
		t.Fatalf("after the notice with a session: jumped %q", jumped)
	}
	// Ctrl-C on the log of an add keeps the prompt in a file and says
	// so in a notice, whose dismissal ends the view.
	log = view.NewLog("t")
	quit := new(atomic.Bool)
	d.run = &running{log: log, done: func(*view.Model) bool { return true }, prompt: "the prompt", quit: quit}
	m.Overlay = log
	log.Handle(view.Key{Kind: view.KeyCtrlC})
	if d.act(m, m.Poll()) {
		t.Fatal("quit ended the view before the notice")
	}
	if !quit.Load() {
		t.Fatal("the add was not told it was quit")
	}
	n, ok = m.Overlay.(*view.Notice)
	if !ok {
		t.Fatalf("no notice on quit: %v", m.Overlay)
	}
	text := view.Text(n.Render(100, 12))
	if !strings.Contains(text, "may run on") || !strings.Contains(text, "kept in") {
		t.Fatalf("quit notice:\n%s", text)
	}
	path := ""
	for _, l := range n.Lines {
		if strings.HasSuffix(l, ".txt") {
			path = l
		}
	}
	if b, err := os.ReadFile(path); err != nil || string(b) != "the prompt" {
		t.Fatalf("kept %q %v", b, err)
	}
	// A second Ctrl-C does not close it before it is read.
	m.Handle(view.Key{Kind: view.KeyCtrlC})
	if n.Done() {
		t.Fatal("Ctrl-C dismissed the quit notice")
	}
	m.Handle(view.Key{Kind: view.KeyEsc})
	if !d.act(m, m.Poll()) {
		t.Fatal("dismissing the quit notice did not end the view")
	}
	// A state directory that cannot hold the file: the prompt is shown.
	bad := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(bad, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LAATMUX_HOME", bad)
	qn := quitNotice("line one\n\tline two")
	if text := view.Text(qn.Render(100, 14)); !strings.Contains(text, "could not be kept") || !strings.Contains(text, "    line two") {
		t.Fatalf("fallback:\n%s", text)
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
	m.SetRows(rows.Build(rows.Input{
		Hosts:    []rows.Host{{Name: "vm", EnvironmentID: "venv", Connected: true, Listed: true, Worktrees: true}},
		Pendings: []protocol.Pending{stuck, running},
	}))
	var dismissed, delivered string
	d := &dash{ctx: context.Background(), cfg: cfg, st: newMerged(), relay: true,
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
	key := func(r rune) view.Action { return m.Handle(view.Key{Rune: r}) }

	// The running one, newest, is first.
	m.Handle(view.Key{Rune: 'g'})
	if r := m.Selection(); r == nil || r.ID() != "add-2" {
		t.Fatalf("selected %+v", r)
	}
	if a := m.Handle(view.Key{Kind: view.KeyEnter}); a.Kind != view.ActionJump || d.jump(m, *m.Selection()) || m.Message != "" {
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
	m.Handle(view.Key{Rune: 'j'})
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
// nothing else of the dashboard's.
func TestTaskAction(t *testing.T) {
	m := &view.Model{Width: 80, Height: 20}
	m.SetRows(rows.Build(rows.Input{
		Hosts:     []rows.Host{{Name: "vm", EnvironmentID: "venv", Connected: true, Listed: true, Worktrees: true}},
		Pendings:  []protocol.Pending{{ID: "add-1", Host: "vm", Repo: "proj", Branch: "fix", SubmittedAt: time.Now()}},
		Worktrees: []protocol.Worktree{{ID: "venv/worktree//w/a", EnvironmentID: "venv", Repo: "proj", Branch: "a", Root: "/w/a"}},
	}))
	m.Handle(view.Key{Rune: 'g'})
	other := func(r rune) view.Action { return view.Action{Kind: view.ActionOther, Key: view.Key{Rune: r}} }
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
	m.Handle(view.Key{Rune: 'j'}) // the worktree row
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
	d := &dash{ctx: context.Background(), cfg: cfg, st: newMerged(), relay: true}
	m := &view.Model{Width: 100, Height: 20}
	expired := protocol.Pending{ID: "add-1", Host: "vm", Repo: "proj", Branch: "b", Sent: true, Taken: true, Done: true, OK: true, Prompt: protocol.DeliveryNotDelivered, AttemptError: protocol.ErrRecoveryExpired, SubmittedAt: time.Now()}
	listed := protocol.Pending{ID: "add-2", Host: "vm", Repo: "proj", Branch: "c", Sent: true, Taken: true, Done: true, OK: true, Prompt: protocol.DeliveryDelivered, SubmittedAt: time.Now().Add(-time.Minute)}
	m.SetRows(rows.Build(rows.Input{Hosts: []rows.Host{{Name: "vm", Connected: true}}, Pendings: []protocol.Pending{expired, listed}}))
	m.Handle(view.Key{Rune: 'g'})
	d.act(m, view.Action{Kind: view.ActionOther, Key: view.Key{Rune: 'p'}})
	if !strings.Contains(m.Message, "laatmux tasks show add-1 prints the prompt, if one was kept") {
		t.Errorf("p on an expired prompt: %q", m.Message)
	}
	d.act(m, view.Action{Kind: view.ActionOther, Key: view.Key{Rune: 's'}})
	if !strings.Contains(m.Message, "pending task") {
		t.Errorf("s on a task: %q", m.Message)
	}
	m.Handle(view.Key{Rune: 'j'})
	d.act(m, view.Action{Kind: view.ActionOther, Key: view.Key{Rune: 'x'}})
	if m.Confirm != "" || !strings.Contains(m.Message, "hands over to its worktree row") {
		t.Errorf("x awaiting the listing: confirm %q message %q", m.Confirm, m.Message)
	}
	gone := listed
	gone.Gone, gone.Root, gone.EnvironmentID, gone.Session = true, "/r/c", "venv", "proj/c"
	// A gone task whose prompt was delivered has nothing kept to show.
	m.SetRows(rows.Build(rows.Input{Hosts: []rows.Host{{Name: "vm", Connected: true}}, Pendings: []protocol.Pending{gone}}))
	m.Handle(view.Key{Rune: 'g'})
	d.act(m, view.Action{Kind: view.ActionOther, Key: view.Key{Rune: 'p'}})
	if !strings.Contains(m.Message, "the prompt is delivered") || strings.Contains(m.Message, "tasks show") {
		t.Errorf("p on a gone, delivered task: %q", m.Message)
	}
	// p on a task whose host is removed, and x on a running task whose
	// replacement only the view has seen, say why not.
	stuck := protocol.Pending{ID: "add-9", Host: "old", Repo: "proj", Branch: "d", Sent: true, Taken: true, Done: true, OK: true, Prompt: protocol.DeliveryNotDelivered, SubmittedAt: time.Now()}
	m.SetRows(rows.Build(rows.Input{Hosts: []rows.Host{{Name: "vm", Connected: true}}, Pendings: []protocol.Pending{stuck}}))
	m.Handle(view.Key{Rune: 'g'})
	d.act(m, view.Action{Kind: view.ActionOther, Key: view.Key{Rune: 'p'}})
	if !strings.Contains(m.Message, "host removed; x dismisses the task") {
		t.Errorf("p on a removed host: %q", m.Message)
	}
	moving := protocol.Pending{ID: "add-8", Host: "vm", EnvironmentID: "venv", Repo: "proj", Branch: "e", Sent: true, Taken: true, SubmittedAt: time.Now()}
	m.SetRows(rows.Build(rows.Input{Hosts: []rows.Host{{Name: "vm", EnvironmentID: "wenv", Connected: true}}, Pendings: []protocol.Pending{moving}}))
	m.Handle(view.Key{Rune: 'g'})
	d.act(m, view.Action{Kind: view.ActionOther, Key: view.Key{Rune: 'x'}})
	if m.Confirm != "" || !strings.Contains(m.Message, "has not yet seen the machine change") {
		t.Errorf("x on an unrecorded replacement: confirm %q message %q", m.Confirm, m.Message)
	}
	d.act(m, view.Action{Kind: view.ActionOther, Key: view.Key{Rune: 'p'}})
	if m.Message != "proj/e: host replaced" {
		t.Errorf("p on an unrecorded replacement offers x: %q", m.Message)
	}
	if _, err := pendingTarget(rows.Row{Name: "proj/c", Pending: &gone}); err == nil || !strings.Contains(err.Error(), "gone") {
		t.Errorf("enter on a gone task: %v", err)
	}
}

// In the sidebar a click that jumps gives the focus back to the pane
// that had it; a key that jumps does not touch it, nor does a click in
// the dashboard's popup, which the jump closes.
func TestClickJumpRefocuses(t *testing.T) {
	t.Setenv("LAATMUX_HOME", t.TempDir())
	cfg := dashConfig(t)
	m := dashModel(cfg)
	row := m.Selection()
	refocused := 0
	var jumped []string
	jumpErr := error(nil)
	d := &dash{ctx: context.Background(), cfg: cfg, st: newMerged(), refocus: func() { refocused++ },
		jumper: func(r rows.Row) error { jumped = append(jumped, r.ID()); return jumpErr }}
	d.jumpAction(m, view.Action{Kind: view.ActionJump, Row: row, Mouse: true})
	if refocused != 1 || len(jumped) != 1 || jumped[0] != row.ID() {
		t.Fatalf("sidebar click: refocused %d jumped %v", refocused, jumped)
	}
	// A jump refused leaves the focus on the view, with the message,
	// and selects the row clicked, ending the following.
	jumpErr = errors.New("no session")
	m.Follow = true
	other := m.Visible()[1].Row
	d.jumpAction(m, view.Action{Kind: view.ActionJump, Row: other, Mouse: true})
	if refocused != 1 || m.Message != "no session" || m.Follow || m.Selection() == nil || m.Selection().ID() != other.ID() {
		t.Fatalf("refused: refocused %d message %q follow %v selected %+v", refocused, m.Message, m.Follow, m.Selection())
	}
	// A failed jump on the row already selected, while following, still
	// makes it the user's.
	m.Follow = true
	m.Selected = 0
	first := m.Visible()[0].Row
	d.jumpAction(m, view.Action{Kind: view.ActionJump, Row: first, Mouse: true})
	if m.Follow {
		t.Fatal("a failed jump on the selected row left the selection following")
	}
	jumpErr = nil
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
// does, and moves no focus; Enter leaves following as it was.
func TestJumpNowhereSelects(t *testing.T) {
	t.Setenv("LAATMUX_HOME", t.TempDir())
	cfg := dashConfig(t)
	m := dashModel(cfg)
	m.Follow = true
	refocused := 0
	d := &dash{ctx: context.Background(), cfg: cfg, st: newMerged(), refocus: func() { refocused++ },
		jumper: func(rows.Row) error { return errors.New("no session") }}
	target := m.Visible()[1].Row
	d.jumpAction(m, view.Action{Kind: view.ActionJump, Row: target})
	if refocused != 0 || m.Follow || m.Selection() == nil || m.Selection().ID() != target.ID() {
		t.Fatalf("digit: refocused %d follow %v selected %+v", refocused, m.Follow, m.Selection())
	}
	// Enter that jumps nowhere was on the selection already: following
	// goes on. The first row is the viewer's own here.
	m.Visible()[0].Row.Current = true
	m.Follow = true
	before := m.Selection().ID()
	a := m.Handle(view.Key{Kind: view.KeyEnter})
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
		out, err := exec.Command("tmux", append([]string{"-S", sock, "-f", "/dev/null"}, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("%v: %v %s", args, err, out)
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
