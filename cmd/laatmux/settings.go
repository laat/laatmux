package main

import (
	"context"
	"time"

	"github.com/laat/laatmux/internal/config"
	"github.com/laat/laatmux/internal/home"
	"github.com/laat/laatmux/internal/view"
)

// The view's settings: sidebar.json holds the start defaults, the view
// and layout last chosen by a key or the CLI and the scope last set by
// the CLI, and the folds the user toggled, shared between the panes.
// At start a pane takes the file's values and the config's for what the
// file lacks; on a change it writes the view, the layout and its folds
// back; every second it looks at the file's mtime and takes the folds
// another pane wrote.

// settingsPoll is how often a pane looks for another's write.
const settingsPoll = time.Second

// startSettings gives the model the file's start defaults over the
// config's: the view and layout unless the caller fixed them, the scope,
// and the folds. It returns the mtime seen, for the poll.
func startSettings(cfg config.Config, m *view.Model, fixedLayout, fixedView bool) time.Time {
	s, mtime, err := home.ReadSidebar()
	if err != nil {
		return mtime
	}
	if v, err := view.ParseView(s.View); err == nil && s.View != "" && !fixedView {
		m.View = v
	}
	if l, err := view.ParseLayout(s.Layout); err == nil && s.Layout != "" && !fixedLayout {
		m.Layout = l
	}
	if sc, err := view.ParseScope(s.Scope); err == nil && s.Scope != "" {
		m.Scope = sc
	}
	m.ApplyFolds(s.FoldMap())
	return mtime
}

// saveSettings writes the model's view and layout as the defaults and
// its toggled folds, each seen now, refreshing the sighting of every
// fold whose node the model has; the scope is the CLI's to write.
func saveSettings(m *view.Model, now time.Time) error {
	folds := m.ToggledFolds()
	return home.UpdateSidebar(now, func(s *home.Sidebar) {
		if m.Layout != view.Strip {
			// A strip's view and layout are its own, not defaults.
			s.View, s.Layout = string(m.View), string(m.Layout)
		}
		s.SetFolds(folds, m.HasNode, now)
	})
}

// watchSettings sends the file's folds to the view whenever its mtime
// moves past the one last seen, until ctx ends.
func watchSettings(ctx context.Context, seen time.Time, cmds chan<- func(*view.Model) view.Action) {
	go func() {
		t := time.NewTicker(settingsPoll)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
			mtime := home.SidebarMtime()
			if !mtime.After(seen) {
				continue
			}
			seen = mtime
			s, _, err := home.ReadSidebar()
			if err != nil {
				continue
			}
			folds := s.FoldMap()
			select {
			case cmds <- func(m *view.Model) view.Action { m.ApplyFolds(folds); return view.Action{} }:
			case <-ctx.Done():
				return
			}
		}
	}()
}
