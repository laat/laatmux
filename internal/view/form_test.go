package view

import (
	"errors"
	"strings"
	"testing"
	"time"
	"unicode"

	"github.com/laat/laatmux/internal/term"
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
	f.Handle(term.Key{Kind: term.KeyTab})
	f.Handle(term.Key{Rune: '-'})
	f.Handle(term.Key{Rune: '2'})
	golden(t, "form-branch", Debug(f.Render(60, 12)))
	f.Handle(term.Key{Kind: term.KeyTab})
	golden(t, "form-chip", Debug(f.Render(60, 12)))
	f.Handle(term.Key{Kind: term.KeyEnter})
	golden(t, "form-picker", Debug(f.Render(60, 12)))
	f.Handle(term.Key{Kind: term.KeyEsc})
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
	long.Handle(term.Key{Kind: term.KeyTab})
	if text := Text(long.Render(40, 12)); !strings.Contains(text, "…") || !strings.Contains(text, "hij█") {
		t.Fatalf("long branch:\n%s", text)
	}
	// A branch preset from a worktree can have a C1 control character,
	// which git takes: the branch line drops it focused or not, short or
	// cut, as fit does.
	for _, b := range []string{"fix\u009b2J", strings.Repeat("abcdefghij", 6) + "\u009b2J"} {
		f := NewForm("t", chips(), b)
		unfocused := Text(f.Render(40, 12))
		f.Handle(term.Key{Kind: term.KeyTab})
		if focused := Text(f.Render(40, 12)); strings.ContainsRune(unfocused+focused, 0x9b) || !strings.Contains(focused, "2J█") {
			t.Errorf("branch %q:\n%s\n%s", b, unfocused, focused)
		}
	}
}

// The renderer is a pure function of the fields, the cursor and the
// size: a render at one height then another draws what a render at
// the second alone draws.
func TestFormRenderStateless(t *testing.T) {
	f := NewForm("t", chips(), "")
	f.SetPrompt(strings.Repeat("line\n", 30))
	f.Handle(term.Key{Kind: term.KeyUp})
	f.Handle(term.Key{Kind: term.KeyUp})
	f.Handle(term.Key{Kind: term.KeyUp})
	f.Render(40, 10)
	after := Text(f.Render(40, 12))
	g := NewForm("t", chips(), "")
	g.SetPrompt(strings.Repeat("line\n", 30))
	g.Handle(term.Key{Kind: term.KeyUp})
	g.Handle(term.Key{Kind: term.KeyUp})
	g.Handle(term.Key{Kind: term.KeyUp})
	if fresh := Text(g.Render(40, 12)); fresh != after {
		t.Fatalf("render depends on the render before:\n%s\n--\n%s", after, fresh)
	}
}

// A paste goes into a picker's filter and the model's as one line,
// its newlines and tabs spaces.
func TestPasteIntoFilters(t *testing.T) {
	p := NewPicker("t", choices(), 0)
	p.Handle(term.Key{Kind: term.KeyPaste, Text: "pro\nj"})
	if p.Filter != "pro j" || p.Done() {
		t.Fatalf("picker filter %q done %v", p.Filter, p.Done())
	}
	var m Model
	m.Filtering = true
	m.Handle(term.Key{Kind: term.KeyPaste, Text: "x\ny\tz"})
	if m.Filter != "x y z" {
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
		f.Handle(term.Key{Rune: r})
	}
	f.Handle(term.Key{Kind: term.KeyNewline})
	f.Handle(term.Key{Kind: term.KeyPaste, Text: "and the\nlint"})
	if f.Prompt() != "Fix the tests\nand the\nlint" || f.Branch() != "fix-the-tests-and-the-lint" || !f.Generated() {
		t.Fatalf("prompt %q branch %q generated %v", f.Prompt(), f.Branch(), f.Generated())
	}
	// Editing keys within the text.
	f.Handle(term.Key{Kind: term.KeyHome})
	f.Handle(term.Key{Kind: term.KeyDelete})
	f.Handle(term.Key{Kind: term.KeyLeft}) // at the start of a line: stays
	f.Handle(term.Key{Kind: term.KeyRune, Rune: 'L'})
	f.Handle(term.Key{Kind: term.KeyEnd})
	f.Handle(term.Key{Kind: term.KeyBackspace})
	f.Handle(term.Key{Kind: term.KeyUp})
	f.Handle(term.Key{Kind: term.KeyUp})
	f.Handle(term.Key{Kind: term.KeyRight})
	f.Handle(term.Key{Kind: term.KeyRight})
	f.Handle(term.Key{Kind: term.KeyDown})
	if f.Prompt() != "Fix the tests\nand the\nLin" {
		t.Fatalf("prompt after edits %q", f.Prompt())
	}
	// Tab to the branch, edit it, and the proposal stops following.
	f.Handle(term.Key{Kind: term.KeyTab})
	if f.Focus() != fieldBranch {
		t.Fatalf("focus %d", f.Focus())
	}
	f.Handle(term.Key{Rune: 'x'})
	f.Handle(term.Key{Kind: term.KeyShiftTab})
	f.Handle(term.Key{Rune: '!'})
	if f.Branch() != "fix-the-tests-and-the-linx" || f.Generated() {
		t.Fatalf("branch %q generated %v", f.Branch(), f.Generated())
	}
	// Clearing it keeps it the user's, so the proposal can be replaced
	// outright.
	f.Handle(term.Key{Kind: term.KeyTab})
	for range len([]rune(f.Branch())) {
		f.Handle(term.Key{Kind: term.KeyBackspace})
	}
	for _, r := range "repair" {
		f.Handle(term.Key{Rune: r})
	}
	if f.Generated() || f.Branch() != "repair" {
		t.Fatalf("replaced branch %q generated %v", f.Branch(), f.Generated())
	}
	// Chips: Tab on round to the repository, Right cycles, Left back.
	f.Handle(term.Key{Kind: term.KeyTab})
	if f.Focus() != fieldRepo {
		t.Fatalf("focus %d", f.Focus())
	}
	f.Handle(term.Key{Kind: term.KeyRight})
	if f.Chips[fieldRepo].Label() != "proj" {
		t.Fatalf("repo %q", f.Chips[fieldRepo].Label())
	}
	f.Handle(term.Key{Kind: term.KeyLeft})
	f.Handle(term.Key{Kind: term.KeyLeft})
	if f.Chips[fieldRepo].Label() != "proj" {
		t.Fatalf("repo after wrap %q", f.Chips[fieldRepo].Label())
	}
	// Enter on a chip opens the picker; a pick sets the chip.
	f.Handle(term.Key{Kind: term.KeyEnter})
	if f.picker == nil {
		t.Fatal("no picker")
	}
	f.Handle(term.Key{Kind: term.KeyUp})
	f.Handle(term.Key{Kind: term.KeyEnter})
	if f.picker != nil || f.Chips[fieldRepo].Label() != "laatmux" {
		t.Fatalf("after the picker: %q", f.Chips[fieldRepo].Label())
	}
	// Submit from the branch line; a bad name is refused with the
	// error, a good one ends the form.
	f.Handle(term.Key{Kind: term.KeyShiftTab})
	f.Handle(term.Key{Rune: '.'})
	f.Handle(term.Key{Rune: '.'})
	f.Handle(term.Key{Kind: term.KeyEnter})
	if f.Done() || f.Error != "bad name" {
		t.Fatalf("bad branch: done %v error %q", f.Done(), f.Error)
	}
	f.Handle(term.Key{Kind: term.KeyBackspace})
	f.Handle(term.Key{Kind: term.KeyBackspace})
	f.Handle(term.Key{Kind: term.KeyEnter})
	if !f.Done() || f.Cancelled {
		t.Fatalf("submit: done %v cancelled %v", f.Done(), f.Cancelled)
	}
	// An empty prompt does not submit from the prompt; Esc cancels.
	g := NewForm("t", chips(), "")
	g.Handle(term.Key{Kind: term.KeyEnter})
	if g.Done() || g.Error == "" {
		t.Fatalf("empty prompt submitted: %v %q", g.Done(), g.Error)
	}
	g.Handle(term.Key{Kind: term.KeyEsc})
	if !g.Done() || !g.Cancelled {
		t.Fatal("esc did not cancel")
	}
	// From the branch line an empty prompt submits with a given branch,
	// the add as it was; a generated one is empty and refused.
	h := NewForm("t", chips(), "fix")
	h.Handle(term.Key{Kind: term.KeyTab})
	h.Handle(term.Key{Kind: term.KeyEnter})
	if !h.Done() || h.Cancelled || h.Prompt() != "" || h.Generated() {
		t.Fatalf("branch submit without a prompt: done %v prompt %q generated %v", h.Done(), h.Prompt(), h.Generated())
	}
	i := NewForm("t", chips(), "")
	i.Propose = propose
	i.Handle(term.Key{Kind: term.KeyTab})
	i.Handle(term.Key{Kind: term.KeyEnter})
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
	f.Handle(term.Key{Kind: term.KeyPaste, Text: "one\ntwo\n"})
	if f.Done() || f.Prompt() != "one\ntwo\n" {
		t.Fatalf("done %v prompt %q", f.Done(), f.Prompt())
	}
	f.Handle(term.Key{Kind: term.KeyTab})
	f.Handle(term.Key{Kind: term.KeyPaste, Text: "a\nb"})
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
	f.Handle(term.Key{Kind: term.KeyHome})
	for range 8 {
		f.Handle(term.Key{Kind: term.KeyUp})
	}
	if !strings.Contains(Text(f.Render(30, 10)), "█word") {
		t.Fatalf("cursor at the start not shown:\n%s", Text(f.Render(30, 10)))
	}
}

// A tab in the prompt is drawn as spaces to the next stop, and the
// cursor moves with it.
func TestFormTabs(t *testing.T) {
	f := NewForm("t", chips(), "")
	f.Handle(term.Key{Kind: term.KeyPaste, Text: "\tx\n\t\ty"})
	text := Text(f.Render(40, 12))
	if !strings.Contains(text, "│     x ") || !strings.Contains(text, "│         y█") {
		t.Fatalf("tabs:\n%s", text)
	}
	if f.Prompt() != "\tx\n\t\ty" {
		t.Fatalf("prompt %q", f.Prompt())
	}
}

// A paste does not acknowledge a failed log.
func TestLogIgnoresPaste(t *testing.T) {
	l := NewLog("t")
	l.End(errors.New("failed"))
	l.Handle(term.Key{Kind: term.KeyPaste, Text: "oops\n"})
	if l.Done() {
		t.Fatal("a paste acknowledged the log")
	}
	l.Handle(term.Key{Rune: 'x'})
	if !l.Done() {
		t.Fatal("a key did not")
	}
}

// Raw bytes through the decoder into a form: an Alt chord is not the
// Esc that cancels, and the Esc that ends a paste with a lost end
// marker is spent on that, so the prompt survives both.
func TestDecoderIntoForm(t *testing.T) {
	for _, in := range []string{"\x1b\x7f", "\x1bb", "\x1bf"} {
		f := NewForm("t", chips(), "")
		f.SetPrompt("a long prompt")
		for _, k := range term.Parse([]byte(in)) {
			f.Handle(k)
		}
		if f.Cancelled || f.Prompt() != "a long prompt" {
			t.Fatalf("%q: cancelled %v prompt %q", in, f.Cancelled, f.Prompt())
		}
	}
	now := time.Unix(0, 0)
	d := term.Decoder{Now: func() time.Time { return now }}
	f := NewForm("t", chips(), "")
	feed := func(b string) {
		for _, k := range d.FeedAt([]byte(b), time.Time{}) {
			f.Handle(k)
		}
		now = now.Add(term.PasteGrace + time.Millisecond)
		for _, k := range d.Flush() {
			f.Handle(k)
		}
	}
	feed("typed by hand ")
	feed("\x1b[200~pasted")
	feed("\r")
	feed("\x1b")
	if f.Cancelled || f.Prompt() != "typed by hand pasted\n" || d.Pending() {
		t.Fatalf("after a stalled paste and Esc: cancelled %v prompt %q pending %v", f.Cancelled, f.Prompt(), d.Pending())
	}
	// The next Esc cancels as usual.
	feed("\x1b")
	if !f.Cancelled {
		t.Fatal("a second Esc did not cancel")
	}
	// A modified Enter, in either extended form, whole or split after
	// the escape and bracket, is a newline and never a submit; a plain
	// one submits.
	for _, in := range [][]string{{"\x1b[13;2u"}, {"\x1b[27;2;13~"}, {"\x1b[2", "7;5;13~"}, {"\x1b[1", "3;2u"}, {"\x1b\r"}} {
		d := term.Decoder{}
		f := NewForm("t", chips(), "")
		f.Propose = propose
		f.SetPrompt("one")
		// A split key's rest comes within the escape wait, so the
		// flush is after the last chunk only.
		for _, b := range in {
			for _, k := range d.FeedAt([]byte(b), time.Time{}) {
				f.Handle(k)
			}
		}
		for _, k := range d.Flush() {
			f.Handle(k)
		}
		if f.Done() || f.Prompt() != "one\n" {
			t.Fatalf("%q: done %v prompt %q", in, f.Done(), f.Prompt())
		}
		for _, k := range term.Parse([]byte("\x1b[13u")) {
			f.Handle(k)
		}
		if !f.Done() {
			t.Fatalf("%q: a plain Enter in the extended form did not submit", in)
		}
	}
}

// A paste on a chip goes into the prompt, which takes the focus; a
// prompt of whitespace is none; a height too short for the form keeps
// the footer, where the error is.
func TestFormPasteOnChipShortAndBlank(t *testing.T) {
	f := NewForm("t", chips(), "")
	f.Handle(term.Key{Kind: term.KeyShiftTab}) // the agent chip
	f.Handle(term.Key{Kind: term.KeyPaste, Text: "pasted"})
	if f.Prompt() != "pasted" || f.Focus() != fieldPrompt {
		t.Fatalf("paste on a chip: prompt %q focus %d", f.Prompt(), f.Focus())
	}
	f.SetPrompt(" \t\n ")
	if f.Prompt() != "" {
		t.Fatalf("whitespace prompt %q", f.Prompt())
	}
	f.Error = "no branch name; give one"
	if text := Text(f.Render(60, 8)); !strings.Contains(text, "no branch name") {
		t.Fatalf("short:\n%s", text)
	}
}

// A tab in a prompt box narrower than a tab stop stays within the box,
// the cursor with it.
func TestNarrowTab(t *testing.T) {
	f := NewForm("t", chips(), "")
	f.SetPrompt("\t\t")
	for _, w := range []int{5, 6, 7} {
		text := Text(f.Render(w, 12))
		if !strings.Contains(text, "█") {
			t.Fatalf("width %d: cursor lost\n%s", w, text)
		}
	}
}

// A symbol with VS16 takes two cells in the prompt's wrap and the
// branch line's tail, as it does in fit: nothing is cut that the wrap
// thought fit, and nothing is wider than asked.
func TestWrapVS16(t *testing.T) {
	f := &Form{prompt: []rune("12345678⚠️x"), focus: fieldPrompt}
	f.cursor = len(f.prompt)
	lines, cur := f.wrapPrompt(10)
	if len(lines) != 2 || lines[0] != "12345678⚠️" || lines[1] != "x█" || cur != 1 {
		t.Errorf("wrapPrompt: %q, cursor line %d", lines, cur)
	}
	if got := tail("abcdefgh⚠️x", 4); width(got) > 4 {
		t.Errorf("tail: %q is %d cells", got, width(got))
	}
	// A C1 control character between the symbol and its selector is
	// not drawn, and the wrap measures the symbol as drawn, two cells:
	// what follows it on the line, the cursor or a rune, is not cut.
	for _, p := range []string{"12345678⚠\u009b️", "12345678⚠\u009b️x"} {
		f := &Form{prompt: []rune(p), focus: fieldPrompt}
		f.cursor = len(f.prompt)
		lines, _ := f.wrapPrompt(10)
		for _, l := range lines {
			if width(l) > 10 {
				t.Errorf("wrapPrompt(%q): line %q is %d cells", p, l, width(l))
			}
		}
		if text := strings.Join(lines, "|"); !strings.HasSuffix(text, "█") || strings.HasSuffix(p, "x") && !strings.Contains(text, "x") {
			t.Errorf("wrapPrompt(%q) = %q", p, lines)
		}
	}
	// A line break or a tab does part them: the symbol ends its line,
	// drawn one cell, and fills the line's last cell.
	for _, p := range []string{"123456789⚠\n️", "123456789⚠\t️"} {
		f := &Form{prompt: []rune(p), focus: fieldBranch}
		if lines, _ := f.wrapPrompt(10); len(lines) == 0 || lines[0] != "123456789⚠" {
			t.Errorf("wrapPrompt(%q) = %q", p, lines)
		}
	}
}

// Left, Right, Backspace and Delete keep a symbol and its selector
// together.
func TestPromptEditVS16(t *testing.T) {
	f := &Form{prompt: []rune("a⚠️b"), focus: fieldPrompt}
	f.cursor = 3
	f.promptKey(term.Key{Kind: term.KeyLeft})
	if f.cursor != 1 {
		t.Errorf("left: cursor %d", f.cursor)
	}
	f.promptKey(term.Key{Kind: term.KeyRight})
	if f.cursor != 3 {
		t.Errorf("right: cursor %d", f.cursor)
	}
	f.promptKey(term.Key{Kind: term.KeyBackspace})
	if string(f.prompt) != "ab" || f.cursor != 1 {
		t.Errorf("backspace: %q at %d", string(f.prompt), f.cursor)
	}
	f = &Form{prompt: []rune("a⚠️b"), focus: fieldPrompt, cursor: 1}
	f.promptKey(term.Key{Kind: term.KeyDelete})
	if string(f.prompt) != "ab" || f.cursor != 1 {
		t.Errorf("delete: %q at %d", string(f.prompt), f.cursor)
	}
	f = &Form{prompt: []rune("1️⃣z"), focus: fieldPrompt, cursor: 4}
	f.promptKey(term.Key{Kind: term.KeyLeft})
	if f.cursor != 3 {
		t.Errorf("left past z: cursor %d", f.cursor)
	}
	f.promptKey(term.Key{Kind: term.KeyBackspace})
	if string(f.prompt) != "z" || f.cursor != 0 {
		t.Errorf("backspace a keycap: %q at %d", string(f.prompt), f.cursor)
	}
	if w := width("1️⃣"); w != 2 {
		t.Errorf("keycap is %d cells", w)
	}
	if got := tail("abcdefgh⚠️x", 2); width(got) > 2 {
		t.Errorf("tail: %q is %d cells", got, width(got))
	}
	// A C1 control character, which is not drawn, goes with the rune
	// before it.
	f = &Form{prompt: []rune("ab\u009bc"), focus: fieldPrompt, cursor: 3}
	f.promptKey(term.Key{Kind: term.KeyLeft})
	if f.cursor != 1 {
		t.Errorf("left over a C1: cursor %d", f.cursor)
	}
	f.cursor = 3
	f.promptKey(term.Key{Kind: term.KeyBackspace})
	if string(f.prompt) != "ac" || f.cursor != 1 {
		t.Errorf("backspace over a C1: %q at %d", string(f.prompt), f.cursor)
	}
}

// Ctrl-J, which Shift-Enter and Ctrl-Enter decode to, submits from a
// chip and from the branch line, as Enter does from the prompt; on a
// chip Enter still opens the picker, and an empty prompt is refused
// from a chip as from the prompt.
func TestFormChipNewlineSubmits(t *testing.T) {
	f := NewForm("add a task", chips(), "")
	f.Propose = propose
	f.Handle(term.Key{Kind: term.KeyShiftTab}) // from the prompt to the agent chip
	if f.Focus() != fieldAgent {
		t.Fatalf("focus %d, want the agent chip", f.Focus())
	}
	f.Handle(term.Key{Kind: term.KeyNewline})
	if f.Done() || f.Error != "the prompt is empty" {
		t.Fatalf("ctrl-j on a chip with no prompt: done %v, error %q", f.Done(), f.Error)
	}
	f.Handle(term.Key{Kind: term.KeyEnter})
	if f.picker == nil {
		t.Fatalf("enter on a chip did not open the picker")
	}
	f.Handle(term.Key{Kind: term.KeyEsc})
	f.Handle(term.Key{Kind: term.KeyTab}) // the prompt
	for _, r := range "Fix it" {
		f.Handle(term.Key{Rune: r})
	}
	f.Handle(term.Key{Kind: term.KeyShiftTab}) // back to the agent chip
	f.Handle(term.Key{Kind: term.KeyNewline})
	if !f.Done() || f.Cancelled || f.Prompt() != "Fix it" || f.Branch() != "fix-it" {
		t.Fatalf("ctrl-j on a chip: done %v cancelled %v prompt %q branch %q", f.Done(), f.Cancelled, f.Prompt(), f.Branch())
	}
	g := NewForm("add a task", chips(), "")
	g.Handle(term.Key{Kind: term.KeyTab}) // from the prompt to the branch line
	for _, r := range "by-hand" {
		g.Handle(term.Key{Rune: r})
	}
	g.Handle(term.Key{Kind: term.KeyNewline})
	if !g.Done() || g.Branch() != "by-hand" {
		t.Fatalf("ctrl-j on the branch line: done %v branch %q", g.Done(), g.Branch())
	}
}

// A click focuses the field under it, by the layout the last Render
// drew: a chip by its columns on the framed row, the prompt box, the
// branch line; a click on the title or the footer changes nothing, and
// the wheel never does. One chip a line when the form is narrow, and
// the top cut to a short height is accounted for.
func TestFormClickFocuses(t *testing.T) {
	f := NewForm("add a task", chips(), "")
	f.Render(80, 12)
	// 80 by 12: the title is line 1; the chips are 28, 21 and 29
	// columns wide with a gap between, on lines 2 to 4; the prompt box
	// is lines 5 to 10, the branch line 11, the hint 12.
	for _, c := range []struct {
		x, y, want int
	}{
		{1, 2, fieldRepo}, {28, 3, fieldRepo}, {30, 3, fieldHost}, {52, 4, fieldAgent}, {80, 3, fieldAgent},
		{10, 5, fieldPrompt}, {10, 10, fieldPrompt}, {3, 11, fieldBranch},
	} {
		f.focus = fieldPrompt
		if c.want == fieldPrompt {
			f.focus = fieldRepo
		}
		f.Handle(term.Key{Kind: term.KeyMouse, X: c.x, Y: c.y})
		if f.Focus() != c.want {
			t.Errorf("click at %d,%d: focus %d, want %d", c.x, c.y, f.Focus(), c.want)
		}
	}
	f.focus = fieldHost
	for _, k := range []term.Key{{Kind: term.KeyMouse, X: 5, Y: 1}, {Kind: term.KeyMouse, X: 5, Y: 12}, {Kind: term.KeyMouse, X: 29, Y: 3}, {Kind: term.KeyMouse, X: 10, Y: 6, Wheel: 1}} {
		f.Handle(k)
		if f.Focus() != fieldHost {
			t.Errorf("%+v moved the focus to %d", k, f.Focus())
		}
	}
	// Narrow: one chip a line, lines 2 to 4.
	n := NewForm("t", chips(), "")
	n.Render(12, 12)
	n.Handle(term.Key{Kind: term.KeyMouse, X: 1, Y: 4})
	if n.Focus() != fieldAgent {
		t.Errorf("narrow click on the third chip line: focus %d", n.Focus())
	}
	// Short: nine lines are needed and eight given, so the title is
	// cut and the first line is the chips' top frame.
	s := NewForm("t", chips(), "")
	s.Render(80, 8)
	s.Handle(term.Key{Kind: term.KeyMouse, X: 30, Y: 1})
	if s.Focus() != fieldHost {
		t.Errorf("short form, click on the first line: focus %d, want the host chip", s.Focus())
	}
}

// The readline chords in the prompt, within the line: Ctrl-A and Ctrl-E,
// Ctrl-B and Ctrl-F, Ctrl-D, Ctrl-U, Ctrl-K and Ctrl-W; on the branch
// line Ctrl-U clears and Ctrl-W takes the last segment. An unknown
// chord changes nothing.
func TestFormReadlineChords(t *testing.T) {
	f := NewForm("t", chips(), "")
	f.Propose = propose
	ctrl := func(r rune) { f.Handle(term.Key{Kind: term.KeyCtrl, Rune: r}) }
	for _, r := range "one two\nthree four" {
		if r == '\n' {
			f.Handle(term.Key{Kind: term.KeyNewline})
		} else {
			f.Handle(term.Key{Rune: r})
		}
	}
	ctrl('a')
	if f.cursor != 8 {
		t.Errorf("ctrl-a: cursor %d, want the second line's start 8", f.cursor)
	}
	ctrl('e')
	if f.cursor != len(f.prompt) {
		t.Errorf("ctrl-e: cursor %d, want the end %d", f.cursor, len(f.prompt))
	}
	ctrl('w')
	if f.Prompt() != "one two\nthree " {
		t.Errorf("ctrl-w: %q", f.Prompt())
	}
	ctrl('b')
	ctrl('b')
	ctrl('k')
	if f.Prompt() != "one two\nthre" {
		t.Errorf("ctrl-b twice then ctrl-k: %q", f.Prompt())
	}
	ctrl('u')
	if f.Prompt() != "one two\n" || f.cursor != 8 {
		t.Errorf("ctrl-u: %q cursor %d", f.Prompt(), f.cursor)
	}
	ctrl('a')
	ctrl('a') // already at the line's start: stays
	ctrl('f')
	ctrl('d')
	ctrl('x')
	if f.Prompt() != "one two\n" || f.cursor != 8 {
		t.Errorf("ctrl-f, ctrl-d at the end of the text, an unknown chord: %q cursor %d", f.Prompt(), f.cursor)
	}
	f.Handle(term.Key{Kind: term.KeyUp})
	ctrl('d')
	if f.Prompt() != "ne two\n" {
		t.Errorf("ctrl-d: %q", f.Prompt())
	}
	// The branch line: cleared of the proposed name first.
	f.Handle(term.Key{Kind: term.KeyTab})
	ctrl('u')
	for _, r := range "fix/one-two" {
		f.Handle(term.Key{Rune: r})
	}
	ctrl('w')
	if f.Branch() != "fix/one-" {
		t.Errorf("ctrl-w on the branch line: %q", f.Branch())
	}
	ctrl('u')
	if f.Branch() != "" || !f.edited {
		t.Errorf("ctrl-u on the branch line: %q edited %v", f.Branch(), f.edited)
	}
}
