package worktree

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/laat/laatmux/internal/protocol"
)

// The facts prune reads: the branch and HEAD, the changed files git
// status lists, the commits ahead of the default branch, origin/HEAD's
// else origin/main and so on down to a local master, and whether the
// branch is on origin with HEAD in it.
func TestReadFacts(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	c := filepath.Join(t.TempDir(), "clone")
	run(t, filepath.Dir(c), "git", "clone", "-q", f.remote, c)
	gitCfg(t, c)
	facts := func(want protocol.RootFacts) {
		t.Helper()
		got, err := ReadFacts(ctx, c)
		if err != nil {
			t.Fatal(err)
		}
		if want.Root, want.Head = c, strings.TrimSpace(run(t, c, "git", "rev-parse", "HEAD")); got != want {
			t.Errorf("facts\n got %+v\nwant %+v", got, want)
		}
	}
	facts(protocol.RootFacts{Branch: "main", Base: "origin/main", OnOrigin: true, Pushed: true})

	run(t, c, "git", "checkout", "-q", "-b", "feature")
	run(t, c, "git", "commit", "-q", "--allow-empty", "-m", "one")
	facts(protocol.RootFacts{Branch: "feature", Base: "origin/main", Ahead: 1})
	run(t, c, "git", "push", "-q", "origin", "feature")
	facts(protocol.RootFacts{Branch: "feature", Base: "origin/main", Ahead: 1, OnOrigin: true, Pushed: true})
	run(t, c, "git", "commit", "-q", "--allow-empty", "-m", "two")
	facts(protocol.RootFacts{Branch: "feature", Base: "origin/main", Ahead: 2, OnOrigin: true})

	// An untracked file and a changed one; ignored ones are no change,
	// and counted apart, a directory once whatever is in it.
	write(t, filepath.Join(c, "new.txt"), "x\n")
	write(t, filepath.Join(c, "README"), "changed\n")
	write(t, filepath.Join(c, ".git", "info", "exclude"), "ignored.txt\nsub/*.log\ncache/\n")
	write(t, filepath.Join(c, "ignored.txt"), "x\n")
	write(t, filepath.Join(c, "sub", "a.log"), "x\n")
	write(t, filepath.Join(c, "cache", "one", "two"), "x\n")
	write(t, filepath.Join(c, "cache", "three"), "x\n")
	facts(protocol.RootFacts{Branch: "feature", Base: "origin/main", Ahead: 2, OnOrigin: true, Changed: 2, Ignored: 2, IgnoredDirs: 1})
	run(t, c, "git", "checkout", "-q", "README")
	for _, p := range []string{"new.txt", "ignored.txt", "sub", "cache"} {
		os.RemoveAll(filepath.Join(c, p))
	}

	// origin/HEAD decides, by the ref's name to the last byte; without
	// it origin/main; without a remote branch the local main.
	run(t, c, "git", "push", "-q", "origin", "feature:trunk")
	run(t, c, "git", "remote", "set-head", "origin", "trunk")
	facts(protocol.RootFacts{Branch: "feature", Base: "origin/trunk", OnOrigin: true})
	run(t, c, "git", "push", "-q", "origin", "main:trunk\u00a0")
	run(t, c, "git", "remote", "set-head", "origin", "trunk\u00a0")
	facts(protocol.RootFacts{Branch: "feature", Base: "origin/trunk\u00a0", Ahead: 2, OnOrigin: true})
	run(t, c, "git", "remote", "set-head", "origin", "-d")
	facts(protocol.RootFacts{Branch: "feature", Base: "origin/main", Ahead: 2, OnOrigin: true})
	run(t, c, "git", "update-ref", "-d", "refs/remotes/origin/main")
	run(t, c, "git", "update-ref", "refs/remotes/origin/master", "main")
	facts(protocol.RootFacts{Branch: "feature", Base: "origin/master", Ahead: 2, OnOrigin: true})
	run(t, c, "git", "update-ref", "-d", "refs/remotes/origin/master")
	facts(protocol.RootFacts{Branch: "feature", Base: "main", Ahead: 2, OnOrigin: true})
	run(t, c, "git", "branch", "-q", "-m", "main", "master")
	facts(protocol.RootFacts{Branch: "feature", Base: "master", Ahead: 2, OnOrigin: true})
	run(t, c, "git", "branch", "-q", "-D", "master")
	facts(protocol.RootFacts{Branch: "feature", OnOrigin: true})

	run(t, c, "git", "checkout", "-q", "--detach")
	facts(protocol.RootFacts{})

	if _, err := ReadFacts(ctx, t.TempDir()); err == nil {
		t.Error("facts outside a repository")
	}
}

// What git worktree remove refuses without force, read as git checks
// it: a lock with its reason, or without one; a submodule checked out
// in the worktree. A worktree with neither has neither.
func TestRemovable(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	c := filepath.Join(t.TempDir(), "clone")
	run(t, filepath.Dir(c), "git", "clone", "-q", f.remote, c)
	gitCfg(t, c)
	wt := func(name string) string {
		// The real path, as git registers it, which Remove matches.
		dir, err := filepath.EvalSymlinks(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		root := filepath.Join(dir, name)
		run(t, c, "git", "worktree", "add", "-q", "-b", name, root, "main")
		return root
	}
	plain, locked, bare, sub, mods := wt("plain"), wt("locked"), wt("bare-lock"), wt("sub"), wt("mods")
	run(t, c, "git", "worktree", "lock", "--reason", "keep me", locked)
	run(t, c, "git", "worktree", "lock", bare)
	run(t, sub, "git", "-c", "protocol.file.allow=always", "submodule", "add", "-q", f.remote, "vendor/proj")
	// A modules directory in the worktree's git dir is a submodule to
	// git, with none in the index.
	if err := os.Mkdir(filepath.Join(strings.TrimSpace(run(t, mods, "git", "rev-parse", "--absolute-git-dir")), "modules"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, k := range []struct {
		root       string
		locked     bool
		reason     string
		submodules bool
	}{
		{plain, false, "", false}, {locked, true, "keep me", false}, {bare, true, "", false}, {sub, false, "", true}, {mods, false, "", true},
	} {
		got, err := ReadFacts(ctx, k.root)
		if err != nil {
			t.Fatal(err)
		}
		if got.Locked != k.locked || got.LockReason != k.reason || got.Submodules != k.submodules {
			t.Errorf("%s: locked %v %q, submodules %v", filepath.Base(k.root), got.Locked, got.LockReason, got.Submodules)
		}
	}
	// git agrees: each one read as kept is refused without force.
	for _, root := range []string{locked, bare, sub, mods} {
		if _, err := Remove(ctx, c, root, false); err == nil {
			t.Errorf("%s: removed without force", filepath.Base(root))
		}
	}
}

// rm's check for prune: HEAD at the commit given passes, another is
// refused, and a root whose directory is gone passes.
func TestHeadIs(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	c := filepath.Join(t.TempDir(), "clone")
	run(t, filepath.Dir(c), "git", "clone", "-q", f.remote, c)
	gitCfg(t, c)
	head := strings.TrimSpace(run(t, c, "git", "rev-parse", "HEAD"))
	if err := HeadIs(ctx, c, head); err != nil {
		t.Fatal(err)
	}
	run(t, c, "git", "commit", "-q", "--allow-empty", "-m", "since")
	if err := HeadIs(ctx, c, head); err == nil || !strings.Contains(err.Error(), "not "+head[:7]+" as it was read") {
		t.Fatalf("moved HEAD: %v", err)
	}
	if err := HeadIs(ctx, filepath.Join(c, "gone"), head); err != nil {
		t.Fatalf("gone root: %v", err)
	}
}

// DeleteBranch deletes a branch at the commit given, its config with
// it; one at another commit, or checked out in a worktree, stays with
// the error; one already gone is no error and nothing deleted.
func TestDeleteBranch(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	c := filepath.Join(t.TempDir(), "clone")
	run(t, filepath.Dir(c), "git", "clone", "-q", f.remote, c)
	gitCfg(t, c)
	head := strings.TrimSpace(run(t, c, "git", "rev-parse", "HEAD"))
	run(t, c, "git", "branch", "done")
	run(t, c, "git", "config", "branch.done."+BaseKey, "origin/main")
	if deleted, err := DeleteBranch(ctx, c, "done", head); err != nil || !deleted {
		t.Fatalf("delete: %v %v", deleted, err)
	}
	if out, err := git(ctx, c, "config", "--get-regexp", `^branch\.done\.`); err == nil {
		t.Errorf("config left: %q", out)
	}
	if deleted, err := DeleteBranch(ctx, c, "done", head); err != nil || deleted {
		t.Fatalf("delete again: %v %v", deleted, err)
	}

	run(t, c, "git", "checkout", "-q", "-b", "moved")
	run(t, c, "git", "commit", "-q", "--allow-empty", "-m", "since")
	run(t, c, "git", "checkout", "-q", "main")
	if deleted, err := DeleteBranch(ctx, c, "moved", head); err == nil || deleted || !strings.Contains(err.Error(), "not "+head[:7]) {
		t.Fatalf("moved: %v %v", deleted, err)
	}
	if _, err := git(ctx, c, "rev-parse", "--verify", "refs/heads/moved"); err != nil {
		t.Fatal("the moved branch went")
	}

	wt := filepath.Join(t.TempDir(), "wt")
	run(t, c, "git", "worktree", "add", "-q", "-b", "used", wt, "main")
	if deleted, err := DeleteBranch(ctx, c, "used", head); err == nil || deleted {
		t.Fatalf("checked out: %v %v", deleted, err)
	}
	if _, err := git(ctx, c, "rev-parse", "--verify", "refs/heads/used"); err != nil {
		t.Fatal("the checked-out branch went")
	}
	if deleted, err := DeleteBranch(ctx, c, "main", head); err == nil || deleted || !strings.Contains(err.Error(), "checked out at") {
		t.Fatalf("the main checkout's branch: %v %v", deleted, err)
	}

	// The deletion itself is conditional: a branch moved between the
	// look and the deletion stays.
	if err := deleteRefAt(ctx, c, "refs/heads/moved", head); err == nil || !strings.Contains(err.Error(), "not "+head[:7]) {
		t.Fatalf("deleteRefAt of a moved branch: %v", err)
	}
	if _, err := git(ctx, c, "rev-parse", "--verify", "refs/heads/moved"); err != nil {
		t.Fatal("the moved branch went")
	}
}
