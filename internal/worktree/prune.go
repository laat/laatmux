package worktree

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
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
	if f.Changed, _, err = readChanges(ctx, root); err != nil {
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
	if err != nil || !ok {
		return f, err
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
	if ref := strings.TrimSpace(out); err == nil && strings.HasPrefix(ref, "refs/remotes/origin/") {
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
// head, as git branch -D does, its config section with it; git refuses
// a branch some worktree has checked out. Not deleted with no error is
// a branch already gone. A branch at another commit is an error saying
// where it is.
func DeleteBranch(ctx context.Context, checkout, branch, head string) (deleted bool, err error) {
	out, err := git(ctx, checkout, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch)
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
	// Between the check and the deletion the branch could move only by
	// a git of the user's: no worktree has it checked out, or git
	// refuses, and the daemon holds every repository.
	if _, err := git(ctx, checkout, "branch", "-D", "--", branch); err != nil {
		return false, err
	}
	return true, nil
}

// short is a commit as git abbreviates it by default, for a message.
func short(oid string) string {
	if len(oid) > 7 {
		return oid[:7]
	}
	return oid
}
