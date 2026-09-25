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
	failed := command.Added{Done: false, Answered: true, Stage: protocol.StageAgent, Root: "/r/b", Prompt: protocol.DeliveryUnknown, Reason: "new-session failed after the session may have been made"}
	if n := undelivered(add, failed); n == nil || !strings.Contains(view.Text(n.Render(80, 12)), "prompt unknown") || strings.Contains(view.Text(n.Render(80, 12)), "without") {
		t.Fatalf("unknown delivery on a failed add:\n%s", view.Text(n.Render(80, 12)))
	}
	// A failure before the agent stage is positively before the send;
	// no result at all is unknown.
	early := command.Added{Answered: true, Stage: protocol.StageFetch}
	if n := undelivered(add, early); n == nil || !strings.Contains(view.Text(n.Render(80, 12)), "failed at fetch, before the prompt was sent") {
		t.Fatalf("early failure:\n%s", view.Text(n.Render(80, 12)))
	}
	lost := command.Added{}
	if n := undelivered(add, lost); n == nil || !strings.Contains(view.Text(n.Render(80, 12)), "outcome unknown") || strings.Contains(view.Text(n.Render(80, 12)), "not sent") {
		t.Fatalf("lost result:\n%s", view.Text(n.Render(80, 12)))
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
}
