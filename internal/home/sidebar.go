package home

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// Sidebar is sidebar.json under the state directory: what the sidebar
// panes and the dashboard keep between runs. The view, layout and scope
// are start defaults: the view and layout last chosen by a key or the
// CLI, the scope last set by the CLI; a pane reads them only when it
// starts, and F is never written. The folds the user toggled are
// shared: every pane reads them again when the file's mtime changes.
// Each fold is kept by node id with the time its node was last seen,
// and one not seen for a day is dropped.
type Sidebar struct {
	View   string          `json:"view,omitempty"`
	Layout string          `json:"layout,omitempty"`
	Scope  string          `json:"scope,omitempty"`
	Folds  map[string]Fold `json:"folds,omitempty"`
	// The dashboard's own view and layout defaults: it opens in a wide
	// popup where compact suits, and neither sidebar.view nor the CLI
	// touches them.
	DashboardView   string `json:"dashboard_view,omitempty"`
	DashboardLayout string `json:"dashboard_layout,omitempty"`
}

// Defaults is the view and layout a host starts in: the sidebar's or
// the dashboard's keys.
func (s *Sidebar) Defaults(dashboard bool) (view, layout *string) {
	if dashboard {
		return &s.DashboardView, &s.DashboardLayout
	}
	return &s.View, &s.Layout
}

// Fold is one node's fold: closed or open, and when the node was last
// seen by a pane writing the file.
type Fold struct {
	Closed bool      `json:"closed"`
	Seen   time.Time `json:"seen"`
}

// FoldTTL is how long a fold outlives its node's last sighting.
const FoldTTL = 24 * time.Hour

// SidebarPath is the file's path.
func SidebarPath() string { return filepath.Join(Dir(), "sidebar.json") }

// ReadSidebar reads sidebar.json and its mtime. A missing file is empty
// state with a zero time.
func ReadSidebar() (Sidebar, time.Time, error) {
	var s Sidebar
	// The contents and the mtime of one file: a rename between a read
	// and a stat of the path would pair old contents with the new
	// file's time, and the poll would miss that write.
	f, err := os.Open(SidebarPath())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return s, time.Time{}, nil
		}
		return s, time.Time{}, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return s, time.Time{}, err
	}
	b, err := io.ReadAll(f)
	if err != nil {
		return s, time.Time{}, err
	}
	if err := json.Unmarshal(b, &s); err != nil {
		return s, time.Time{}, err
	}
	return s, st.ModTime(), nil
}

// SidebarMtime is the file's mtime, zero when it is not there: what a
// pane polls to learn of another's write.
func SidebarMtime() time.Time {
	st, err := os.Stat(SidebarPath())
	if err != nil {
		return time.Time{}
	}
	return st.ModTime()
}

// UpdateSidebar applies fn to the file's state and writes it back:
// read, modify and write under an exclusive flock on a lock file beside
// it, the write to a temporary file renamed into place, so two panes
// toggling folds at once lose neither change and a crash mid-write
// leaves the old file. Folds not seen for a day are dropped on the way.
func UpdateSidebar(now time.Time, fn func(*Sidebar)) error {
	if err := ensure(); err != nil {
		return err
	}
	lock, err := os.OpenFile(SidebarPath()+".lock", os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return err
	}
	defer func() { _ = syscall.Flock(int(lock.Fd()), syscall.LOCK_UN) }()
	s, _, err := ReadSidebar()
	if err != nil {
		// A file that does not parse is replaced, not kept broken.
		s = Sidebar{}
	}
	fn(&s)
	for id, f := range s.Folds {
		if now.Sub(f.Seen) > FoldTTL {
			delete(s.Folds, id)
		}
	}
	if len(s.Folds) == 0 {
		s.Folds = nil
	}
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return WriteAtomic(SidebarPath(), append(b, '\n'))
}

// SetFolds writes a pane's toggled folds into the file, each seen now,
// those carried by a handoff only where the file has none, and
// refreshes the sighting of every fold whose node the pane has: what a
// pane does when a fold changes, and once an hour for the sightings.
func (s *Sidebar) SetFolds(folds map[string]bool, carried map[string]bool, present func(id string) bool, now time.Time) {
	if s.Folds == nil {
		s.Folds = map[string]Fold{}
	}
	for id, closed := range folds {
		if _, has := s.Folds[id]; has && carried[id] {
			continue
		}
		s.Folds[id] = Fold{Closed: closed, Seen: now}
	}
	for id, f := range s.Folds {
		if present != nil && present(id) {
			f.Seen = now
			s.Folds[id] = f
		}
	}
}

// FoldMap is the folds as the view takes them.
func (s Sidebar) FoldMap() map[string]bool {
	out := map[string]bool{}
	for id, f := range s.Folds {
		out[id] = f.Closed
	}
	return out
}
