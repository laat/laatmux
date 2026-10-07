package home

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// WriteAtomic replaces the file whole and leaves no temporary, after a
// write that succeeded and after one whose rename failed; its errors
// name the files quoted.
func TestWriteAtomic(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "f.json")
	for _, want := range []string{"one", "two"} {
		if err := WriteAtomic(p, []byte(want)); err != nil {
			t.Fatal(err)
		}
		if b, err := os.ReadFile(p); err != nil || string(b) != want {
			t.Fatalf("read %q %v, want %q", b, err, want)
		}
	}
	// A non-empty directory in the file's place: the rename fails and
	// the temporary goes with it.
	full := filepath.Join(dir, "full")
	if err := os.MkdirAll(filepath.Join(full, "x"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := WriteAtomic(full, []byte("three")); err == nil {
		t.Fatal("a rename onto a non-empty directory succeeded")
	}
	if left, _ := filepath.Glob(filepath.Join(dir, "*.tmp.*")); len(left) != 0 {
		t.Fatalf("temp files left: %v", left)
	}
	if b, err := os.ReadFile(p); err != nil || string(b) != "two" {
		t.Fatalf("after the failure %q %v", b, err)
	}
	// The errors name the files as tmux.Printable shows them, here in a
	// directory with a tab and an ESC in its name: the failed rename
	// both, a write into a directory that is gone the temporary; os's
	// cause is kept.
	odd := filepath.Join(dir, "d\tir\x1b[31m")
	full = filepath.Join(odd, "full")
	if err := os.MkdirAll(filepath.Join(full, "x"), 0o700); err != nil {
		t.Fatal(err)
	}
	tmp := func(p string) string { return p + ".tmp." + strconv.Itoa(os.Getpid()) }
	var le *os.LinkError
	if err := WriteAtomic(full, nil); err == nil || !strings.HasPrefix(err.Error(), "rename "+strconv.Quote(tmp(full))+" "+strconv.Quote(full)+": ") || !errors.As(err, &le) {
		t.Errorf("rename onto a directory: %v", err)
	}
	gone := filepath.Join(odd, "gone", "f.json")
	if err := WriteAtomic(gone, nil); err == nil || err.Error() != "open "+strconv.Quote(tmp(gone))+": no such file or directory" || !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("write into a directory that is gone: %v", err)
	}
}

// Temporary knows both names a dead write leaves, and no record's.
func TestTemporary(t *testing.T) {
	for name, want := range map[string]bool{
		"a.json.tmp.12345": true,
		"a.json.tmp":       true,
		// An id that contains the temporary's own suffix.
		"a.json.tmp.1.json.tmp.2": true,
		"a.json.tmp.1.json":       false,
		"a.json":                  false,
		"job.tmp.json":            false,
		"a.json.tmp.json":         false,
		"a.json.tmp.":             false,
		"a.json.tmp.x":            false,
		"a.json.tmp.-1":           false,
		"a.json.tmp.+1":           false,
		"a.tmp":                   false,
	} {
		if got := Temporary(name); got != want {
			t.Errorf("Temporary(%q) = %v, want %v", name, got, want)
		}
	}
}
