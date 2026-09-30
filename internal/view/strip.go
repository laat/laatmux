package view

import (
	"strconv"
	"strings"
	"time"

	"github.com/laat/laatmux/internal/palette"
	"github.com/laat/laatmux/internal/rows"
)

// The strip: the sidebar along the top, the agent view as a row of
// chips item_width wide, separated by ` │ `, each showing as many of
// the top template's lines as the height allows. The strip scrolls
// sideways to keep the selection in view; chips past the right edge
// are counted there. It shows the agent view only, and Tab does
// nothing. The footer takes the last line only while it has something
// to say: a question, a message, the filter.

// Strip is the layout of a sidebar at the top.
const Strip Layout = "strip"

// stripSep is what separates the chips.
const stripSep = " │ "

// DefaultItemWidth is a chip's width when the model sets none.
const DefaultItemWidth = 24

// itemWidth is the chips' width.
func (m *Model) itemWidth() int {
	if m.ItemWidth <= 0 {
		return DefaultItemWidth
	}
	return min(m.ItemWidth, max(m.Width, 1))
}

// renderStrip draws the strip: the chips from the scroll on, the
// selection in view, the count of chips past the edge in the last
// column, and the footer when it has something to say.
func (m *Model) renderStrip() []Line {
	m.Selection()
	vis := m.Visible()
	iw, sep := m.itemWidth(), width(stripSep)
	height := m.Height
	footer := m.Confirm != "" || m.Message != "" || m.Filtering || m.Filter != ""
	if footer {
		// The footer takes the last line, the only one too: a question
		// is not to be missed.
		height--
	}
	perLine := max((m.Width+sep)/(iw+sep), 1)
	// The scroll: the selection in view, and never past the end.
	if m.Selected >= 0 {
		if m.Selected < m.hscroll {
			m.hscroll = m.Selected
		}
		if m.Selected >= m.hscroll+perLine {
			m.hscroll = m.Selected - perLine + 1
		}
	}
	if m.hscroll > len(vis)-perLine {
		m.hscroll = len(vis) - perLine
	}
	if m.hscroll < 0 {
		m.hscroll = 0
	}
	m.hitPrevIDs, m.hitPrevTop, m.hitPrevAt = m.hitIDs, m.hitTop, m.hitAt
	m.hitIDs, m.hitTop, m.hitAt = nil, 0, m.Now
	m.hitColsPrev, m.hitCols = m.hitCols, nil
	m.hitLines = height
	tmpl := m.templates().Top
	lines := make([]Line, height)
	if len(vis) == 0 {
		text := "No agents running"
		if m.Loading {
			text = frame(m.Now) + " Loading"
			m.spinning = true
		}
		lines[0] = Line{Spans: []Span{{Text: fit(text, m.Width)}}, Dim: true}
	}
	col := 0
	shown := 0
	for i := m.hscroll; i < len(vis) && col+iw <= m.Width; i++ {
		r := vis[i].Row
		m.rowIdx = 0
		if r.Numbered() {
			m.rowIdx = i + 1
		}
		selected := vis[i].Index == m.Selected
		m.hitCols = append(m.hitCols, hitCol{from: col, to: col + iw, id: r.ID()})
		for l := 0; l < height; l++ {
			var spans []Span
			if l < len(tmpl) && !tmpl[l].Blank() {
				spans = m.line(tmpl[l], *r, iw)
			}
			if n := iw - spansWidth(spans); n > 0 {
				spans = append(spans, Span{Text: strings.Repeat(" ", n)})
			}
			if col > 0 {
				lines[l].Spans = append(lines[l].Spans, Span{Text: stripSep, Fg: palette.Border, Dim: true})
			}
			if selected {
				// The band on the chip alone: reverse video span by span.
				for j := range spans {
					spans[j].band = true
				}
			}
			lines[l].Spans = append(lines[l].Spans, spans...)
			lines[l].Dim = false
		}
		col += iw + sep
		shown++
	}
	// The count of chips past the edge and the scope in force, at the
	// right end of the first line, the last chip giving way to them.
	mark := ""
	if more := len(vis) - m.hscroll - shown; more > 0 {
		mark = "→" + strconv.Itoa(more)
	}
	if s := m.ScopeLabel(); s != "" {
		mark = strings.TrimSpace(mark + " [" + s + "]")
	}
	if mark != "" && height > 0 && m.Width > width(mark) {
		room := m.Width - spansWidth(lines[0].Spans)
		if room < width(mark)+1 {
			lines[0].Spans = clip(lines[0].Spans, m.Width-width(mark)-1)
			room = m.Width - spansWidth(lines[0].Spans)
		}
		lines[0].Spans = append(lines[0].Spans, Span{Text: strings.Repeat(" ", max(room-width(mark), 0))}, Span{Text: mark, Dim: true})
	}
	for l := range lines {
		lines[l].Spans = clip(lines[l].Spans, m.Width)
		for _, sp := range lines[l].Spans {
			if sp.spin {
				m.spinning = true
			}
			if sp.tick {
				m.ticking = true
			}
		}
	}
	if footer {
		lines = append(lines, m.footer())
	}
	return lines[:m.Height]
}

// stripKey is what the strip does with the keys that mean something
// else in a list: the arrows and h, l move sideways, Tab does nothing,
// v neither, and a click lands on the chip under it.
func (m *Model) stripKey(k Key) (Action, bool) {
	switch k.Kind {
	case KeyLeft:
		m.move(-1)
	case KeyRight:
		m.move(1)
	case KeyTab:
	case KeyMouse:
		if k.Wheel != 0 {
			m.move(k.Wheel)
			return Action{}, true
		}
		if i := m.hitChip(k.X, k.Y, k.At); i >= 0 {
			if r := m.Visible()[i].Row; r.Kind == rows.KindFold {
				// The stale fold's chip opens and closes it, as its
				// row does in the list.
				m.moveTo(i)
				m.toggleFold(r)
				return Action{}, true
			}
			a := m.jumpTo(i)
			a.Mouse = a.Kind == ActionJump
			return a, true
		}
	case KeyRune:
		switch k.Rune {
		case 'h':
			m.move(-1)
		case 'l':
			m.move(1)
		case 'v', 's':
		default:
			return Action{}, false
		}
	default:
		return Action{}, false
	}
	return Action{}, true
}

// hitCol is a chip's columns and its row, for a click.
type hitCol struct {
	from, to int
	id       string
}

// hitChip is the visible index of the chip a click at column x on line
// y hit, -1 for none: the footer is no chip, and a click from before
// the last draw is on the chips drawn then.
func (m *Model) hitChip(x, y int, at time.Time) int {
	cols := m.hitCols
	if !at.IsZero() && at.Before(m.hitAt) {
		if m.hitPrevAt.IsZero() || at.Before(m.hitPrevAt) {
			return -1
		}
		cols = m.hitColsPrev
	}
	if y < 1 || y > m.hitLines {
		return -1
	}
	for _, c := range cols {
		if x-1 >= c.from && x-1 < c.to {
			for _, it := range m.Visible() {
				if it.Row.ID() == c.id {
					return it.Index
				}
			}
		}
	}
	return -1
}
