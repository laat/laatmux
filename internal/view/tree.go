package view

import (
	"fmt"
	"strings"

	"github.com/laat/laatmux/internal/palette"
	"github.com/laat/laatmux/internal/rows"
)

// The two views as the model shows them (rows/tree.go says how they
// are built): the agent view, one tile per agent with the stale ones
// folded, and the tree, with folds. Tab switches; the selection follows
// across, by node id.

// View is which of the two the model shows.
type View string

const (
	ViewAgents View = "agents"
	ViewTree   View = "tree"
)

// ParseView reads a view as config writes it; "" is agents.
func ParseView(s string) (View, error) {
	switch s {
	case "", "agents":
		return ViewAgents, nil
	case "tree":
		return ViewTree, nil
	}
	return "", fmt.Errorf("view %q is not agents or tree", s)
}

// Set is the model's rows after a refresh, in the order they depend on:
// the handoffs first, which the anchor lookup consults; the tree, whose
// order the agent view's selection follows across a handoff; then the
// tiles.
func (m *Model) Set(tree []rows.Row, tiles rows.Rows, handoffs map[string]string) {
	m.Handoffs = handoffs
	m.SetTree(tree)
	m.SetRows(tiles)
}

// SetTree replaces the tree's nodes, keeping the selection on the node
// it was on, as SetRows does for the rows. A task that handed over
// passes its fold to the node that takes its children, unless the user
// has set that node's own.
func (m *Model) SetTree(nodes []rows.Row) {
	// The owner of each worktree's children before: a task line that
	// stops standing, at a handoff or failing after the worktree was
	// made, passes its fold to the node that takes them.
	owners := map[string]string{}
	for i := range m.Tree {
		n := &m.Tree[i]
		if n.Depth == 1 && n.Children > 0 {
			if id := worktreeOf(n); id != "" {
				owners[id] = n.ID()
			}
		}
	}
	m.Tree = nodes
	carry := func(from, worktreeID string) {
		closed, ok := m.folds[from]
		if !ok {
			return
		}
		// The node holding the worktree's children now: its line, or
		// the task standing for it.
		succ := m.successor(worktreeID)
		if succ == "" || succ == from {
			return
		}
		// Consumed either way: an old fold is no later owner's. The
		// value written is the user's, the file's as last seen, not a
		// reveal this pane made over it; the reveal stays on screen.
		toggled := m.toggled[from]
		written, wasFile := m.applied[from]
		if !wasFile {
			written = closed
		}
		delete(m.folds, from)
		delete(m.toggled, from)
		delete(m.applied, from)
		if !m.toggled[succ] {
			m.setFold(succ, closed)
			if toggled {
				// The user's fold, now under the node taking the
				// children: written under that id, unless the file
				// has one there already from a pane that was ahead.
				m.markToggled(succ)
				if m.carried == nil {
					m.carried = map[string]bool{}
				}
				m.carried[succ] = true
				if m.applied == nil {
					m.applied = map[string]bool{}
				}
				m.applied[succ] = written
			}
		}
	}
	for worktreeID, from := range owners {
		if m.indexOf(from) < 0 || m.successor(worktreeID) != from {
			carry(from, worktreeID)
		}
	}
	for task, to := range m.Handoffs {
		carry(task, to)
	}
	m.reselect()
}

// successor is the id of the node holding a worktree's children: the
// worktree's line, else the task line standing for it; "" when neither
// is in the tree.
func (m *Model) successor(worktreeID string) string {
	standing := ""
	for _, n := range m.Tree {
		if n.Depth != 1 || worktreeOf(&n) != worktreeID {
			continue
		}
		if n.Kind == rows.KindWorktree {
			return n.ID()
		}
		if n.Children > 0 || standing == "" {
			standing = n.ID()
		}
	}
	return standing
}

// worktreeOf is the worktree a depth-1 line is or stands for: the
// record's id, or the one a task's add makes, before the host lists it.
func worktreeOf(n *rows.Row) string {
	if n.Worktree != nil {
		return n.Worktree.ID
	}
	return n.Alias()
}

// OwnerLine is the depth-1 line holding a worktree's children: its
// line, or the task standing for it; nil for none.
func (m *Model) OwnerLine(worktreeID string) *rows.Row {
	if i := m.indexOf(m.successor(worktreeID)); i >= 0 {
		return &m.Tree[i]
	}
	return nil
}

// closed reports whether a foldable node is folded, deciding a worktree
// or task line's fold the first time it is shown holding a child: open
// when an agent in it is blocked, working or done, folded otherwise;
// repositories start open. After that it stays as it is until toggled.
func (m *Model) closed(r *rows.Row) bool {
	if !r.Foldable() {
		return false
	}
	id := r.ID()
	if r.Kind == rows.KindFold {
		// The stale fold is ShowHidden's, kept in the folds map under
		// its node id for the file.
		return !m.ShowHidden
	}
	if c, ok := m.folds[id]; ok {
		return c
	}
	if r.Kind == rows.KindRepo {
		return false
	}
	c := !m.anyWants(r)
	m.setFold(id, c)
	return c
}

// anyWants is whether a live agent under a line is blocked, working or
// done, whatever its workspace: what opens the line's first fold.
func (m *Model) anyWants(line *rows.Row) bool {
	i := m.indexOf(line.ID())
	for j := i + 1; i >= 0 && j < len(m.Tree) && m.Tree[j].Depth > line.Depth; j++ {
		if m.Tree[j].Wants() {
			return true
		}
	}
	return false
}

func (m *Model) setFold(id string, closed bool) {
	if m.folds == nil {
		m.folds = map[string]bool{}
	}
	m.folds[id] = closed
}

// toggleFold folds or unfolds a node, as the user's own choice.
func (m *Model) toggleFold(r *rows.Row) {
	if !r.Foldable() {
		return
	}
	if r.Kind == rows.KindFold {
		m.showHidden(!m.ShowHidden)
		return
	}
	m.setFold(r.ID(), !m.closed(r))
	m.markToggled(r.ID())
}

// showHidden opens or closes the stale fold as the user's own choice,
// kept under its node id like a line's.
func (m *Model) showHidden(open bool) {
	m.ShowHidden = open
	m.setFold(rows.NodeStale, !open)
	m.markToggled(rows.NodeStale)
}

// markToggled records a fold as the user's own: shared between panes,
// kept across a handoff, and what the host persists, once.
func (m *Model) markToggled(id string) {
	if m.toggled == nil {
		m.toggled = map[string]bool{}
	}
	if m.dirty == nil {
		m.dirty = map[string]bool{}
	}
	m.toggled[id], m.dirty[id] = true, true
	m.settings = true
}

// SettingsChanged reports, once, that the view, layout, scope or a
// fold the user set changed since the last call: the host persists
// what it keeps.
func (m *Model) SettingsChanged() bool {
	c := m.settings
	m.settings = false
	return c
}

// ToggledFolds is the folds the user set, by node id, closed or open:
// this pane's own and those taken from the file.
func (m *Model) ToggledFolds() map[string]bool {
	out := map[string]bool{}
	for id := range m.toggled {
		if c, ok := m.folds[id]; ok {
			out[id] = c
		}
	}
	return out
}

// ChangedDefaults reports, once, whether the view and the layout were
// chosen by a key since the last call: what the host writes as the
// start defaults, and only then.
func (m *Model) ChangedDefaults() (view, layout bool) {
	view, layout = m.viewSet, m.layoutSet
	m.viewSet, m.layoutSet = false, false
	return view, layout
}

// DirtyFolds is the folds this pane set since it was last asked, by
// node id: what it writes to the file, so a fold taken from another
// pane is never written back over that pane's later change. carried
// names those a handoff carried, which the file takes only where it
// has none: every running pane carries the same value, and one ahead
// may have changed it since.
func (m *Model) DirtyFolds() (folds, carried map[string]bool) {
	folds, carried = map[string]bool{}, map[string]bool{}
	if m.applied == nil {
		m.applied = map[string]bool{}
	}
	for id := range m.dirty {
		if c, ok := m.folds[id]; ok {
			if m.carried[id] {
				carried[id] = true
				// The user's value carried, not the reveal shown here.
				if a, ok := m.applied[id]; ok {
					c = a
				}
			}
			folds[id] = c
			// What the file holds now, as far as this pane knows: a
			// later change by another pane is then a change.
			m.applied[id] = c
		}
	}
	m.dirty, m.carried = nil, nil
	return folds, carried
}

// ApplyFolds takes folds another pane set, or the file's at start, as
// the user's own here, the selection kept on its row. A value the file
// held the last time is not applied again: a fold this pane opened to
// reveal a selection, its own and not written, stays as it is until
// the file changes.
func (m *Model) ApplyFolds(folds map[string]bool) {
	if m.applied == nil {
		m.applied = map[string]bool{}
	}
	// A fold the file held and dropped, its node not seen for a day:
	// forgotten here too, unless this pane set it since.
	for id := range m.applied {
		if _, still := folds[id]; !still && !m.dirty[id] {
			delete(m.applied, id)
			delete(m.toggled, id)
			delete(m.folds, id)
		}
	}
	for id, closed := range folds {
		if was, ok := m.applied[id]; ok && was == closed {
			continue
		}
		m.applied[id] = closed
		if id == rows.NodeStale {
			m.ShowHidden = !closed
		}
		m.setFold(id, closed)
		if m.toggled == nil {
			m.toggled = map[string]bool{}
		}
		m.toggled[id] = true
	}
	m.reselect()
}

// foldAll opens every fold when any shown is closed, else closes every
// one; the agent view's f toggles its stale fold.
func (m *Model) foldAll() {
	if m.View != ViewTree {
		m.showHidden(!m.ShowHidden)
		return
	}
	// The folds shown decide, not ones the filter hides: a hidden fold
	// closed would make the first f change nothing on screen.
	anyClosed := false
	if m.scope() != ScopeAll {
		// The repository line over the viewer's, folded by a pane on
		// all: a closed fold shown, so f opens, and the line itself
		// opened here alone, a reveal that is not written.
		for _, it := range m.treeItems() {
			if it.Row != nil && it.Row.Kind == rows.KindRepo && m.closed(it.Row) {
				m.setFold(it.Row.ID(), false)
				anyClosed = true
			}
		}
	}
	for _, it := range m.treeItems() {
		if it.Row == nil || !it.Row.Foldable() {
			continue
		}
		if it.Row.Kind == rows.KindRepo && m.scope() != ScopeAll {
			continue
		}
		if m.closed(it.Row) {
			anyClosed = true
		}
	}
	// The nodes the scope and the filter leave, whether a fold hides
	// them or not: f in a pane on session leaves the other worktrees'
	// folds, shared with every pane, alone.
	shown := m.treeShown()
	for i, in := range m.treeScoped() {
		shown[i] = shown[i] && in
	}
	for i := range m.Tree {
		r := &m.Tree[i]
		if !shown[i] || !r.Foldable() || r.Kind == rows.KindFold {
			continue
		}
		if r.Kind == rows.KindRepo && m.scope() != ScopeAll {
			// The repository line over the viewer's is shared with the
			// panes on all, which it would hide whole.
			continue
		}
		m.setFold(r.ID(), !anyClosed)
		m.markToggled(r.ID())
	}
}

// treeItems is the tree as drawn: the nodes the filter and the folds
// leave, a folded line's children hidden. The filter keeps the lines
// whose name or host matches, with their children, and the repository
// or the other-sessions header over any line it keeps; a matching
// repository alone shows as a line without.
func (m *Model) treeItems() []Item {
	shown := m.treeShown()
	for i, in := range m.treeScoped() {
		shown[i] = shown[i] && in
	}
	var out []Item
	n := 0
	hideBelow := -1
	for i := range m.Tree {
		r := &m.Tree[i]
		if hideBelow >= 0 && r.Depth > hideBelow {
			continue
		}
		hideBelow = -1
		if !shown[i] {
			continue
		}
		if r.Kind == rows.KindGroup {
			// Other sessions: a header, not a row, and none without a
			// line under it.
			under := false
			for j := i + 1; j < len(m.Tree) && m.Tree[j].Depth > 0; j++ {
				if shown[j] {
					under = true
					break
				}
			}
			if under {
				out = append(out, Item{Header: r.Name, Index: -1})
			}
			continue
		}
		out = append(out, Item{Row: r, Index: n})
		n++
		if r.Foldable() && m.closed(r) {
			hideBelow = r.Depth
		}
	}
	return out
}

// treeShown is which nodes the filter leaves.
func (m *Model) treeShown() []bool {
	shown := make([]bool, len(m.Tree))
	if m.Filter == "" {
		for i := range shown {
			shown[i] = true
		}
		return shown
	}
	parent := -1 // the depth-0 node over the current ones
	line := -1   // the depth-1 line over the current children
	for i := range m.Tree {
		r := &m.Tree[i]
		switch r.Depth {
		case 0:
			parent, line = i, -1
			shown[i] = m.matches(r)
		case 1:
			line = i
			shown[i] = m.matches(r)
			if shown[i] && parent >= 0 {
				shown[parent] = true
			}
		default:
			shown[i] = line >= 0 && shown[line]
		}
	}
	return shown
}

// Switch shows the other view, with the selection on the node the
// selected one resolves to there: an agent on its tile or its node, a
// task on itself, a worktree line or what is under it on the worktree's
// first agent, a repository line on its first worktree's. A target a
// fold hides opens the fold. Nothing to resolve to leaves the selection
// on no row; a following selection follows on.
func (m *Model) Switch() {
	m.settings, m.viewSet = true, true
	r := m.Selection()
	target := ""
	if r != nil {
		target = m.crossID(*r)
	}
	if m.View == ViewTree {
		m.View = ViewAgents
	} else {
		m.View = ViewTree
	}
	if m.Follow {
		// The followed row in the new view, unfolded if a fold hides it.
		if id := m.followedID(); id != "" {
			m.reveal(id)
		}
		m.commit()
		return
	}
	if target == "" {
		m.Selected, m.lost = -1, true
		return
	}
	m.reveal(target)
	m.anchor, m.alias, m.lost = target, "", false
	m.reselect()
}

// followedID is the id of the viewer's own row in the current view,
// whether or not a fold hides it: the first tile that is the viewer's
// in the agent view, the viewer's line in the tree.
func (m *Model) followedID() string {
	if m.View == ViewTree {
		for _, n := range m.Tree {
			if n.Current {
				return n.ID()
			}
		}
		return ""
	}
	// The viewer's tile is in the main group whatever its state.
	for _, r := range m.Rows.Main {
		if r.Current {
			return r.ID()
		}
	}
	return ""
}

// crossID is the id of the node the row resolves to in the other view.
func (m *Model) crossID(r rows.Row) string {
	switch {
	case r.Pending != nil:
		return r.Pending.ID
	case r.Kind == rows.KindTile, r.Kind == rows.KindAgent:
		if r.Agent != nil {
			return r.Agent.ID
		}
		return ""
	case r.Kind == rows.KindWorktree, r.Kind == rows.KindPane, r.Kind == rows.KindRun:
		if r.Worktree == nil {
			return ""
		}
		return m.firstAgentUnder(r.Worktree.ID)
	case r.Kind == rows.KindRepo:
		for _, n := range m.Tree[m.indexOf(r.ID())+1:] {
			if n.Depth == 0 {
				break
			}
			if n.Depth == 1 && n.Worktree != nil {
				if id := m.firstAgentUnder(n.Worktree.ID); id != "" {
					return id
				}
			}
		}
	}
	return ""
}

// firstAgentUnder is the first agent, in the tree's order, under the
// worktree's line or the task line standing for it.
func (m *Model) firstAgentUnder(worktreeID string) string {
	under := false
	for _, n := range m.Tree {
		switch {
		case n.Depth <= 1:
			under = n.Depth == 1 && n.Worktree != nil && n.Worktree.ID == worktreeID && n.Children > 0
		case under && n.Kind == rows.KindAgent:
			return n.Agent.ID
		}
	}
	return ""
}

// LineFor is the depth-1 line on a host whose workspace session
// attaches to a managed session: the worktree with that home, the one
// whose agent laatmux made at the root is in it with the home lost, or
// the task standing for a worktree the host has not listed; of
// several, one whose own session it is before one whose root agent is
// in it; nil for none, and for no host (rows.HomeLine). A pane's jump
// goes by it, and so do z and S on a managed agent of no worktree.
func (m *Model) LineFor(host, session string) *rows.Row {
	if i := rows.HomeLine(m.Tree, host, session); i >= 0 {
		return &m.Tree[i]
	}
	return nil
}

// HasNode reports whether the tree has a node with the id: whether a
// fold's node is in sight; the stale fold always is.
func (m *Model) HasNode(id string) bool { return id == rows.NodeStale || m.indexOf(id) >= 0 }

// indexOf is a node's index in the tree, -1 when none has the id.
func (m *Model) indexOf(id string) int {
	for i := range m.Tree {
		if m.Tree[i].ID() == id {
			return i
		}
	}
	return -1
}

// reveal opens what hides the node with the id: in the tree the folds
// over it, in the agent view the stale fold.
func (m *Model) reveal(id string) {
	if m.View == ViewTree {
		i := m.indexOf(id)
		if i < 0 {
			return
		}
		depth := m.Tree[i].Depth
		for j := i - 1; j >= 0 && depth > 0; j-- {
			if n := &m.Tree[j]; n.Depth < depth {
				if n.Foldable() {
					m.setFold(n.ID(), false)
				}
				depth = n.Depth
			}
		}
		return
	}
	for _, r := range m.Rows.Stale {
		if r.ID() == id {
			m.ShowHidden = true
		}
	}
}

// parentOf is the index among the visible items of the line over the
// selected child, -1 when the selection is no child.
func (m *Model) parentOf() int {
	vis := m.Visible()
	if m.Selected < 0 || m.Selected >= len(vis) {
		return -1
	}
	depth := vis[m.Selected].Row.Depth
	if depth < 2 {
		return -1
	}
	for i := m.Selected - 1; i >= 0; i-- {
		if vis[i].Row.Depth < depth {
			return i
		}
	}
	return -1
}

// tabAt is the view a click at column x on the tab line names, "" for
// neither: " Agents │ Tree ".
func tabAt(x int) View {
	switch {
	case x >= 2 && x <= 7:
		return ViewAgents
	case x >= 11 && x <= 14:
		return ViewTree
	}
	return ""
}

// tabs is the line above the list naming the views, the shown one bold.
func (m *Model) tabs() Line {
	var spans []Span
	for i, v := range []View{ViewAgents, ViewTree} {
		if i > 0 {
			spans = append(spans, Span{Text: " │ ", Fg: palette.Border, Dim: true})
		}
		name := map[View]string{ViewAgents: "Agents", ViewTree: "Tree"}[v]
		if v == m.View {
			spans = append(spans, Span{Text: name, Bold: true, Fg: palette.Header})
		} else {
			spans = append(spans, Span{Text: name, Dim: true})
		}
	}
	return Line{Spans: clip(append([]Span{{Text: " "}}, spans...), m.Width)}
}

// treeLine draws one node of the tree, or the agent view's stale fold,
// numbered idx among the nodes the digits count. The other-sessions
// group is not drawn here: treeItems makes it a header item.
func (m *Model) treeLine(r rows.Row, idx int) []Line {
	w := m.Width
	t := m.templates().Tree
	var spans []Span
	switch r.Kind {
	case rows.KindFold:
		mark := "▾ "
		if m.closed(&r) {
			mark = "▸ "
		}
		spans = []Span{{Text: mark + r.Name, Fg: palette.Header, Dim: true}}
	case rows.KindRepo:
		spans = m.line(t.Repo, r, w, idx)
	case rows.KindWorktree, rows.KindTask:
		spans = m.line(t.Worktree, r, w, idx)
	case rows.KindAgent:
		if r.Depth == 1 {
			spans = m.otherSession(r, w)
			break
		}
		spans = m.line(t.Agent, r, w, idx)
	case rows.KindPane:
		spans = m.line(t.Pane, r, w, idx)
	case rows.KindRun:
		spans = m.line(t.Run, r, w, idx)
	}
	return []Line{{Dim: r.Dim, Spans: clip(spans, w)}}
}

// otherSession is a session's line in other sessions: its name, the
// host as the {host} token draws it, then the agent's icon and name; a
// line of its own, not the agent template's. On a line too narrow the
// server after the host gives way to the icon, cut with …, down to the
// host alone; then the line is clipped.
func (m *Model) otherSession(r rows.Row, w int) []Span {
	name := Span{Text: strings.Repeat("  ", r.Depth) + "  " + r.Name}
	icon := m.iconSpan(r)
	host := m.where(r)
	// What the name, the brackets, the gap and the icon leave the host.
	room := w - spansWidth([]Span{name, {Text: " ()  "}, icon})
	if bare := r.HostName(); width(host.Text) > room {
		if room > width(bare) {
			host.Text = cutSpans([]Span{{Text: host.Text}}, room)[0].Text
		} else {
			host.Text = bare
		}
	}
	host.Text = " (" + host.Text + ")"
	return []Span{name, host, {Text: "  "}, icon, {Text: " " + r.AgentName()}}
}

// pinned is the repository line to draw at the top of the body when the
// tree has scrolled past it: the line of the repository holding the node
// at the top of the window, when that node is not a repository line
// itself. nil otherwise.
func (m *Model) pinned(items []Item, ids []string) (*Line, string) {
	if m.scroll <= 0 || m.scroll >= len(ids) || ids[m.scroll] == "" {
		// The top of the window a header: no repository over it.
		return nil, ""
	}
	top := ids[m.scroll]
	var repo *rows.Row
	for _, it := range items {
		if it.Row == nil {
			// A header, other sessions: what is under it has no
			// repository.
			repo = nil
			continue
		}
		if it.Row.Depth == 0 {
			repo = it.Row
		}
		if it.Row.ID() == top {
			break
		}
	}
	if repo == nil || repo.ID() == top || repo.Kind != rows.KindRepo {
		return nil, ""
	}
	l := m.treeLine(*repo, 0)[0]
	return &l, repo.ID()
}

// AgentsUnder counts the agents the tree has under a worktree's line, or
// the task line standing for it.
func (m *Model) AgentsUnder(worktreeID string) int {
	n := 0
	under := false
	for _, r := range m.Tree {
		switch {
		case r.Depth <= 1:
			under = r.Depth == 1 && r.Worktree != nil && r.Worktree.ID == worktreeID
		case under && r.Kind == rows.KindAgent:
			n++
		}
	}
	return n
}

// selectAncestor puts the selection on the nearest visible line over the
// node with the id, when a fold hid the node.
func (m *Model) selectAncestor(id string) {
	i := m.indexOf(id)
	if i < 0 {
		return
	}
	depth := m.Tree[i].Depth
	for j := i - 1; j >= 0; j-- {
		if n := &m.Tree[j]; n.Depth < depth {
			if m.Select(n.ID()) {
				return
			}
			depth = n.Depth
		}
	}
}
