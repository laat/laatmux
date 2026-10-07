package view

import (
	"fmt"

	"github.com/laat/laatmux/internal/rows"
)

// Scope is what a pane shows, defined by the viewer's row, the one
// Following picks: every row; the viewer's worktree, in the agent view
// every agent of it whatever session each runs in and every task at its
// root, in the tree its line, or the task standing for it, with its
// children and any other task at its root beside it, under its
// repository; or every line under the viewer's worktree's repository,
// and in the agent view that repository's agents and tasks. Session and
// project also keep every row that is the viewer's, whatever its
// worktree. One in other sessions has none: an agent observed in a
// window of the viewer's workspace session, or a managed agent of no
// worktree in the home session of the viewer's line. Another worktree's
// line is the viewer's, with its children, when one of its agents is
// observed in a window of the viewer's session, or runs in the managed
// session a plain attachment the viewer is in shows. With no worktree,
// session and project are the viewer's line alone, and a pane in a
// session that is no row's shows the empty state under both.
type Scope string

const (
	ScopeAll     Scope = "all"
	ScopeSession Scope = "session"
	ScopeProject Scope = "project"
)

// ParseScope reads a scope from the config or the CLI; "" is all.
func ParseScope(s string) (Scope, error) {
	switch Scope(s) {
	case "", ScopeAll:
		return ScopeAll, nil
	case ScopeSession, ScopeProject:
		return Scope(s), nil
	}
	return "", fmt.Errorf("scope %q is not all, session or project", s)
}

// scope is the model's scope, all until set.
func (m *Model) scope() Scope {
	if m.Scope == "" {
		return ScopeAll
	}
	return m.Scope
}

// ToggleScope is F: to session, and pressed again back to the scope
// the pane had before, whatever set it; a pane already on session goes
// to all.
func (m *Model) ToggleScope() {
	m.settings = true
	switch m.scope() {
	case ScopeSession:
		if m.prevScope == "" || m.prevScope == ScopeSession {
			m.Scope = ScopeAll
		} else {
			m.Scope = m.prevScope
		}
		m.prevScope = ""
	default:
		m.prevScope = m.scope()
		m.Scope = ScopeSession
	}
}

// viewerWorktree is the worktree the viewer's row is or is under, "",
// and its repository node; ok is false when no row is the viewer's.
func (m *Model) viewerWorktree() (worktree, repo string, ok bool) {
	// The tree first: a child that is the viewer's marks its line.
	parent := ""
	for i := range m.Tree {
		r := &m.Tree[i]
		if r.Depth == 0 {
			parent = r.ID()
		}
		if !r.Current || r.Depth == 0 {
			continue
		}
		w := worktreeOf(r)
		if r.Depth > 1 {
			// A child: its line's worktree.
			for j := i - 1; j >= 0; j-- {
				if m.Tree[j].Depth == 1 {
					w = worktreeOf(&m.Tree[j])
					break
				}
			}
		}
		if parent != "" && m.Tree[m.indexOf(parent)].Kind != rows.KindRepo {
			parent = ""
		}
		return w, parent, true
	}
	// The viewer's tile is in the main group whatever its state.
	for i := range m.Rows.Main {
		if r := &m.Rows.Main[i]; r.Current {
			return m.tileWorktree(r), m.tileRepo(r), true
		}
	}
	return "", "", false
}

// inScope reports whether a tile is in the scope: under all, every
// one; under session, the viewer's worktree's agents and tasks; under
// project, its repository's; under both, every tile that is the
// viewer's.
func (m *Model) inScope(r *rows.Row) bool {
	if m.scope() == ScopeAll {
		return true
	}
	w, repo, ok := m.viewerWorktree()
	if !ok {
		return false
	}
	if r.Current || w == "" {
		// A tile that is the viewer's whatever its worktree: one in
		// other sessions has none, and one of another worktree is the
		// viewer's through its line, as treeScoped keeps the line. With
		// no worktree, project is session, the viewer's tile alone.
		return r.Current
	}
	if m.scope() == ScopeProject && repo != "" {
		return m.tileRepo(r) == repo
	}
	return m.tileWorktree(r) == w
}

// tileWorktree is the worktree a tile is of: its record's, the one its
// task makes, or, for the add's agent before the listing, its task
// line's in the tree.
func (m *Model) tileWorktree(r *rows.Row) string {
	switch {
	case r.Worktree != nil:
		return r.Worktree.ID
	case r.Pending != nil:
		return r.Pending.WorktreeID()
	}
	if line := m.lineOver(r.ID()); line != nil {
		return worktreeOf(line)
	}
	return ""
}

// lineOver is the depth-1 line over a child node in the tree, nil for
// none and for a node that is no child.
func (m *Model) lineOver(id string) *rows.Row {
	i := m.indexOf(id)
	if i < 0 || m.Tree[i].Depth < 2 {
		return nil
	}
	for j := i - 1; j >= 0; j-- {
		if m.Tree[j].Depth == 1 {
			return &m.Tree[j]
		}
		if m.Tree[j].Depth == 0 {
			break
		}
	}
	return nil
}

// repoOver is the repository node over a node in the tree, "" for none
// or for the other-sessions group.
func (m *Model) repoOver(id string) string {
	for j := m.indexOf(id); j >= 0; j-- {
		if m.Tree[j].Depth == 0 {
			if m.Tree[j].Kind == rows.KindRepo {
				return m.Tree[j].ID()
			}
			return ""
		}
	}
	return ""
}

// tileRepo is the repository node a tile is under, as the tree names
// it; for the add's agent before the listing, its task line's.
func (m *Model) tileRepo(r *rows.Row) string {
	switch {
	case r.Worktree != nil && r.Worktree.Source != "":
		return rows.RepoNode(r.Worktree.Source)
	case r.Worktree != nil:
		return rows.LabelRepoNode(r.Worktree.Repo)
	case r.Pending != nil && r.Pending.Source != "":
		return rows.RepoNode(r.Pending.Source)
	case r.Pending != nil:
		return rows.LabelRepoNode(r.Pending.Repo)
	}
	if line := m.lineOver(r.ID()); line != nil {
		return m.repoOver(line.ID())
	}
	return m.repoOver(r.ID())
}

// scopeWorktree is the worktree a depth-1 line is or stands for, a
// task's whether it stands or failed: a failed task at the viewer's
// root is the viewer's under session.
func scopeWorktree(r *rows.Row) string {
	if r.Pending != nil {
		return r.Pending.WorktreeID()
	}
	return worktreeOf(r)
}

// treeScoped is which tree nodes the scope leaves: under session the
// lines of the viewer's worktree with their children and the repository
// over them; under project every node under the viewer's repository
// line; under both every line that is the viewer's with its children,
// one in other sessions or another worktree's, and the node over it;
// the other-sessions header only with a line under it.
func (m *Model) treeScoped() []bool {
	keep := make([]bool, len(m.Tree))
	if m.scope() == ScopeAll {
		for i := range keep {
			keep[i] = true
		}
		return keep
	}
	w, repo, ok := m.viewerWorktree()
	if !ok {
		return keep
	}
	// Project is the repository's every line, with a worktree to be
	// under it; without one it is session, the viewer's line alone.
	project := m.scope() == ScopeProject && repo != "" && w != ""
	parent, line := -1, -1
	inRepo := false // under the viewer's repository line, for project
	for i := range m.Tree {
		r := &m.Tree[i]
		switch r.Depth {
		case 0:
			parent, line = i, -1
			inRepo = project && r.ID() == repo
			keep[i] = inRepo
		case 1:
			line = i
			switch {
			case project:
				keep[i] = inRepo
			case w != "":
				keep[i] = scopeWorktree(r) == w
			}
			// A line that is the viewer's whatever its worktree, as the
			// agent view keeps its tiles; the node over it is kept for
			// it, not for the lines beside it.
			keep[i] = keep[i] || r.Current
			if keep[i] && parent >= 0 {
				keep[parent] = true
			}
		default:
			keep[i] = line >= 0 && keep[line]
		}
	}
	return keep
}

// ScopeLabel is the scope as the footer names it, "" for all.
func (m *Model) ScopeLabel() string {
	if m.scope() == ScopeAll {
		return ""
	}
	return string(m.scope())
}
