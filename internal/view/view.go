// Package view is the list view the sidebar pane and the dashboard popup
// share: rows with a selection, a header of host problems and a footer,
// drawn into a tmux pane's worth of terminal. The renderer is a function
// from the model to lines, keeping what the next frame, click or refresh
// needs: the scroll, the click map and the spinner flags, the selection
// settled, a new line's first fold. The layouts are tested against
// golden strings without a terminal; the terminal and its keys are
// internal/term, the loop run.go.
package view

import (
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/laat/laatmux/internal/palette"
	"github.com/laat/laatmux/internal/protocol"
	"github.com/laat/laatmux/internal/rows"
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
	View View
	Tree []rows.Row
	// Scope is what the pane shows by the viewer's row: all, session or
	// project; prevScope what F goes back to.
	Scope     Scope
	prevScope Scope
	settings  bool            // a setting changed since SettingsChanged last asked
	viewSet   bool            // the view chosen by a key since ChangedDefaults last asked
	layoutSet bool            // the layout likewise
	dirty     map[string]bool // the folds set here since DirtyFolds last asked
	carried   map[string]bool // of those, the ones a handoff carried
	applied   map[string]bool // the file's folds as last applied
	// AskQuit has q and Ctrl-C ask before the view ends, in a sidebar
	// pane. HelpTitle and Help are the ? overlay's title and the host's
	// own lines after the shared keys.
	AskQuit   bool
	HelpTitle string
	Help      []string
	// ItemWidth is a strip's chip width; hscroll the first chip drawn
	// and hitCols the chips' columns, for a click.
	ItemWidth    int
	hscroll      int
	hitCols      []hitCol
	hitColsPrev  []hitCol
	hitLines     int // the strip's lines of chips, as last drawn
	hitLinesPrev int
	folds        map[string]bool
	toggled      map[string]bool
	Layout       Layout
	Titles       bool   // compact draws the pane title under each row
	LocalHost    string // the host whose tag is not dimmed
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
	// AgentIcons is the config's agent icons over the defaults, for the
	// {agent_icon} token; JumpKeys that the tmux jump keys are bound,
	// which {jump_key} names.
	AgentIcons map[string]AgentIcon
	JumpKeys   bool
	tmpl       *Templates // the lines' templates, the defaults until set
	scroll     int        // first body line drawn
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
	// its worktree is followed to the worktree's first agent's tile,
	// in the tree's order, out of the stale fold when it is in it.
	first := ""
	if m.View != ViewTree && handed != "" {
		first = m.firstAgentUnder(handed)
		if first != "" && !m.ShowHidden {
			for _, r := range m.Rows.Stale {
				if r.ID() == first {
					m.ShowHidden = true
					vis = m.Visible()
				}
			}
		}
	}
	switch {
	case find(id(anchor)):
	case find(id(alias)):
	case find(standing(anchor)):
	case find(id(handed)):
	case find(id(first)):
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

// Item is one entry of the list as drawn: a row, or a header in the
// tree.
type Item struct {
	Row    *rows.Row
	Header string
	// Index is the item's position among the selectable rows, -1 for
	// a header.
	Index int
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
	add := func(rs []rows.Row) {
		for i := range rs {
			r := &rs[i]
			if !m.matches(r) || !m.inScope(r) {
				continue
			}
			out = append(out, Item{Row: r, Index: n})
			n++
		}
	}
	add(m.Rows.Main)
	// The stale fold holds the stale agents and those of settled
	// workspaces.
	stale := m.count(m.Rows.Stale)
	if stale == 0 {
		return out
	}
	m.stale = rows.Row{Kind: rows.KindFold, Node: rows.NodeStale, Name: fmt.Sprintf("%d stale", stale), Children: stale}
	out = append(out, Item{Row: &m.stale, Index: n})
	n++
	if m.ShowHidden {
		add(m.Rows.Stale)
	}
	return out
}

func (m *Model) count(rs []rows.Row) int {
	n := 0
	for i := range rs {
		if m.matches(&rs[i]) && m.inScope(&rs[i]) {
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
// Follow holds, when no visible row is the viewer's own. It commits the
// selection first: while following, the selection is found afresh on
// every read, so a filter typed or cleared and a group expanded or
// collapsed move it as a refresh does.
func (m *Model) Selection() *rows.Row { return m.commit() }

// commit settles the selection after a change of the rows, the filter
// or the folds, and records the row as the anchor for the next SetRows:
// following, it is the viewer's row again; a user's is clamped to the
// list, or stays on no row while lost. Called for the effect by the
// keys that change what is visible; Selection is it with the row.
func (m *Model) commit() *rows.Row {
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
	Bg   string // a template's #[bg=…]; "" for the line's
	own  bool   // the look is the span's own: a template's style leaves it
	band bool   // the selection's band on this span alone: a strip's chip
	// label marks the viewer's own label, what primary builds on the
	// viewer's own row: the primary label and the pane's suffix, which
	// a dim line leaves in their colour and not faint. The mark, not
	// the colour, which a template can give any text.
	label bool
	spin  bool
	// faded marks a span dimmed as a dim line draws it, so on a line
	// that is not dim, a dim row's chip in the strip, it is drawn as
	// that line would draw it: faint, and in a theme with colours in
	// the dimmed colour.
	faded bool
	// tick marks a time in seconds, `m:ss`, so Render knows the clock
	// on screen moves every second.
	tick bool
}

// dimmed is the span as a dim line draws it: dim throughout, without a
// template's background, and without its colour but for the viewer's
// own label, which keeps its colour and is not faint. ANSI draws a dim
// line's spans so off the band, and the strip a dim row's chip, whose
// line is not dim: the span is faded, so there too it is drawn in the
// dimmed colour the line would give it.
func (s Span) dimmed() Span {
	s.Bg = ""
	if !s.label {
		s.Dim, s.Fg, s.faded = true, "", true
	} else {
		s.Dim = false
	}
	return s
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

// bold and dim are one-span lines in those attributes, fit to the
// width: a title, a message and an error are bold, a hint dim.
func bold(s string, w int) Line { return Line{Spans: []Span{{Text: fit(s, w)}}, Bold: true} }
func dim(s string, w int) Line  { return Line{Spans: []Span{{Text: fit(s, w)}}, Dim: true} }

// framed is an overlay's frame h lines tall: the lines so far, blank
// lines to the height but one, and foot as the last line, whatever the
// body's length.
func framed(out []Line, h int, foot Line) []Line {
	for len(out) < h-1 {
		out = append(out, plain(""))
	}
	return append(out[:h-1], foot)
}

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
	if m.Layout == Strip {
		return m.renderStrip()
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
	// starts is the line each row begins on, for the count of rows
	// below the window; the stale fold is a row.
	var starts []int
	selStart, selEnd := -1, -1
	m.commit()
	items := m.Items()
	numbered := 0 // the rows the digits count, for {idx}
	for _, it := range items {
		if it.Row != nil {
			starts = append(starts, len(lines))
		}
		var ls []Line
		if it.Row == nil {
			ls = []Line{{Spans: []Span{{Text: fit(it.Header, m.Width), Fg: palette.Header, Dim: true}}}}
		} else {
			idx := 0
			if it.Row.Numbered() {
				numbered++
				idx = numbered
			}
			ls = m.row(*it.Row, idx)
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
					if n := m.Width - spansWidth(ls[i].Spans); n > 0 {
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
	// rowsFrom counts the rows that begin at or after line i, a
	// partly shown row not among them.
	rowsFrom := func(i int) int {
		n := 0
		for _, st := range starts {
			if st >= i {
				n++
			}
		}
		return n
	}
	// The scroll: where it was, back up to the selection when that is
	// above, never past the end with a full window; then down, a line
	// at a time, until the selection is in the window. Two lines the
	// window gives up when needed, decided at each candidate: in the
	// tree, the repository line of the node at the top, pinned above
	// the window while the list scrolls past it; and "↓ N more" at the
	// bottom for rows left below, where only rows count: a group's
	// header or a divider left below is no reason to give up a line.
	// Deciding both at the scroll drawn makes the render its own fixed
	// point: the next one, from the same state, draws the same.
	window := body
	reserved, tail, more := false, false, 0
	s := m.scroll
	if selStart >= 0 && selStart < s {
		s = selStart
	}
	if s > len(lines)-body {
		s = len(lines) - body
	}
	if s < 0 {
		s = 0
	}
	for ; ; s++ {
		m.scroll = s
		reserved = false
		if m.View == ViewTree && body >= 4 {
			l, _ := m.pinned(items, ids)
			reserved = l != nil
		}
		window = body
		if reserved {
			window--
		}
		tail = window > 1 && rowsFrom(s+window) > 0
		more = 0
		if tail {
			window--
			more = rowsFrom(s + window)
		}
		// A tile taller than the window shows its head.
		if selStart < 0 || selStart == s || selStart > s && selEnd <= s+window || s >= len(lines)-1 {
			break
		}
	}
	m.hitPrevIDs, m.hitPrevTop, m.hitPrevAt = m.hitIDs, m.hitTop, m.hitAt
	m.hitIDs = make([]string, body)
	m.hitTop = len(m.Header)
	if m.Tabs {
		m.hitTop++
	}
	m.hitAt = m.Now
	shift := 0
	if reserved {
		pinned, id := m.pinned(items, ids)
		out = append(out, *pinned)
		m.hitIDs[0] = id
		shift = 1
	}
	for i := shift; i < body; i++ {
		switch j := m.scroll + i - shift; {
		case tail && i-shift == window:
			out = append(out, dim(fmt.Sprintf("↓ %d more", more), m.Width))
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

func (m *Model) footer() Line {
	switch {
	case m.mode() == modeConfirm:
		return bold(m.Confirm, m.Width)
	case m.Message != "":
		return bold(m.Message, m.Width)
	case m.mode() == modeFilter:
		return plain(fit("/"+m.Filter+"_", m.Width))
	case m.Filter != "":
		return plain(fit("/"+m.Filter+"  (esc clears)", m.Width))
	}
	hint := m.Hint
	if s := m.ScopeLabel(); s != "" {
		// The scope in force, ahead of the keys.
		hint = "[" + s + "]  " + hint
	}
	return dim(hint, m.Width)
}

// row draws one row in the current layout, numbered idx among the rows
// the digits count (0 for one they do not); a tree's node, and the
// stale fold, are one line each.
func (m *Model) row(r rows.Row, idx int) []Line {
	if r.Kind != rows.KindTile {
		return m.treeLine(r, idx)
	}
	if m.Layout == Compact {
		return m.compact(r, idx)
	}
	return m.tile(r, idx)
}

// where is an agent's host: the host, with the server after it for an
// agent observed off the managed server, as ls prints it; dim for every
// host but this machine. The {host} token draws it on a tile or an
// agent node, and the other-sessions line in its brackets.
func (m *Model) where(r rows.Row) Span {
	s := r.HostName()
	if r.Agent != nil {
		if srv := r.Agent.Server; srv != protocol.ServerLaatmux {
			s += "/" + srv
		}
	}
	return Span{Text: s, Dim: r.Host != m.LocalHost}
}

// primary is the primary label as a span: bold, in the current
// worktree's colour, and marked the viewer's label, on the viewer's own
// row, which the gutter's `>` marked before.
func (m *Model) primary(r rows.Row, p string) Span {
	sp := Span{Text: p}
	if r.Current {
		sp.Bold, sp.Fg, sp.label = true, palette.CurrentWorktreeFg, true
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
	rebase, committed, uncommitted := gitRebase(g), gitCommitted(g), gitUncommitted(g)
	for _, try := range [][]Span{joinSpans(rebase, committed, uncommitted), joinSpans(rebase, uncommitted), rebase} {
		if len(try) > 0 && spansWidth(try) <= w {
			if g.Stale {
				gitStale(try)
			}
			return try
		}
	}
	return nil
}

// gitSync is how the branch stands against its base, in at most w
// cells: →base when the base is not main, master or the branch itself,
// its origin/ taken off, the whole at most gitBaseWidth cells, cut
// with …; the conflict mark ! in red; ↑A and ↓B. When the line is too
// narrow the base is cut further, to four cells at the least, then
// goes, then ↓B, then ↑A. A refresh that timed out leaves them dim;
// nil when there is nothing to say.
func gitSync(r rows.Row, w int) []Span {
	if r.Worktree == nil || r.Worktree.Git == nil || w <= 0 {
		return nil
	}
	g := r.Worktree.Git
	var base, conflict, ahead, behind []Span
	if short := strings.TrimPrefix(g.Base, "origin/"); short != "" && short != "main" && short != "master" && short != r.Worktree.Branch {
		if width(short) > gitBaseWidth-1 {
			short = fit(short, gitBaseWidth-2) + "…"
		}
		base = []Span{{Text: "→" + short}}
	}
	if g.Conflict != nil && *g.Conflict {
		conflict = []Span{{Text: "!", Fg: palette.Danger, Bold: true}}
	}
	if g.Ahead > 0 {
		ahead = []Span{{Text: "↑" + strconv.Itoa(g.Ahead)}}
	}
	if g.Behind > 0 {
		behind = []Span{{Text: "↓" + strconv.Itoa(g.Behind)}}
	}
	rest := joinSpans(conflict, ahead, behind)
	if base != nil && spansWidth(joinSpans(base, rest)) > w {
		// The base cut to the room left beside the rest, → and two
		// letters at the least.
		room := w - spansWidth(rest)
		if len(rest) > 0 {
			room--
		}
		if room >= 4 {
			base = cutSpans(base, room)
		}
	}
	for _, try := range [][]Span{joinSpans(base, rest), rest, joinSpans(conflict, ahead), conflict} {
		if len(try) > 0 && spansWidth(try) <= w {
			if g.Stale {
				gitStale(try)
			}
			return try
		}
	}
	return nil
}

// gitBaseWidth is the most cells →base takes, the arrow counted,
// before it is cut: a base is a branch name, which can be long, and
// says less than the stats and the checks beside it.
const gitBaseWidth = 12

// gitRebase is the rebase mark R, nil when not rebasing.
func gitRebase(g *protocol.GitStatus) []Span {
	if !g.Rebasing {
		return nil
	}
	return []Span{{Text: "R", Fg: palette.Warning, Bold: true}}
}

// gitCommitted is the committed diff against the base, +N -M dim in
// green and red; nil when zero.
func gitCommitted(g *protocol.GitStatus) []Span {
	if g.Committed == [2]int{} {
		return nil
	}
	return []Span{
		{Text: fmt.Sprintf("+%d", g.Committed[0]), Fg: palette.Success, Dim: true},
		{Text: " "},
		{Text: fmt.Sprintf("-%d", g.Committed[1]), Fg: palette.Danger, Dim: true},
	}
}

// gitUncommitted is ✎ and the uncommitted diff, +X -Y bold, a count
// past the limits marked +; dirty with no lines, a mode change or a
// binary file, is the mark alone; nil when clean.
func gitUncommitted(g *protocol.GitStatus) []Span {
	if g.Uncommitted == [2]int{} && !g.Dirty {
		return nil
	}
	out := []Span{{Text: "✎"}}
	if g.Uncommitted != [2]int{} || g.UncommittedPartial {
		added := fmt.Sprintf("+%d", g.Uncommitted[0])
		if g.UncommittedPartial {
			added += "+"
		}
		out = append(out,
			Span{Text: " "},
			Span{Text: added, Fg: palette.Success, Bold: true},
			Span{Text: " "},
			Span{Text: fmt.Sprintf("-%d", g.Uncommitted[1]), Fg: palette.Danger, Bold: true})
	}
	return out
}

// joinSpans is the parts with a space between them, the empty ones
// left out.
func joinSpans(parts ...[]Span) []Span {
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

// gitStale makes spans from an answer that is stale dim and plain: the
// git stats of a refresh that timed out, a PR's number, state, checks
// or detail from a query that failed or an answer gone old. Dim with no
// colour, they take a template style's background alone (styled), so a
// style does not colour or embolden them again.
func gitStale(spans []Span) []Span {
	for i := range spans {
		spans[i].Dim, spans[i].Bold, spans[i].Fg = true, false, ""
	}
	return spans
}

// spansWidth is the cells spans take.
func spansWidth(spans []Span) int {
	n := 0
	for _, sp := range spans {
		n += width(sp.Text)
	}
	return n
}

// tile is the tiles template's lines, three by default: the head; the
// secondary label and the host tag; and the pane title, or what the row
// is instead. A divider follows. An empty third line keeps its place,
// so tiles keep their height; a blank template is no line.
func (m *Model) tile(r rows.Row, idx int) []Line {
	var out []Line
	for _, t := range m.templates().Tiles {
		if t.Blank() {
			continue
		}
		out = append(out, Line{Dim: r.Dim, Spans: m.line(t, r, m.Width, idx)})
	}
	return append(out, Line{Spans: []Span{{Text: strings.Repeat("─", m.Width), Fg: palette.Border, Dim: true}}})
}

// compact is the compact template's one line: the head with the
// secondary label and host tag after the primary. With Titles, the
// third tile line follows.
func (m *Model) compact(r rows.Row, idx int) []Line {
	t := m.templates()
	lines := []Line{{Dim: r.Dim, Spans: m.line(t.Compact, r, m.Width, idx)}}
	if m.Titles && len(t.Tiles) >= 3 && !t.Tiles[2].Blank() {
		lines = append(lines, Line{Dim: r.Dim, Spans: m.line(t.Tiles[2], r, m.Width, idx)})
	}
	return lines
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

// encode is the frame as the terminal draws it, each line with its
// attributes in the theme.
func encode(lines []Line, th palette.Theme) []string {
	out := make([]string, len(lines))
	for i, l := range lines {
		out[i] = ANSI(l, th)
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

// ANSI encodes a line for the terminal in a theme, ending with a reset.
// A span's own attributes are set for the span and the line's restored
// after it. In a theme with colours a span's colour is drawn, and a dim
// line is drawn in the dimmed colour throughout, as are a dim row's
// faded spans on a line that is not dim, but for the viewer's own
// row's label; plain text keeps the terminal's own foreground, which
// is right whatever the background, and dim text on a template's
// background is drawn in the dim colour that reads on it, not faint
// in the terminal's. The selection is the highlight background with
// the theme's text on it when the theme knows the terminal's
// background, and reverse video otherwise, as it is without
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
		if l.Dim && !band {
			// A dim line is dim throughout; under the band its spans
			// keep their colour.
			s = s.dimmed()
		}
		fg, bg := "", ""
		if colour && s.Fg != "" {
			fg = th.SGR(s.Fg, false)
		}
		if colour && s.Bg != "" && !band && !s.band {
			// A template's background; the selection's band stays
			// the band, so the selected row is told apart.
			bg = th.SGR(s.Bg, true)
		}
		pre := ""
		if s.band {
			// The band on the span alone, a strip's chip: the
			// highlight background under the span's own colour, or
			// reverse video without one, where a colour would land in
			// the background: the attributes alone, as the list's band.
			if colour && !th.Guessed {
				pre = th.SGR(palette.HighlightRowBg, true) + th.SGR(palette.Text, false)
			} else {
				pre, fg, bg = "\x1b[7m", "", ""
			}
		}
		// A span's faint is for a theme without colours; with them its
		// colour, the border's say, is faint enough.
		faint := s.Dim && !l.Dim && fg == "" && !(s.band && pre == "\x1b[7m")
		if faint && s.faded && colour {
			// A dim row's span on a line that is not dim, its chip in
			// the strip: faint in the dimmed colour, as its line is.
			fg = th.SGR(palette.Dimmed, false)
		}
		if faint && bg != "" {
			// Faint in the terminal's colour can all but vanish on a
			// template's background, light grey on yellow: the dim
			// colour that reads on it instead, but on a colour 0 to 15,
			// which the terminal alone knows.
			if c := th.DimmedOn(s.Bg); c != "" {
				fg, faint = c, false
			}
		}
		if faint || s.Bold || fg != "" || bg != "" || pre != "" {
			if s.label && l.Dim && !band {
				// The viewer's label is not faint on a dim line.
				b.WriteString("\x1b[22m")
			}
			b.WriteString(pre)
			if faint {
				b.WriteString("\x1b[2m")
			}
			if s.Bold {
				b.WriteString("\x1b[1m")
			}
			b.WriteString(fg)
			b.WriteString(bg)
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
