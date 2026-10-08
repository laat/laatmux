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

	// An untracked file and a changed one; an ignored one is no change.
	write(t, filepath.Join(c, "new.txt"), "x\n")
	write(t, filepath.Join(c, "README"), "changed\n")
	write(t, filepath.Join(c, ".git", "info", "exclude"), "ignored.txt\n")
	write(t, filepath.Join(c, "ignored.txt"), "x\n")
	facts(protocol.RootFacts{Branch: "feature", Base: "origin/main", Ahead: 2, OnOrigin: true, Changed: 2})
	run(t, c, "git", "checkout", "-q", "README")
	os.Remove(filepath.Join(c, "new.txt"))

	// origin/HEAD decides; without it origin/main; without a remote
	// branch the local main.
	run(t, c, "git", "push", "-q", "origin", "feature:trunk")
	run(t, c, "git", "remote", "set-head", "origin", "trunk")
	facts(protocol.RootFacts{Branch: "feature", Base: "origin/trunk", OnOrigin: true})
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
}
