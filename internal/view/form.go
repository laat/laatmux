package view

import (
	"strings"
)

// Form is the task form of milestone four: three chips for the
// repository, the host and the agent, a prompt box, and a branch line
// after it, filled from the prompt as it is typed until the user edits
// it. Tab and Shift-Tab move between the fields; on a chip Left and
// Right cycle its candidates and Enter opens the picker with its
// filter; in the prompt typing edits, Ctrl-J inserts a newline, and
// Enter submits when the prompt is not empty; on the branch line typing
// edits it, and Enter submits. Esc cancels the whole form. A field with
// one candidate is shown, not skipped, so the form reads the same every
// time. Pasting is text inserted where the cursor is, line breaks
// included, never a submit and never a tab. The renderer is a pure
// function of the fields, the cursor and the size.
type Form struct {
	Title string
	Hint  string
	// Chips are the repository, the host and the agent, in that order.
	Chips [3]Chip
	// Propose derives the branch line from the prompt; nil proposes
	// nothing. Validate checks the branch at submit; its error stays in
	// the footer until the next key.
	Propose  func(prompt string) string
	Validate func(branch string) error
	// Note, when set, is a line under the hint about the chosen host,
	// such as its daemon not supporting tasks.
	Note func(f *Form) string
	// Error is what refused the last submit.
	Error string
	// Cancelled is set when Esc ended the form.
	Cancelled bool

	prompt []rune
	cursor int    // into prompt
	branch string // the line as shown
	edited bool   // the branch line is the user's, not the proposal
	focus  int    // 0..2 the chips, 3 the prompt, 4 the branch
	picker *Picker
	done   bool
}

// Chip is one of the form's choices: a title, the candidates, and the
// one selected.
type Chip struct {
	Title    string
	Choices  []Choice
	Selected int
}

// Label is the selected candidate's label.
func (c Chip) Label() string {
	if c.Selected < 0 || c.Selected >= len(c.Choices) {
		return ""
	}
	return c.Choices[c.Selected].Label
}

const (
	fieldRepo = iota
	fieldHost
	fieldAgent
	fieldPrompt
	fieldBranch
)

// NewForm makes a form with the chips given, the cursor in the prompt
// and the branch line empty; a branch given is the user's from the
// start, as a on a worktree row without a session wants.
func NewForm(title string, chips [3]Chip, branch string) *Form {
	f := &Form{Title: title, Chips: chips, focus: fieldPrompt, Hint: "tab next field  enter submit  ctrl-j newline  esc cancel"}
	if branch != "" {
		f.branch, f.edited = branch, true
	}
	return f
}

func (f *Form) Done() bool { return f.done }

// Reopen puts a submitted form back up with the error that refused it,
// its fields as they were.
func (f *Form) Reopen(err string) {
	f.done, f.Cancelled, f.Error = false, false, err
}

// Prompt is the prompt's text.
func (f *Form) Prompt() string { return string(f.prompt) }

// Branch is the branch line: the proposal, or what the user made it.
func (f *Form) Branch() string { return f.branch }

// Generated reports whether the branch is the proposal, for the host to
// make unique, rather than a name the user gave.
func (f *Form) Generated() bool { return !f.edited }

// Focus is the field the cursor is in.
func (f *Form) Focus() int { return f.focus }

// SetPrompt replaces the prompt, cursor at the end, and refreshes the
// proposal.
func (f *Form) SetPrompt(s string) {
	f.prompt = []rune(s)
	f.cursor = len(f.prompt)
	f.propose()
}

// propose refreshes the branch line from the prompt while it follows.
func (f *Form) propose() {
	if f.edited || f.Propose == nil {
		return
	}
	f.branch = f.Propose(string(f.prompt))
}

// Handle applies one key.
func (f *Form) Handle(k Key) {
	if f.done {
		return
	}
	f.Error = ""
	if f.picker != nil {
		f.picker.Handle(k)
		if f.picker.Done() {
			if f.picker.Chosen >= 0 {
				f.Chips[f.focus].Selected = f.picker.Chosen
			}
			f.picker = nil
		}
		return
	}
	switch k.Kind {
	case KeyEsc, KeyCtrlC:
		f.Cancelled, f.done = true, true
		return
	case KeyTab:
		f.focus = (f.focus + 1) % 5
		return
	case KeyShiftTab:
		f.focus = (f.focus + 4) % 5
		return
	case KeyMouse:
		return
	}
	switch f.focus {
	case fieldRepo, fieldHost, fieldAgent:
		f.chipKey(k)
	case fieldPrompt:
		f.promptKey(k)
	case fieldBranch:
		f.branchKey(k)
	}
}

// chipKey is a key on a chip: Left and Right cycle, Enter opens the
// picker.
func (f *Form) chipKey(k Key) {
	c := &f.Chips[f.focus]
	n := len(c.Choices)
	switch k.Kind {
	case KeyLeft, KeyUp:
		if n > 0 {
			c.Selected = (c.Selected + n - 1) % n
		}
	case KeyRight, KeyDown:
		if n > 0 {
			c.Selected = (c.Selected + 1) % n
		}
	case KeyEnter:
		if n > 0 {
			f.picker = NewPicker(f.Title+": "+c.Title, c.Choices, c.Selected)
		}
	}
}

// promptKey edits the prompt.
func (f *Form) promptKey(k Key) {
	switch k.Kind {
	case KeyRune:
		f.insert([]rune{k.Rune})
	case KeyNewline:
		f.insert([]rune{'\n'})
	case KeyPaste:
		f.insert([]rune(k.Text))
	case KeyBackspace:
		if f.cursor > 0 {
			f.prompt = append(f.prompt[:f.cursor-1], f.prompt[f.cursor:]...)
			f.cursor--
			f.propose()
		}
	case KeyDelete:
		if f.cursor < len(f.prompt) {
			f.prompt = append(f.prompt[:f.cursor], f.prompt[f.cursor+1:]...)
			f.propose()
		}
	case KeyLeft:
		// Within the line: Up and Down change lines.
		if f.cursor > f.lineStart(f.cursor) {
			f.cursor--
		}
	case KeyRight:
		if f.cursor < f.lineEnd(f.cursor) {
			f.cursor++
		}
	case KeyHome:
		f.cursor = f.lineStart(f.cursor)
	case KeyEnd:
		f.cursor = f.lineEnd(f.cursor)
	case KeyUp:
		f.cursor = f.lineStart(f.cursor)
		if f.cursor > 0 {
			f.cursor = f.lineStart(f.cursor - 1)
		}
	case KeyDown:
		if end := f.lineEnd(f.cursor); end < len(f.prompt) {
			f.cursor = end + 1
		}
	case KeyEnter:
		f.submit()
	}
}

func (f *Form) insert(rs []rune) {
	out := make([]rune, 0, len(f.prompt)+len(rs))
	out = append(out, f.prompt[:f.cursor]...)
	out = append(out, rs...)
	out = append(out, f.prompt[f.cursor:]...)
	f.prompt = out
	f.cursor += len(rs)
	f.propose()
}

// lineStart and lineEnd are the bounds of the prompt line around i.
func (f *Form) lineStart(i int) int {
	for i > 0 && f.prompt[i-1] != '\n' {
		i--
	}
	return i
}

func (f *Form) lineEnd(i int) int {
	for i < len(f.prompt) && f.prompt[i] != '\n' {
		i++
	}
	return i
}

// branchKey edits the branch line; an edit stops it following the
// prompt, an emptied line included, so the proposal can be replaced
// outright.
func (f *Form) branchKey(k Key) {
	switch k.Kind {
	case KeyRune:
		f.branch += string(k.Rune)
		f.edited = true
	case KeyPaste:
		f.branch += strings.ReplaceAll(pasteLine(k.Text), " ", "")
		f.edited = true
	case KeyBackspace:
		if r := []rune(f.branch); len(r) > 0 {
			f.branch = string(r[:len(r)-1])
		}
		f.edited = true
	case KeyEnter:
		f.submit()
	}
}

// submit ends the form when the prompt is not empty and the branch
// passes validation.
func (f *Form) submit() {
	if strings.TrimSpace(string(f.prompt)) == "" {
		f.Error = "the prompt is empty"
		return
	}
	if f.branch == "" {
		f.Error = "no branch name; give one"
		return
	}
	if f.Validate != nil {
		if err := f.Validate(f.branch); err != nil {
			f.Error = err.Error()
			return
		}
	}
	f.done = true
}

// Render draws the form in the width and height: the title, the chips
// on one line with their frames, the prompt box wrapped and scrolled so
// the cursor line shows, the branch line, and the footer with the hint
// or the error. The focused field is marked and coloured.
func (f *Form) Render(w, h int) []Line {
	if w <= 0 || h <= 0 {
		return nil
	}
	if f.picker != nil {
		return f.picker.Render(w, h)
	}
	out := []Line{{Spans: []Span{{Text: fit(f.Title, w)}}, Bold: true}}
	out = append(out, f.chipLines(w)...)
	// Footer: the hint, or the error; the note under it when there is
	// room and one to give.
	var foot []Line
	switch {
	case f.Error != "":
		foot = append(foot, Line{Spans: []Span{{Text: fit(f.Error, w)}}, Bold: true})
	default:
		foot = append(foot, Line{Spans: []Span{{Text: fit(f.Hint, w)}}, Dim: true})
	}
	if f.Note != nil {
		if n := f.Note(f); n != "" {
			foot = append(foot, Line{Spans: []Span{{Text: fit(n, w)}}, Bold: true})
		}
	}
	// The branch line, and the prompt box in what is left. A focused
	// line longer than the room shows its end, where the cursor is.
	avail := max(w-8, 0)
	branch := Line{Spans: []Span{{Text: "branch  "}, {Text: fit(f.branch, avail), Dim: !f.edited}}}
	if f.focus == fieldBranch {
		branch = Line{Spans: []Span{{Text: "branch  ", Fg: focusFg}, {Text: tail(f.branch+"█", avail)}}}
	}
	boxLines := h - len(out) - 1 - len(foot)
	if boxLines < 3 {
		boxLines = 3
	}
	out = append(out, f.promptBox(w, boxLines)...)
	out = append(out, branch)
	out = append(out, foot...)
	for len(out) < h {
		out = append(out, plain(""))
	}
	return out[:h]
}

// focusFg is the colour of the focused field's frame.
const focusFg = 36

// chipLines draws the three chips on three lines: a top frame with the
// title, the value, a bottom frame. The widths are split 4:3:4 of what
// the gaps leave.
func (f *Form) chipLines(w int) []Line {
	avail := w - 2
	if avail < 12 {
		// Too narrow for frames: one line each.
		var out []Line
		for i, c := range f.Chips {
			l := plain(fit(c.Title+" "+c.Label(), w))
			if f.focus == i {
				l = Line{Spans: []Span{{Text: fit(c.Title+" "+c.Label(), w), Fg: focusFg}}}
			}
			out = append(out, l)
		}
		return out
	}
	widths := [3]int{avail * 4 / 11, avail * 3 / 11, 0}
	widths[2] = avail - widths[0] - widths[1]
	var top, mid, bot Line
	for i, c := range f.Chips {
		cw := widths[i]
		title := " " + c.Title + " "
		if width(title) > cw-2 {
			title = fit(title, cw-2)
		}
		t := "┌" + title + strings.Repeat("─", max(cw-2-width(title), 0)) + "┐"
		v := "│" + pad(fit(" "+c.Label(), cw-2), cw-2) + "│"
		b := "└" + strings.Repeat("─", cw-2) + "┘"
		fg := 0
		if f.focus == i {
			fg = focusFg
		}
		gap := ""
		if i > 0 {
			gap = " "
		}
		top.Spans = append(top.Spans, Span{Text: gap + t, Fg: fg})
		mid.Spans = append(mid.Spans, Span{Text: gap + v, Fg: fg})
		bot.Spans = append(bot.Spans, Span{Text: gap + b, Fg: fg})
	}
	return []Line{top, mid, bot}
}

// tail is the last w cells of s, with an ellipsis first when it was
// cut.
func tail(s string, w int) string {
	if width(s) <= w {
		return s
	}
	rs := []rune(s)
	n := 0
	i := len(rs)
	for i > 0 && n+runeWidth(rs[i-1]) <= w-1 {
		i--
		n += runeWidth(rs[i])
	}
	return "…" + string(rs[i:])
}

// pad fills s to w cells with spaces.
func pad(s string, w int) string {
	if n := w - width(s); n > 0 {
		return s + strings.Repeat(" ", n)
	}
	return s
}

// promptBox draws the prompt in a frame of n lines: the text wrapped to
// the inner width, the cursor shown as a block, the box scrolled so the
// cursor line is visible.
func (f *Form) promptBox(w, n int) []Line {
	inner := w - 4
	if inner < 1 {
		inner = 1
	}
	fg := 0
	if f.focus == fieldPrompt {
		fg = focusFg
	}
	top := "┌ prompt " + strings.Repeat("─", max(w-2-width(" prompt "), 0)) + "┐"
	bot := "└" + strings.Repeat("─", max(w-2, 0)) + "┘"
	lines, cursorLine := f.wrapPrompt(inner)
	body := n - 2
	if body < 1 {
		body = 1
	}
	// The viewport is derived, not kept: the box starts at the top
	// until the cursor line would fall below it, then ends at the
	// cursor line, so the same fields, cursor and size draw the same.
	scroll := 0
	if cursorLine >= body {
		scroll = cursorLine - body + 1
	}
	out := []Line{{Spans: []Span{{Text: fit(top, w), Fg: fg}}}}
	for i := 0; i < body; i++ {
		text := ""
		if j := scroll + i; j < len(lines) {
			text = lines[j]
		}
		out = append(out, Line{Spans: []Span{{Text: "│ ", Fg: fg}, {Text: pad(fit(text, inner), inner)}, {Text: " │", Fg: fg}}})
	}
	return append(out, Line{Spans: []Span{{Text: fit(bot, w), Fg: fg}}})
}

// wrapPrompt wraps the prompt to lines of at most inner cells, with the
// cursor drawn as a block in the line it is on, and returns the lines
// and the index of the cursor's line. A line breaks at a newline, and
// at the last space that fits, else where the width runs out.
func (f *Form) wrapPrompt(inner int) ([]string, int) {
	var lines []string
	cursorLine := 0
	var cur []rune
	curW := 0
	flush := func() {
		lines = append(lines, string(cur))
		cur, curW = nil, 0
	}
	drawCursor := f.focus == fieldPrompt
	for i := 0; i <= len(f.prompt); i++ {
		if i == f.cursor && drawCursor {
			if curW+1 > inner {
				flush()
			}
			cursorLine = len(lines)
			cur = append(cur, '█')
			curW++
		}
		if i == len(f.prompt) {
			break
		}
		r := f.prompt[i]
		if r == '\n' {
			flush()
			continue
		}
		rw := runeWidth(r)
		if curW+rw > inner {
			// Break at the last space of the line when there is one
			// past its first third, carrying the word over.
			if sp := lastSpace(cur); sp > len(cur)/3 {
				carry := append([]rune(nil), cur[sp+1:]...)
				cur = cur[:sp]
				flush()
				cur = carry
				curW = 0
				for _, c := range cur {
					curW += runeWidth(c)
				}
				if strings.ContainsRune(string(carry), '█') {
					cursorLine = len(lines)
				}
			} else {
				flush()
			}
		}
		cur = append(cur, r)
		curW += rw
	}
	flush()
	return lines, cursorLine
}

func lastSpace(rs []rune) int {
	for i := len(rs) - 1; i >= 0; i-- {
		if rs[i] == ' ' {
			return i
		}
	}
	return -1
}
