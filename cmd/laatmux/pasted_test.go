package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/laat/laatmux/internal/command"
	"github.com/laat/laatmux/internal/config"
	"github.com/laat/laatmux/internal/home"
	"github.com/laat/laatmux/internal/merged"
	"github.com/laat/laatmux/internal/protocol"
	"github.com/laat/laatmux/internal/term"
	"github.com/laat/laatmux/internal/view"
)

// pick opens the repository chip's picker from the prompt, pastes text
// into its filter and presses Enter.
func pick(form *view.Form, text string) {
	for form.Focus() != 0 {
		form.Handle(term.Key{Kind: term.KeyShiftTab})
	}
	form.Handle(term.Key{Kind: term.KeyEnter})
	form.Handle(term.Key{Kind: term.KeyPaste, Text: text})
	form.Handle(term.Key{Kind: term.KeyEnter})
}

// A repository's source pasted into the repository chip's picker that
// no known repository has becomes the add's repository, named among the
// known ones, with the host and agent an unknown repository gets, and
// its entry carries the source for the host to clone; the same paste of
// a listed repository in another form is that repository; a paste that
// is no source takes nothing.
func TestFormPastedSource(t *testing.T) {
	t.Setenv("LAATMUX_HOME", t.TempDir())
	cfg := dashConfig(t)
	f := &addForm{repos: cfg.Repos, hosts: cfg.Hosts[:2], agents: cfg.AgentNames()}
	var last home.Last
	last.Set("git@github.com:laat/laatmux.git", home.LastRepo{Host: "vm", Agent: "codex"})
	form := buildForm(cfg, f, last, "laatmux", "", "", nil)
	if form.Chips[1].Label() != "vm" || form.Chips[2].Label() != "codex" {
		t.Fatalf("laatmux's last: %q %q", form.Chips[1].Label(), form.Chips[2].Label())
	}
	pick(form, "  git@github.com:nrkno/pin-scripts.git\n")
	if form.Chips[0].Label() != "pin-scripts" || form.Chips[1].Label() != "mac" || form.Chips[2].Label() != "claude" {
		t.Fatalf("after the paste: %q %q %q", form.Chips[0].Label(), form.Chips[1].Label(), form.Chips[2].Label())
	}
	if n := form.Note(form); !strings.Contains(n, "new repository") {
		t.Fatalf("note %q", n)
	}
	form.SetPrompt("Fix it")
	form.Handle(term.Key{Kind: term.KeyNewline})
	if !form.Done() {
		t.Fatalf("submit: %q", form.Error)
	}
	d := &dash{ctx: context.Background(), cfg: cfg, st: merged.New(), add: f}
	var got command.Add
	d.submit = func(a command.Add) (string, error) { got = a; return "add-1", nil }
	if !d.submitForm(&view.Model{}, f, form) {
		t.Fatal("not accepted")
	}
	if got.Repo.Source != "git@github.com:nrkno/pin-scripts.git" || got.Repo.Name != "pin-scripts" || got.Host.Name != "mac" || got.Agent != "claude" {
		t.Fatalf("add %+v", got)
	}
	if e := got.Request("x").RepoEntry; e == nil || e.Source != got.Repo.Source || e.Name != "pin-scripts" {
		t.Fatalf("entry %+v", e)
	}

	// proj by its https form: proj, not new; its note is none.
	form = buildForm(cfg, f, last, "laatmux", "", "", nil)
	pick(form, "https://github.com/laat/proj")
	if form.Chips[0].Label() != "proj" || form.Chips[0].Selected != 1 || len(form.Chips[0].Choices) != 2 {
		t.Fatalf("a listed source: %+v", form.Chips[0])
	}
	if n := form.Note(form); n != "" {
		t.Fatalf("note %q", n)
	}
	form.SetPrompt("Fix it")
	form.Handle(term.Key{Kind: term.KeyNewline})
	d.submitForm(&view.Model{}, f, form)
	if got.Repo.Name != "proj" {
		t.Fatalf("add %+v", got)
	}

	// No source: the picker stays up with no match.
	form = buildForm(cfg, f, last, "laatmux", "", "", nil)
	pick(form, "pin-scripts")
	if form.Chips[0].Label() != "laatmux" || !strings.Contains(view.Text(form.Render(80, 12)), "no match") {
		t.Fatalf("no source took %q", form.Chips[0].Label())
	}

	// A config without repositories: the form is up, says how to get
	// one, refuses to submit without one, and takes a paste.
	cfg.Repos = nil
	f = &addForm{hosts: cfg.Hosts[:2], agents: cfg.AgentNames()}
	d = &dash{ctx: context.Background(), cfg: cfg, st: merged.New()}
	d.submit = func(a command.Add) (string, error) { got = a; return "add-2", nil }
	m := dashModel(cfg)
	d.act(m, view.Action{Kind: view.ActionOther, Key: term.Key{Rune: 'a'}})
	form, ok := m.Overlay.(*view.Form)
	if !ok {
		t.Fatalf("no form without repositories: %q", m.Message)
	}
	if n := form.Note(form); !strings.Contains(n, "no repositories known") {
		t.Fatalf("note %q", n)
	}
	form.SetPrompt("Fix it")
	form.Handle(term.Key{Kind: term.KeyEnter})
	if form.Done() || form.Error != "no repository chosen" {
		t.Fatalf("submit without a repository: %v %q", form.Done(), form.Error)
	}
	pick(form, "git@github.com:nrkno/pin-scripts.git")
	form.Handle(term.Key{Kind: term.KeyNewline})
	if !d.act(m, m.Poll()) || got.Repo.Name != "pin-scripts" {
		t.Fatalf("add %+v, message %q", got, m.Message)
	}
}

// add's --repo: a listed repository by name or by a source in any form,
// not new; a repository a host has discovered, by its label or its
// source, not new either; a source no known repository has, new under a
// name no known repository has; a name that is neither, refused naming
// what is known.
func TestAddRepoFlag(t *testing.T) {
	cfg := dashConfig(t)
	ctx := context.Background()
	st := merged.New()
	st.Apply(protocol.Message{Type: protocol.TypeSnapshot, Hosts: []protocol.HostStatus{
		{Name: "vm", SSH: "vm", EnvironmentID: "venv", Connected: true, Listed: true,
			Repos: &protocol.RepoSet{Checkouts: []protocol.Checkout{{Repo: "notes", Source: "git@github.com:laat/notes.git", Root: "/r/notes"}}}},
		// box labels notes otherwise.
		{Name: "box", SSH: "box", EnvironmentID: "benv", Connected: true, Listed: true,
			Repos: &protocol.RepoSet{Checkouts: []protocol.Checkout{{Repo: "notes2", Source: "git@github.com:laat/notes.git", Root: "/r/notes2"}}}},
	}})
	known := func() merged.Known { return st.Known(cfg) }
	for _, c := range []struct {
		flag, name string
		isNew      bool
	}{
		{"proj", "proj", false},
		{"https://github.com/laat/proj", "proj", false},
		{"notes", "notes", false},
		{"notes2", "notes", false},
		{"https://github.com/laat/notes", "notes", false},
		{"git@github.com:nrkno/pin-scripts.git", "pin-scripts", true},
		{"https://github.com/nrkno/proj.git", "nrkno-proj", true},
		// notes and notes2 are hosts' labels for another source: box's
		// checkout of it is in notes2.
		{"https://github.com/nrkno/notes.git", "nrkno-notes", true},
		{"https://github.com/nrkno/notes2.git", "nrkno-notes2", true},
	} {
		r, isNew, err := addRepo(ctx, cfg, known, c.flag, "")
		if err != nil || r.Name != c.name || isNew != c.isNew {
			t.Errorf("%s: %+v %v %v", c.flag, r, isNew, err)
		}
	}
	if _, _, err := addRepo(ctx, cfg, known, "nope", ""); err == nil || err.Error() != `unknown repository "nope": not checked out on any host and not configured; known: laatmux, proj, notes` {
		t.Errorf("nope: %v", err)
	}
	none := func() merged.Known { return merged.ConfigOnly(config.Config{}) }
	if _, isNew, err := addRepo(ctx, config.Config{}, none, "/srv/git/proj.git", ""); err == nil || isNew {
		t.Errorf("a path is no forge source: %v %v", isNew, err)
	}
	r, isNew, err := addRepo(ctx, cfg, known, "https://laat:ghp_secret@github.com/nrkno/pin-scripts.git", "")
	if err != nil || !isNew || r.Source != "https://github.com/nrkno/pin-scripts.git" {
		t.Errorf("a credential: %+v %v %v", r, isNew, err)
	}
}

// The repository picker reads the config again as it opens: a source
// added since the form was made, by an earlier add, is a listed
// candidate, taken as listed, not new; the selection stays on its
// repository; a pasted candidate the config lists now is dropped for
// the listed one. A pasted URL's credential is left out, and the note
// says so.
func TestFormReloadsRepos(t *testing.T) {
	cfg := dashConfig(t)
	fresh := cfg
	fresh.Repos = append(append([]config.Repo(nil), cfg.Repos...), config.Repo{Source: "git@github.com:nrkno/pin-scripts.git", Name: "pin-scripts"})
	reads := 0
	f := &addForm{repos: cfg.Repos, hosts: cfg.Hosts[:2], agents: cfg.AgentNames(), reload: func() (config.Config, error) {
		reads++
		if reads == 1 {
			return cfg, nil
		}
		return fresh, nil
	}}
	var last home.Last
	form := buildForm(cfg, f, last, "proj", "", "", nil)
	pick(form, "https://ghp_secret@github.com/nrkno/pin-scripts")
	if form.Chips[0].Label() != "pin-scripts" || form.Chips[0].Choices[2].Detail != "https://github.com/nrkno/pin-scripts" {
		t.Fatalf("pasted: %+v", form.Chips[0])
	}
	if n := form.Note(form); !strings.Contains(n, "without the pasted URL's credential") {
		t.Fatalf("note %q", n)
	}
	if _, isNew := f.repo(form, form.Chips[0].Selected); !isNew {
		t.Fatal("not new before the config lists it")
	}
	// The config lists it now: the picker opening reads it, the pasted
	// candidate gives way to the listed one, which stays selected.
	pick(form, "git@github.com:nrkno/pin-scripts.git")
	c := form.Chips[0]
	if reads != 2 || len(c.Choices) != 3 || c.Selected != 2 || c.Choices[2].Detail != "git@github.com:nrkno/pin-scripts.git" || len(f.repos) != 3 {
		t.Fatalf("after the reload: reads %d %+v", reads, c)
	}
	if repo, isNew := f.repo(form, c.Selected); isNew || repo.Name != "pin-scripts" {
		t.Fatalf("listed now: %+v new %v", repo, isNew)
	}
	if n := form.Note(form); n != "" {
		t.Fatalf("note %q", n)
	}
}

// A pasted candidate kept over a reload is named again among the
// repositories listed now: one whose name another repository took since
// gets a name of its own, so the add never carries a name the config
// gives another source.
func TestFormRefreshRenames(t *testing.T) {
	cfg := dashConfig(t)
	fresh := cfg
	fresh.Repos = append(append([]config.Repo(nil), cfg.Repos...), config.Repo{Source: "git@github.com:b/scripts.git", Name: "scripts"})
	reads := 0
	f := &addForm{repos: cfg.Repos, hosts: cfg.Hosts[:2], agents: cfg.AgentNames(), reload: func() (config.Config, error) {
		reads++
		if reads == 1 {
			return cfg, nil
		}
		return fresh, nil
	}}
	form := buildForm(cfg, f, home.Last{}, "proj", "", "", nil)
	pick(form, "git@github.com:a/scripts.git")
	if form.Chips[0].Label() != "scripts" {
		t.Fatalf("pasted as %q", form.Chips[0].Label())
	}
	for form.Focus() != 0 {
		form.Handle(term.Key{Kind: term.KeyShiftTab})
	}
	form.Handle(term.Key{Kind: term.KeyEnter}) // the picker opens, the config read again
	form.Handle(term.Key{Kind: term.KeyEsc})
	c := form.Chips[0]
	if len(c.Choices) != 4 || c.Choices[2].Label != "scripts" || c.Choices[3].Label != "a-scripts" || c.Selected != 3 {
		t.Fatalf("after the reload: %+v", c)
	}
	if repo, isNew := f.repo(form, c.Selected); !isNew || repo.Name != "a-scripts" || repo.Source != "git@github.com:a/scripts.git" {
		t.Fatalf("repo %+v new %v", repo, isNew)
	}
}

// A reload keeps the selection on its repository by source, wherever
// the config puts it now; a selected repository the config no longer
// lists gives way to the first, and the host and agent follow it; a chip
// that had nothing to choose from has nothing chosen after.
func TestFormRefreshSelection(t *testing.T) {
	cfg := dashConfig(t)
	var last home.Last
	last.Set("git@github.com:laat/laatmux.git", home.LastRepo{Host: "vm", Agent: "codex"})
	var next config.Config
	f := &addForm{repos: cfg.Repos, hosts: cfg.Hosts[:2], agents: cfg.AgentNames(), reload: func() (config.Config, error) { return next, nil }}
	open := func(form *view.Form) {
		for form.Focus() != 0 {
			form.Handle(term.Key{Kind: term.KeyShiftTab})
		}
		form.Handle(term.Key{Kind: term.KeyEnter})
		form.Handle(term.Key{Kind: term.KeyEsc})
	}
	form := buildForm(cfg, f, last, "proj", "", "", nil)
	// A repository listed before proj: proj is still the one chosen.
	next = cfg
	next.Repos = append([]config.Repo{{Source: "git@github.com:laat/first.git", Name: "first"}}, cfg.Repos...)
	open(form)
	if form.Chips[0].Label() != "proj" || form.Chips[0].Selected != 2 {
		t.Fatalf("after an entry before it: %+v", form.Chips[0])
	}
	// proj gone: the first, laatmux here, with its last host and agent.
	next.Repos = []config.Repo{cfg.Repos[0]}
	open(form)
	if form.Chips[0].Label() != "laatmux" || form.Chips[1].Label() != "vm" || form.Chips[2].Label() != "codex" {
		t.Fatalf("after proj went: %q %q %q", form.Chips[0].Label(), form.Chips[1].Label(), form.Chips[2].Label())
	}
	// A chip that had nothing: nothing chosen, and a submit is refused.
	empty := cfg
	empty.Repos = nil
	f = &addForm{hosts: cfg.Hosts[:2], agents: cfg.AgentNames(), reload: func() (config.Config, error) { return cfg, nil }}
	form = buildForm(empty, f, last, "", "", "", nil)
	open(form)
	if len(form.Chips[0].Choices) != 2 || form.Chips[0].Label() != "" {
		t.Fatalf("an empty chip after a reload: %+v", form.Chips[0])
	}
	form.SetPrompt("Fix it")
	form.Handle(term.Key{Kind: term.KeyNewline})
	if form.Done() || form.Error != "no repository chosen" {
		t.Fatalf("submit: %v %q", form.Done(), form.Error)
	}
}

// A form left up while the config changes takes the change as a picker
// opens: the host and agent chips offer the hosts and agents listed
// now, each kept on its candidate by name, else on the default the
// config gives now; the submit sends the copy rules of that read.
func TestFormReloadsHostsAndAgents(t *testing.T) {
	t.Setenv("LAATMUX_HOME", t.TempDir())
	cfg := dashConfig(t)
	next, err := config.Parse([]byte(`
hosts:
  - name: vm
    ssh: vm
    repos: /r
    worktrees: /w
  - name: pc
    repos: /r
    worktrees: /w
agents:
  claude: {cmd: [claude]}
  gemini: {cmd: [gemini]}
default_agent: gemini
repos:
  - git@github.com:laat/laatmux.git
  - git@github.com:laat/proj.git
copy: ["*.local"]
`))
	if err != nil {
		t.Fatal(err)
	}
	f := &addForm{repos: cfg.Repos, hosts: addHosts(cfg), agents: cfg.AgentNames(), reload: func() (config.Config, error) { return next, nil }}
	form := buildForm(cfg, f, home.Last{}, "proj", "", "", nil)
	if form.Chips[1].Label() != "mac" || form.Chips[2].Label() != "claude" {
		t.Fatalf("at build: %q %q", form.Chips[1].Label(), form.Chips[2].Label())
	}
	// The user picks vm and codex by cycling.
	for form.Focus() != 1 {
		form.Handle(term.Key{Kind: term.KeyShiftTab})
	}
	form.Handle(term.Key{Kind: term.KeyRight})
	form.Handle(term.Key{Kind: term.KeyTab})
	form.Handle(term.Key{Kind: term.KeyRight})
	if form.Chips[1].Label() != "vm" || form.Chips[2].Label() != "codex" {
		t.Fatalf("picked: %q %q", form.Chips[1].Label(), form.Chips[2].Label())
	}
	// The agent picker opens with the config changed: vm stays, codex
	// is gone and the default is gemini now.
	form.Handle(term.Key{Kind: term.KeyEnter})
	form.Handle(term.Key{Kind: term.KeyEsc})
	labels := func(c view.Chip) string {
		var out []string
		for _, ch := range c.Choices {
			out = append(out, ch.Label)
		}
		return strings.Join(out, ",")
	}
	if labels(form.Chips[1]) != "vm,pc" || form.Chips[1].Label() != "vm" || labels(form.Chips[2]) != "claude,gemini" || form.Chips[2].Label() != "gemini" {
		t.Fatalf("after the reload: hosts %s (%q) agents %s (%q)", labels(form.Chips[1]), form.Chips[1].Label(), labels(form.Chips[2]), form.Chips[2].Label())
	}
	form.SetPrompt("Fix it")
	form.Handle(term.Key{Kind: term.KeyNewline})
	if !form.Done() {
		t.Fatalf("submit: %q", form.Error)
	}
	d := &dash{ctx: context.Background(), cfg: cfg, st: merged.New(), add: f}
	var got command.Add
	d.submit = func(a command.Add) (string, error) { got = a; return "add-1", nil }
	if !d.submitForm(&view.Model{}, f, form) {
		t.Fatal("not accepted")
	}
	if got.Host.Name != "vm" || got.Agent != "gemini" || got.Repo.Name != "proj" || strings.Join(got.Copy, ",") != "*.local" {
		t.Fatalf("add %+v", got)
	}
}

// A picker opening on a config file that does not load keeps what the
// form offers and says why in the note, until a read succeeds.
func TestFormReloadFails(t *testing.T) {
	cfg := dashConfig(t)
	f := &addForm{repos: cfg.Repos, hosts: addHosts(cfg), agents: cfg.AgentNames(), reload: func() (config.Config, error) { return config.Config{}, errors.New("yaml: bad") }}
	form := buildForm(cfg, f, home.Last{}, "proj", "", "", nil)
	open := func() {
		for form.Focus() != 2 {
			form.Handle(term.Key{Kind: term.KeyShiftTab})
		}
		form.Handle(term.Key{Kind: term.KeyEnter})
		form.Handle(term.Key{Kind: term.KeyEsc})
	}
	open()
	if n := form.Note(form); n != "config: yaml: bad; the form keeps what it offers" || len(form.Chips[1].Choices) != 2 || form.Chips[2].Label() != "claude" {
		t.Fatalf("after a failed read: note %q hosts %+v agent %q", n, form.Chips[1].Choices, form.Chips[2].Label())
	}
	f.reload = func() (config.Config, error) { return cfg, nil }
	open()
	if n := form.Note(form); n != "" {
		t.Fatalf("after a read that succeeded: note %q", n)
	}
}

// serve's read of the config file answers the list at first and after
// a change, and not between, with the copy rules for every worktree, a
// repository's copy and setup and the agents' commands as the file has
// them at each read.
func TestConfigReread(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.yaml")
	t.Setenv("LAATMUX_CONFIG", p)
	t.Setenv("LAATMUX_HOME", t.TempDir())
	if err := os.WriteFile(p, []byte("repos:\n  - git@x:o/a.git\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	reread := configReread()
	if r, changed, err := reread(); err != nil || !changed || len(r.Listed.Repos) != 1 || r.Listed.Repos[0].Name != "a" || r.Listed.Copy != nil || len(r.Agents) != 0 {
		t.Fatalf("first read: %+v %v %v", r, changed, err)
	}
	if _, changed, _ := reread(); changed {
		t.Fatal("changed with no change")
	}
	if err := os.WriteFile(p, []byte("repos:\n  - git@x:o/a.git\n  - git@x:o/p.git\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if r, changed, err := reread(); err != nil || !changed || len(r.Listed.Repos) != 2 || r.Listed.Repos[1].Source != "git@x:o/p.git" || r.Listed.Repos[1].Name != "p" {
		t.Fatalf("after the edit: %+v %v %v", r, changed, err)
	}
	edited := "copy: [\"*.local\"]\nagents:\n  claude: {cmd: [claude, --edited]}\nrepos:\n  - source: git@x:o/a.git\n    copy: [.envrc]\n    setup: [make]\n"
	if err := os.WriteFile(p, []byte(edited), 0o600); err != nil {
		t.Fatal(err)
	}
	r, changed, err := reread()
	l := r.Listed
	if err != nil || !changed || len(l.Repos) != 1 || fmt.Sprint(l.Copy, l.Repos[0].Copy, l.Repos[0].Setup, r.Agents) != "[*.local] [.envrc] [make] map[claude:[claude --edited]]" {
		t.Fatalf("after the edit: %+v %v %v", r, changed, err)
	}
	// The hosts' read: a file being written in place, empty for now,
	// keeps the daemon's hosts rather than leave the local one alone.
	if err := os.WriteFile(p, []byte("hosts:\n  - name: mac\n  - name: vm\n    ssh: vm\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if hosts, err := configHosts(); err != nil || len(hosts) != 2 {
		t.Fatalf("hosts: %v %v", hosts, err)
	}
	if err := os.WriteFile(p, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if hosts, err := configHosts(); err != config.ErrWriting {
		t.Fatalf("hosts of a file being written: %v %v", hosts, err)
	}
}
