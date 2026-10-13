package logfile

import (
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return string(b)
}

// A file older than MaxAge is rotated at the next write: the live file
// becomes .1, the kept files shift up, the oldest beyond Keep goes.
func TestRotateByAge(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "daemon.log")
	now := time.Date(2026, 10, 13, 9, 0, 0, 0, time.Local)
	clock := func() time.Time { return now }
	w, err := Open(path, Options{Keep: 2, Now: clock})
	if err != nil {
		t.Fatal(err)
	}
	lg := log.New(w, "laatmux ", log.LstdFlags)
	lg.Print("one")
	now = now.Add(23 * time.Hour)
	lg.Print("two")
	if got := read(t, path); strings.Count(got, "\n") != 2 {
		t.Fatalf("rotated early: %q", got)
	}
	now = now.Add(2 * time.Hour)
	lg.Print("three")
	if got := read(t, path); !strings.HasSuffix(got, "three\n") || strings.Count(got, "\n") != 1 {
		t.Fatalf("live after rotation: %q", got)
	}
	if got := read(t, path+".1"); !strings.Contains(got, "one") || !strings.Contains(got, "two") {
		t.Fatalf(".1 after rotation: %q", got)
	}
	// The live file's age counts from its first line: another day on
	// shifts .1 to .2; a third rotation drops it, Keep being 2.
	now = now.Add(25 * time.Hour)
	lg.Print("four")
	now = now.Add(25 * time.Hour)
	lg.Print("five")
	if got := read(t, path+".2"); !strings.Contains(got, "three") {
		t.Fatalf(".2 = %q", got)
	}
	if got := read(t, path+".1"); !strings.Contains(got, "four") {
		t.Fatalf(".1 = %q", got)
	}
	if _, err := os.Stat(path + ".3"); !os.IsNotExist(err) {
		t.Fatalf(".3 kept beyond Keep: %v", err)
	}
	if got := read(t, path); !strings.HasSuffix(got, "five\n") {
		t.Fatalf("live = %q", got)
	}
	w.Close()
}

// A write that would take the file past MaxSize rotates first; the
// write itself goes to the new file whole.
func TestRotateBySize(t *testing.T) {
	path := filepath.Join(t.TempDir(), "daemon.log")
	w, err := Open(path, Options{MaxSize: 100})
	if err != nil {
		t.Fatal(err)
	}
	line := strings.Repeat("x", 39) + "\n"
	w.Write([]byte(line))
	w.Write([]byte(line))
	w.Write([]byte(line))
	if got := read(t, path); got != line {
		t.Fatalf("live = %q", got)
	}
	if got := read(t, path+".1"); got != line+line {
		t.Fatalf(".1 = %q", got)
	}
	w.Close()
}

// The age of a file that exists when the daemon starts is read from
// its first line, so a daemon restarted into an old file rotates it at
// its first write; a file with no date is as old as its last change.
func TestOpenReadsAge(t *testing.T) {
	path := filepath.Join(t.TempDir(), "daemon.log")
	now := time.Date(2026, 10, 13, 9, 0, 0, 0, time.Local)
	os.WriteFile(path, []byte("laatmux 2026/10/10 15:34:05 procs %201: stat /dev/ttys054: no such file or directory\n"), 0o600)
	w, err := Open(path, Options{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	w.Write([]byte("new\n"))
	if got := read(t, path); got != "new\n" {
		t.Fatalf("old file not rotated at start: %q", got)
	}
	if got := read(t, path+".1"); !strings.Contains(got, "procs %201") {
		t.Fatalf(".1 = %q", got)
	}
	w.Close()
	// Fresh, so the next write stays; a file from today also stays.
	path2 := filepath.Join(t.TempDir(), "daemon.log")
	os.WriteFile(path2, []byte("laatmux 2026/10/13 08:00:00 serve\n"), 0o600)
	w, err = Open(path2, Options{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	w.Write([]byte("more\n"))
	if got := read(t, path2); !strings.HasSuffix(got, "serve\nmore\n") {
		t.Fatalf("today's file rotated: %q", got)
	}
	w.Close()
	// No date: the change time counts, which is now.
	path3 := filepath.Join(t.TempDir(), "daemon.log")
	os.WriteFile(path3, []byte("panic: boom\n"), 0o600)
	w, err = Open(path3, Options{Now: time.Now})
	if err != nil {
		t.Fatal(err)
	}
	w.Write([]byte("after\n"))
	if got := read(t, path3); got != "panic: boom\nafter\n" {
		t.Fatalf("undated file rotated: %q", got)
	}
	w.Close()
}
