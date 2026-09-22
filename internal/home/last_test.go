package home

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestLastRoundTrip(t *testing.T) {
	t.Setenv("LAATMUX_HOME", t.TempDir())
	l, err := ReadLast()
	if err != nil || len(l.Repos) != 0 {
		t.Fatalf("empty: %+v %v", l, err)
	}
	if err := UpdateLast(func(l *Last) { l.Set("src-a", LastRepo{Host: "vm", Agent: "claude"}) }); err != nil {
		t.Fatal(err)
	}
	l, err = ReadLast()
	if err != nil || l.Get("src-a") != (LastRepo{Host: "vm", Agent: "claude"}) || l.Get("src-b") != (LastRepo{}) {
		t.Fatalf("after write: %+v %v", l, err)
	}
	if _, err := os.Stat(filepath.Join(Dir(), "last.json")); err != nil {
		t.Fatal(err)
	}
	if left, _ := filepath.Glob(filepath.Join(Dir(), "last.json.tmp.*")); len(left) != 0 {
		t.Fatalf("temp files left: %v", left)
	}
}

// Concurrent updates must each keep the other's defaults: the read, modify
// and write run under one lock.
func TestLastConcurrentUpdatesKeepEachOther(t *testing.T) {
	t.Setenv("LAATMUX_HOME", t.TempDir())
	const n = 20
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			key := string(rune('a' + i))
			errs <- UpdateLast(func(l *Last) { l.Set(key, LastRepo{Host: "h" + key}) })
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	l, err := ReadLast()
	if err != nil {
		t.Fatal(err)
	}
	if len(l.Repos) != n {
		t.Fatalf("lost updates: %d of %d kept", len(l.Repos), n)
	}
}

func TestLastCorruptFileIsAnError(t *testing.T) {
	t.Setenv("LAATMUX_HOME", t.TempDir())
	os.MkdirAll(Dir(), 0o700)
	os.WriteFile(filepath.Join(Dir(), "last.json"), []byte("{"), 0o600)
	if _, err := ReadLast(); err == nil {
		t.Fatal("corrupt file read as empty")
	}
	if err := UpdateLast(func(l *Last) {}); err == nil {
		t.Fatal("corrupt file overwritten")
	}
}
