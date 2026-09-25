package view

import (
	"errors"
	"strings"
	"testing"
	"time"
	"unicode"
)

// propose is a stand-in for the branch proposal: the first words,
// lowercased, joined with dashes.
func propose(prompt string) string {
	var words []string
	for _, w := range strings.FieldsFunc(strings.ToLower(prompt), func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) }) {
		words = append(words, w)
	}
	return strings.Join(words, "-")
}

func chips() [3]Chip {
	return [3]Chip{
		{Title: "repository", Choices: []Choice{{Label: "laatmux", Detail: "git@github.com:laat/laatmux.git"}, {Label: "proj"}}},
		{Title: "host", Choices: []Choice{{Label: "vm", Detail: "ssh vm"}, {Label: "mac"}}, Selected: 0},
		{Title: "agent", Choices: []Choice{{Label: "claude"}}},
	}
}

// The form draws the title, the chips with the focused one coloured,
// the prompt box with the cursor, the branch line following the prompt
// dim, and the hint; a branch the user edited is not dim.
func TestFormRender(t *testing.T) {
	f := NewForm("add a task", chips(), "")
	f.Propose = propose
	golden(t, "form-empty", Debug(f.Render(60, 12)))
	f.SetPrompt("Make the sidebar follow the current row when the sort moves it, and select nothing when no row is this session.")
	golden(t, "form-prompt", Debug(f.Render(60, 12)))
	f.Handle(Key{Kind: KeyTab})
	f.Handle(Key{Rune: '-'})
	f.Handle(Key{Rune: '2'})
	golden(t, "form-branch", Debug(f.Render(60, 12)))
	f.Handle(Key{Kind: KeyTab})
	golden(t, "form-chip", Debug(f.Render(60, 12)))
	f.Handle(Key{Kind: KeyEnter})
	golden(t, "form-picker", Debug(f.Render(60, 12)))
	f.Handle(Key{Kind: KeyEsc})
	f.Note = func(f *Form) string {
		if f.Chips[fieldHost].Label() == "vm" {
			return "tasks not supported by vm's daemon"
		}
		return ""
	}
	f.Error = "\"bad..name\" is not a valid branch name"
	golden(t, "form-error", Debug(f.Render(60, 12)))
	// Narrow: the chips lose their frames, the box keeps its width.
	golden(t, "form-narrow", Debug(NewForm("add a task", chips(), "fix").Render(14, 10)))
	// A focused branch longer than the room shows its end, cursor
	// included.
	long := NewForm("t", chips(), strings.Repeat("abcdefghij", 6))
	long.Handle(Key{Kind: KeyTab})
	if text := Text(long.Render(40, 12)); !strings.Contains(text, "…") || !strings.Contains(text, "hij█") {
		t.Fatalf("long branch:\n%s", text)
	}
}

// The renderer is a pure function of the fields, the cursor and the
// size: a render at one height then another draws what a render at
// the second alone draws.
func TestFormRenderStateless(t *testing.T) {
	f := NewForm("t", chips(), "")
	f.SetPrompt(strings.Repeat("line\n", 30))
	f.Handle(Key{Kind: KeyUp})
	f.Handle(Key{Kind: KeyUp})
	f.Handle(Key{Kind: KeyUp})
	f.Render(40, 10)
	after := Text(f.Render(40, 12))
	g := NewForm("t", chips(), "")
	g.SetPrompt(strings.Repeat("line\n", 30))
	g.Handle(Key{Kind: KeyUp})
	g.Handle(Key{Kind: KeyUp})
	g.Handle(Key{Kind: KeyUp})
	if fresh := Text(g.Render(40, 12)); fresh != after {
		t.Fatalf("render depends on the render before:\n%s\n--\n%s", after, fresh)
	}
}

// A paste goes into a picker's filter and a prompt's text as one line.
func TestPasteIntoFilters(t *testing.T) {
	p := NewPicker("t", choices(), 0)
	p.Handle(Key{Kind: KeyPaste, Text: "pro\nj"})
	if p.Filter != "pro j" || p.Done() {
		t.Fatalf("picker filter %q done %v", p.Filter, p.Done())
	}
	pr := NewPrompt("t", "", nil)
	pr.Handle(Key{Kind: KeyPaste, Text: "a\tb"})
	if pr.Text != "a b" || pr.Done() {
		t.Fatalf("prompt text %q", pr.Text)
	}
	var m Model
	m.Filtering = true
	m.Handle(Key{Kind: KeyPaste, Text: "x\ny"})
	if m.Filter != "x y" {
		t.Fatalf("model filter %q", m.Filter)
	}
}

// Keys: Tab and Shift-Tab cycle the fields; Left and Right cycle a
// chip; typing, newlines, pastes and the editing keys work in the
// prompt; the branch follows the prompt until edited, and follows
// again when cleared; Enter submits from the prompt or the branch when
// the prompt has text and the branch passes; Esc cancels.
func TestFormHandle(t *testing.T) {
	f := NewForm("t", chips(), "")
	f.Propose = propose
	f.Validate = func(b string) error {
		if strings.Contains(b, "..") {
			return errors.New("bad name")
		}
		return nil
	}
	if f.Focus() != fieldPrompt {
		t.Fatalf("focus %d", f.Focus())
	}
	for _, r := range "Fix the tests" {
		f.Handle(Key{Rune: r})
	}
	f.Handle(Key{Kind: KeyNewline})
	f.Handle(Key{Kind: KeyPaste, Text: "and the\nlint"})
	if f.Prompt() != "Fix the tests\nand the\nlint" || f.Branch() != "fix-the-tests-and-the-lint" || !f.Generated() {
		t.Fatalf("prompt %q branch %q generated %v", f.Prompt(), f.Branch(), f.Generated())
	}
	// Editing keys within the text.
	f.Handle(Key{Kind: KeyHome})
	f.Handle(Key{Kind: KeyDelete})
	f.Handle(Key{Kind: KeyLeft}) // at the start of a line: stays
	f.Handle(Key{Kind: KeyRune, Rune: 'L'})
	f.Handle(Key{Kind: KeyEnd})
	f.Handle(Key{Kind: KeyBackspace})
	f.Handle(Key{Kind: KeyUp})
	f.Handle(Key{Kind: KeyUp})
	f.Handle(Key{Kind: KeyRight})
	f.Handle(Key{Kind: KeyRight})
	f.Handle(Key{Kind: KeyDown})
	if f.Prompt() != "Fix the tests\nand the\nLin" {
		t.Fatalf("prompt after edits %q", f.Prompt())
	}
	// Tab to the branch, edit it, and the proposal stops following.
	f.Handle(Key{Kind: KeyTab})
	if f.Focus() != fieldBranch {
		t.Fatalf("focus %d", f.Focus())
	}
	f.Handle(Key{Rune: 'x'})
	f.Handle(Key{Kind: KeyShiftTab})
	f.Handle(Key{Rune: '!'})
	if f.Branch() != "fix-the-tests-and-the-linx" || f.Generated() {
		t.Fatalf("branch %q generated %v", f.Branch(), f.Generated())
	}
	// Clearing it keeps it the user's, so the proposal can be replaced
	// outright.
	f.Handle(Key{Kind: KeyTab})
	for range len([]rune(f.Branch())) {
		f.Handle(Key{Kind: KeyBackspace})
	}
	for _, r := range "repair" {
		f.Handle(Key{Rune: r})
	}
	if f.Generated() || f.Branch() != "repair" {
		t.Fatalf("replaced branch %q generated %v", f.Branch(), f.Generated())
	}
	// Chips: Tab on round to the repository, Right cycles, Left back.
	f.Handle(Key{Kind: KeyTab})
	if f.Focus() != fieldRepo {
		t.Fatalf("focus %d", f.Focus())
	}
	f.Handle(Key{Kind: KeyRight})
	if f.Chips[fieldRepo].Label() != "proj" {
		t.Fatalf("repo %q", f.Chips[fieldRepo].Label())
	}
	f.Handle(Key{Kind: KeyLeft})
	f.Handle(Key{Kind: KeyLeft})
	if f.Chips[fieldRepo].Label() != "proj" {
		t.Fatalf("repo after wrap %q", f.Chips[fieldRepo].Label())
	}
	// Enter on a chip opens the picker; a pick sets the chip.
	f.Handle(Key{Kind: KeyEnter})
	if f.picker == nil {
		t.Fatal("no picker")
	}
	f.Handle(Key{Kind: KeyUp})
	f.Handle(Key{Kind: KeyEnter})
	if f.picker != nil || f.Chips[fieldRepo].Label() != "laatmux" {
		t.Fatalf("after the picker: %q", f.Chips[fieldRepo].Label())
	}
	// Submit from the branch line; a bad name is refused with the
	// error, a good one ends the form.
	f.Handle(Key{Kind: KeyShiftTab})
	f.Handle(Key{Rune: '.'})
	f.Handle(Key{Rune: '.'})
	f.Handle(Key{Kind: KeyEnter})
	if f.Done() || f.Error != "bad name" {
		t.Fatalf("bad branch: done %v error %q", f.Done(), f.Error)
	}
	f.Handle(Key{Kind: KeyBackspace})
	f.Handle(Key{Kind: KeyBackspace})
	f.Handle(Key{Kind: KeyEnter})
	if !f.Done() || f.Cancelled {
		t.Fatalf("submit: done %v cancelled %v", f.Done(), f.Cancelled)
	}
	// An empty prompt does not submit from the prompt; Esc cancels.
	g := NewForm("t", chips(), "")
	g.Handle(Key{Kind: KeyEnter})
	if g.Done() || g.Error == "" {
		t.Fatalf("empty prompt submitted: %v %q", g.Done(), g.Error)
	}
	g.Handle(Key{Kind: KeyEsc})
	if !g.Done() || !g.Cancelled {
		t.Fatal("esc did not cancel")
	}
	// From the branch line an empty prompt submits with a given branch,
	// the add as it was; a generated one is empty and refused.
	h := NewForm("t", chips(), "fix")
	h.Handle(Key{Kind: KeyTab})
	h.Handle(Key{Kind: KeyEnter})
	if !h.Done() || h.Cancelled || h.Prompt() != "" || h.Generated() {
		t.Fatalf("branch submit without a prompt: done %v prompt %q generated %v", h.Done(), h.Prompt(), h.Generated())
	}
	i := NewForm("t", chips(), "")
	i.Propose = propose
	i.Handle(Key{Kind: KeyTab})
	i.Handle(Key{Kind: KeyEnter})
	if i.Done() || !strings.Contains(i.Error, "no branch name") {
		t.Fatalf("empty generated branch: done %v error %q", i.Done(), i.Error)
	}
	// A branch given at the start is the user's.
	e := NewForm("t", chips(), "fix")
	e.Propose = propose
	e.SetPrompt("Something else")
	if e.Branch() != "fix" || e.Generated() {
		t.Fatalf("given branch %q generated %v", e.Branch(), e.Generated())
	}
}

// A pasted line break in the prompt is a newline, and in the branch
// line it is dropped; a paste never submits.
func TestFormPaste(t *testing.T) {
	f := NewForm("t", chips(), "")
	f.Handle(Key{Kind: KeyPaste, Text: "one\ntwo\n"})
	if f.Done() || f.Prompt() != "one\ntwo\n" {
		t.Fatalf("done %v prompt %q", f.Done(), f.Prompt())
	}
	f.Handle(Key{Kind: KeyTab})
	f.Handle(Key{Kind: KeyPaste, Text: "a\nb"})
	if f.Branch() != "ab" || f.Done() {
		t.Fatalf("branch %q", f.Branch())
	}
}

// The prompt box wraps at spaces and scrolls to keep the cursor line
// on screen.
func TestFormWrapAndScroll(t *testing.T) {
	f := NewForm("t", chips(), "")
	f.SetPrompt(strings.Repeat("word ", 40))
	lines := f.Render(30, 10)
	text := Text(lines)
	if !strings.Contains(text, "█") {
		t.Fatalf("no cursor drawn:\n%s", text)
	}
	for _, l := range strings.Split(text, "\n") {
		if width(l) > 30 {
			t.Fatalf("line wider than the box: %q", l)
		}
	}
	f.Handle(Key{Kind: KeyHome})
	for range 8 {
		f.Handle(Key{Kind: KeyUp})
	}
	if !strings.Contains(Text(f.Render(30, 10)), "█word") {
		t.Fatalf("cursor at the start not shown:\n%s", Text(f.Render(30, 10)))
	}
}

// The decoder reads the editing keys, Shift-Tab, a bracketed paste
// whole, even split across reads, with its line breaks as newlines,
// and does not flush a paste under way.
func TestDecoderPasteAndKeys(t *testing.T) {
	cases := map[string][]Key{
		"\t":                           {{Kind: KeyTab}},
		"\x1b[Z":                       {{Kind: KeyShiftTab}},
		"\n":                           {{Kind: KeyNewline}},
		"\x1b[C\x1b[D":                 {{Kind: KeyRight}, {Kind: KeyLeft}},
		"\x1b[H\x1b[F":                 {{Kind: KeyHome}, {Kind: KeyEnd}},
		"\x1b[1~\x1b[4~":               {{Kind: KeyHome}, {Kind: KeyEnd}},
		"\x1b[3~":                      {{Kind: KeyDelete}},
		"\x1bOC\x1bOH":                 {{Kind: KeyRight}, {Kind: KeyHome}},
		"\x1b[200~a\r\nb\x1b[201~":     {{Kind: KeyPaste, Text: "a\nb"}},
		"x\x1b[200~\t\x1b[A\x1b[201~y": {{Rune: 'x'}, {Kind: KeyPaste, Text: "\t"}, {Rune: 'y'}},
	}
	for in, want := range cases {
		got := Parse([]byte(in))
		if len(got) != len(want) {
			t.Errorf("Parse(%q) = %+v, want %+v", in, got, want)
			continue
		}
		for i := range got {
			if got[i] != want[i] {
				t.Errorf("Parse(%q)[%d] = %+v, want %+v", in, i, got[i], want[i])
			}
		}
	}
	in := "j\x1b[200~line one\nline two\x1b[201~k"
	want := Parse([]byte(in))
	for cut := 1; cut < len(in); cut++ {
		var d Decoder
		got := d.Feed([]byte(in[:cut]))
		got = append(got, d.Feed([]byte(in[cut:]))...)
		got = append(got, d.Flush()...)
		if len(got) != len(want) {
			t.Fatalf("split at %d = %+v, want %+v", cut, got, want)
		}
		for i := range got {
			if got[i] != want[i] {
				t.Fatalf("split at %d: key %d = %+v, want %+v", cut, i, got[i], want[i])
			}
		}
	}
	// The start of a paste marker split by a slow read survives the
	// flush too: dropped, the text's line breaks would be Enter.
	var split Decoder
	if got := split.Feed([]byte("\x1b[200")); len(got) != 0 || !split.Pending() {
		t.Fatalf("marker prefix: %+v", got)
	}
	if got := split.Flush(); len(got) != 0 || !split.Pending() {
		t.Fatalf("flush on a marker prefix: %+v", got)
	}
	got := split.Feed([]byte("~fix\rmore\x1b[201~"))
	if len(got) != 1 || got[0].Kind != KeyPaste || got[0].Text != "fix\nmore" {
		t.Fatalf("paste after the split marker: %+v", got)
	}
	// A paste under way survives a flush: the escape wait must not cut
	// a long paste short.
	var d Decoder
	if got := d.Feed([]byte("\x1b[200~abc")); len(got) != 0 || !d.Pending() {
		t.Fatalf("paste start: %+v", got)
	}
	if got := d.Flush(); len(got) != 0 || !d.Pending() {
		t.Fatalf("flush during a paste: %+v pending %v", got, d.Pending())
	}
	if got := d.Feed([]byte("def\x1b[201~")); len(got) != 1 || got[0].Text != "abcdef" || d.Pending() {
		t.Fatalf("paste end: %+v", got)
	}
}

// A tab in the prompt is drawn as spaces to the next stop, and the
// cursor moves with it.
func TestFormTabs(t *testing.T) {
	f := NewForm("t", chips(), "")
	f.Handle(Key{Kind: KeyPaste, Text: "\tx\n\t\ty"})
	text := Text(f.Render(40, 12))
	if !strings.Contains(text, "│     x ") || !strings.Contains(text, "│         y█") {
		t.Fatalf("tabs:\n%s", text)
	}
	if f.Prompt() != "\tx\n\t\ty" {
		t.Fatalf("prompt %q", f.Prompt())
	}
}

// A paste whose end marker never arrives is taken as it is once its
// bytes have stopped for the grace, or once it is past the size cap,
// so Esc works again.
func TestDecoderPasteBounded(t *testing.T) {
	now := time.Unix(1000, 0)
	d := Decoder{now: func() time.Time { return now }}
	if got := d.Feed([]byte("\x1b[200~lost")); len(got) != 0 || !d.Pending() {
		t.Fatalf("start: %+v", got)
	}
	if got := d.Flush(); len(got) != 0 || !d.Pending() {
		t.Fatalf("flush within the grace: %+v", got)
	}
	now = now.Add(pasteGrace + time.Millisecond)
	got := d.Flush()
	if len(got) != 1 || got[0].Kind != KeyPaste || got[0].Text != "lost" || d.Pending() {
		t.Fatalf("flush past the grace: %+v pending %v", got, d.Pending())
	}
	if got := d.Feed([]byte("\x1b")); len(got) != 0 {
		t.Fatalf("after: %+v", got)
	}
	if got := d.Flush(); len(got) != 1 || got[0].Kind != KeyEsc {
		t.Fatalf("esc after a bounded paste: %+v", got)
	}
	var big Decoder
	big.Feed([]byte("\x1b[200~"))
	got = big.Feed([]byte(strings.Repeat("a", pasteMax+1)))
	if len(got) != 1 || got[0].Kind != KeyPaste || len(got[0].Text) != pasteMax+1 || big.Pending() {
		t.Fatalf("size cap: %d keys pending %v", len(got), big.Pending())
	}
}
