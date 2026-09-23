package view

import (
	"errors"
	"strings"
	"testing"
)

func choices() []Choice {
	return []Choice{
		{Label: "laatmux", Detail: "git@github.com:laat/laatmux.git"},
		{Label: "proj", Detail: "git@github.com:laat/proj.git"},
		{Label: "other", Detail: "https://example.com/o/other.git"},
	}
}

// A picker draws its title, the filter line, the entries with the
// selection reversed and the detail dim, and the hint.
func TestPickerRender(t *testing.T) {
	p := NewPicker("add: repository", choices(), 1)
	golden(t, "picker", Debug(p.Render(60, 8)))
	p.Filter = "zzz"
	golden(t, "picker-nomatch", Debug(p.Render(60, 6)))
}

// Typing filters on the label and the detail, keeping the selection on
// the same entry while it matches; arrows and the wheel move; Enter
// picks the entry; a click picks the one under it; Esc cancels.
func TestPickerHandle(t *testing.T) {
	p := NewPicker("t", choices(), 2)
	p.Render(60, 8)
	for _, r := range "ot" {
		p.Handle(Key{Rune: r})
	}
	if m := p.Matches(); len(m) != 1 || m[0] != 2 || p.Selected != 0 {
		t.Errorf("filter ot: matches %v selected %d", m, p.Selected)
	}
	p.Handle(Key{Kind: KeyBackspace})
	p.Handle(Key{Kind: KeyBackspace})
	if p.Filter != "" || p.Selected != 2 {
		t.Errorf("after backspace: filter %q selected %d", p.Filter, p.Selected)
	}
	p.Handle(Key{Rune: 'e'}) // example.com matches by detail
	if m := p.Matches(); len(m) != 1 || m[0] != 2 {
		t.Errorf("detail filter: %v", m)
	}
	p.Handle(Key{Kind: KeyEsc})
	if !p.Done() || p.Chosen != -1 {
		t.Errorf("esc: done=%v chosen=%d", p.Done(), p.Chosen)
	}

	p = NewPicker("t", choices(), 0)
	p.Render(60, 8)
	p.Handle(Key{Kind: KeyDown})
	p.Handle(Key{Kind: KeyMouse, Wheel: 1})
	p.Handle(Key{Kind: KeyMouse, Wheel: 1})
	if p.Selected != 2 {
		t.Errorf("down past the end: %d", p.Selected)
	}
	p.Handle(Key{Kind: KeyEnter})
	if !p.Done() || p.Chosen != 2 {
		t.Errorf("enter: done=%v chosen=%d", p.Done(), p.Chosen)
	}

	p = NewPicker("t", choices(), 0)
	p.Render(60, 8)
	p.Handle(Key{Kind: KeyMouse, X: 3, Y: 4}) // title, filter, then the second entry
	if !p.Done() || p.Chosen != 1 {
		t.Errorf("click: done=%v chosen=%d", p.Done(), p.Chosen)
	}
	p = NewPicker("t", nil, 0)
	p.Handle(Key{Kind: KeyEnter})
	if p.Done() {
		t.Error("enter on an empty picker finished it")
	}
}

// A prompt edits its text, refuses Enter while the validator does and
// shows why, and cancels on Esc.
func TestPrompt(t *testing.T) {
	p := NewPrompt("branch", "fix", func(s string) error {
		if strings.HasSuffix(s, "-") {
			return errors.New("must not end with -")
		}
		return nil
	})
	p.Handle(Key{Rune: '-'})
	p.Handle(Key{Kind: KeyEnter})
	if p.Done() || p.Error != "must not end with -" {
		t.Errorf("invalid: done=%v error=%q", p.Done(), p.Error)
	}
	golden(t, "prompt", Debug(p.Render(40, 5)))
	p.Handle(Key{Kind: KeyBackspace})
	if p.Error != "" {
		t.Error("error stayed after a key")
	}
	p.Handle(Key{Kind: KeyEnter})
	if !p.Done() || p.Text != "fix" {
		t.Errorf("valid: done=%v text=%q", p.Done(), p.Text)
	}
	p = NewPrompt("branch", "", nil)
	p.Handle(Key{Kind: KeyEsc})
	if !p.Done() || !p.Cancelled {
		t.Error("esc did not cancel")
	}
}

// A log shows its last lines and is done at once when the command
// succeeds; a failure stays until a key; Ctrl-C while running quits.
func TestLog(t *testing.T) {
	l := NewLog("add proj/x on vm with claude")
	for _, s := range []string{"resolve   done  git@x:o/proj.git", "worktree  start /w/x", "worktree  done  /w/x"} {
		l.Append(s)
	}
	golden(t, "log-running", Debug(l.Render(50, 4)))
	l.Handle(Key{Rune: 'q'})
	if l.Done() {
		t.Error("q while running finished the log")
	}
	l.End(errors.New("rm failed: git worktree remove /w/x: fatal: '/w/x' contains modified or untracked files, use --force to delete it  (X force-removes)"))
	golden(t, "log-failed", Debug(l.Render(50, 8)))
	if l.Done() {
		t.Error("a failure finished without a key")
	}
	l.Handle(Key{Kind: KeyMouse, Wheel: 1})
	if l.Done() {
		t.Error("the wheel acknowledged the failure")
	}
	l.Handle(Key{Rune: 'x'})
	if !l.Done() {
		t.Error("a key did not acknowledge the failure")
	}
	l = NewLog("t")
	l.End(nil)
	if !l.Done() {
		t.Error("success did not finish at once")
	}
	l = NewLog("t")
	l.Handle(Key{Kind: KeyCtrlC})
	if !l.Done() || !l.Quit {
		t.Error("ctrl-c did not quit")
	}
}

// Wrapping breaks at a space when one is near the end, else mid-word,
// and never loses text.
func TestWrap(t *testing.T) {
	got := wrap("git worktree remove /w/x: fatal: contains modified files, use --force", 30)
	want := []string{"git worktree remove /w/x:", "fatal: contains modified", "files, use --force"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("wrap = %q", got)
	}
	if got := wrap(strings.Repeat("x", 25), 10); len(got) != 3 || got[2] != "xxxxx" {
		t.Errorf("mid-word = %q", got)
	}
	// A wide rune at a width of one cell still moves on.
	if got := wrap("日本x", 1); strings.Join(got, "|") != "日|本|x" {
		t.Errorf("wide at width 1 = %q", got)
	}
}

// With an overlay up the model hands it every key and reports its
// finishing; a confirm line takes the next key, y confirming and
// anything else withdrawing it; the footer shows the question.
func TestModelOverlayAndConfirm(t *testing.T) {
	m := &Model{Width: 40, Height: 5, Hint: "hint"}
	p := NewPicker("t", choices(), 0)
	m.Overlay = p
	if a := m.Handle(Key{Rune: 'p'}); a.Kind != ActionNone || p.Filter != "p" {
		t.Errorf("p with a picker up: %+v filter=%q", a, p.Filter)
	}
	if got := Text(m.Render()); !strings.HasPrefix(got, "t\n> p_\n") {
		t.Errorf("render did not draw the overlay:\n%s", got)
	}
	if a := m.Handle(Key{Kind: KeyEnter}); a.Kind != ActionOverlay {
		t.Errorf("enter: %+v", a)
	}
	m.Overlay = nil
	m.Ask("remove proj/x on vm (/w/x)? y/n", "rm")
	if got := Text(m.Render()); !strings.Contains(got, "remove proj/x on vm (/w/x)? y/n") {
		t.Errorf("confirm not in the footer:\n%s", got)
	}
	if a := m.Handle(Key{Rune: 'n'}); a.Kind != ActionNone || m.Confirm != "" || m.ConfirmTag != "" {
		t.Errorf("n: %+v confirm=%q tag=%q", a, m.Confirm, m.ConfirmTag)
	}
	m.Ask("q?", "rm")
	if a := m.Handle(Key{Rune: 'y'}); a.Kind != ActionConfirm || m.ConfirmTag != "rm" || m.Confirm != "" {
		t.Errorf("y: %+v tag=%q", a, m.ConfirmTag)
	}
	l := NewLog("t")
	m.Overlay = l
	if a := m.Poll(); a.Kind != ActionNone {
		t.Errorf("poll while running: %+v", a)
	}
	l.End(nil)
	if a := m.Poll(); a.Kind != ActionOverlay {
		t.Errorf("poll after success: %+v", a)
	}
}
