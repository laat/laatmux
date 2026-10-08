package view

import (
	"path"
	"strings"
	"testing"

	"github.com/laat/laatmux/internal/term"
)

// pasted stands in for the host's Other: a filter that starts git@ is
// a source, proj's in the form without .git, else a new repository
// named for its last element; anything else names nothing.
func pasted(filter string) (Choice, bool) {
	switch {
	case filter == "git@github.com:laat/proj":
		return choices()[1], true
	case strings.HasPrefix(filter, "git@"):
		return Choice{Label: strings.TrimSuffix(path.Base(filter), ".git"), Detail: filter}, true
	}
	return Choice{}, false
}

// A filter Other names an entry for is that entry alone: a new one,
// marked new, taken with Chosen past the choices and the entry in
// Taken, even where its text is part of a listed source; a choice in
// another form, taken as that choice; by Enter or by a click. A filter
// that names nothing filters as before, and Enter with no match does
// nothing.
func TestPickerOther(t *testing.T) {
	p := NewPicker("t", choices(), 0)
	p.Other = pasted
	p.Handle(term.Key{Kind: term.KeyPaste, Text: "git@github.com:nrkno/pin-scripts.git"})
	text := Text(p.Render(80, 8))
	if !strings.Contains(text, "  pin-scripts  git@github.com:nrkno/pin-scripts.git  new\n") || strings.Contains(text, "no match") {
		t.Fatalf("render:\n%s", text)
	}
	p.Handle(term.Key{Kind: term.KeyEnter})
	if !p.Done() || p.Chosen != len(choices()) || p.Taken != (Choice{Label: "pin-scripts", Detail: "git@github.com:nrkno/pin-scripts.git"}) {
		t.Fatalf("enter: done %v chosen %d taken %+v", p.Done(), p.Chosen, p.Taken)
	}

	// Part of laatmux's source, and a repository of its own.
	p = NewPicker("t", choices(), 0)
	p.Other = pasted
	p.Handle(term.Key{Kind: term.KeyPaste, Text: "git@github.com:laat/laat"})
	if len(p.Matches()) != 1 {
		t.Fatalf("the filter is not in laatmux's source: %v", p.Matches())
	}
	p.Render(80, 8)
	p.Handle(term.Key{Kind: term.KeyEnter})
	if p.Chosen != len(choices()) || p.Taken.Label != "laat" {
		t.Fatalf("a source inside a listed one: chosen %d taken %+v", p.Chosen, p.Taken)
	}

	// proj in another form: proj, not new.
	p = NewPicker("t", choices(), 0)
	p.Other = pasted
	p.Handle(term.Key{Kind: term.KeyPaste, Text: "git@github.com:laat/proj"})
	if text := Text(p.Render(80, 8)); strings.Contains(text, "new") || !strings.Contains(text, "  proj  git@github.com:laat/proj.git") {
		t.Fatalf("render of a listed source:\n%s", text)
	}
	p.Handle(term.Key{Kind: term.KeyMouse, X: 3, Y: 3}) // title, filter, the entry
	if !p.Done() || p.Chosen != 1 {
		t.Fatalf("click: done %v chosen %d", p.Done(), p.Chosen)
	}

	// No source: the filter as before.
	p = NewPicker("t", choices(), 0)
	p.Other = pasted
	for _, r := range "proj" {
		p.Handle(term.Key{Rune: r})
	}
	p.Handle(term.Key{Kind: term.KeyEnter})
	if !p.Done() || p.Chosen != 1 {
		t.Fatalf("filter proj: done %v chosen %d", p.Done(), p.Chosen)
	}
	p = NewPicker("t", choices(), 0)
	p.Other = pasted
	p.Handle(term.Key{Kind: term.KeyPaste, Text: "zzz"})
	if text := Text(p.Render(80, 8)); !strings.Contains(text, "no match") {
		t.Fatalf("render with nothing:\n%s", text)
	}
	p.Handle(term.Key{Kind: term.KeyEnter})
	if p.Done() {
		t.Fatal("enter with no match and no source finished the picker")
	}
}

// On the form, the entry taken is added to the chip's candidates and
// selected, and the host told; a chip with no candidates opens its
// picker when it has Other, and a submit with nothing chosen there is
// refused.
func TestFormTakesOther(t *testing.T) {
	cs := chips()
	cs[0].Other = pasted
	f := NewForm("add a task", cs, "")
	f.Propose = propose
	var changed []int
	f.Changed = func(_ *Form, chip int) { changed = append(changed, chip) }
	f.SetPrompt("Fix it")
	for range 3 {
		f.Handle(term.Key{Kind: term.KeyShiftTab}) // to the repository chip
	}
	f.Handle(term.Key{Kind: term.KeyEnter})
	f.Handle(term.Key{Kind: term.KeyPaste, Text: "git@github.com:nrkno/pin-scripts.git"})
	f.Handle(term.Key{Kind: term.KeyEnter})
	c := f.Chips[0]
	if f.picker != nil || len(c.Choices) != 3 || c.Selected != 2 || c.Label() != "pin-scripts" || c.Choices[2].Detail != "git@github.com:nrkno/pin-scripts.git" {
		t.Fatalf("chip after the take: %+v", c)
	}
	if len(changed) != 1 || changed[0] != 0 {
		t.Fatalf("changed %v", changed)
	}
	// The same source again is the candidate it became.
	f.Handle(term.Key{Kind: term.KeyEnter})
	f.Handle(term.Key{Kind: term.KeyPaste, Text: "git@github.com:nrkno/pin-scripts.git"})
	f.Handle(term.Key{Kind: term.KeyEnter})
	if len(f.Chips[0].Choices) != 3 || f.Chips[0].Selected != 2 {
		t.Fatalf("the source taken twice: %+v", f.Chips[0])
	}
	f.Handle(term.Key{Kind: term.KeyNewline})
	if !f.Done() || f.Chips[0].Label() != "pin-scripts" {
		t.Fatalf("submit: done %v error %q", f.Done(), f.Error)
	}

	cs = chips()
	cs[0].Choices, cs[0].Other = nil, pasted
	g := NewForm("add a task", cs, "")
	g.Propose = propose
	g.SetPrompt("Fix it")
	g.Handle(term.Key{Kind: term.KeyEnter})
	if g.Done() || g.Error != "no repository chosen" {
		t.Fatalf("submit with no repository: done %v error %q", g.Done(), g.Error)
	}
	for range 3 {
		g.Handle(term.Key{Kind: term.KeyShiftTab})
	}
	g.Handle(term.Key{Kind: term.KeyEnter})
	if g.picker == nil {
		t.Fatal("enter on an empty chip with Other did not open the picker")
	}
	g.Handle(term.Key{Kind: term.KeyPaste, Text: "git@github.com:nrkno/pin-scripts.git"})
	g.Handle(term.Key{Kind: term.KeyEnter})
	g.Handle(term.Key{Kind: term.KeyNewline})
	if !g.Done() || g.Chips[0].Label() != "pin-scripts" {
		t.Fatalf("submit after the take: done %v error %q", g.Done(), g.Error)
	}
	// Without Other an empty chip opens nothing, as before.
	cs = chips()
	cs[0].Choices = nil
	h := NewForm("add a task", cs, "")
	for range 3 {
		h.Handle(term.Key{Kind: term.KeyShiftTab})
	}
	h.Handle(term.Key{Kind: term.KeyEnter})
	if h.picker != nil {
		t.Fatal("an empty chip without Other opened a picker")
	}
}
