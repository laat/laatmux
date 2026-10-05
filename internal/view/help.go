package view

import (
	"strings"

	"github.com/laat/laatmux/internal/term"
)

// Help is the ? overlay: the keys, one a line, scrolled by the arrows
// and the wheel, closed by any other key.
type Help struct {
	Title  string
	Lines  []string
	done   bool
	scroll int
	body   int
}

// helpKeys are the keys both views share, sidebar and dashboard alike.
var helpKeys = []string{
	"j k ↑ ↓      move",
	"g G          first, last",
	"1..9         jump to the nth tile or worktree line",
	"Enter click  jump; fold a repository line or the stale fold",
	"/            filter; Esc clears",
	"Tab          switch view: agents, tree",
	"h l ← →      fold, unfold; h on a child goes to its line",
	"s            toggle the fold at the selection",
	"f            open every fold when any is closed, else close every one",
	"v            tiles or compact",
	"F            scope to the viewer's session, and back",
	"z            settle or unsettle",
	"?            this help",
}

// stripKeys are the strip's: no views, folds or layouts there.
var stripKeys = []string{
	"h l ← → j k  move",
	"g G          first, last",
	"1..9         jump to the nth chip",
	"Enter click  jump; the stale chip folds",
	"/            filter; Esc clears",
	"f            the stale chip, open or closed",
	"F            scope to the viewer's session, and back",
	"z            settle or unsettle",
	"?            this help",
}

// NewHelp is the overlay for the keys: the shared ones, or the strip's
// with strip set, then the host's own, the dashboard's actions say.
func NewHelp(title string, strip bool, extra ...string) *Help {
	keys := helpKeys
	if strip {
		keys = stripKeys
	}
	return &Help{Title: title, Lines: append(append([]string{}, keys...), extra...)}
}

func (h *Help) Render(w, hgt int) []Line {
	if w <= 0 || hgt <= 0 {
		return nil
	}
	if hgt < 3 {
		// A strip a line or two high: the keys alone, scrolled.
		h.body = hgt
		if h.scroll > len(h.Lines)-h.body {
			h.scroll = len(h.Lines) - h.body
		}
		if h.scroll < 0 {
			h.scroll = 0
		}
		var out []Line
		for i := 0; i < hgt; i++ {
			if j := h.scroll + i; j < len(h.Lines) {
				out = append(out, plain(fit(h.Lines[j], w)))
			} else {
				out = append(out, plain(""))
			}
		}
		return out
	}
	out := []Line{{Spans: []Span{{Text: fit(h.Title, w)}}, Bold: true}}
	h.body = max(hgt-2, 1)
	if h.scroll > len(h.Lines)-h.body {
		h.scroll = len(h.Lines) - h.body
	}
	if h.scroll < 0 {
		h.scroll = 0
	}
	for i := 0; i < h.body; i++ {
		if j := h.scroll + i; j < len(h.Lines) {
			out = append(out, plain(fit(h.Lines[j], w)))
		} else {
			out = append(out, plain(""))
		}
	}
	for len(out) < hgt-1 {
		out = append(out, plain(""))
	}
	hint := "any key closes"
	if len(h.Lines) > h.body {
		hint = "↑ ↓ scroll  any other key closes"
	}
	out = append(out, Line{Spans: []Span{{Text: fit(hint, w)}}, Dim: true})
	return out[:hgt]
}

func (h *Help) Handle(k term.Key) {
	switch {
	case k.Kind == term.KeyUp || k.Kind == term.KeyMouse && k.Wheel < 0:
		h.scroll--
	case k.Kind == term.KeyDown || k.Kind == term.KeyMouse && k.Wheel > 0:
		h.scroll++
	case k.Kind == term.KeyMouse:
		// A click or a release is not a key pressed.
	default:
		h.done = true
	}
}

func (h *Help) Done() bool { return h.done }

// Text is the help as plain lines, for tests.
func (h *Help) Text() string { return strings.Join(h.Lines, "\n") }
