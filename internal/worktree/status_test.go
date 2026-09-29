package worktree

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func gitCfg(t testing.TB, dir string) {
	run(t, dir, "git", "config", "user.email", "t@example.com")
	run(t, dir, "git", "config", "user.name", "t")
}

// add records a new branch's base, resolved, and nothing for a branch
// that exists; a key already there stays.
func TestAddRecordsBase(t *testing.T) {
	f := newFixture(t)
	a, _, err := f.add("feature")
	if err != nil {
		t.Fatal(err)
	}
	c := f.checkout()
	if got := strings.TrimSpace(run(t, c, "git", "config", "--get", "branch.feature."+BaseKey)); got != "origin/main" {
		t.Errorf("new branch base %q", got)
	}
	run(t, c, "git", "push", "-q", "origin", "main:refs/heads/remote-only")
	if _, _, err := f.add("remote-only"); err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(run(t, c, "git", "config", "--get", "branch.remote-only."+BaseKey)); got != "origin/main" {
		t.Errorf("tracking branch base %q", got)
	}
	run(t, c, "git", "branch", "by-hand", "main")
	if _, _, err := f.add("by-hand"); err != nil {
		t.Fatal(err)
	}
	if out, err := git(f.ctx, c, "config", "--get", "branch.by-hand."+BaseKey); err == nil {
		t.Errorf("existing branch got a base: %q", out)
	}
	run(t, c, "git", "config", "branch.feature."+BaseKey, "origin/other")
	if err := SetBase(f.ctx, c, "feature"); err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(run(t, a.Root, "git", "config", "--get", "branch.feature."+BaseKey)); got != "origin/other" {
		t.Errorf("a key already there was replaced: %q", got)
	}
}

// Status: committed against the base, ahead and behind, uncommitted with
// untracked and binary files, dirty, conflict, rebase, and the base
// branch itself; the untracked counts cached by size and mtime.
func TestStatus(t *testing.T) {
	f := newFixture(t)
	a, _, err := f.add("feature")
	if err != nil {
		t.Fatal(err)
	}
	root := a.Root
	gitCfg(t, root)
	var cache StatusCache
	st, head, paths, err := Status(f.ctx, root, "feature", &cache)
	if err != nil {
		t.Fatal(err)
	}
	if st.Base != "origin/main" || st.Committed != [2]int{} || st.Ahead != 0 || head == "" || paths.GitDir == "" {
		t.Fatalf("fresh: %+v head %q paths %+v", st, head, paths)
	}
	// The fixture's setup leaves untracked files behind: counted from
	// here.
	setup := st.Uncommitted

	// Two commits on the branch, one on the base behind it.
	write(t, filepath.Join(root, "a.txt"), "1\n2\n3\n")
	run(t, root, "git", "add", "a.txt")
	run(t, root, "git", "commit", "-q", "-m", "a")
	write(t, filepath.Join(root, "README"), "changed\n")
	run(t, root, "git", "commit", "-q", "-am", "readme")
	c := f.checkout()
	gitCfg(t, c)
	write(t, filepath.Join(c, "base.txt"), "x\n")
	run(t, c, "git", "add", "base.txt")
	run(t, c, "git", "commit", "-q", "-m", "base")
	run(t, c, "git", "push", "-q", "origin", "main")
	run(t, c, "git", "fetch", "-q", "origin")
	// Uncommitted: a change, an untracked text file without a final
	// newline, an ignored file, and a binary one.
	write(t, filepath.Join(root, "a.txt"), "1\n2\n")
	write(t, filepath.Join(root, "new.txt"), "x\ny")
	write(t, filepath.Join(root, ".gitignore"), "ignored.txt\n")
	write(t, filepath.Join(root, "ignored.txt"), "a\nb\nc\n")
	write(t, filepath.Join(root, "bin.dat"), "a\x00b\n")
	st, _, _, err = Status(f.ctx, root, "feature", &cache)
	if err != nil {
		t.Fatal(err)
	}
	// Committed: a.txt +3, README +1 -1. Uncommitted: a.txt -1, new.txt
	// 2 lines, .gitignore 1 line, bin.dat 0.
	if st.Committed != [2]int{4, 1} || st.Ahead != 2 || st.Behind != 1 || st.Uncommitted != [2]int{setup[0] + 3, setup[1] + 1} || !st.Dirty || st.UncommittedPartial {
		t.Errorf("stats: %+v", st)
	}
	if st.Conflict == nil || *st.Conflict {
		t.Errorf("conflict: %v", st.Conflict)
	}
	// A cached untracked file is not read again while its size and
	// mtime hold: its content changed behind the key reads as before.
	old, _ := os.Stat(filepath.Join(root, "new.txt"))
	write(t, filepath.Join(root, "new.txt"), "q\nz")
	os.Chtimes(filepath.Join(root, "new.txt"), old.ModTime(), old.ModTime())
	cache.pair.Head = "" // committed recomputed, untracked from the cache
	if st2, _, _, _ := Status(f.ctx, root, "feature", &cache); st2.Uncommitted != st.Uncommitted {
		t.Errorf("cached untracked: %+v", st2.Uncommitted)
	}

	// A conflict with the base.
	write(t, filepath.Join(c, "README"), "base side\n")
	run(t, c, "git", "commit", "-q", "-am", "conflict")
	run(t, c, "git", "push", "-q", "origin", "main")
	run(t, root, "git", "fetch", "-q", "origin")
	st, _, _, err = Status(f.ctx, root, "feature", &cache)
	if err != nil {
		t.Fatal(err)
	}
	if st.Conflict == nil || !*st.Conflict || st.Behind != 2 {
		t.Errorf("conflict: %v behind %d", st.Conflict, st.Behind)
	}

	// Rebasing.
	os.MkdirAll(filepath.Join(paths.GitDir, "rebase-merge"), 0o755)
	st, _, _, _ = Status(f.ctx, root, "feature", &cache)
	if !st.Rebasing {
		t.Error("rebase not seen")
	}
	os.RemoveAll(filepath.Join(paths.GitDir, "rebase-merge"))

	// Many untracked files: a lower bound.
	for i := 0; i < untrackedFiles+5; i++ {
		write(t, filepath.Join(root, "many", strings.Repeat("f", 1)+time.Duration(i).String()), "l\n")
	}
	st, _, _, _ = Status(f.ctx, root, "feature", &cache)
	if !st.UncommittedPartial {
		t.Error("past the file limit, not partial")
	}
}

// On the base branch only the uncommitted side is read; a branch with no
// key compares with origin/HEAD's branch.
func TestStatusOnBase(t *testing.T) {
	f := newFixture(t)
	a, _, err := f.add("main-copy")
	if err != nil {
		t.Fatal(err)
	}
	c := f.checkout()
	run(t, a.Root, "git", "config", "--unset", "branch.main-copy."+BaseKey)
	var cache StatusCache
	st, _, _, err := Status(f.ctx, a.Root, "main-copy", &cache)
	if err != nil || st.Base != "origin/main" {
		t.Fatalf("no key: %+v %v", st, err)
	}
	if !onBase("origin/main", "main") || !onBase("main", "main") || onBase("origin/main", "feature") || onBase("origin/main", "") {
		t.Error("onBase")
	}
	st, _, _, err = Status(f.ctx, c, "main", &cache)
	if err != nil || st.Conflict != nil || st.Committed != [2]int{} {
		t.Errorf("on base: %+v %v", st, err)
	}
}

func TestNumstat(t *testing.T) {
	if got := numstat("3\t1\ta.txt\n-\t-\tbin\n10\t0\tb c\n"); got != [2]int{13, 1} {
		t.Errorf("numstat %v", got)
	}
}

// BenchmarkStatus is one refresh of a worktree of a repository with
// 2000 files, two commits on the branch, a changed file and ten
// untracked ones, with the cache warm as the daemon keeps it: what a
// worktree costs every five seconds while it has a session.
func BenchmarkStatus(b *testing.B) {
	t := b
	f := newFixture(t)
	c := func() string {
		if _, _, err := f.add("seed"); err != nil {
			b.Fatal(err)
		}
		return f.checkout()
	}()
	gitCfg(t, c)
	for i := 0; i < 2000; i++ {
		write(t, filepath.Join(c, "src", strconv.Itoa(i%40), strconv.Itoa(i)+".txt"), strings.Repeat("line of text\n", 50))
	}
	run(t, c, "git", "add", ".")
	run(t, c, "git", "commit", "-q", "-m", "files")
	run(t, c, "git", "push", "-q", "origin", "main")
	a, _, err := f.add("bench")
	if err != nil {
		b.Fatal(err)
	}
	gitCfg(t, a.Root)
	for i := 0; i < 2; i++ {
		write(t, filepath.Join(a.Root, "src", "0", strconv.Itoa(i*40)+".txt"), strings.Repeat("changed\n", 10))
		run(t, a.Root, "git", "commit", "-q", "-am", "c")
	}
	write(t, filepath.Join(a.Root, "src", "1", "1.txt"), "x\n") // an existing file changed
	for i := 0; i < 10; i++ {
		write(t, filepath.Join(a.Root, "new", strconv.Itoa(i)+".txt"), "a\nb\n")
	}
	var cache StatusCache
	if _, _, _, err := Status(f.ctx, a.Root, "bench", &cache); err != nil {
		b.Fatal(err)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, _, _, err := Status(f.ctx, a.Root, "bench", &cache); err != nil {
			b.Fatal(err)
		}
	}
}

// Review round 1: a staged change the working tree undoes is dirty; the
// committed stats are of the commits given, whatever the names point at
// now; a local base with a slash is watched under refs/heads; a cancelled
// resolution is an error, not a missing base; an unreadable untracked
// file is a lower bound, counted once it can be read.
func TestStatusEdges(t *testing.T) {
	f := newFixture(t)
	a, _, err := f.add("edges")
	if err != nil {
		t.Fatal(err)
	}
	root := a.Root
	gitCfg(t, root)
	var cache StatusCache
	if _, _, _, err := Status(f.ctx, root, "edges", &cache); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"log", ".envrc"} {
		os.Remove(filepath.Join(root, p))
	}
	write(t, filepath.Join(root, "README"), "staged\n")
	run(t, root, "git", "add", "README")
	write(t, filepath.Join(root, "README"), "hello\n") // the working tree back at HEAD
	st, head, _, err := Status(f.ctx, root, "edges", &cache)
	if err != nil {
		t.Fatal(err)
	}
	if !st.Dirty {
		t.Errorf("a staged change undone in the working tree is clean: %+v", st)
	}

	// Committed by commits: the base moves, the old pair still counts
	// against the old base.
	c := f.checkout()
	gitCfg(t, c)
	oldBase := strings.TrimSpace(run(t, c, "git", "rev-parse", "origin/main"))
	write(t, filepath.Join(c, "more.txt"), "m\n")
	run(t, c, "git", "add", "more.txt")
	run(t, c, "git", "commit", "-q", "-m", "more")
	run(t, c, "git", "push", "-q", "origin", "main")
	run(t, c, "git", "fetch", "-q", "origin")
	got, err := readCommitted(f.ctx, root, Pair{Base: oldBase, Head: head})
	if err != nil || got.Behind != 0 {
		t.Errorf("old pair: %+v %v", got, err)
	}

	// A local base with a slash.
	run(t, c, "git", "branch", "release/next", "main")
	run(t, root, "git", "config", "branch.edges."+BaseKey, "release/next")
	cache = StatusCache{}
	st, _, paths, err := Status(f.ctx, root, "edges", &cache)
	if err != nil || st.Base != "release/next" {
		t.Fatalf("slashed base: %+v %v", st, err)
	}
	want := filepath.Join(paths.CommonDir, "refs", "heads", "release", "next")
	found := false
	for _, r := range paths.Refs {
		found = found || r == want
	}
	if !found {
		t.Errorf("watched %v, want %s", paths.Refs, want)
	}

	// A cancelled resolution.
	ctx, cancel := context.WithCancel(f.ctx)
	cancel()
	if _, _, _, err := resolveBase(ctx, root, "edges"); err == nil {
		t.Error("a cancelled resolution is not an error")
	}

	// An unreadable untracked file.
	p := filepath.Join(root, "secret.txt")
	write(t, p, "a\nb\n")
	os.Chmod(p, 0)
	st, _, _, _ = Status(f.ctx, root, "edges", &cache)
	if !st.UncommittedPartial {
		t.Error("an unreadable file is not a lower bound")
	}
	os.Chmod(p, 0o644)
	st, _, _, _ = Status(f.ctx, root, "edges", &cache)
	if st.UncommittedPartial || st.Uncommitted[0] < 2 {
		t.Errorf("readable again, not counted: %+v", st)
	}
}

// Review round 1 (Opus): a refresh never rewrites the index, even with
// files whose stat data changed and content did not; an orphan branch,
// with no merge base, gets ahead and behind alone.
func TestStatusIndexAndOrphan(t *testing.T) {
	f := newFixture(t)
	a, _, err := f.add("idx")
	if err != nil {
		t.Fatal(err)
	}
	root := a.Root
	gitCfg(t, root)
	var cache StatusCache
	Status(f.ctx, root, "idx", &cache)
	gitDir := strings.TrimSpace(run(t, root, "git", "rev-parse", "--absolute-git-dir"))
	index := filepath.Join(gitDir, "index")
	past := time.Now().Add(-time.Hour)
	os.Chtimes(index, past, past)
	later := time.Now().Add(time.Minute)
	os.Chtimes(filepath.Join(root, "README"), later, later) // same content, new stat
	if _, _, _, err := Status(f.ctx, root, "idx", &cache); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(index); !fi.ModTime().Equal(past) {
		t.Errorf("the index was rewritten: %v", fi.ModTime())
	}
	if _, err := os.Stat(index + ".lock"); err == nil {
		t.Error("an index lock left behind")
	}

	run(t, root, "git", "checkout", "-q", "--orphan", "lonely")
	run(t, root, "git", "commit", "-q", "-m", "orphan")
	cache = StatusCache{}
	st, _, _, err := Status(f.ctx, root, "lonely", &cache)
	if err != nil {
		t.Fatalf("orphan: %v", err)
	}
	if st.Ahead != 1 || st.Behind < 1 || st.Committed != [2]int{} || st.Conflict != nil {
		t.Errorf("orphan: %+v", st)
	}
}

// A git call that times out returns at once, though a child it started
// holds its output: the process group is killed and the pipes drained
// for a bounded time.
func TestStatusGitTimeoutWithChild(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "git"), "#!/bin/sh\nsleep 30 &\nsleep 30\n")
	os.Chmod(filepath.Join(dir, "git"), 0o755)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	oldT, oldW := GitTimeout, gitWaitDelay
	GitTimeout, gitWaitDelay = 200*time.Millisecond, 200*time.Millisecond
	defer func() { GitTimeout, gitWaitDelay = oldT, oldW }()
	start := time.Now()
	_, err := statusGit(context.Background(), dir, "status")
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > 2*time.Second {
		t.Errorf("%v after %v", err, time.Since(start))
	}
}

// The committed stats are recomputed when the shallow boundary changes,
// though neither commit moved: a deepened history may have a merge base
// now.
func TestStatusShallowInKey(t *testing.T) {
	f := newFixture(t)
	a, _, err := f.add("shal")
	if err != nil {
		t.Fatal(err)
	}
	var cache StatusCache
	_, _, paths, err := Status(f.ctx, a.Root, "shal", &cache)
	if err != nil {
		t.Fatal(err)
	}
	cache.committed.Ahead = 99 // a sentinel: kept while the key holds
	if st, _, _, _ := Status(f.ctx, a.Root, "shal", &cache); st.Ahead != 99 {
		t.Fatalf("the cache was not used: %+v", st)
	}
	write(t, filepath.Join(paths.CommonDir, "shallow"), "")
	if st, _, _, _ := Status(f.ctx, a.Root, "shal", &cache); st.Ahead == 99 {
		t.Error("a new shallow boundary kept the cached stats")
	}
}
