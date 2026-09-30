package home

import (
	"os"
	"sync"
	"testing"
	"time"
)

// sidebar.json: the defaults and the folds written under the lock and
// replaced by rename, two writers at once losing neither's fold, a fold
// not seen for a day dropped, the sighting refreshed for nodes present,
// and a broken file replaced.
func TestSidebarFile(t *testing.T) {
	t.Setenv("LAATMUX_HOME", t.TempDir())
	now := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	s, mtime, err := ReadSidebar()
	if err != nil || !mtime.IsZero() || s.View != "" {
		t.Fatalf("empty: %+v %v %v", s, mtime, err)
	}
	if err := UpdateSidebar(now, func(s *Sidebar) {
		s.View, s.Layout, s.Scope = "tree", "compact", "session"
		s.SetFolds(map[string]bool{"a": true, "old": false}, nil, now)
	}); err != nil {
		t.Fatal(err)
	}
	s, mtime, err = ReadSidebar()
	if err != nil || mtime.IsZero() || s.View != "tree" || s.Layout != "compact" || s.Scope != "session" || !s.Folds["a"].Closed || s.Folds["old"].Closed || !s.Folds["a"].Seen.Equal(now) {
		t.Fatalf("written: %+v %v %v", s, mtime, err)
	}
	if got := s.FoldMap(); len(got) != 2 || !got["a"] || got["old"] {
		t.Errorf("fold map: %v", got)
	}
	// Two writers at once: each adds a fold, and both stay.
	var wg sync.WaitGroup
	for _, id := range []string{"b", "c"} {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			_ = UpdateSidebar(now, func(s *Sidebar) { s.SetFolds(map[string]bool{id: true}, nil, now) })
		}(id)
	}
	wg.Wait()
	s, _, _ = ReadSidebar()
	if !s.Folds["b"].Closed || !s.Folds["c"].Closed || !s.Folds["a"].Closed {
		t.Errorf("concurrent writes: %v", s.Folds)
	}
	// A day on: the folds whose nodes a writer still has are seen
	// again, the others dropped.
	later := now.Add(FoldTTL + time.Hour)
	if err := UpdateSidebar(later, func(s *Sidebar) {
		s.SetFolds(nil, func(id string) bool { return id == "a" }, later)
	}); err != nil {
		t.Fatal(err)
	}
	s, _, _ = ReadSidebar()
	if len(s.Folds) != 1 || !s.Folds["a"].Seen.Equal(later) {
		t.Errorf("after a day: %v", s.Folds)
	}
	// A broken file is replaced.
	if err := os.WriteFile(SidebarPath(), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ReadSidebar(); err == nil {
		t.Error("a broken file read without error")
	}
	if err := UpdateSidebar(later, func(s *Sidebar) { s.View = "agents" }); err != nil {
		t.Fatal(err)
	}
	if s, _, err := ReadSidebar(); err != nil || s.View != "agents" {
		t.Errorf("replaced: %+v %v", s, err)
	}
	if !SidebarMtime().After(time.Time{}) {
		t.Error("no mtime")
	}
}
