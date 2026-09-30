package view

import (
	"fmt"
	"strings"
	"time"

	"github.com/laat/laatmux/internal/palette"
	"github.com/laat/laatmux/internal/protocol"
	"github.com/laat/laatmux/internal/rows"
)

// The two views, as milestone five's note settles them: the agent view,
// one tile per agent with the stale ones folded, and the tree, the
// repositories with their worktrees and what runs in each, with folds.
// Tab switches; the selection follows across, by node id.

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
		// Consumed either way: an old fold is no later owner's.
		delete(m.folds, from)
		delete(m.toggled, from)
		if !m.toggled[succ] {
			m.setFold(succ, closed)
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
	if c, ok := m.folds[id]; ok {
		return c
	}
	switch r.Kind {
	case rows.KindRepo:
		return false
	case rows.KindFold:
		return !m.ShowHidden
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
		m.ShowHidden = !m.ShowHidden
		return
	}
	m.setFold(r.ID(), !m.closed(r))
	if m.toggled == nil {
		m.toggled = map[string]bool{}
	}
	m.toggled[r.ID()] = true
}

// foldAll opens every fold when any shown is closed, else closes every
// one; the agent view's f toggles its stale fold.
func (m *Model) foldAll() {
	if m.View != ViewTree {
		m.ShowHidden = !m.ShowHidden
		return
	}
	anyClosed := false
	for i := range m.Tree {
		if r := &m.Tree[i]; r.Foldable() && m.closed(r) {
			anyClosed = true
		}
	}
	for i := range m.Tree {
		if r := &m.Tree[i]; r.Foldable() {
			m.setFold(r.ID(), !anyClosed)
		}
	}
}

// treeItems is the tree as drawn: the nodes the filter and the folds
// leave, a folded line's children hidden. The filter keeps the lines
// whose name or host matches, with their children, and the repository
// or the other-sessions header over any line it keeps; a matching
// repository alone shows as a line without.
func (m *Model) treeItems() []Item {
	shown := m.treeShown()
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
			// Other sessions: a header, not a row.
			out = append(out, Item{Header: r.Name, Group: GroupMain, Index: -1})
			continue
		}
		out = append(out, Item{Row: r, Group: GroupMain, Index: n})
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
		m.Selection()
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
	for _, rs := range [][]rows.Row{m.Rows.Main, m.Rows.Stale, m.Rows.Settled} {
		for _, r := range rs {
			if r.Current {
				return r.ID()
			}
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
// the task standing for a worktree the host has not listed; nil for
// none. A pane's jump goes by it.
func (m *Model) LineFor(host, session string) *rows.Row {
	if session == "" {
		return nil
	}
	for i := range m.Tree {
		n := &m.Tree[i]
		if n.Depth == 1 && n.Host == host && n.Home() == session {
			return n
		}
	}
	return nil
}

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

// treeLine draws one node of the tree, or the agent view's stale fold.
func (m *Model) treeLine(r rows.Row) []Line {
	w := m.Width
	var spans []Span
	right := []Span{}
	indent := strings.Repeat("  ", r.Depth)
	switch r.Kind {
	case rows.KindFold:
		mark := "▾ "
		if m.closed(&r) {
			mark = "▸ "
		}
		spans = []Span{{Text: mark + r.Name, Fg: palette.Header, Dim: true}}
	case rows.KindRepo:
		mark := ""
		if m.closed(&r) {
			mark = "▸ "
		}
		spans = []Span{{Text: mark + r.Name, Fg: palette.Header, Bold: true}}
	case rows.KindGroup:
		spans = []Span{{Text: r.Name, Fg: palette.Header, Dim: true}}
	case rows.KindWorktree, rows.KindTask:
		mark := "  "
		if r.Foldable() {
			mark = "▾ "
			if m.closed(&r) {
				mark = "▸ "
			}
		}
		p, _ := r.Labels()
		if r.Orphaned {
			p = r.Name
		}
		spans = []Span{{Text: indent + mark}, m.primary(r, w)}
		spans[1].Text = p
		if r.Host != "" {
			spans = append(spans, Span{Text: " (" + r.Host + ")", Dim: r.Host != m.LocalHost})
		}
		switch {
		case r.Pending != nil:
			right = append(right, Span{Text: r.State(), Fg: palette.Warning})
		case r.Orphaned:
			right = append(right, Span{Text: "worktree gone", Dim: true})
		default:
			right = append(right, gitSpans(r, w/2)...)
			if pr := m.prSpans(r, w/3); len(pr) > 0 {
				if len(right) > 0 {
					right = append(right, Span{Text: "  "})
				}
				right = append(right, pr...)
			}
		}
		if r.Foldable() && m.closed(&r) && r.Worst != nil {
			// The most pressing agent inside, so a blocked or done one
			// is not missed.
			icon := m.iconSpan(*r.Worst)
			if strings.TrimSpace(icon.Text) != "" {
				right = append(right, Span{Text: "  "}, icon)
			}
		}
	case rows.KindAgent:
		icon := m.iconSpan(r)
		name := r.AgentName()
		if r.Depth == 1 {
			// A session in other sessions: its name, host, then the
			// agent.
			spans = []Span{{Text: indent + "  " + r.Name}, {Text: " (" + r.Host + ")", Dim: r.Host != m.LocalHost}, {Text: "  "}, icon, {Text: " " + name}}
			break
		}
		title := ""
		if r.Agent.Liveness == protocol.Gone {
			title = "gone"
		} else {
			p, sec := r.Labels()
			title = cleanTitle(r.Agent.Title, p, sec, r.Host, m.Machine)
		}
		spans = []Span{{Text: indent}, icon, {Text: " " + name}}
		if title != "" {
			spans = append(spans, Span{Text: "  " + title, Dim: true})
		}
	case rows.KindPane:
		spans = []Span{{Text: indent + "$ " + r.Name}}
	case rows.KindRun:
		spans = []Span{{Text: indent + "▶ " + r.Name}}
		if r.Run != nil {
			right = append(right, Span{Text: elapsed(m.Now.Sub(r.Run.StartedAt)), tick: m.Now.Sub(r.Run.StartedAt) < time.Hour})
		}
	default:
		spans = []Span{{Text: indent + r.Name}}
	}
	// The right side against the edge, the line clipped to fit; a
	// narrow line drops the stats first, then the PR, and keeps the
	// folded line's icon last, never the name.
	left := spansWidth(spans)
	for _, try := range [][]Span{right, afterSep(right), afterSep(afterSep(right))} {
		if n := spansWidth(try); n > 0 && left+1+n <= w {
			spans = append(spans, Span{Text: strings.Repeat(" ", w-left-n)})
			spans = append(spans, try...)
			break
		}
	}
	return []Line{{Dim: r.Dim, Spans: clip(spans, w)}}
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
	l := m.treeLine(*repo)[0]
	return &l, repo.ID()
}

// afterSep is the spans after the first two-space separator span, nil when there is
// none.
func afterSep(spans []Span) []Span {
	for i, sp := range spans {
		if sp.Text == "  " && i > 0 {
			return spans[i+1:]
		}
	}
	return nil
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
