package view

import (
	"fmt"
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
	if got := ANSI(Line{Dim: true, Spans: []Span{{Text: "me", Bold: true, Fg: palette.CurrentWorktreeFg, label: true}}}, th); !strings.Contains(got, th.SGR(palette.CurrentWorktreeFg, false)+"me") {
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
	if txt := Text(m.Render()); !strings.Contains(txt, "No agents running") || m.Spinning() {
		t.Errorf("empty: %q", txt)
	}
	m.View = ViewTree
	if txt := Text(m.Render()); !strings.Contains(txt, "No worktrees") {
		t.Errorf("empty tree: %q", txt)
	}
	m.View = ViewAgents
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
	// the stale fold one row among them.
	if want := fmt.Sprintf("↓ %d more\n", total-2); last != want {
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
// the line, and a dim line in the dimmed colour, a span's own colour
// included; a dim line under the band keeps its spans' colours. Without
// colours the selection is reverse video and a dim line the dim
// attribute alone.
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
	dim.Reverse = true
	if got := ANSI(dim, th); !strings.Contains(got, th.SGR(palette.Accent, false)+"x") {
		t.Errorf("colour dim selection: %q", got)
	}
	dim.Reverse = false
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
	if got := ANSI(Line{Spans: []Span{{Text: "─", Fg: palette.Border, Dim: true}}}, th); strings.Contains(got, "\x1b[2m") {
		t.Errorf("divider made faint on top of its colour: %q", got)
	}
	if got := ANSI(Line{Spans: []Span{{Text: "─", Fg: palette.Border, Dim: true}}}, palette.Mono()); !strings.Contains(got, "\x1b[2m") {
		t.Errorf("divider without colours not faint: %q", got)
	}
	if got := ANSI(Line{Dim: true, Spans: []Span{{Text: "me", Bold: true, Fg: palette.CurrentWorktreeFg, label: true}}}, th); !strings.Contains(got, "\x1b[1m"+th.SGR(palette.CurrentWorktreeFg, false)+"me") || strings.Contains(got, "\x1b[2") {
		t.Errorf("the viewer's label on a dim line: %q", got)
	}
	now := time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)
	in := fixtureInput(now)
	m := &Model{Rows: rows.Agents(in, rows.Tree(in)), Now: now, Layout: Tiles, Width: 35, Height: 4}
	m.Selected = 0
	if txt := Text(m.Render()); !strings.Contains(txt, "fix-ls") {
		t.Errorf("short pane lost the selected tile's head:\n%s", txt)
	}
	// Two tiles and the collapsed group below: nothing more to count.
	two := rows.Rows{Main: m.Rows.Main[:2], Stale: m.Rows.Stale}
	m = &Model{Rows: two, Now: now, Layout: Tiles, Width: 35, Height: 10, Selected: 1}
	txt := Text(m.Render())
	if strings.Contains(txt, "more") || !strings.Contains(txt, "stale") {
		t.Errorf("the stale fold under the tiles:\n%s", txt)
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
		if spansWidth(l.Spans) > 1 {
			t.Errorf("Loading wider than the pane: %q", Text([]Line{l}))
		}
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
							if n := spansWidth(l.Spans); n > w {
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

// Without colours the selection is reverse video alone: no span
// colours, no dimming, which would turn into the background. With
// colours it is the band whether or not the background was answered,
// since the band sets both its colours.
func TestMonoSelection(t *testing.T) {
	th := palette.Mono()
	l := Line{Reverse: true, Dim: true, Spans: []Span{{Text: "a", Fg: palette.Info}, {Text: "b", Dim: true}}}
	got := ANSI(l, th)
	if strings.Contains(got, "38;") || strings.Contains(got, "48;") || strings.Contains(got, "\x1b[2m") || !strings.Contains(got, "\x1b[7m") {
		t.Errorf("%q", got)
	}
	dark, _ := palette.New(true, nil)
	if got := ANSI(l, dark); !strings.HasPrefix(got, dark.SGR(palette.HighlightRowBg, true)) || strings.Contains(got, "\x1b[7m") {
		t.Errorf("a selection with colours is the band, not reverse video: %q", got)
	}
}

// TestDimLineNotFaintWithColours pins #333: a dim line in a colour
// theme is the dimmed colour alone, never the faint attribute on top of
// it, which a terminal that blends faint towards the background darkens
// a second time; without colours it is faint.
func TestDimLineNotFaintWithColours(t *testing.T) {
	dark, _ := palette.New(true, nil)
	l := Line{Dim: true, Spans: []Span{{Text: "enter jump  q quit"}}}
	if got := ANSI(l, dark); strings.Contains(got, "\x1b[2m") || !strings.HasPrefix(got, dark.SGR(palette.Dimmed, false)) {
		t.Errorf("a dim line with colours: %q", got)
	}
	if got := ANSI(l, palette.Mono()); !strings.HasPrefix(got, "\x1b[2m") {
		t.Errorf("a dim line without colours: %q", got)
	}
}
