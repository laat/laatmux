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
		{"fix-ls", ""},   // the primary label
		{"laatmux", ""},  // the secondary
		{"vm", ""},       // the host
		{"vm.local", ""}, // the host with a domain
		{"  ", ""},
		{"vmware notes", "vmware notes"},
		{"AM-KWMQF9PMFC", ""},       // the machine's name, as tmux titles a pane
		{"AM-KWMQF9PMFC.local", ""}, // with its domain
	} {
		if got := cleanTitle(c.in, "fix-ls", "laatmux", "vm", "AM-KWMQF9PMFC"); got != c.want {
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
	// Eight lines show the first two tiles whole; the rest are below.
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
