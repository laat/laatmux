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
	"unicode"

	"github.com/laat/laatmux/internal/palette"
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
	Rows rows.Rows
	// View is which of the two views is shown; Tree is the tree's nodes,
	// which SetTree sets; folds is the tree's fold state by node id,
	// true for folded, and toggled which the user set.
	View      View
	Tree      []rows.Row
	folds     map[string]bool
	toggled   map[string]bool
	Layout    Layout
	Titles    bool   // compact draws the pane title under each row
	LocalHost string // the host whose tag is not dimmed
	// Header lines are drawn above the list: hosts that are not
	// connected and listed, the local daemon being down.
	Header []HeaderLine
	// Hint is the footer when nothing else claims it.
	Hint          string
	Now           time.Time
	Width, Height int

	Filter     string
	Filtering  bool // typing into the filter
	ShowHidden bool // the agent view's stale fold is open
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
	// Icons is the icon set statuses are drawn with. Loading is that the
	// host has no snapshot yet: the body says so.
	Icons   Icons
	Loading bool
	// Tabs draws the line naming the views above the list, which Tab
	// and a click on it switch.
	Tabs bool
	// Machine is this machine's host name, which tmux titles a pane with
	// until its program sets a title: such a title is dropped.
	Machine string
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
	// spinning is whether the last Render drew a spinner frame, ticking
	// whether it drew a time in seconds.
	spinning bool
	ticking  bool
	// stale is the agent view's fold row, made on each Items.
	stale rows.Row
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
	m.reselect()
}

// reselect finds the selection again after the rows or the tree
// changed, as SetRows describes.
func (m *Model) reselect() {
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
	// The agent view has no worktree rows: a task that handed over to
	// its worktree is followed to the worktree's first tile there,
	// out of the stale fold when it is in it.
	tileIn := func(want string) func(r *rows.Row) bool {
		return func(r *rows.Row) bool {
			return want != "" && r.Kind == rows.KindTile && r.Worktree != nil && r.Worktree.ID == want
		}
	}
	if m.View != ViewTree && handed != "" && !m.ShowHidden {
		for _, r := range m.Rows.Stale {
			if tileIn(handed)(&r) {
				m.ShowHidden = true
				vis = m.Visible()
			}
		}
	}
	switch {
	case find(id(anchor)):
	case find(id(alias)):
	case find(standing(anchor)):
	case find(id(handed)):
	case find(tileIn(handed)):
	case find(standing(alias)):
	case find(standing(handed)):
	default:
		// The anchor is kept: a refresh that coalesced to nothing, a
		// reconnect's first snapshot say, gives the row back, and the
		// selection with it.
		m.Selected, m.lost = -1, true
	}
}

// HeaderLine is a line above the list; Down is that it says something is
// down, drawn in danger, where connecting and the like are a warning.
type HeaderLine struct {
	Text string
	Down bool
}

// Group is which group a row is in.
type Group int

const (
	GroupMain Group = iota
	GroupStale
	GroupSettled
	GroupOrphaned
)

// Item is one entry of the list as drawn: a row, or a group header.
type Item struct {
	Row    *rows.Row
	Header string
	Group  Group
	// Index is the item's position among the selectable rows, -1 for
	// a header.
	Index int
	// Hidden is how many rows a collapsed group's header stands for.
	Hidden int
}

// Items is the list as drawn. In the tree view the nodes the filter and
// the folds leave; in the agent view the main rows, then the stale fold,
// a selectable row of its own, and the stale rows when it is open. The
// filter keeps rows whose name or host contains it, case-insensitively.
func (m *Model) Items() []Item {
	if m.View == ViewTree {
		return m.treeItems()
	}
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
	// The stale fold holds the stale agents and those of settled
	// workspaces; the settled and orphaned groups of the old mixed list
	// fold with them.
	stale := m.count(m.Rows.Stale) + m.count(m.Rows.Settled) + m.count(m.Rows.Orphaned)
	if stale == 0 {
		return out
	}
	m.stale = rows.Row{Kind: rows.KindFold, Node: rows.NodeStale, Name: fmt.Sprintf("%d stale", stale), Children: stale}
	hidden := 0
	if !m.ShowHidden {
		hidden = stale
	}
	out = append(out, Item{Row: &m.stale, Group: GroupStale, Index: n, Hidden: hidden})
	n++
	if m.ShowHidden {
		add(m.Rows.Stale, GroupStale)
		add(m.Rows.Settled, GroupStale)
		add(m.Rows.Orphaned, GroupStale)
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

// Span is a run of text with its own attributes. Fg is a palette name,
// or a colour as the config writes it for an agent's own; "" is the
// terminal's own. spin marks the spinner, so Render knows one is drawn.
type Span struct {
	Text string
	Dim  bool
	Bold bool
	Fg   string
	spin bool
	// tick marks a time in seconds, `m:ss`, so Render knows the clock
	// on screen moves every second.
	tick bool
}

// Spinning reports whether the last Render drew a spinner, so the host
// ticks the spinner only while one is on screen: a working row that is
// filtered out, in a collapsed group, or scrolled off with its icon, is
// not drawn and not ticked for.
func (m *Model) Spinning() bool { return m.spinning }

// Ticking reports whether the last Render drew a time in seconds, so
// the host redraws every second while one is on screen and not at all
// otherwise.
func (m *Model) Ticking() bool { return m.ticking }

// Line is one drawn line: spans and line-wide attributes. Reverse is the
// selection: a background band in a theme with colours, reverse video
// without.
type Line struct {
	Spans   []Span
	Dim     bool
	Reverse bool
	Bold    bool
}

func plain(s string) Line { return Line{Spans: []Span{{Text: s}}} }

// Render draws the model into exactly Height lines of at most Width
// cells each, and records which body line shows which row for the mouse.
// Before the first snapshot the body says it is loading, and a list with
// nothing to show says so. When rows are below the window, its last line
// counts them.
func (m *Model) Render() []Line {
	m.spinning, m.ticking = false, false
	if m.Width <= 0 || m.Height <= 0 {
		return nil
	}
	if m.Overlay != nil {
		return m.Overlay.Render(m.Width, m.Height)
	}
	var out []Line
	if m.Tabs {
		out = append(out, m.tabs())
	}
	for _, h := range m.Header {
		fg := palette.Warning
		if h.Down {
			fg = palette.Danger
		}
		out = append(out, Line{Spans: []Span{{Text: fit(h.Text, m.Width), Fg: fg}}, Bold: true})
	}
	body := m.Height - len(out) - 1
	if body < 1 {
		body = 1
	}
	var lines []Line
	var ids []string
	// starts is where each row begins, and what it counts for below
	// the window: a row one, a collapsed group's header its rows.
	type start struct{ line, count int }
	var starts []start
	selStart, selEnd := -1, -1
	m.Selection()
	items := m.Items()
	for _, it := range items {
		switch {
		case it.Row != nil:
			starts = append(starts, start{len(lines), 1})
		case it.Hidden > 0:
			starts = append(starts, start{len(lines), it.Hidden})
		}
		var ls []Line
		if it.Row == nil {
			ls = []Line{{Spans: []Span{{Text: fit(it.Header, m.Width), Fg: palette.Header, Dim: true}}}}
		} else {
			ls = m.row(*it.Row)
			if it.Index == m.Selected {
				// The divider after a tile is not the tile: a short
				// pane shows the tile's lines, its head first.
				selStart, selEnd = len(lines), len(lines)+len(ls)
				if m.Layout != Compact && len(ls) > 1 {
					selEnd--
				}
				for i := range ls {
					ls[i].Reverse = true
					// The band spans the width, not the text alone.
					if n := m.Width - lineWidth(ls[i]); n > 0 {
						ls[i].Spans = append(ls[i].Spans, Span{Text: strings.Repeat(" ", n)})
					}
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
	if m.Loading || len(items) == 0 {
		starts = nil
	}
	switch {
	case m.Loading:
		lines, ids = []Line{{Spans: clip([]Span{{Text: frame(m.Now), Fg: palette.Info, spin: true}, {Text: " Loading"}}, m.Width)}}, []string{""}
	case len(items) == 0 && m.Filter != "":
		lines, ids = []Line{{Spans: []Span{{Text: fit("Nothing matches /"+m.Filter, m.Width)}}, Dim: true}}, []string{""}
	case len(items) == 0 && m.View == ViewTree:
		lines, ids = []Line{{Spans: []Span{{Text: fit("No worktrees", m.Width)}}, Dim: true}}, []string{""}
	case len(items) == 0:
		lines, ids = []Line{{Spans: []Span{{Text: fit("No agents running", m.Width)}}, Dim: true}}, []string{""}
	}
	// Scroll so the selection is on screen, moving as little as
	// possible; a separator after the selected tile may fall off. With
	// rows below the window its last line is the count of them, so the
	// window is a line shorter.
	// In the tree, the repository line of the node at the top stays
	// pinned above the window while the list scrolls past it, and the
	// window is a line shorter for it.
	window := body
	pinAt := func() int {
		if m.View != ViewTree || m.scroll <= 0 || body < 4 {
			return -1
		}
		return m.scroll
	}
	scrollTo := func() {
		if selStart >= 0 {
			if selStart < m.scroll {
				m.scroll = selStart
			}
			if selEnd > m.scroll+window {
				m.scroll = selEnd - window
			}
			// A tile taller than the window shows its head.
			if selEnd-selStart > window {
				m.scroll = selStart
			}
		}
		if m.scroll > len(lines)-window {
			m.scroll = len(lines) - window
		}
		if m.scroll < 0 {
			m.scroll = 0
		}
	}
	scrollTo()
	if pinAt() >= 0 {
		window = body - 1
		scrollTo()
	}
	// rowsFrom counts the rows that begin at or after line i, a
	// partly shown row not among them, with a collapsed group's.
	rowsFrom := func(i int) int {
		n := 0
		for _, st := range starts {
			if st.line >= i {
				n += st.count
			}
		}
		return n
	}
	more := 0
	if window > 1 && rowsFrom(m.scroll+window) > 0 {
		// Only rows count: a group's header or a divider left below is
		// no reason to give up a line.
		full := window
		window--
		scrollTo()
		more = rowsFrom(m.scroll + window)
		if more == 0 {
			window = full
			scrollTo()
		}
	}
	m.hitPrevIDs, m.hitPrevTop, m.hitPrevAt = m.hitIDs, m.hitTop, m.hitAt
	m.hitIDs = make([]string, body)
	m.hitTop = len(m.Header)
	if m.Tabs {
		m.hitTop++
	}
	m.hitAt = m.Now
	var pinned *Line
	pinnedID := ""
	if pinAt() >= 0 {
		pinned, pinnedID = m.pinned(items, ids)
	}
	shift := 0
	if pinned != nil {
		out = append(out, *pinned)
		m.hitIDs[0] = pinnedID
		shift = 1
	}
	for i := shift; i < body; i++ {
		switch j := m.scroll + i - shift; {
		case i-shift == window:
			out = append(out, Line{Spans: []Span{{Text: fit(fmt.Sprintf("↓ %d more", more), m.Width)}}, Dim: true})
		case j < len(lines):
			out = append(out, lines[j])
			m.hitIDs[i] = ids[j]
		default:
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
			if sp.spin {
				m.spinning = true
			}
			if sp.tick {
				m.ticking = true
			}
		}
	}
	return out
}

// lineWidth is the cells a line's spans take.
func lineWidth(l Line) int {
	n := 0
	for _, sp := range l.Spans {
		n += width(sp.Text)
	}
	return n
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

// row draws one row in the current layout; a tree's node, and the stale
// fold, are one line each.
func (m *Model) row(r rows.Row) []Line {
	if r.Kind != rows.KindTile {
		return m.treeLine(r)
	}
	if m.Layout == Compact {
		return m.compact(r)
	}
	return m.tile(r)
}

// where is the host tag: @host, with the server after it for an agent
// observed off the managed server, as ls prints it; dim for every host
// but this machine.
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

// primary is the primary label as a span: bold, in the current
// worktree's colour, on the viewer's own row, which the gutter's `>`
// marked before.
func (m *Model) primary(r rows.Row, w int) Span {
	p, _ := r.Labels()
	if r.Suffix != "" {
		p += " " + r.Suffix
	}
	sp := Span{Text: fit(p, w)}
	if r.Current {
		sp.Bold, sp.Fg = true, palette.CurrentWorktreeFg
	}
	return sp
}

// since is the time on the right of a row's first line: since its
// agent's status changed, or since a task was submitted; "" for a row
// with neither. secs is that it is in seconds, under an hour.
func (m *Model) since(r rows.Row) (t string, secs bool) {
	var d time.Duration
	switch {
	case r.Pending != nil:
		d = m.Now.Sub(r.Pending.SubmittedAt)
	case r.Agent != nil:
		d = m.Now.Sub(r.Agent.ActivityAt)
	default:
		return "", false
	}
	return elapsed(d), d < time.Hour
}

// third is what a row's third line says: a task's state and its detail;
// for a row without an agent, what it is instead; for a gone agent, that
// it is gone; else the pane title, cleaned.
func (m *Model) third(r rows.Row) string {
	switch {
	case r.Pending != nil:
		s := r.State()
		if d := r.Detail(); d != "" {
			s += ": " + d
		}
		return s
	case r.Agent == nil:
		return r.State()
	case r.Agent.Liveness == protocol.Gone:
		return r.AgentName() + " gone"
	}
	p, sec := r.Labels()
	return cleanTitle(r.Agent.Title, p, sec, r.Host, m.Machine)
}

// head is a row's first line: the stripe, the icon, the primary label,
// and the time since its status changed against the right edge.
func (m *Model) head(r rows.Row) []Span {
	w := m.Width
	icon := m.iconSpan(r)
	lead := []Span{m.stripe(r), {Text: " "}, icon, {Text: " "}}
	used := 1 + 1 + iconWidth + 1
	if w <= used {
		return clip(lead, w)
	}
	t, secs := m.since(r)
	room := w - used
	if t != "" && room-width(t)-1 >= 4 {
		p := m.primary(r, room-width(t)-1)
		gap := room - width(p.Text) - width(t)
		return append(lead, p, Span{Text: strings.Repeat(" ", gap)}, Span{Text: t, tick: secs})
	}
	return append(lead, m.primary(r, room))
}

// second is a row's second line after the stripe: the secondary label
// and the host tag, then the worktree's diff stats against the right
// edge, as much of them as the room past the labels takes.
func (m *Model) second(r rows.Row, indent string) []Span {
	_, sec := r.Labels()
	room := m.Width - 1 - width(indent)
	where := m.where(r)
	labels := width(where.Text)
	if sec != "" {
		labels += width(sec) + 1
	}
	stats := gitSpans(r, room-labels-1)
	if n := spansWidth(stats); n > 0 {
		room -= n + 1
	}
	var out []Span
	if sec == "" {
		where.Text = fit(where.Text, room)
		out = []Span{m.stripe(r), {Text: indent}, where}
	} else {
		s := fit(sec, room)
		out = []Span{m.stripe(r), {Text: indent + s}}
		if room-width(s)-1 > 0 {
			where.Text = fit(where.Text, room-width(s)-1)
			out = append(out, Span{Text: " "}, where)
		}
	}
	if len(stats) > 0 {
		gap := m.Width - spansWidth(out) - spansWidth(stats)
		out = append(out, Span{Text: strings.Repeat(" ", max(gap, 1))})
		out = append(out, stats...)
	}
	return out
}

// gitSpans is a worktree's diff stats in at most w cells: the rebase
// mark R; the committed diff against the base, +N -M in green and red;
// then ✎ and the uncommitted diff, +X -Y bold, a count past the limits
// marked +. A part that is zero is left out. When the line is too narrow
// the committed part goes first, then all but the rebase mark. A refresh
// that timed out leaves them dim.
func gitSpans(r rows.Row, w int) []Span {
	if r.Worktree == nil || r.Worktree.Git == nil || w <= 0 {
		return nil
	}
	g := r.Worktree.Git
	var rebase, committed, uncommitted []Span
	if g.Rebasing {
		rebase = []Span{{Text: "R", Fg: palette.Warning, Bold: true}}
	}
	if g.Committed != [2]int{} {
		committed = []Span{
			{Text: fmt.Sprintf("+%d", g.Committed[0]), Fg: palette.Success, Dim: true},
			{Text: " "},
			{Text: fmt.Sprintf("-%d", g.Committed[1]), Fg: palette.Danger, Dim: true},
		}
	}
	if g.Uncommitted != [2]int{} || g.Dirty {
		added := fmt.Sprintf("+%d", g.Uncommitted[0])
		if g.UncommittedPartial {
			added += "+"
		}
		uncommitted = []Span{{Text: "✎"}}
		if g.Uncommitted != [2]int{} || g.UncommittedPartial {
			// Dirty with no lines, a mode change or a binary file, is
			// the mark alone.
			uncommitted = append(uncommitted,
				Span{Text: " "},
				Span{Text: added, Fg: palette.Success, Bold: true},
				Span{Text: " "},
				Span{Text: fmt.Sprintf("-%d", g.Uncommitted[1]), Fg: palette.Danger, Bold: true})
		}
	}
	join := func(parts ...[]Span) []Span {
		var out []Span
		for _, p := range parts {
			if len(p) == 0 {
				continue
			}
			if len(out) > 0 {
				out = append(out, Span{Text: " "})
			}
			out = append(out, p...)
		}
		return out
	}
	for _, try := range [][]Span{join(rebase, committed, uncommitted), join(rebase, uncommitted), rebase} {
		if len(try) > 0 && spansWidth(try) <= w {
			if g.Stale {
				for i := range try {
					try[i].Dim, try[i].Bold, try[i].Fg = true, false, ""
				}
			}
			return try
		}
	}
	return nil
}

// spansWidth is the cells spans take.
func spansWidth(spans []Span) int {
	n := 0
	for _, sp := range spans {
		n += width(sp.Text)
	}
	return n
}

// tile is three lines: the head; the secondary label and the host tag;
// and the pane title, or what the row is instead. A divider follows. An
// empty third line keeps its place, so tiles keep their height.
func (m *Model) tile(r rows.Row) []Line {
	const indent = "    "
	third := m.titleLine(r, indent)
	return []Line{
		{Dim: r.Dim, Spans: m.head(r)},
		{Dim: r.Dim, Spans: clip(m.second(r, indent), m.Width)},
		{Dim: r.Dim, Spans: clip(third, m.Width)},
		{Spans: []Span{{Text: strings.Repeat("─", m.Width), Fg: palette.Border, Dim: true}}},
	}
}

// compact is one line: the head with the secondary label and host tag
// after the primary. With Titles, the third line of a tile follows.
func (m *Model) compact(r rows.Row) []Line {
	w := m.Width
	icon := m.iconSpan(r)
	lead := []Span{m.stripe(r), {Text: " "}, icon, {Text: " "}}
	used := 1 + 1 + iconWidth + 1
	var line []Span
	if w <= used {
		line = clip(lead, w)
	} else {
		t, secs := m.since(r)
		room := w - used
		if t != "" && room-width(t)-1 >= 4 {
			room -= width(t) + 1
		} else {
			t = ""
		}
		p := m.primary(r, room)
		line = append(lead, p)
		left := room - width(p.Text)
		_, sec := r.Labels()
		if sec != "" && left > 2 {
			s := fit(sec, left-1)
			line = append(line, Span{Text: " " + s})
			left -= 1 + width(s)
		}
		if where := m.where(r); left > 1 {
			where.Text = fit(where.Text, left-1)
			line = append(line, Span{Text: " "}, where)
			left -= 1 + width(where.Text)
		}
		if stats := gitSpans(r, left-2); len(stats) > 0 {
			// Against the time, or the right edge without one.
			n := spansWidth(stats)
			line = append(line, Span{Text: strings.Repeat(" ", left-n)})
			line = append(line, stats...)
			left = 0
		}
		if t != "" {
			line = append(line, Span{Text: strings.Repeat(" ", left+1)}, Span{Text: t, tick: secs})
		}
	}
	lines := []Line{{Dim: r.Dim, Spans: line}}
	if m.Titles {
		lines = append(lines, Line{Dim: r.Dim, Spans: clip(m.titleLine(r, "    "), w)})
	}
	return lines
}

// titleLine is a row's third line: the stripe, the pane title or what
// the row is instead, and the branch's PR and checks against the right
// edge, as much of them as the room past a few cells of title takes.
func (m *Model) titleLine(r rows.Row, indent string) []Span {
	room := m.Width - 1 - len(indent)
	out := []Span{m.stripe(r)}
	if room <= 0 {
		return out
	}
	pr := m.prSpans(r, room-1-min(room/3, 12))
	n := spansWidth(pr)
	if n > 0 {
		room -= n + 1
	}
	title := fit(m.third(r), room)
	out = append(out, Span{Text: indent + title})
	if n > 0 {
		out = append(out, Span{Text: strings.Repeat(" ", max(room-width(title), 0)+1)})
		out = append(out, pr...)
	}
	return out
}

// prSpans is the branch's PR and checks in at most w cells: #N, green
// when open, purple when merged, red when closed, dim when a draft;
// then the checks, ✓ in green, × 3/5 in red, or a spinner and 3/5 in
// purple; a stale answer dim with ? after. On main or master the PR is
// left out, and the checks unless they fail. When narrow the counts go
// first, then the PR.
func (m *Model) prSpans(r rows.Row, w int) []Span {
	b := r.Branch
	if b == nil || w <= 0 {
		return nil
	}
	mainline := r.Worktree != nil && (r.Worktree.Branch == "main" || r.Worktree.Branch == "master")
	var pr []Span
	if b.PR != nil && !mainline {
		// Without colours the states still differ: open bold, merged
		// plain, closed and draft faint.
		sp := Span{Text: fmt.Sprintf("#%d", b.PR.Number)}
		switch {
		case b.PR.Draft:
			sp.Dim = true
		case b.PR.State == "open":
			sp.Fg, sp.Bold = palette.Success, true
		case b.PR.State == "merged":
			sp.Fg = palette.Accent
		default:
			sp.Fg, sp.Dim = palette.Danger, true
		}
		pr = []Span{sp}
	}
	var mark, counts []Span
	if c := b.Checks; c != nil && (!mainline || c.State == protocol.ChecksFailure) {
		ratio := fmt.Sprintf("%d/%d", c.Passed, c.Total)
		ascii := m.Icons.Set == IconsASCII
		switch c.State {
		case protocol.ChecksSuccess:
			mark = []Span{{Text: map[bool]string{false: "✓", true: "ok"}[ascii], Fg: palette.Success}}
		case protocol.ChecksFailure:
			mark = []Span{{Text: map[bool]string{false: "×", true: "x"}[ascii], Fg: palette.Danger}}
			counts = []Span{{Text: " " + ratio, Fg: palette.Danger}}
		case protocol.ChecksPending:
			// The spinner spins on a live row with a fresh answer; a
			// stale or dim one stands still.
			spinning := !b.Stale && !r.Dim && !ascii
			text := string([]rune(spinnerFrames[0])[0])
			switch {
			case ascii:
				text = "*"
			case spinning:
				text = string([]rune(frame(m.Now))[0])
			}
			mark = []Span{{Text: text, Fg: palette.Accent, spin: spinning}}
			counts = []Span{{Text: " " + ratio, Fg: palette.Accent}}
		}
	}
	join := func(parts ...[]Span) []Span {
		var out []Span
		for _, p := range parts {
			if len(p) == 0 {
				continue
			}
			if len(out) > 0 {
				out = append(out, Span{Text: " "})
			}
			out = append(out, p...)
		}
		if len(out) > 0 && b.Stale {
			out = append(out, Span{Text: "?"})
			for i := range out {
				out[i].Dim, out[i].Fg = true, ""
			}
		}
		return out
	}
	checks := append(append([]Span{}, mark...), counts...)
	for _, try := range [][]Span{join(pr, checks), join(pr, mark), join(mark)} {
		if len(try) > 0 && spansWidth(try) <= w {
			return try
		}
	}
	return nil
}

// clip cuts spans to w cells, keeping each span's attributes.
func clip(spans []Span, w int) []Span {
	var out []Span
	for _, sp := range spans {
		if w <= 0 {
			break
		}
		t := fit(sp.Text, w)
		w -= width(t)
		sp.Text = t
		out = append(out, sp)
	}
	return out
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
// for bold, then the text with dim spans between ‹ and ›, bold ones
// between « and », and coloured ones between ⟨ and ⟩ with the palette
// name first.
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
			t := s.Text
			if s.Fg != "" {
				t = "⟨" + s.Fg + ":" + t + "⟩"
			}
			if s.Bold {
				t = "«" + t + "»"
			}
			if s.Dim {
				t = "‹" + t + "›"
			}
			b.WriteString(t)
		}
		b.WriteByte('\n')
	}
	return b.String()
}

// ANSI encodes a line for the terminal in a theme, ending with a reset.
// A span's own attributes are set for the span and the line's restored
// after it. In a theme with colours a span's colour is drawn, and a dim
// line is drawn in the dimmed colour throughout, but for the viewer's
// own row's label; plain text keeps the terminal's own foreground, which
// is right whatever the background. The selection is the highlight
// background with the theme's text on it when the theme knows the
// terminal's background, and reverse video otherwise, as it is without
// colours, where the attributes are all there is: under reverse video
// a line has no colours and no dimming, which would land in the
// background.
func ANSI(l Line, th palette.Theme) string {
	colour := !th.Mono && th.SGR(palette.Text, false) != ""
	band := colour && !th.Guessed && l.Reverse
	if l.Reverse && !band {
		// Reverse video swaps every colour into the background: the
		// selection with a guessed background is the terminal's own
		// pair, reversed, and the attributes alone.
		colour = false
	}
	var b strings.Builder
	attrs := func() {
		switch {
		case band:
			b.WriteString(th.SGR(palette.HighlightRowBg, true))
			// A dim line under the band is drawn in the text colour,
			// not faint: the stripe and icon say it is dim.
			b.WriteString(th.SGR(palette.Text, false))
		case l.Reverse:
			b.WriteString("\x1b[7m")
		}
		if l.Dim && !band && !l.Reverse {
			b.WriteString("\x1b[2m")
			if colour {
				b.WriteString(th.SGR(palette.Dimmed, false))
			}
		}
		if l.Bold {
			b.WriteString("\x1b[1m")
		}
	}
	attrs()
	for _, s := range l.Spans {
		fg := ""
		current := s.Fg == palette.CurrentWorktreeFg
		if colour && s.Fg != "" && (!l.Dim || band || current) {
			fg = th.SGR(s.Fg, false)
		}
		// A span's faint is for a theme without colours; with them its
		// colour, the border's say, is faint enough.
		faint := s.Dim && !l.Dim && fg == ""
		if faint || s.Bold || fg != "" {
			if current && l.Dim && !band {
				// The viewer's label is not faint on a dim line.
				b.WriteString("\x1b[22m")
			}
			if faint {
				b.WriteString("\x1b[2m")
			}
			if s.Bold {
				b.WriteString("\x1b[1m")
			}
			b.WriteString(fg)
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
	rs := []rune(s)
	for i, r := range rs {
		n += cellWidth(rs, i, r)
	}
	return n
}

// cellWidth is the cells rs[i] takes: runeWidth, but a one-cell symbol
// followed by the emoji variation selector, U+FE0F, is drawn as an emoji,
// two cells, as ⚠️ and ✔️ are.
func cellWidth(rs []rune, i int, r rune) int {
	w := runeWidth(r)
	if w == 1 && i+1 < len(rs) && rs[i+1] == 0xfe0f {
		return 2
	}
	return w
}

func runeWidth(r rune) int {
	switch {
	case r < 0x20, r == 0x7f:
		return 0
	case r < 0x300:
		return 1
	case r >= 0x300 && r <= 0x36f, r >= 0x200b && r <= 0x200f, r >= 0xfe00 && r <= 0xfe0f,
		r >= 0x1f3fb && r <= 0x1f3ff, r >= 0xe0100 && r <= 0xe01ef,
		unicode.In(r, unicode.Mn, unicode.Me):
		// Combining marks, the keycap's U+20E3 among them, joiners,
		// selectors and skin tones.
		return 0
	case r >= 0x1100 && r <= 0x115f,
		r >= 0x2e80 && r <= 0xa4cf && r != 0x303f,
		r >= 0xac00 && r <= 0xd7a3,
		r >= 0xf900 && r <= 0xfaff,
		r >= 0xfe30 && r <= 0xfe4f,
		r >= 0xff00 && r <= 0xff60,
		r >= 0xffe0 && r <= 0xffe6,
		r >= 0x1f000 && r <= 0x1faff,
		r >= 0x20000 && r <= 0x3fffd,
		emojiWide(r):
		return 2
	}
	return 1
}

// emojiWide is the runes below the emoji planes that terminals draw two
// cells wide by default: the symbols Unicode gives emoji presentation,
// ✅ ⌛ ⭐ ❌ and the like, which a status icon set in the config may be.
func emojiWide(r rune) bool {
	switch {
	case r == 0x231a, r == 0x231b, r >= 0x23e9 && r <= 0x23ec, r == 0x23f0, r == 0x23f3,
		r == 0x25fd, r == 0x25fe, r == 0x2614, r == 0x2615, r >= 0x2648 && r <= 0x2653,
		r == 0x267f, r == 0x2693, r == 0x26a1, r == 0x26aa, r == 0x26ab, r == 0x26bd, r == 0x26be,
		r == 0x26c4, r == 0x26c5, r == 0x26ce, r == 0x26d4, r == 0x26ea, r == 0x26f2, r == 0x26f3,
		r == 0x26f5, r == 0x26fa, r == 0x26fd, r == 0x2705, r == 0x270a, r == 0x270b, r == 0x2728,
		r == 0x274c, r == 0x274e, r >= 0x2753 && r <= 0x2755, r == 0x2757, r >= 0x2795 && r <= 0x2797,
		r == 0x27b0, r == 0x27bf, r == 0x2b1b, r == 0x2b1c, r == 0x2b50, r == 0x2b55:
		return true
	}
	return false
}

// fit trims s to at most w cells, dropping control characters.
func fit(s string, w int) string {
	var b strings.Builder
	n := 0
	rs := []rune(s)
	for i, r := range rs {
		rw := cellWidth(rs, i, r)
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
