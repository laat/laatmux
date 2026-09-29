package view

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/laat/laatmux/internal/palette"
	"github.com/laat/laatmux/internal/protocol"
	"github.com/laat/laatmux/internal/rows"
)

// Each set's icon per status, the config's over the set's, and the
// spinner where working has none.
func TestIcons(t *testing.T) {
	for _, c := range []struct {
		ic        Icons
		s         Status
		want      string
		spinsWant bool
	}{
		{Icons{}, StatusWaiting, "💬", false},
		{Icons{}, StatusWorking, "", true},
		{Icons{Set: IconsNerdFont}, StatusWaiting, "\uf075", false},
		{Icons{Set: IconsASCII}, StatusWaiting, "!", false},
		{Icons{Set: IconsASCII}, StatusWorking, "*", false},
		{Icons{Waiting: "?"}, StatusWaiting, "?", false},
		{Icons{Working: "~"}, StatusWorking, "~", false},
		{Icons{Set: "bogus"}, StatusDone, "✅", false},
	} {
		if got := c.ic.icon(c.s); got != c.want {
			t.Errorf("%+v %v: %q, want %q", c.ic, c.s, got, c.want)
		}
	}
	// A one-cell icon is padded to the spinner's two cells, so labels
	// line up.
	now := time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)
	m := &Model{Now: now, Icons: Icons{Set: IconsASCII}}
	r := rows.Row{Agent: &protocol.Agent{Activity: protocol.Blocked, Liveness: protocol.Alive}}
	if sp := m.iconSpan(r); sp.Text != "! " || sp.Fg != palette.Accent {
		t.Errorf("ascii blocked: %+v", sp)
	}
	r.Agent.Activity = protocol.Working
	if sp := m.iconSpan(r); sp.Text != "* " || sp.spin {
		t.Errorf("ascii working: %+v", sp)
	}
}

func TestAgentIconFor(t *testing.T) {
	if a, ok := AgentIconFor("claude", nil); !ok || a.Icon != "CC" || a.Color != "#d97757" {
		t.Errorf("default: %+v %v", a, ok)
	}
	if a, ok := AgentIconFor("claude", map[string]AgentIcon{"claude": {Icon: "C"}}); !ok || a.Icon != "C" || a.Color != "#d97757" {
		t.Errorf("icon over, colour kept: %+v %v", a, ok)
	}
	if _, ok := AgentIconFor("aider", nil); ok {
		t.Error("an agent no table knows has an icon")
	}
	if a, ok := AgentIconFor("claude", map[string]AgentIcon{"claude": {Color: "#ff0000"}}); !ok || a.Icon != "CC" || a.Color != "#ff0000" {
		t.Errorf("colour over, icon kept: %+v %v", a, ok)
	}
	if a, ok := AgentIconFor("aider", map[string]AgentIcon{"aider": {Icon: "AI"}}); !ok || a.Icon != "AI" {
		t.Errorf("an agent the config gives an icon: %+v %v", a, ok)
	}
	// An agent's own colour reaches the terminal, and the viewer's own
	// label keeps its colour on a dim line.
	th, _ := palette.New(true, nil)
	if got := ANSI(Line{Spans: []Span{{Text: "CC", Fg: "#d97757"}}}, th); !strings.Contains(got, "\x1b[38;2;217;119;87mCC") {
		t.Errorf("an agent's own colour: %q", got)
	}
	if got := ANSI(Line{Dim: true, Spans: []Span{{Text: "me", Bold: true, Fg: palette.CurrentWorktreeFg}}}, th); !strings.Contains(got, th.SGR(palette.CurrentWorktreeFg, false)+"me") {
		t.Errorf("the viewer's label on a dim line: %q", got)
	}
}

func TestCleanTitle(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"✳ Done. Tests pass", "Done. Tests pass"},
		{"⠋⠙ Editing src/api.ts", "Editing src/api.ts"},
		{"◐ Thinking", "Thinking"},
		{"OC | Refactoring", "Refactoring"},
		{"● ✓ ok", "ok"},
		{"Claude Code", ""},
		{"✳ Claude Code v2", ""},
		{"zsh", ""},
		{"fix-ls", ""},     // the primary label
		{"laatmux", ""},    // the secondary
		{"vm", ""},         // the host
		{"vm.py", "vm.py"}, // a file, not the host
		{"vm.py: fix parsing", "vm.py: fix parsing"},
		{"  ", ""},
		{"vmware notes", "vmware notes"},
		{"AM-KWMQF9PMFC", ""},       // the machine's name, as tmux titles a pane
		{"AM-KWMQF9PMFC.local", ""}, // with its domain
		{"AM-KWMQF9PMFC.go", "AM-KWMQF9PMFC.go"},
		{"AM-KWMQF9PMFC.yaml", "AM-KWMQF9PMFC.yaml"},
		{"AM-KWMQF9PMFC.test.ts", "AM-KWMQF9PMFC.test.ts"},
	} {
		if got := cleanTitle(c.in, "fix-ls", "laatmux", "vm", "AM-KWMQF9PMFC.local"); got != c.want {
			t.Errorf("%q: %q, want %q", c.in, got, c.want)
		}
	}
}

func TestElapsed(t *testing.T) {
	for d, want := range map[time.Duration]string{
		-time.Second:                    "0:00",
		0:                               "0:00",
		8 * time.Second:                 "0:08",
		110 * time.Second:               "1:50",
		59*time.Minute + 59*time.Second: "59:59",
		time.Hour:                       "1h",
		23 * time.Hour:                  "23h",
		26 * time.Hour:                  "1d",
		80 * time.Hour:                  "3d",
	} {
		if got := elapsed(d); got != want {
			t.Errorf("%v: %q, want %q", d, got, want)
		}
	}
}

// Before the first snapshot the body says it is loading, with a spinner
// that ticks; a list with nothing, or nothing the filter keeps, says so.
func TestLoadingAndEmpty(t *testing.T) {
	now := time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)
	m := &Model{Now: now, Width: 30, Height: 5, Loading: true}
	txt := Text(m.Render())
	if !strings.Contains(txt, "Loading") || !m.Spinning() {
		t.Errorf("loading: %q spinning %v", txt, m.Spinning())
	}
	m.Loading = false
	if txt := Text(m.Render()); !strings.Contains(txt, "No worktrees or agents") || m.Spinning() {
		t.Errorf("empty: %q", txt)
	}
	m = model(now)
	m.Width, m.Height, m.Filter = 40, 6, "nothing-like-it"
	if txt := Text(m.Render()); !strings.Contains(txt, "Nothing matches /nothing-like-it") {
		t.Errorf("empty filter: %q", txt)
	}
}

// Rows below the window are counted on its last line, by row, not by
// line; the line maps to no row for the mouse.
func TestMoreBelow(t *testing.T) {
	now := time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)
	m := model(now)
	m.Header = nil
	m.Layout, m.Width, m.Height = Tiles, 35, 10 // nine body lines
	lines := m.Render()
	body := lines[:len(lines)-1]
	last := Text(body[len(body)-1:])
	total := len(m.Visible())
	// Eight lines show the first two tiles whole; the rest are below,
	// with the two the collapsed groups hide.
	if want := fmt.Sprintf("↓ %d more\n", total-2+2); last != want {
		t.Errorf("last body line %q, want %q", last, want)
	}
	if m.hitIDs[len(body)-1] != "" {
		t.Error("the count line maps to a row")
	}
	m.Height = 60
	if txt := Text(m.Render()); strings.Contains(txt, "more") {
		t.Errorf("everything on screen and a count:\n%s", txt)
	}
}

// A theme with colours draws the selection as a background band across
// the line and a dim line in the dimmed colour, a span's own colour
// included; without colours the selection is reverse video and a dim
// line the dim attribute alone.
func TestANSIThemes(t *testing.T) {
	th, _ := palette.New(true, nil)
	sel := Line{Reverse: true, Spans: []Span{{Text: "x", Fg: palette.Info}}}
	if got := ANSI(sel, th); !strings.HasPrefix(got, th.SGR(palette.HighlightRowBg, true)) || strings.Contains(got, "\x1b[7m") {
		t.Errorf("colour selection: %q", got)
	}
	if got := ANSI(sel, palette.Mono()); !strings.HasPrefix(got, "\x1b[7m") {
		t.Errorf("mono selection: %q", got)
	}
	dim := Line{Dim: true, Spans: []Span{{Text: "x", Fg: palette.Accent}}}
	got := ANSI(dim, th)
	if !strings.Contains(got, th.SGR(palette.Dimmed, false)) || strings.Contains(got, th.SGR(palette.Accent, false)) {
		t.Errorf("colour dim line: %q", got)
	}
	if got := ANSI(dim, palette.Mono()); got != "\x1b[2mx\x1b[0m" {
		t.Errorf("mono dim line: %q", got)
	}
	bold := Line{Spans: []Span{{Text: "me", Bold: true, Fg: palette.CurrentWorktreeFg}}}
	if got := ANSI(bold, th); !strings.Contains(got, "\x1b[1m"+th.SGR(palette.CurrentWorktreeFg, false)+"me") {
		t.Errorf("bold coloured span: %q", got)
	}
	if got := ANSI(bold, palette.Mono()); !strings.Contains(got, "\x1b[1mme") {
		t.Errorf("bold span without colours: %q", got)
	}
}

// The terminal's late answer to the background query is dropped whole,
// whichever terminator ends it, and keys around it survive.
func TestOSCAnswerDropped(t *testing.T) {
	for _, in := range []string{"a\x1b]11;rgb:1a1a/1b1b/2626\x1b\\b", "a\x1b]11;rgb:ffff/ffff/ffff\x07b"} {
		keys := Parse([]byte(in))
		var got []rune
		for _, k := range keys {
			if k.Kind == KeyRune {
				got = append(got, k.Rune)
			}
		}
		if string(got) != "ab" || len(keys) != 2 {
			t.Errorf("%q: %+v", in, keys)
		}
	}
}

// Background reads the terminal's answer to OSC 11, and gives up on a
// terminal that does not answer.
func TestBackground(t *testing.T) {
	devnull, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer devnull.Close()
	for answer, want := range map[string]bool{"\x1b]11;rgb:ffff/ffff/ffff\x1b\\": false, "\x1b]11;rgb:0000/0000/0000\x07": true} {
		r, w, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		w.WriteString(answer)
		term := &Term{in: r, out: devnull}
		if dark, ok := term.Background(time.Second); !ok || dark != want {
			t.Errorf("%q: dark %v ok %v", answer, dark, ok)
		}
		r.Close()
		w.Close()
	}
	r, w, _ := os.Pipe()
	defer r.Close()
	defer w.Close()
	start := time.Now()
	if _, ok := (&Term{in: r, out: devnull}).Background(50 * time.Millisecond); ok || time.Since(start) > time.Second {
		t.Errorf("no answer: ok %v after %v", ok, time.Since(start))
	}
}

// Keys that come while the terminal is asked for its background are
// kept for Run, and an answer cut by the deadline is waited on for its
// end.
func TestBackgroundKeepsInput(t *testing.T) {
	devnull, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer devnull.Close()
	r, w, _ := os.Pipe()
	w.WriteString("j\x1b]11;rgb:0000/0000/0000\x07k")
	term := &Term{in: r, out: devnull}
	if dark, ok := term.Background(time.Second); !ok || !dark || string(term.pending) != "jk" {
		t.Errorf("dark %v ok %v pending %q", dark, ok, term.pending)
	}
	r.Close()
	w.Close()
	r, w, _ = os.Pipe()
	defer r.Close()
	defer w.Close()
	w.WriteString("\x1b]11;rgb:ffff/")
	go func() {
		time.Sleep(150 * time.Millisecond)
		w.WriteString("ffff/ffff\x1b\\")
	}()
	term = &Term{in: r, out: devnull}
	if dark, ok := term.Background(50 * time.Millisecond); !ok || dark || len(term.pending) != 0 {
		t.Errorf("split answer: dark %v ok %v pending %q", dark, ok, term.pending)
	}
}

// An OSC answer cut by a flush is swallowed through BEL or ST when its
// rest comes, whether the ST is split or not; past the bound in time
// the bytes are keys again.
func TestOSCAcrossFlush(t *testing.T) {
	runes := func(ks []Key) string {
		var b strings.Builder
		for _, k := range ks {
			if k.Kind == KeyRune {
				b.WriteRune(k.Rune)
			}
		}
		return b.String()
	}
	now := time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	for _, parts := range [][]string{
		{"\x1b]11;rgb:", "1111/2222/3333\x07j"},
		{"\x1b]11;rgb:0/0/0\x1b", "\\j"},
		{"\x1b]11;rgb:0/0", "/0\x1b", "\\j"},
	} {
		d := Decoder{now: clock}
		var got string
		for _, p := range parts {
			got += runes(d.Feed([]byte(p)))
			got += runes(d.Flush())
		}
		if got != "j" {
			t.Errorf("%q: %q", parts, got)
		}
	}
	// Alt-] and then, a while later, keys: they are the user's.
	d := Decoder{now: clock}
	d.Feed([]byte("\x1b]"))
	d.Flush()
	now = now.Add(oscWait + time.Millisecond)
	if got := runes(d.Feed([]byte("jk"))); got != "jk" {
		t.Errorf("keys after the bound: %q", got)
	}
}

// Rendering details the review asked for: the theme's text colour on a
// plain line; a short pane showing the selected tile's head, not its
// divider; no count line for a group header left below; two-cell emoji
// measured as two; Loading within one cell.
func TestChromeEdges(t *testing.T) {
	// Plain text keeps the terminal's foreground; under the band, in a
	// theme that knows the background, the text is the theme's; with the
	// background guessed the selection is reverse video; a divider's
	// border colour is not made faint as well; the viewer's label on a
	// dim line is not faint.
	th, _ := palette.New(true, nil)
	if got := ANSI(plain("x"), th); got != "x\x1b[0m" {
		t.Errorf("plain line: %q", got)
	}
	sel := Line{Reverse: true, Spans: []Span{{Text: "x"}}}
	if got := ANSI(sel, th); !strings.HasPrefix(got, th.SGR(palette.HighlightRowBg, true)+th.SGR(palette.Text, false)) {
		t.Errorf("selection with a known background: %q", got)
	}
	guessed := th
	guessed.Guessed = true
	if got := ANSI(sel, guessed); !strings.HasPrefix(got, "\x1b[7m") || strings.Contains(got, "48;") {
		t.Errorf("selection with a guessed background: %q", got)
	}
	if got := ANSI(Line{Spans: []Span{{Text: "─", Fg: palette.Border, Dim: true}}}, th); strings.Contains(got, "\x1b[2m") {
		t.Errorf("divider made faint on top of its colour: %q", got)
	}
	if got := ANSI(Line{Spans: []Span{{Text: "─", Fg: palette.Border, Dim: true}}}, palette.Mono()); !strings.Contains(got, "\x1b[2m") {
		t.Errorf("divider without colours not faint: %q", got)
	}
	if got := ANSI(Line{Dim: true, Spans: []Span{{Text: "me", Bold: true, Fg: palette.CurrentWorktreeFg}}}, th); !strings.Contains(got, "\x1b[22m\x1b[1m"+th.SGR(palette.CurrentWorktreeFg, false)+"me") {
		t.Errorf("the viewer's label on a dim line: %q", got)
	}
	now := time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)
	in := fixtureInput(now)
	m := &Model{Rows: rows.Build(in), Now: now, Layout: Tiles, Width: 35, Height: 4}
	m.Selected = 0
	if txt := Text(m.Render()); !strings.Contains(txt, "fix-ls") {
		t.Errorf("short pane lost the selected tile's head:\n%s", txt)
	}
	// Two tiles and the collapsed group below: nothing more to count.
	two := rows.Rows{Main: m.Rows.Main[:2], Settled: m.Rows.Settled}
	m = &Model{Rows: two, Now: now, Layout: Tiles, Width: 35, Height: 10, Selected: 1}
	txt := Text(m.Render())
	if strings.Contains(txt, "more") || !strings.Contains(txt, "settled") {
		t.Errorf("group header under the tiles:\n%s", txt)
	}
	if width("✅") != 2 || width("⭐") != 2 || width("a") != 1 {
		t.Errorf("widths: ✅ %d ⭐ %d", width("✅"), width("⭐"))
	}
	m = &Model{Now: now, Icons: Icons{Waiting: "✅"}}
	r := rows.Row{Agent: &protocol.Agent{Activity: protocol.Blocked, Liveness: protocol.Alive}}
	if sp := m.iconSpan(r); width(sp.Text) != iconWidth {
		t.Errorf("a wide override is %d cells: %q", width(sp.Text), sp.Text)
	}
	m = &Model{Now: now, Width: 1, Height: 3, Loading: true}
	for _, l := range m.Render() {
		if lineWidth(l) > 1 {
			t.Errorf("Loading wider than the pane: %q", Text([]Line{l}))
		}
	}
}

// Alt-] is the user's: dropped alone as the Alt chord it is, the key or
// the click after it read, flushed or not.
func TestAltBracket(t *testing.T) {
	keys := Parse([]byte("\x1b]j\x1b[<0;5;3M"))
	if len(keys) != 2 || keys[0].Kind != KeyRune || keys[0].Rune != 'j' || keys[1].Kind != KeyMouse {
		t.Errorf("Alt-] then a key and a click: %+v", keys)
	}
	var d Decoder
	d.Feed([]byte("\x1b]"))
	d.Flush()
	if ks := d.Feed([]byte("j")); len(ks) != 1 || ks[0].Rune != 'j' {
		t.Errorf("a key after a flushed Alt-]: %+v", ks)
	}
}

// No line is wider than the pane, and nothing panics, at any width and
// height, in either layout, whatever is selected, with the icon sets.
func TestWidthSweep(t *testing.T) {
	now := time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)
	for _, layout := range []Layout{Tiles, Compact} {
		for _, icons := range []Icons{{}, {Set: IconsASCII}, {Waiting: "✅"}, {Waiting: "⚠️"}} {
			for w := 1; w <= 60; w++ {
				for _, h := range []int{1, 2, 3, 5, 9, 20} {
					for _, sel := range []int{0, 3, 8} {
						m := model(now)
						m.Layout, m.Icons, m.Width, m.Height, m.Selected, m.Titles = layout, icons, w, h, sel, true
						for _, l := range m.Render() {
							if n := lineWidth(l); n > w {
								t.Fatalf("layout %v icons %+v %dx%d selected %d: a line of %d cells: %q", layout, icons, w, h, sel, n, Text([]Line{l}))
							}
						}
					}
				}
			}
		}
	}
}

// With NO_COLOR the form's focus still shows: the focused chip's frame
// is bold.
func TestFormFocusWithoutColour(t *testing.T) {
	f := NewForm("add a task", [3]Chip{{Title: "repository", Choices: []Choice{{Label: "laatmux"}}}, {Title: "host", Choices: []Choice{{Label: "vm"}}}, {Title: "agent", Choices: []Choice{{Label: "claude"}}}}, "")
	f.focus = 0
	lines := f.Render(80, 20)
	found := false
	for _, l := range lines {
		if strings.Contains(Text([]Line{l}), "repository") && strings.Contains(ANSI(l, palette.Mono()), "\x1b[1m") {
			found = true
		}
	}
	if !found {
		t.Errorf("no bold focus without colour:\n%s", Debug(lines))
	}
}

// An answer cut anywhere in its header, `ESC ] 1 1 ;`, is swallowed
// when its rest comes; a bare Alt-] still is not armed against.
func TestOSCCutInHeader(t *testing.T) {
	now := time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	full := "\x1b]11;rgb:1a1a/1b1b/2626\x1b\\"
	for cut := 2; cut < len(full); cut++ {
		d := Decoder{now: clock}
		d.ExpectAnswer(now.Add(time.Second))
		var ks []Key
		ks = append(ks, d.Feed([]byte(full[:cut]))...)
		ks = append(ks, d.Flush()...)
		ks = append(ks, d.Feed([]byte(full[cut:]+"j"))...)
		ks = append(ks, d.Flush()...)
		if len(ks) != 1 || ks[0].Rune != 'j' {
			t.Errorf("cut at %d: %+v", cut, ks)
		}
	}
}

// The query finds the answer past an echo of itself and past an Alt-]
// typed before it, and keeps the Alt-] and the keys for Run.
func TestBackgroundPastEchoAndAlt(t *testing.T) {
	devnull, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer devnull.Close()
	for _, in := range []string{
		"\x1b]11;?\x1b\\\x1b]11;rgb:ffff/ffff/ffff\x07j",
		"\x1b]\x1b]11;rgb:ffff/ffff/ffff\x07j",
	} {
		r, w, _ := os.Pipe()
		w.WriteString(in)
		term := &Term{in: r, out: devnull}
		start := time.Now()
		dark, ok := term.Background(150 * time.Millisecond)
		if !ok || dark || time.Since(start) > 100*time.Millisecond || !strings.HasSuffix(string(term.pending), "j") {
			t.Errorf("%q: dark %v ok %v pending %q after %v", in, dark, ok, term.pending, time.Since(start))
		}
		r.Close()
		w.Close()
	}
}

// Past the time an answer is expected, the keys typed after an Alt-]
// are the user's, whatever a flush cuts; a string whose number and
// semicolon came is still dropped.
func TestOSCNotExpected(t *testing.T) {
	now := time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	d := Decoder{now: clock}
	d.ExpectAnswer(now.Add(-time.Second))
	var ks []Key
	ks = append(ks, d.Feed([]byte("\x1b]"))...)
	ks = append(ks, d.Flush()...)
	ks = append(ks, d.Feed([]byte("1j"))...)
	ks = append(ks, d.Flush()...)
	if len(ks) != 2 || ks[0].Rune != '1' || ks[1].Rune != 'j' {
		t.Errorf("Alt-] then 1j: %+v", ks)
	}
	ks = append(d.Feed([]byte("\x1b]11;rgb:1a")), d.Flush()...)
	ks = append(ks, d.Feed([]byte("1a/1b1b/2626\x07j"))...)
	ks = append(ks, d.Flush()...)
	if len(ks) != 1 || ks[0].Rune != 'j' {
		t.Errorf("a cut answer: %+v", ks)
	}
}

// The answer arriving ends the expectation.
func TestOSCAnswerEndsExpectation(t *testing.T) {
	now := time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)
	d := Decoder{now: func() time.Time { return now }}
	d.ExpectAnswer(now.Add(time.Second))
	d.Feed([]byte("\x1b]11;rgb:1a1a/1b1b/2626\x07"))
	if !d.expectUntil.IsZero() {
		t.Errorf("still expecting until %v", d.expectUntil)
	}
}

// Under a guessed background the selection is reverse video alone: no
// span colours, no dimming, which would turn into the background.
func TestGuessedSelection(t *testing.T) {
	th, _ := palette.New(true, nil)
	th.Guessed = true
	l := Line{Reverse: true, Dim: true, Spans: []Span{{Text: "a", Fg: palette.Info}, {Text: "b", Dim: true}}}
	got := ANSI(l, th)
	if strings.Contains(got, "38;") || strings.Contains(got, "48;") || strings.Contains(got, "\x1b[2m") || !strings.Contains(got, "\x1b[7m") {
		t.Errorf("%q", got)
	}
	l.Reverse = false
	if got := ANSI(l, th); !strings.Contains(got, "38;") {
		t.Errorf("unselected has no colour: %q", got)
	}
}
