package daemon

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/laat/laatmux/internal/protocol"
	"github.com/laat/laatmux/internal/worktree"
)

// pruneCheckout clones the store's remote as its main checkout, with an
// identity for commits, and returns a func that makes a worktree on a
// new branch from origin/main under the worktrees directory.
func pruneCheckout(t *testing.T, store *worktree.Store, remote string) (string, func(branch string) string) {
	t.Helper()
	co := filepath.Join(store.Dirs.Repos[0], "proj")
	mkdirs(t, store.Dirs.Repos[0])
	sh(t, store.Dirs.Repos[0], "git", "clone", "-q", remote, co)
	sh(t, co, "git", "config", "user.email", "t@example.com")
	sh(t, co, "git", "config", "user.name", "t")
	return co, func(branch string) string {
		t.Helper()
		root := store.Dirs.Worktree("proj", branch)
		sh(t, co, "git", "worktree", "add", "-q", "-b", branch, root, "origin/main")
		return root
	}
}

// The facts message against real repositories: a clean worktree at
// main, one with a commit, one dirty, one pushed and one whose branch
// is gone from origin, each as git has it; a detached one has no
// branch; the main checkout and a directory that is no worktree have
// an error each, and the rest are read all the same, in the order
// asked.
func TestFacts(t *testing.T) {
	store, remote := newStore(t)
	co, add := pruneCheckout(t, store, remote)
	clean, ahead, dirty, pushed, gone := add("clean"), add("ahead"), add("dirty"), add("pushed"), add("gone")
	for _, root := range []string{ahead, pushed, gone} {
		sh(t, root, "git", "commit", "-q", "--allow-empty", "-m", "work")
	}
	os.WriteFile(filepath.Join(dirty, "new.txt"), []byte("x\n"), 0o644)
	sh(t, pushed, "git", "push", "-q", "origin", "pushed")
	sh(t, gone, "git", "push", "-q", "origin", "gone")
	sh(t, gone, "git", "push", "-q", "origin", "--delete", "gone")
	detached := store.Dirs.Worktree("proj", "detached")
	sh(t, co, "git", "worktree", "add", "-q", "--detach", detached, "origin/main")
	outside := t.TempDir()

	d, _ := addDaemon(t, store)
	if !protocol.Has(d.capabilities(), protocol.CapPrune) {
		t.Fatalf("caps %v", d.capabilities())
	}
	pc := conn(t, d)
	roots := []string{clean, ahead, dirty, pushed, gone, detached, co, outside}
	pc.Write(protocol.Message{Type: protocol.TypeFacts, ID: "f1", Roots: roots})
	res, _ := result(t, pc, "f1")
	if !res.OK || len(res.Facts) != len(roots) {
		t.Fatalf("result %+v", res)
	}
	main := gitOut(t, co, "rev-parse", "origin/main")
	want := []protocol.RootFacts{
		{Root: clean, Branch: "clean", Head: main, Base: "origin/main"},
		{Root: ahead, Branch: "ahead", Head: gitOut(t, ahead, "rev-parse", "HEAD"), Base: "origin/main", Ahead: 1},
		{Root: dirty, Branch: "dirty", Head: main, Base: "origin/main", Changed: 1},
		{Root: pushed, Branch: "pushed", Head: gitOut(t, pushed, "rev-parse", "HEAD"), Base: "origin/main", Ahead: 1, OnOrigin: true, Pushed: true},
		{Root: gone, Branch: "gone", Head: gitOut(t, gone, "rev-parse", "HEAD"), Base: "origin/main", Ahead: 1},
		{Root: detached, Head: main, Base: "origin/main"},
	}
	for i, w := range want {
		if res.Facts[i] != w {
			t.Errorf("facts %d\n got %+v\nwant %+v", i, res.Facts[i], w)
		}
	}
	for _, f := range res.Facts[len(want):] {
		if !strings.Contains(f.Error, "is not a worktree under the worktrees directory") || f.Head != "" || f.Branch != "" {
			t.Errorf("not a worktree: %+v", f)
		}
	}
	if res.Facts[6].Root != co || res.Facts[7].Root != outside {
		t.Errorf("roots out of order: %+v", res.Facts[6:])
	}
}

// rm with head removes only a worktree still at that commit, and with
// delete_branch deletes its branch once the worktree is gone, said in
// the branch stage; a run that removes no worktree, one gone already,
// keeps the branch, since which clone it was in is not known; and
// delete_branch alone is refused before anything is done.
func TestRmHeadAndBranch(t *testing.T) {
	store, remote := newStore(t)
	co, add := pruneCheckout(t, store, remote)
	done, kept := add("done"), add("kept")
	main := gitOut(t, co, "rev-parse", "origin/main")
	d, _ := addDaemon(t, store)
	pc := conn(t, d)

	pc.Write(protocol.Message{Type: protocol.TypeRm, ID: "r0", Repo: remote, Branch: "done", Root: done, DeleteBranch: true})
	if res, _ := result(t, pc, "r0"); res.OK || !strings.Contains(res.Error, "only given the branch and the commit") {
		t.Fatalf("delete_branch without head: %+v", res)
	}
	sh(t, done, "git", "commit", "-q", "--allow-empty", "-m", "since")
	pc.Write(protocol.Message{Type: protocol.TypeRm, ID: "r1", Repo: remote, Branch: "done", Root: done, Head: main, DeleteBranch: true})
	if res, _ := result(t, pc, "r1"); res.OK || !strings.Contains(res.Error, "as it was read; not removed") {
		t.Fatalf("moved HEAD: %+v", res)
	}
	if _, err := os.Stat(done); err != nil {
		t.Fatalf("the worktree went: %v", err)
	}
	head := gitOut(t, done, "rev-parse", "HEAD")
	pc.Write(protocol.Message{Type: protocol.TypeRm, ID: "r2", Repo: remote, Branch: "done", Root: done, Head: head, DeleteBranch: true})
	res, progress := result(t, pc, "r2")
	if !res.OK || !hasProgress(progress, protocol.StageBranch, protocol.StateDone, "deleted branch done") {
		t.Fatalf("rm: %+v %+v", res, progress)
	}
	if _, err := os.Stat(done); err == nil {
		t.Fatal("the worktree is still there")
	}
	if out := gitOut(t, co, "branch", "--list", "done"); out != "" {
		t.Fatalf("branch left: %q", out)
	}

	// Removed by hand: this run removes nothing, and the branch stays.
	sh(t, co, "git", "worktree", "remove", kept)
	pc.Write(protocol.Message{Type: protocol.TypeRm, ID: "r3", Repo: remote, Branch: "kept", Root: kept, Head: main, DeleteBranch: true})
	res, progress = result(t, pc, "r3")
	if !res.OK || !hasProgress(progress, protocol.StageBranch, protocol.StateSkip, "branch kept kept: no worktree of it was removed here") {
		t.Fatalf("rm of a worktree gone: %+v %+v", res, progress)
	}
	if out := gitOut(t, co, "branch", "--list", "kept"); out == "" {
		t.Fatal("the branch went")
	}
}

// gitOut is git's output in dir, trimmed.
func gitOut(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %s: %v", strings.Join(args, " "), err)
	}
	return strings.TrimSpace(string(out))
}
