// Package view is the list view the sidebar pane and the dashboard popup
// share: rows with a selection, a header of host problems and a footer,
// drawn into a tmux pane's worth of terminal. The renderer is a pure
// function from the model to lines, so the layouts are tested against
// golden strings without a terminal; the terminal, raw mode and keys
// are in term.go and run.go.
package view

import (
	"fmt"
	"strings"
	"time"

	"github.com/laat/laatmux/internal/protocol"
	"github.com/laat/laatmux/internal/rows"
	"github.com/laat/laatmux/internal/tmux"
)

// Layout is how a row is drawn.
type Layout string

const (
	Tiles   Layout = "tiles"   // three lines per row, for a narrow sidebar
	Compact Layout = "compact" // one line per row, two with titles
)

// ParseLayout reads a layout as config writes it; "" is Tiles.
func ParseLayout(s string) (Layout, error) {
	switch Layout(s) {
	case "", Tiles:
		return Tiles, nil
	case Compact:
		return Compact, nil
	}
	return "", fmt.Errorf("layout %q is not tiles or compact", s)
}

// Model is the state of one view.
type Model struct {
	Rows      rows.Rows
	Layout    Layout
	Titles    bool   // compact draws the pane title under each row
	LocalHost string // the host whose tag is not dimmed
	// Header lines are drawn above the list: hosts that are not
	// connected and listed, the local daemon being down.
	Header []string
	// Hint is the footer when nothing else claims it.
	Hint          string
	Now           time.Time
	Width, Height int

	Filter     string
	Filtering  bool // typing into the filter
	ShowHidden bool // the settled and stale groups are expanded
	Selected   int  // index into Visible; -1 for none while Follow holds
	// Follow keeps the selection on the viewer's own row, wherever the
	// sort moves it, and on nothing when there is no such row, until a
	// key or the wheel moves the selection; from then on the selection
	// is the user's and stays on the row it was put on.
	Follow  bool
	Message string
	// Confirm is a question in the footer; y answers it and any other
	// key withdraws it. Tag says what was asked, for the host.
	Confirm    string
	ConfirmTag string
	// Overlay, when set, takes the screen and the keys until Done.
	Overlay Overlay
	scroll  int // first body line drawn
	// hitIDs is the id of the row each body line drew, "" for none, and
	// hitTop the header lines above the body, both as the last Render
	// drew them: a click names what was on screen, which a refresh or
	// a filter since may have moved.
	hitIDs []string
	hitTop int
	// hitAt is when that render was on the terminal, Now at the time,
	// set again by Run once the frame is written, and the hitPrev fields
	// the render before it, for a click read before the last draw.
	hitAt      time.Time
	hitPrevIDs []string
	hitPrevTop int
	hitPrevAt  time.Time
	// Handoffs are the pending tasks that have handed over to their
	// worktree rows, command id to worktree id, as the merged stream
	// carried them: an anchor on a task the view never saw hand over
	// finds its worktree row through them.
	Handoffs map[string]string
	// anchor is the id of the selected row, so a refresh that reorders
	// or removes rows keeps the selection on the same workspace rather
	// than on the same index, which Enter would then jump to; alias is
	// that row's alias, the worktree row a pending task becomes. lost is
	// a user's selection whose row went with nothing to follow it to:
	// it is on no row until the user moves it.
	anchor string
	alias  string
	lost   bool
	// spinning is whether the last Render drew a spinner frame.
	spinning bool
}

// SetRows replaces the rows, keeping the selection on the row it was on
// when that row is still visible. The anchor is a pair, the row's id
// and its alias, and the lookup takes, in order: the row with the
// anchor's id; the row whose id is the anchor's alias, the worktree
// row once the pending task has gone; the row whose alias is the
// anchor's id, the task standing for a worktree row again; the
// worktree row the handoffs say the anchor's task became; and last,
// for the alias or the handoff when that worktree row is hidden, the
// task standing for it. It
// re-anchors on what it found. A row found by none of these is gone,
// and the selection is cleared rather than left at an index another
// row has taken, until the row is back or the user moves it. While
// Follow holds the selection is the viewer's own row instead, or none.
func (m *Model) SetRows(rs rows.Rows) {
	m.Rows = rs
	if m.Follow {
		m.Selected = m.followed(m.Visible())
		return
	}
	if m.anchor == "" {
		return
	}
	vis := m.Visible()
	find := func(match func(r *rows.Row) bool) bool {
		for _, it := range vis {
			if match(it.Row) {
				m.Selected, m.anchor, m.alias, m.lost = it.Index, it.Row.ID(), it.Row.Alias(), false
				return true
			}
		}
		return false
	}
	anchor, alias, handed := m.anchor, m.alias, m.Handoffs[m.anchor]
	id := func(want string) func(r *rows.Row) bool {
		return func(r *rows.Row) bool { return want != "" && r.ID() == want }
	}
	// A worktree row can be hidden behind another task that stands for
	// it, one whose alias it is: that task is the row's stand-in, taken
	// only when the row itself is not there.
	standing := func(want string) func(r *rows.Row) bool {
		return func(r *rows.Row) bool { return want != "" && r.Alias() == want }
	}
	switch {
	case find(id(anchor)):
	case find(id(alias)):
	case find(standing(anchor)):
	case find(id(handed)):
	case find(standing(alias)):
	case find(standing(handed)):
	default:
		// The anchor is kept: a refresh that coalesced to nothing, a
		// reconnect's first snapshot say, gives the row back, and the
		// selection with it.
		m.Selected, m.lost = -1, true
	}
}

// Group is which group a row is in.
type Group int

const (
	GroupMain Group = iota
	GroupSettled
	GroupStale
)

// Item is one entry of the list as drawn: a row, or a group header.
type Item struct {
	Row    *rows.Row
	Header string
	Group  Group
	// Index is the item's position among the selectable rows, -1 for
	// a header.
	Index int
}

// Items is the list as drawn: main rows, then the settled and stale
// groups, collapsed to one header line unless ShowHidden. The filter
// keeps rows whose name or host contains it, case-insensitively.
func (m *Model) Items() []Item {
	var out []Item
	n := 0
	add := func(rs []rows.Row, g Group) int {
		added := 0
		for i := range rs {
			r := &rs[i]
			if !m.matches(r) {
				continue
			}
			out = append(out, Item{Row: r, Group: g, Index: n})
			n++
			added++
		}
		return added
	}
	add(m.Rows.Main, GroupMain)
	settled, stale := m.count(m.Rows.Settled), m.count(m.Rows.Stale)
	if settled+stale == 0 {
		return out
	}
	if !m.ShowHidden {
		var parts []string
		if settled > 0 {
			parts = append(parts, fmt.Sprintf("settled %d", settled))
		}
		if stale > 0 {
			parts = append(parts, fmt.Sprintf("stale %d", stale))
		}
		out = append(out, Item{Header: strings.Join(parts, "  ") + "  (f shows)", Group: GroupSettled, Index: -1})
		return out
	}
	if settled > 0 {
		out = append(out, Item{Header: "settled", Group: GroupSettled, Index: -1})
		add(m.Rows.Settled, GroupSettled)
	}
	if stale > 0 {
		out = append(out, Item{Header: "stale", Group: GroupStale, Index: -1})
		add(m.Rows.Stale, GroupStale)
	}
	return out
}

func (m *Model) count(rs []rows.Row) int {
	n := 0
	for i := range rs {
		if m.matches(&rs[i]) {
			n++
		}
	}
	return n
}

func (m *Model) matches(r *rows.Row) bool {
	if m.Filter == "" {
		return true
	}
	f := strings.ToLower(m.Filter)
	return strings.Contains(strings.ToLower(r.Name), f) || strings.Contains(strings.ToLower(r.Host), f)
}

// Visible is the selectable rows in display order.
func (m *Model) Visible() []Item {
	var out []Item
	for _, it := range m.Items() {
		if it.Row != nil {
			out = append(out, it)
		}
	}
	return out
}

// followed is the index of the viewer's own row among the visible ones,
// -1 when none is: the filter or a collapsed group can hide it.
func (m *Model) followed(vis []Item) int {
	for _, it := range vis {
		if it.Row.Current {
			return it.Index
		}
	}
	return -1
}

// Selection is the selected row, nil when the list is empty or, while
// Follow holds, when no visible row is the viewer's own. It also records
// the row as the anchor for the next SetRows. While following, the
// selection is found afresh on every read, so a filter typed or cleared
// and a group expanded or collapsed move it as a refresh does.
func (m *Model) Selection() *rows.Row {
	vis := m.Visible()
	if m.Follow {
		m.Selected = m.followed(vis)
	}
	m.clamp(len(vis))
	if len(vis) == 0 || m.Selected < 0 {
		if !m.lost {
			m.anchor, m.alias = "", ""
		}
		return nil
	}
	r := vis[m.Selected].Row
	m.anchor, m.alias = r.ID(), r.Alias()
	return r
}

// clamp keeps the selection inside the list. A following selection may
// be on nothing, and so may a user's whose row went; otherwise a user's
// selection is on a row whenever there is one.
func (m *Model) clamp(n int) {
	if m.Selected >= n {
		m.Selected = n - 1
	}
	if m.Selected < 0 {
		if m.Follow || m.lost {
			m.Selected = -1
			return
		}
		m.Selected = 0
	}
}

// Span is a run of text with its own attributes. Fg is an SGR colour
// code, 0 for the terminal's own.
type Span struct {
	Text string
	Dim  bool
	Fg   int
}

// The spinner a working row's mark cycles through: braille frames as
// workmux drew them, in cyan, one frame per spinTick from the clock,
// so the panes in every window spin in step. It replaces the "*" ls
// prints for a live working agent in the views only; a dim row, whose
// agent is gone or whose host is down, keeps the mark.
var spinnerFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

const (
	spinTick  = 100 * time.Millisecond
	spinnerFg = 36 // cyan
)

// spins reports whether the row's mark is the spinner: a live working
// agent on a row that is not dim, or a pending task that runs.
func spins(r rows.Row) bool {
	if r.Pending != nil {
		return !r.NeedsUser()
	}
	return r.Agent != nil && !r.Dim && r.Agent.Activity == protocol.Working && r.Agent.Liveness == protocol.Alive
}

// Spinning reports whether the last Render drew a spinner, so the host
// ticks the spinner only while one is on screen: a working row that is
// filtered out, in a collapsed group, or scrolled off with its mark, is
// not drawn and not ticked for.
func (m *Model) Spinning() bool { return m.spinning }

// marked is the head of a row as spans, the gutter, the mark with its
// colour, and the rest, clipped to w cells so a narrow pane keeps the
// mark's colour rather than flattening it into text.
func marked(gutter string, mark Span, rest string, w int) []Span {
	switch {
	case w <= 0:
		return nil
	case w == 1:
		return []Span{{Text: gutter}}
	case w == 2:
		return []Span{{Text: gutter}, mark}
	}
	return []Span{{Text: gutter}, mark, {Text: fit(rest, w-2)}}
}

// mark is the row's mark as a span: the spinner frame for Now on a
// spinning row, else the mark ls prints.
func (m *Model) mark(r rows.Row) Span {
	if spins(r) {
		// A zero Now, before the first draw, is a negative count.
		n := int64(len(spinnerFrames))
		i := (m.Now.UnixNano()/int64(spinTick))%n + n
		return Span{Text: spinnerFrames[i%n], Fg: spinnerFg}
	}
	return Span{Text: r.Mark()}
}

// Line is one drawn line: spans and line-wide attributes.
type Line struct {
	Spans   []Span
	Dim     bool
	Reverse bool
	Bold    bool
}

func plain(s string) Line { return Line{Spans: []Span{{Text: s}}} }

// Render draws the model into exactly Height lines of at most Width
// cells each, and records which body line shows which row for the mouse.
func (m *Model) Render() []Line {
	m.spinning = false
	if m.Width <= 0 || m.Height <= 0 {
		return nil
	}
	if m.Overlay != nil {
		return m.Overlay.Render(m.Width, m.Height)
	}
	var out []Line
	for _, h := range m.Header {
		out = append(out, Line{Spans: []Span{{Text: fit(h, m.Width)}}, Bold: true})
	}
	body := m.Height - len(out) - 1
	if body < 1 {
		body = 1
	}
	var lines []Line
	var ids []string
	selStart, selEnd := -1, -1
	m.Selection()
	for _, it := range m.Items() {
		var ls []Line
		if it.Row == nil {
			ls = []Line{{Spans: []Span{{Text: fit(it.Header, m.Width)}}, Dim: true}}
		} else {
			ls = m.row(*it.Row)
			if it.Index == m.Selected {
				selStart, selEnd = len(lines), len(lines)+len(ls)
				for i := range ls {
					ls[i].Reverse = true
				}
			}
		}
		id := ""
		if it.Row != nil {
			id = it.Row.ID()
		}
		for range ls {
			ids = append(ids, id)
		}
		lines = append(lines, ls...)
	}
	// Scroll so the selection is on screen, moving as little as
	// possible; a separator after the selected tile may fall off.
	if selStart >= 0 {
		if selStart < m.scroll {
			m.scroll = selStart
		}
		if selEnd > m.scroll+body {
			m.scroll = selEnd - body
		}
	}
	if m.scroll > len(lines)-body {
		m.scroll = len(lines) - body
	}
	if m.scroll < 0 {
		m.scroll = 0
	}
	m.hitPrevIDs, m.hitPrevTop, m.hitPrevAt = m.hitIDs, m.hitTop, m.hitAt
	m.hitIDs = make([]string, body)
	m.hitTop = len(m.Header)
	m.hitAt = m.Now
	for i := 0; i < body; i++ {
		if j := m.scroll + i; j < len(lines) {
			out = append(out, lines[j])
			m.hitIDs[i] = ids[j]
		} else {
			out = append(out, plain(""))
		}
	}
	for len(out) < m.Height-1 {
		out = append(out, plain(""))
	}
	out = append(out, m.footer())
	out = out[:m.Height]
	// What spins is what is drawn: a body line the height cuts off
	// below the header lines does not count.
	for _, l := range out {
		for _, sp := range l.Spans {
			if sp.Fg == spinnerFg {
				m.spinning = true
			}
		}
	}
	return out
}

func (m *Model) footer() Line {
	switch {
	case m.Confirm != "":
		return Line{Spans: []Span{{Text: fit(m.Confirm, m.Width)}}, Bold: true}
	case m.Message != "":
		return Line{Spans: []Span{{Text: fit(m.Message, m.Width)}}, Bold: true}
	case m.Filtering:
		return plain(fit("/"+m.Filter+"_", m.Width))
	case m.Filter != "":
		return plain(fit("/"+m.Filter+"  (esc clears)", m.Width))
	}
	return Line{Spans: []Span{{Text: fit(m.Hint, m.Width)}}, Dim: true}
}

// row draws one row in the current layout.
func (m *Model) row(r rows.Row) []Line {
	if m.Layout == Compact {
		return m.compact(r)
	}
	return m.tile(r)
}

func (m *Model) gutter(r rows.Row) string {
	if r.Current {
		return ">"
	}
	return " "
}

// where is the host tag: @host, with the server after it for an agent
// observed off the managed server, as ls prints it.
func (m *Model) where(r rows.Row) Span {
	host := r.Host
	if host == "" {
		host = "?"
	}
	s := "@" + host
	if r.Agent != nil {
		if srv := rows.Server(*r.Agent); srv != tmux.LaatmuxServer.Label() {
			s += "/" + srv
		}
	}
	return Span{Text: s, Dim: r.Host != m.LocalHost}
}

func (m *Model) activity(r rows.Row) string {
	s := string(r.Agent.Activity)
	if r.Agent.Liveness == protocol.Gone {
		s += " (gone)"
	}
	return s
}

func (m *Model) age(r rows.Row) string {
	return strings.TrimSpace(rows.Ago(m.Now.Sub(r.Agent.ActivityAt)))
}

// tile is three lines: the mark and name with the host tag right-aligned,
// the agent with its activity and age, and the pane title; two lines
// for a row without an agent, whose second says what it is instead.
// A separator follows.
func (m *Model) tile(r rows.Row) []Line {
	w := m.Width
	where := m.where(r)
	mark := m.mark(r)
	head := m.gutter(r) + mark.Text + " "
	nameW := w - width(head) - width(where.Text) - 1
	first := Line{Dim: r.Dim}
	if nameW < 4 {
		first.Spans = marked(m.gutter(r), mark, " "+r.Name, w)
	} else {
		name := fit(r.Name, nameW)
		gap := w - width(head) - width(name) - width(where.Text)
		first.Spans = []Span{{Text: m.gutter(r)}, mark, {Text: " " + name + strings.Repeat(" ", gap)}, where}
	}
	lines := []Line{first}
	switch {
	case r.Pending != nil:
		// Where the add is, and the detail or the reason under it.
		lines = append(lines, Line{Dim: r.Dim, Spans: []Span{{Text: fit("   "+r.State(), w)}}})
		if d := r.Detail(); d != "" {
			lines = append(lines, Line{Dim: r.Dim, Spans: []Span{{Text: fit("   "+d, w)}}})
		}
	case r.Agent == nil:
		lines = append(lines, Line{Dim: r.Dim, Spans: []Span{{Text: fit("   "+r.State(), w)}}})
	default:
		lines = append(lines,
			Line{Dim: r.Dim, Spans: []Span{{Text: fit("   "+r.AgentName()+"  "+m.activity(r)+"  "+m.age(r), w)}}},
			Line{Dim: r.Dim, Spans: []Span{{Text: fit("   "+strings.TrimSpace(r.Agent.Title), w)}}})
	}
	return append(lines, Line{Dim: true, Spans: []Span{{Text: strings.Repeat("─", w)}}})
}

// compact is one line: mark, activity, agent, name, host tag and age;
// a row without an agent puts what it is in the activity and agent
// columns. With Titles, the pane title follows on a second line.
func (m *Model) compact(r rows.Row) []Line {
	w := m.Width
	where := m.where(r)
	mark := m.mark(r)
	left := m.gutter(r) + mark.Text + " "
	age := ""
	if r.Agent == nil || r.Pending != nil {
		left += fmt.Sprintf("%-16s ", fit(r.State(), 16))
	} else {
		left += fmt.Sprintf("%-8s %-7s ", r.Agent.Activity, r.AgentName())
		age = " " + rows.Ago(m.Now.Sub(r.Agent.ActivityAt))
		if r.Agent.Liveness == protocol.Gone {
			age += " gone"
		}
	}
	nameW := w - width(left) - 1 - width(where.Text) - width(age)
	line := Line{Dim: r.Dim}
	rest := left[len(m.gutter(r))+len(mark.Text):]
	if nameW < 4 {
		line.Spans = marked(m.gutter(r), mark, rest+r.Name, w)
	} else {
		name := fit(r.Name, nameW)
		line.Spans = []Span{{Text: m.gutter(r)}, mark, {Text: rest + name + strings.Repeat(" ", nameW-width(name)+1)}, where, {Text: age}}
	}
	lines := []Line{line}
	switch {
	case m.Titles && r.Pending != nil:
		// The state is cut to its column; the title line has it whole
		// with the detail or the reason.
		s := r.State()
		if d := r.Detail(); d != "" {
			s += ": " + d
		}
		lines = append(lines, Line{Dim: r.Dim, Spans: []Span{{Text: fit("     "+s, w)}}})
	case m.Titles && r.Agent != nil:
		lines = append(lines, Line{Dim: r.Dim, Spans: []Span{{Text: fit("     "+strings.TrimSpace(r.Agent.Title), w)}}})
	}
	return lines
}

// Text is the lines as plain text, one per line, for tests and for a
// terminal without attributes.
func Text(lines []Line) string {
	var b strings.Builder
	for _, l := range lines {
		for _, s := range l.Spans {
			b.WriteString(s.Text)
		}
		b.WriteByte('\n')
	}
	return b.String()
}

// Debug is the lines with their attributes made visible, for golden
// tests: a flag column with S for the selection, D for a dim line, B
// for bold, then the text with dim spans between ‹ and › and coloured
// spans between ⟨ and ⟩.
func Debug(lines []Line) string {
	var b strings.Builder
	for _, l := range lines {
		flags := []byte("...")
		if l.Reverse {
			flags[0] = 'S'
		}
		if l.Dim {
			flags[1] = 'D'
		}
		if l.Bold {
			flags[2] = 'B'
		}
		b.Write(flags)
		b.WriteByte('|')
		for _, s := range l.Spans {
			switch {
			case s.Dim:
				b.WriteString("‹" + s.Text + "›")
			case s.Fg != 0:
				b.WriteString("⟨" + s.Text + "⟩")
			default:
				b.WriteString(s.Text)
			}
		}
		b.WriteByte('\n')
	}
	return b.String()
}

// ANSI encodes a line for the terminal, ending with a reset. A span's
// own attribute, dim or a colour, is set for the span and the line's
// restored after it.
func ANSI(l Line) string {
	var b strings.Builder
	attrs := func() {
		if l.Reverse {
			b.WriteString("\x1b[7m")
		}
		if l.Dim {
			b.WriteString("\x1b[2m")
		}
		if l.Bold {
			b.WriteString("\x1b[1m")
		}
	}
	attrs()
	for _, s := range l.Spans {
		if (s.Dim && !l.Dim) || s.Fg != 0 {
			if s.Dim && !l.Dim {
				b.WriteString("\x1b[2m")
			}
			if s.Fg != 0 {
				fmt.Fprintf(&b, "\x1b[%dm", s.Fg)
			}
			b.WriteString(s.Text)
			b.WriteString("\x1b[0m")
			attrs()
			continue
		}
		b.WriteString(s.Text)
	}
	b.WriteString("\x1b[0m")
	return b.String()
}

// width is the number of terminal cells s takes: wide East Asian and
// emoji runes count two, combining marks, joiners, variation selectors
// and skin-tone modifiers none, everything else one. An approximation
// of what the terminal does, without a grapheme library: a title with
// a joined emoji sequence may measure wide by a cell or two, and the
// line is trimmed to the measure, so the worst case is a short title,
// not a wrapped line.
func width(s string) int {
	n := 0
	for _, r := range s {
		n += runeWidth(r)
	}
	return n
}

func runeWidth(r rune) int {
	switch {
	case r < 0x20, r == 0x7f:
		return 0
	case r < 0x300:
		return 1
	case r >= 0x300 && r <= 0x36f, r >= 0x200b && r <= 0x200f, r >= 0xfe00 && r <= 0xfe0f,
		r >= 0x1f3fb && r <= 0x1f3ff, r >= 0xe0100 && r <= 0xe01ef:
		return 0
	case r >= 0x1100 && r <= 0x115f,
		r >= 0x2e80 && r <= 0xa4cf && r != 0x303f,
		r >= 0xac00 && r <= 0xd7a3,
		r >= 0xf900 && r <= 0xfaff,
		r >= 0xfe30 && r <= 0xfe4f,
		r >= 0xff00 && r <= 0xff60,
		r >= 0xffe0 && r <= 0xffe6,
		r >= 0x1f000 && r <= 0x1faff,
		r >= 0x20000 && r <= 0x3fffd:
		return 2
	}
	return 1
}

// fit trims s to at most w cells, dropping control characters.
func fit(s string, w int) string {
	var b strings.Builder
	n := 0
	for _, r := range s {
		rw := runeWidth(r)
		if rw == 0 && r < 0x20 || r == 0x7f {
			continue
		}
		if n+rw > w {
			break
		}
		b.WriteRune(r)
		n += rw
	}
	return b.String()
}
