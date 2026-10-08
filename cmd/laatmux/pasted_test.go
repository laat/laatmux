package main

import (
	"context"
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
}
