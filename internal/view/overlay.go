package view

import (
	"fmt"
	"strings"
	"sync"
	"unicode/utf8"
)

// Overlay takes the screen and the keys while it is up: a picker, a
// text prompt, a running command's progress. The host sets it on the
// model and clears it when Done says the overlay has finished, with
// what it chose or how it ended left in the overlay itself.
type Overlay interface {
	Render(width, height int) []Line
	Handle(k Key)
	// Done reports that the overlay has finished: a choice made or
	// cancelled, a command ended and, when it failed, acknowledged.
	Done() bool
}

// Choice is one entry of a Picker: a label and a dim detail after it.
type Choice struct {
	Label  string
	Detail string
}

// Picker is a list to choose one entry from, filtered by typing: every
// printable key goes to the filter, the arrows and the wheel move,
// Enter picks, a click picks the entry under it, Esc cancels. The
// letters do not navigate as they do in the list, since a filter that
// starts typing at once is worth more in a list of a handful of names.
type Picker struct {
	Title   string
	Hint    string
	Choices []Choice
	Filter  string
	// Selected is the index into the matching entries.
	Selected int
	// Chosen is the index into Choices once Done, -1 when cancelled.
	Chosen int
	done   bool
	scroll int
	hits   []int // screen line -> index into Choices, -1 for none
	top    int   // lines above the list
}

// NewPicker makes a picker over the choices with the entry at
// preselect selected, or the first when it is out of range.
func NewPicker(title string, choices []Choice, preselect int) *Picker {
	p := &Picker{Title: title, Choices: choices, Chosen: -1, Hint: "type to filter  enter pick  esc back"}
	if preselect >= 0 && preselect < len(choices) {
		p.Selected = preselect
	}
	return p
}

// Matches is the indexes of the choices the filter keeps, in order.
func (p *Picker) Matches() []int {
	var out []int
	f := strings.ToLower(p.Filter)
	for i, c := range p.Choices {
		if f == "" || strings.Contains(strings.ToLower(c.Label), f) || strings.Contains(strings.ToLower(c.Detail), f) {
			out = append(out, i)
		}
	}
	return out
}

func (p *Picker) Done() bool { return p.done }

// Handle applies one key.
func (p *Picker) Handle(k Key) {
	if p.done {
		return
	}
	m := p.Matches()
	clamp := func() {
		if n := len(p.Matches()); p.Selected >= n {
			p.Selected = n - 1
		}
		if p.Selected < 0 {
			p.Selected = 0
		}
	}
	clamp()
	switch k.Kind {
	case KeyEsc, KeyCtrlC:
		p.Chosen, p.done = -1, true
	case KeyEnter:
		if len(m) > 0 {
			p.Chosen, p.done = m[p.Selected], true
		}
	case KeyUp:
		p.Selected--
	case KeyDown:
		p.Selected++
	case KeyBackspace:
		if r := []rune(p.Filter); len(r) > 0 {
			p.Filter = string(r[:len(r)-1])
			// The selection follows the entry, not the position.
			p.keep(m)
		}
	case KeyMouse:
		if k.Wheel != 0 {
			p.Selected += k.Wheel
			break
		}
		if i := k.Y - 1 - p.top; i >= 0 && i < len(p.hits) && p.hits[i] >= 0 {
			p.Chosen, p.done = p.hits[i], true
		}
	case KeyRune:
		p.Filter += string(k.Rune)
		p.keep(m)
	case KeyPaste:
		p.Filter += pasteLine(k.Text)
		p.keep(m)
	}
	clamp()
}

// keep moves the selection to the same entry under the new filter when
// it still matches, else to the top.
func (p *Picker) keep(before []int) {
	if p.Selected < 0 || p.Selected >= len(before) {
		p.Selected = 0
		return
	}
	want := before[p.Selected]
	for i, j := range p.Matches() {
		if j == want {
			p.Selected = i
			return
		}
	}
	p.Selected = 0
}

// Render draws the title, the filter line, the matching entries with
// the selection reversed and the detail dim, and the hint.
func (p *Picker) Render(w, h int) []Line {
	if w <= 0 || h <= 0 {
		return nil
	}
	out := []Line{{Spans: []Span{{Text: fit(p.Title, w)}}, Bold: true}, plain(fit("> "+p.Filter+"_", w))}
	p.top = len(out)
	body := h - len(out) - 1
	if body < 1 {
		body = 1
	}
	m := p.Matches()
	if p.Selected >= len(m) {
		p.Selected = len(m) - 1
	}
	if p.Selected < 0 {
		p.Selected = 0
	}
	if p.Selected < p.scroll {
		p.scroll = p.Selected
	}
	if p.Selected >= p.scroll+body {
		p.scroll = p.Selected - body + 1
	}
	if p.scroll > len(m)-body {
		p.scroll = len(m) - body
	}
	if p.scroll < 0 {
		p.scroll = 0
	}
	p.hits = make([]int, body)
	for i := 0; i < body; i++ {
		p.hits[i] = -1
		j := p.scroll + i
		if j >= len(m) {
			out = append(out, plain(""))
			continue
		}
		c := p.Choices[m[j]]
		p.hits[i] = m[j]
		label := fit("  "+c.Label, w)
		l := Line{Spans: []Span{{Text: label}}, Reverse: j == p.Selected}
		if c.Detail != "" && w-width(label) > 4 {
			l.Spans = append(l.Spans, Span{Text: fit("  "+c.Detail, w-width(label)), Dim: true})
		}
		out = append(out, l)
	}
	if len(m) == 0 {
		out[p.top] = Line{Spans: []Span{{Text: fit("  no match", w)}}, Dim: true}
	}
	for len(out) < h-1 {
		out = append(out, plain(""))
	}
	out = append(out, Line{Spans: []Span{{Text: fit(p.Hint, w)}}, Dim: true})
	return out[:h]
}

// Prompt is a one-line text entry: Enter accepts when Validate, if set,
// takes the text, else its error shows until the next key; Esc cancels.
type Prompt struct {
	Title    string
	Hint     string
	Text     string
	Validate func(string) error
	Error    string
	// Cancelled is set when Esc ended the prompt.
	Cancelled bool
	done      bool
}

// NewPrompt makes a prompt with the text pre-filled.
func NewPrompt(title, text string, validate func(string) error) *Prompt {
	return &Prompt{Title: title, Text: text, Validate: validate, Hint: "enter ok  esc back"}
}

func (p *Prompt) Done() bool { return p.done }

func (p *Prompt) Handle(k Key) {
	if p.done {
		return
	}
	p.Error = ""
	switch k.Kind {
	case KeyEsc, KeyCtrlC:
		p.Cancelled, p.done = true, true
	case KeyEnter:
		if p.Validate != nil {
			if err := p.Validate(p.Text); err != nil {
				p.Error = err.Error()
				return
			}
		}
		p.done = true
	case KeyBackspace:
		if r := []rune(p.Text); len(r) > 0 {
			p.Text = string(r[:len(r)-1])
		}
	case KeyRune:
		p.Text += string(k.Rune)
	case KeyPaste:
		p.Text += pasteLine(k.Text)
	}
}

func (p *Prompt) Render(w, h int) []Line {
	if w <= 0 || h <= 0 {
		return nil
	}
	out := []Line{{Spans: []Span{{Text: fit(p.Title, w)}}, Bold: true}, plain(fit("> "+p.Text+"_", w))}
	if p.Error != "" {
		out = append(out, Line{Spans: []Span{{Text: fit(p.Error, w)}}, Bold: true})
	}
	for len(out) < h-1 {
		out = append(out, plain(""))
	}
	out = append(out, Line{Spans: []Span{{Text: fit(p.Hint, w)}}, Dim: true})
	return out[:h]
}

// Log is a running command's progress: lines appended from another
// goroutine as they arrive, then the outcome. A command that succeeds
// is done at once; one that fails stays on screen, its error in the
// footer, until a key acknowledges it. Ctrl-C while it runs marks the
// log Quit: the caller leaves the view, and the command runs on in the
// daemon as one does when its client goes away.
type Log struct {
	Title string
	mu    sync.Mutex
	lines []string
	err   error
	ended bool
	acked bool
	// Quit is set when Ctrl-C ended the wait for a running command.
	Quit bool
}

func NewLog(title string) *Log { return &Log{Title: title} }

// Append adds a line. Safe from any goroutine.
func (l *Log) Append(s string) {
	l.mu.Lock()
	l.lines = append(l.lines, s)
	l.mu.Unlock()
}

// End records the outcome. Safe from any goroutine.
func (l *Log) End(err error) {
	l.mu.Lock()
	l.ended, l.err = true, err
	l.mu.Unlock()
}

// Err is the outcome once ended.
func (l *Log) Err() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.err
}

// Ended reports that the command has ended, whatever the outcome.
func (l *Log) Ended() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.ended
}

func (l *Log) Done() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.Quit || l.ended && (l.err == nil || l.acked)
}

// Handle acknowledges a failure with any key but a mouse event, which
// a wheel over the popup would send, and a paste, which is never a
// key pressed on purpose.
func (l *Log) Handle(k Key) {
	l.mu.Lock()
	defer l.mu.Unlock()
	switch {
	case l.ended:
		l.acked = l.acked || (k.Kind != KeyMouse && k.Kind != KeyPaste)
	case k.Kind == KeyCtrlC:
		l.Quit = true
	}
}

// Render draws the title, the last lines that fit, and the footer:
// running, or failed and how to go on. The error is wrapped over as
// many lines as it takes, in bold, under the progress: git's refusals
// are long and the end, where its own hint is, must not fall off.
func (l *Log) Render(w, h int) []Line {
	if w <= 0 || h <= 0 {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	out := []Line{{Spans: []Span{{Text: fit(l.Title, w)}}, Bold: true}}
	body := h - 2
	if body < 1 {
		body = 1
	}
	var lines []Line
	for _, s := range l.lines {
		lines = append(lines, plain(fit(s, w)))
	}
	if l.ended && l.err != nil {
		for _, s := range wrap(l.err.Error(), w) {
			lines = append(lines, Line{Spans: []Span{{Text: s}}, Bold: true})
		}
	}
	if len(lines) > body {
		lines = lines[len(lines)-body:]
	}
	out = append(out, lines...)
	for len(out) < h-1 {
		out = append(out, plain(""))
	}
	var foot Line
	switch {
	case !l.ended:
		foot = Line{Spans: []Span{{Text: fit("running  (ctrl-c leaves it running on the host)", w)}}, Dim: true}
	case l.err != nil:
		foot = Line{Spans: []Span{{Text: fit("failed  (any key returns)", w)}}, Bold: true}
	default:
		foot = Line{Spans: []Span{{Text: fit("done", w)}}, Dim: true}
	}
	return append(out[:h-1], foot)
}

// hardWrap splits s into lines of at most w cells exactly where the
// width runs out, keeping every space, with tabs drawn as spaces to
// the next stop of four and other control characters dropped.
func hardWrap(s string, w int) []string {
	if w < 1 {
		return nil
	}
	var out []string
	var cur []rune
	n := 0
	flush := func() {
		out = append(out, string(cur))
		cur, n = nil, 0
	}
	for _, r := range s {
		if r == '\t' {
			k := 4 - n%4
			if n+k > w {
				flush()
				k = 4
			}
			for range k {
				cur = append(cur, ' ')
			}
			n += k
			continue
		}
		if r < 0x20 || r == 0x7f {
			continue
		}
		rw := runeWidth(r)
		if n+rw > w && n > 0 {
			flush()
		}
		cur = append(cur, r)
		n += rw
	}
	flush()
	return out
}

// wrap splits s into lines of at most w cells, at spaces where one
// falls in the last third of the line, else mid-word. Control
// characters are dropped first, line breaks and tabs becoming spaces:
// an error that quotes a setup command's output may carry an escape
// sequence, and drawn raw it could clear the screen it is meant to
// stay on.
func wrap(s string, w int) []string {
	if w < 1 {
		return nil
	}
	s = strings.Map(func(r rune) rune {
		switch {
		case r == '\n' || r == '\r' || r == '\t':
			return ' '
		case r < 0x20 || r == 0x7f:
			return -1
		}
		return r
	}, s)
	var out []string
	for s != "" {
		if width(s) <= w {
			out = append(out, s)
			break
		}
		line := fit(s, w)
		cut := len(line)
		if cut == 0 {
			// A rune wider than the line: take it anyway, so the loop
			// moves on.
			_, cut = utf8.DecodeRuneInString(s)
		}
		if i := strings.LastIndexByte(line, ' '); i > 0 && width(line[:i]) >= w*2/3 {
			cut = i
		}
		out = append(out, strings.TrimRight(s[:cut], " "))
		s = strings.TrimLeft(s[cut:], " ")
	}
	return out
}

// Notice is text kept on screen until dismissed: a title, lines wrapped
// to the width, scrolled with the arrows and the wheel, and a footer.
// Enter, Esc and q dismiss it; other keys do nothing, so a prompt shown
// for copying is not lost to a stray key.
type Notice struct {
	Title  string
	Lines  []string
	Footer string
	// Verbatim is the index of the first line kept as it is: wrapped
	// only where the width runs out, tabs drawn as spaces to the next
	// stop, no space dropped, so a prompt shown for copying reads as it
	// was typed. Lines before it are prose, wrapped at spaces.
	Verbatim int
	scroll   int
	done     bool
}

func NewNotice(title string, lines []string, footer string) *Notice {
	return &Notice{Title: title, Lines: lines, Footer: footer}
}

func (n *Notice) Done() bool { return n.done }

func (n *Notice) Handle(k Key) {
	switch k.Kind {
	case KeyEnter, KeyEsc, KeyCtrlC:
		n.done = true
	case KeyUp:
		n.scroll--
	case KeyDown:
		n.scroll++
	case KeyMouse:
		n.scroll += k.Wheel
	case KeyRune:
		switch k.Rune {
		case 'q':
			n.done = true
		case 'k':
			n.scroll--
		case 'j':
			n.scroll++
		}
	}
	if n.scroll < 0 {
		n.scroll = 0
	}
}

func (n *Notice) Render(w, h int) []Line {
	if w <= 0 || h <= 0 {
		return nil
	}
	var body []Line
	for i, s := range n.Lines {
		if s == "" {
			body = append(body, plain(""))
			continue
		}
		parts := wrap(s, w)
		if n.Verbatim > 0 && i >= n.Verbatim {
			parts = hardWrap(s, w)
		}
		for _, part := range parts {
			body = append(body, plain(part))
		}
	}
	room := h - 2
	if room < 1 {
		room = 1
	}
	if n.scroll > len(body)-room {
		n.scroll = max(len(body)-room, 0)
	}
	out := []Line{{Spans: []Span{{Text: fit(n.Title, w)}}, Bold: true}}
	for i := 0; i < room; i++ {
		if j := n.scroll + i; j < len(body) {
			out = append(out, body[j])
		} else {
			out = append(out, plain(""))
		}
	}
	foot := n.Footer
	if len(body) > room {
		foot = fmt.Sprintf("%s  (%d-%d of %d lines, arrows scroll)", n.Footer, n.scroll+1, min(n.scroll+room, len(body)), len(body))
	}
	return append(out[:h-1], Line{Spans: []Span{{Text: fit(foot, w)}}, Dim: true})
}
