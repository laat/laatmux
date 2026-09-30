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

// settingsHost is which of the file's defaults a view takes and
// writes: the sidebar's, or the dashboard's own keys.
type settingsHost struct {
	dashboard   bool
	fixedLayout bool // a flag chose the layout: neither read nor written
	fixedView   bool // the strip: the agent view, neither read nor written
	fixedScope  bool // the dashboard: at all, whatever the file says
}

// startSettings gives the model the file's start defaults over the
// config's: the host's view and layout unless fixed, the scope unless
// the host starts at all, and the folds. It returns the mtime seen,
// for the poll.
func startSettings(cfg config.Config, m *view.Model, h settingsHost) time.Time {
	if sc, err := view.ParseScope(cfg.Sidebar.Scope); err == nil && !h.fixedScope {
		m.Scope = sc
	}
	s, mtime, err := home.ReadSidebar()
	if err != nil {
		return mtime
	}
	view0, layout0 := s.Defaults(h.dashboard)
	if v, err := view.ParseView(*view0); err == nil && *view0 != "" && !h.fixedView {
		m.View = v
	}
	if l, err := view.ParseLayout(*layout0); err == nil && *layout0 != "" && !h.fixedLayout {
		m.Layout = l
	}
	if sc, err := view.ParseScope(s.Scope); err == nil && s.Scope != "" && !h.fixedScope {
		m.Scope = sc
	}
	m.ApplyFolds(s.FoldMap())
	return mtime
}

// saveSettings writes the view and layout the user chose by a key as
// the host's defaults, and the folds the model set since it last
// wrote, each seen now, refreshing the sighting of every fold whose
// node the model has; a fold taken from another pane is not written
// back, so that pane's later change is never lost; the scope is the
// CLI's to write.
func saveSettings(m *view.Model, now time.Time, h settingsHost) error {
	folds, carried := m.DirtyFolds()
	viewSet, layoutSet := m.ChangedDefaults()
	if m.Layout == view.Strip || h.fixedView {
		// A strip's view and layout are its own, not defaults.
		viewSet, layoutSet = false, false
	}
	if h.fixedLayout {
		// A layout a flag chose is not the last chosen by a key.
		layoutSet = false
	}
	if len(folds) == 0 && !viewSet && !layoutSet {
		return nil
	}
	return home.UpdateSidebar(now, func(s *home.Sidebar) {
		view0, layout0 := s.Defaults(h.dashboard)
		if viewSet {
			*view0 = string(m.View)
		}
		if layoutSet {
			*layout0 = string(m.Layout)
		}
		s.SetFolds(folds, carried, m.HasNode, now)
	})
}

// touchSettings refreshes the sighting of every fold whose node the
// model has, without a value changed: a node in sight for a day is not
// dropped by a write elsewhere.
func touchSettings(m *view.Model, now time.Time) error {
	if len(m.ToggledFolds()) == 0 {
		return nil
	}
	return home.UpdateSidebar(now, func(s *home.Sidebar) { s.SetFolds(nil, nil, m.HasNode, now) })
}

// touchEvery is how often a pane refreshes its folds' sightings.
const touchEvery = time.Hour

// watchSettings sends the file's folds to the view whenever its mtime
// moves past the one last seen, and once an hour a refresh of the
// sightings, until ctx ends.
func watchSettings(ctx context.Context, seen time.Time, cmds chan<- func(*view.Model) view.Action) {
	go func() {
		t := time.NewTicker(settingsPoll)
		defer t.Stop()
		touch := time.NewTicker(touchEvery)
		defer touch.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-touch.C:
				select {
				case cmds <- func(m *view.Model) view.Action { _ = touchSettings(m, time.Now()); return view.Action{} }:
				case <-ctx.Done():
					return
				}
				continue
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
