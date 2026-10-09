package worktree

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/laat/laatmux/internal/protocol"
	"github.com/laat/laatmux/internal/tmux"
)

// What prune decides a worktree on, read as the status refresh reads
// git: every call `--no-optional-locks`, with the refresh's timeout,
// and never to the network. See protocol.RootFacts.

// ReadFacts reads the facts at a worktree root. The branch is HEAD's
// as git has it now, which the caller compares with the listing it
// decided to ask on. An error is a read that failed, a git call that
// timed out say; a repository with no default branch to compare with
// is no error, and has no base.
func ReadFacts(ctx context.Context, root string) (protocol.RootFacts, error) {
	f := protocol.RootFacts{Root: root}
	g := func(args ...string) (string, error) { return statusGit(ctx, root, args...) }
	out, err := g("symbolic-ref", "--quiet", "HEAD")
	var ee *exec.ExitError
	switch {
	case err == nil:
		f.Branch = strings.TrimPrefix(strings.TrimRight(out, "\n"), "refs/heads/")
	case errors.As(err, &ee) && ee.ExitCode() == 1:
		// Detached.
	default:
		return protocol.RootFacts{Root: root}, err
	}
	branch := f.Branch
	out, err = g("rev-parse", "--verify", "HEAD^{commit}")
	if err != nil {
		return protocol.RootFacts{Root: root}, err
	}
	f.Head = strings.TrimSpace(out)
	ch, err := readChanges(ctx, root, true)
	if err != nil {
		return protocol.RootFacts{Root: root}, err
	}
	f.Changed, f.Ignored, f.IgnoredDirs = ch.changed, ch.ignored, ch.ignoredDirs
	if f.Locked, f.LockReason, f.Submodules, err = removable(ctx, root); err != nil {
		return protocol.RootFacts{Root: root}, err
	}
	base, baseOID, err := defaultBase(ctx, root)
	if err != nil {
		return protocol.RootFacts{Root: root}, err
	}
	if base != "" {
		out, err := g("rev-list", "--count", baseOID+".."+f.Head)
		if err != nil {
			return protocol.RootFacts{Root: root}, err
		}
		if f.Ahead, err = strconv.Atoi(strings.TrimSpace(out)); err != nil {
			return protocol.RootFacts{Root: root}, fmt.Errorf("git rev-list --count: %q", strings.TrimSpace(out))
		}
		f.Base = base
	}
	if branch == "" {
		return f, nil
	}
	remote, ok, err := commitOf(ctx, root, "refs/remotes/origin/"+branch)
	switch {
	case err != nil:
		return protocol.RootFacts{Root: root}, err
	case !ok:
		return f, nil
	}
	f.OnOrigin = true
	_, err = g("merge-base", "--is-ancestor", f.Head, remote)
	switch {
	case err == nil:
		f.Pushed = true
	case errors.As(err, &ee) && ee.ExitCode() == 1:
	default:
		return protocol.RootFacts{Root: root}, err
	}
	return f, nil
}

// removable is what git worktree remove refuses without force, as git
// itself checks it: a lock, the locked file in the worktree's git dir,
// with its reason; and a submodule, a modules directory there or a
// submodule of the index with its own .git in the working tree.
func removable(ctx context.Context, root string) (locked bool, reason string, submodules bool, err error) {
	out, err := statusGit(ctx, root, "rev-parse", "--absolute-git-dir")
	if err != nil {
		return false, "", false, err
	}
	gitDir := strings.TrimSuffix(out, "\n")
	if b, err := os.ReadFile(filepath.Join(gitDir, "locked")); err == nil {
		locked, reason = true, strings.TrimSpace(string(b))
	} else if !errors.Is(err, os.ErrNotExist) {
		return false, "", false, tmux.PrintablePath(err)
	}
	if fi, err := os.Stat(filepath.Join(gitDir, "modules")); err == nil && fi.IsDir() {
		return locked, reason, true, nil
	}
	// The index of a large repository is read only where a submodule
	// may be: a repository with a .gitmodules. A repository nested
	// without one is a submodule to git all the same, whose removal git
	// refuses: the cost of the fast path is that failure.
	if _, err := os.Lstat(filepath.Join(root, ".gitmodules")); err != nil {
		return locked, reason, false, nil
	}
	out, err = statusGit(ctx, root, "ls-files", "--stage", "-z")
	if err != nil {
		return false, "", false, err
	}
	for _, e := range strings.Split(out, "\x00") {
		meta, path, ok := strings.Cut(e, "\t")
		if !ok || !strings.HasPrefix(meta, "160000 ") {
			continue
		}
		if _, err := os.Lstat(filepath.Join(root, path, ".git")); err == nil {
			return locked, reason, true, nil
		}
	}
	return locked, reason, false, nil
}

// defaultBase is the repository's default branch as prune compares
// with it, by its short name, and its commit: origin/HEAD's branch,
// else origin/main, origin/master, main, master, the first that is a
// commit. "" when none is. Unlike the status refresh's base, a
// branch's laatmux-base key does not count: what a branch was made
// from need not be where its work lands.
func defaultBase(ctx context.Context, root string) (name, oid string, err error) {
	var candidates []string
	out, err := statusGit(ctx, root, "symbolic-ref", "--quiet", "refs/remotes/origin/HEAD")
	if timedOut(err) {
		return "", "", err
	}
	// Only the line end is git's: a branch may end in U+00A0, which
	// TrimSpace would take.
	if ref := strings.TrimSuffix(out, "\n"); err == nil && strings.HasPrefix(ref, "refs/remotes/origin/") {
		candidates = append(candidates, ref)
	}
	candidates = append(candidates, "refs/remotes/origin/main", "refs/remotes/origin/master", "refs/heads/main", "refs/heads/master")
	for _, ref := range candidates {
		oid, ok, err := commitOf(ctx, root, ref)
		if err != nil {
			return "", "", err
		}
		if ok {
			return strings.TrimPrefix(strings.TrimPrefix(ref, "refs/remotes/"), "refs/heads/"), oid, nil
		}
	}
	return "", "", nil
}

// commitOf is the commit a full ref names, not ok when there is no
// such ref or it names no commit. An error is a call that timed out or
// was cancelled.
func commitOf(ctx context.Context, root, ref string) (oid string, ok bool, err error) {
	out, err := statusGit(ctx, root, "rev-parse", "--verify", "--quiet", ref+"^{commit}")
	switch {
	case timedOut(err):
		return "", false, err
	case err != nil:
		return "", false, nil
	}
	oid = strings.TrimSpace(out)
	return oid, oid != "", nil
}

// timedOut is a git call that timed out or was cancelled, which says
// nothing about the ref it asked for.
func timedOut(err error) bool {
	return errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled)
}

// HeadIs refuses a worktree whose HEAD is not at head, the commit the
// caller decided on: rm's check for prune, which removes only what it
// read. A root whose directory is gone has nothing in it to lose, and
// passes.
func HeadIs(ctx context.Context, root, head string) error {
	if _, err := os.Lstat(root); errors.Is(err, os.ErrNotExist) {
		return nil
	}
	out, err := statusGit(ctx, root, "rev-parse", "--verify", "HEAD^{commit}")
	if err != nil {
		return fmt.Errorf("HEAD at %s: %w", tmux.Printable(root), err)
	}
	if now := strings.TrimSpace(out); now != head {
		return fmt.Errorf("HEAD at %s is %s now, not %s as it was read; not removed", tmux.Printable(root), short(now), short(head))
	}
	return nil
}

// DeleteBranch deletes a branch of the checkout when it is still at
// head, with its reflog and then its config section, as git branch -D
// deletes them. A branch some worktree of the checkout has checked
// out, the main one included, stays, as git branch -D keeps it. Not
// deleted with no error is a branch already gone. A branch at another
// commit is an error saying where it is.
func DeleteBranch(ctx context.Context, checkout, branch, head string) (deleted bool, err error) {
	ref := "refs/heads/" + branch
	out, err := git(ctx, checkout, "rev-parse", "--verify", "--quiet", ref)
	var ee *exec.ExitError
	switch {
	case errors.As(err, &ee) && ee.ExitCode() == 1:
		return false, nil
	case err != nil:
		return false, err
	}
	if now := strings.TrimSpace(out); now != head {
		return false, fmt.Errorf("it is at %s now, not %s", short(now), short(head))
	}
	// A branch made a symbolic ref since names another branch, whose
	// commit the check above read: not the branch the worktree had.
	_, err = git(ctx, checkout, "symbolic-ref", "--quiet", ref)
	switch {
	case err == nil:
		return false, errors.New("it is a symbolic ref now")
	case !errors.As(err, &ee) || ee.ExitCode() != 1:
		return false, err
	}
	// git branch -D also keeps a branch a worktree is rebasing or
	// bisecting, which the listing shows detached; the worktree this
	// rm removed had the branch checked out until now, so no other can
	// be in either on it without a forced checkout.
	entries, err := ListWorktrees(ctx, checkout)
	if err != nil {
		return false, err
	}
	for _, e := range entries {
		if e.Branch == branch {
			return false, fmt.Errorf("it is checked out at %s", tmux.Printable(e.Root))
		}
	}
	if err := deleteRefAt(ctx, checkout, ref, head); err != nil {
		return false, err
	}
	// Best effort, as the ref is gone: a branch with no config has no
	// section, which git reports as an error.
	_, _ = git(ctx, checkout, "config", "--remove-section", "branch."+branch)
	return true, nil
}

// deleteRefAt deletes a ref that is at head. With the old value git
// deletes it only if it is still there under the ref's lock: a commit
// made on the branch since DeleteBranch looked, by a git of the
// user's, is refused rather than deleted. --no-deref deletes the ref
// itself, never the branch a symbolic ref made since would name.
func deleteRefAt(ctx context.Context, checkout, ref, head string) error {
	if _, err := git(ctx, checkout, "update-ref", "--no-deref", "-d", ref, head); err != nil {
		if out, rerr := git(ctx, checkout, "rev-parse", "--verify", "--quiet", ref); rerr == nil && strings.TrimSpace(out) != head {
			return fmt.Errorf("it is at %s now, not %s", short(strings.TrimSpace(out)), short(head))
		}
		return err
	}
	return nil
}

// short is a commit as git abbreviates it by default, for a message.
func short(oid string) string {
	if len(oid) > 7 {
		return oid[:7]
	}
	return oid
}
