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
// and in the agent view that repository's agents and tasks. With no
// worktree, session and project are the viewer's line alone, and a pane
// in a session that is no row's shows the empty state under both.
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
	for _, rs := range [][]rows.Row{m.Rows.Main, m.Rows.Stale, m.Rows.Settled, m.Rows.Orphaned} {
		for i := range rs {
			if r := &rs[i]; r.Current {
				return tileWorktree(r), tileRepo(r), true
			}
		}
	}
	return "", "", false
}

// inScope reports whether a tile is in the scope: under all, every
// one; under session, the viewer's worktree's agents and tasks, or the
// viewer's own tile without a worktree; under project, its repository's.
func (m *Model) inScope(r *rows.Row) bool {
	if m.scope() == ScopeAll {
		return true
	}
	w, repo, ok := m.viewerWorktree()
	if !ok {
		return false
	}
	if w == "" {
		return r.Current
	}
	if m.scope() == ScopeProject && repo != "" {
		return tileRepo(r) == repo
	}
	return tileWorktree(r) == w
}

// tileWorktree is the worktree a tile is of: its record's, or the one
// its task makes.
func tileWorktree(r *rows.Row) string {
	switch {
	case r.Worktree != nil:
		return r.Worktree.ID
	case r.Pending != nil:
		return r.Pending.WorktreeID()
	}
	return ""
}

// tileRepo is the repository node a tile is under, as the tree names
// it.
func tileRepo(r *rows.Row) string {
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
	return ""
}

// treeScoped is which tree nodes the scope leaves: under session the
// lines of the viewer's worktree with their children and the repository
// over them, or the viewer's line alone; under project every node under
// the viewer's repository line; the other-sessions header only with a
// line under it.
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
	parent, line := -1, -1
	for i := range m.Tree {
		r := &m.Tree[i]
		switch r.Depth {
		case 0:
			parent, line = i, -1
			keep[i] = m.scope() == ScopeProject && repo != "" && r.ID() == repo
		case 1:
			line = i
			switch {
			case m.scope() == ScopeProject && repo != "":
				keep[i] = parent >= 0 && keep[parent]
			case w != "":
				keep[i] = worktreeOf(r) == w
			default:
				keep[i] = r.Current
			}
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
