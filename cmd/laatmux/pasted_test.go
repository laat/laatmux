package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/laat/laatmux/internal/command"
	"github.com/laat/laatmux/internal/config"
	"github.com/laat/laatmux/internal/home"
	"github.com/laat/laatmux/internal/merged"
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
// the config does not list becomes the add's repository, named as the
// config will list it, with the host and agent an unknown repository
// gets, and the add asks for it to be remembered; the same paste of a
// listed repository in another form is that repository, and the add
// does not; a paste that is no source takes nothing.
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
	if got.Repo.Source != "git@github.com:nrkno/pin-scripts.git" || got.Repo.Name != "pin-scripts" || !got.Remember || got.Host.Name != "mac" || got.Agent != "claude" {
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
	if got.Repo.Name != "proj" || got.Remember {
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
	if n := form.Note(form); !strings.Contains(n, "no repositories configured") {
		t.Fatalf("note %q", n)
	}
	form.SetPrompt("Fix it")
	form.Handle(term.Key{Kind: term.KeyEnter})
	if form.Done() || form.Error != "no repository chosen" {
		t.Fatalf("submit without a repository: %v %q", form.Done(), form.Error)
	}
	pick(form, "git@github.com:nrkno/pin-scripts.git")
	form.Handle(term.Key{Kind: term.KeyNewline})
	if !d.act(m, m.Poll()) || got.Repo.Name != "pin-scripts" || !got.Remember {
		t.Fatalf("add %+v, message %q", got, m.Message)
	}
}

// add's --repo: a listed repository by name or by a source in any form,
// not new; a source the config does not list, new under the name it
// will be listed as; a name that is neither, refused as before.
func TestAddRepoFlag(t *testing.T) {
	cfg := dashConfig(t)
	ctx := context.Background()
	for _, c := range []struct {
		flag, name string
		isNew      bool
	}{
		{"proj", "proj", false},
		{"https://github.com/laat/proj", "proj", false},
		{"git@github.com:nrkno/pin-scripts.git", "pin-scripts", true},
		{"https://github.com/nrkno/proj.git", "nrkno-proj", true},
	} {
		r, isNew, err := addRepo(ctx, cfg, c.flag)
		if err != nil || r.Name != c.name || isNew != c.isNew {
			t.Errorf("%s: %+v %v %v", c.flag, r, isNew, err)
		}
	}
	if _, _, err := addRepo(ctx, cfg, "nope"); err == nil || !strings.Contains(err.Error(), `unknown repository "nope"`) {
		t.Errorf("nope: %v", err)
	}
	if _, isNew, err := addRepo(ctx, config.Config{}, "/srv/git/proj.git"); err == nil || isNew {
		t.Errorf("a path is no forge source: %v %v", isNew, err)
	}
	r, isNew, err := addRepo(ctx, cfg, "https://laat:ghp_secret@github.com/nrkno/pin-scripts.git")
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

// serve's hooks on the config file: the store's read answers the list
// at first and after the relay's append, and not between.
func TestConfigHooks(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.yaml")
	t.Setenv("LAATMUX_CONFIG", p)
	t.Setenv("LAATMUX_HOME", t.TempDir())
	if err := os.WriteFile(p, []byte("repos:\n  - git@x:o/a.git\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	readRepos, appendRepo := configHooks()
	if rs, changed, err := readRepos(); err != nil || !changed || len(rs) != 1 || rs[0].Name != "a" {
		t.Fatalf("first read: %v %v %v", rs, changed, err)
	}
	if _, changed, _ := readRepos(); changed {
		t.Fatal("changed with no change")
	}
	if added, err := appendRepo("git@x:o/p.git", "p"); err != nil || !added {
		t.Fatalf("append: %v %v", added, err)
	}
	if rs, changed, err := readRepos(); err != nil || !changed || len(rs) != 2 || rs[1].Source != "git@x:o/p.git" || rs[1].Name != "p" {
		t.Fatalf("after the append: %v %v %v", rs, changed, err)
	}
}
