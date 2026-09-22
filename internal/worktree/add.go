package worktree

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/laat/laatmux/internal/config"
	"github.com/laat/laatmux/internal/protocol"
)

// Reporter receives one call per step of add: the stage, a
// protocol.State* value, and a detail line.
type Reporter func(stage, state, detail string)

// StageError is a failed stage: which one, and why.
type StageError struct {
	Stage string
	Err   error
}

func (e *StageError) Error() string { return e.Stage + ": " + e.Err.Error() }
func (e *StageError) Unwrap() error { return e.Err }

func fail(stage string, err error) error { return &StageError{Stage: stage, Err: err} }

// Added is what the git stages of add leave behind for the agent stage.
type Added struct {
	Checkout string
	Root     string // as git registered it
}

// Add runs the resolve, clone, fetch, worktree, copy and setup stages for
// a branch of repo. Every step that mutates something has its own check,
// so a retry after a crash skips exactly what is done and finishes what
// is not. A failed stage stops the sequence with a StageError and leaves
// the worktree in place for a retry.
func (s *Store) Add(ctx context.Context, repo Repo, branch string, report Reporter) (Added, error) {
	if report == nil {
		report = func(string, string, string) {}
	}
	var a Added

	// resolve
	stage := protocol.StageResolve
	if err := checkBranch(ctx, branch); err != nil {
		return a, fail(stage, err)
	}
	checkout, found, err := s.Checkout(ctx, repo)
	if err != nil {
		return a, fail(stage, err)
	}
	if !found {
		checkout = s.Dirs.Checkout(repo.Name)
	}
	a.Checkout = checkout
	a.Root = s.Dirs.Worktree(repo.Name, branch)
	if found {
		entries, err := ListWorktrees(ctx, checkout)
		if err != nil {
			return a, fail(stage, err)
		}
		// Only a worktree under the worktrees directory is the worktree
		// for the branch; one elsewhere fails the worktree stage below.
		for _, e := range entries {
			if e.Branch == branch && !e.Prunable && s.Owns(e.Root) {
				a.Root = e.Root
			}
		}
	}
	report(stage, protocol.StateDone, fmt.Sprintf("checkout %s, worktree %s", checkout, a.Root))

	// clone
	stage = protocol.StageClone
	if found {
		report(stage, protocol.StateSkip, "checkout exists")
	} else {
		if _, err := os.Stat(checkout); err == nil {
			url, hasOrigin, _ := s.origin(ctx, checkout)
			switch {
			case hasOrigin:
				return a, fail(stage, fmt.Errorf("%s exists with origin %s, not %s", checkout, url, repo.Source))
			default:
				return a, fail(stage, fmt.Errorf("%s exists and is not a checkout of %s", checkout, repo.Source))
			}
		}
		if err := os.MkdirAll(s.Dirs.Repos, 0o755); err != nil {
			return a, fail(stage, err)
		}
		report(stage, protocol.StateStart, "git clone "+repo.Source+" "+checkout)
		if err := runStreaming(ctx, s.Dirs.Repos, report, stage, gitEnv(), "git", "clone", "--", repo.Source, checkout); err != nil {
			return a, fail(stage, err)
		}
		report(stage, protocol.StateDone, "cloned")
	}

	// fetch: never skipped; the branch base must be fresh.
	stage = protocol.StageFetch
	report(stage, protocol.StateStart, "git fetch origin")
	if err := runStreaming(ctx, checkout, report, stage, gitEnv(), "git", "fetch", "origin"); err != nil {
		return a, fail(stage, err)
	}
	report(stage, protocol.StateDone, "fetched")

	// worktree
	stage = protocol.StageWorktree
	if _, err := git(ctx, checkout, "remote", "set-head", "origin", "--auto"); err != nil {
		return a, fail(stage, err)
	}
	if _, err := git(ctx, checkout, "worktree", "prune"); err != nil {
		return a, fail(stage, err)
	}
	report(stage, protocol.StateDone, "origin/HEAD refreshed, stale worktrees pruned")
	switch {
	case refExists(ctx, checkout, "refs/heads/"+branch):
		report(stage, protocol.StateSkip, "branch "+branch+" exists, used as is")
	case refExists(ctx, checkout, "refs/remotes/origin/"+branch):
		report(stage, protocol.StateStart, "git branch --track "+branch+" origin/"+branch)
		if _, err := git(ctx, checkout, "branch", "--track", branch, "origin/"+branch); err != nil {
			return a, fail(stage, err)
		}
		report(stage, protocol.StateDone, "branch "+branch+" tracks origin/"+branch)
	default:
		// --no-track: the new branch has no remote counterpart yet, and an
		// upstream of origin/HEAD would make push refuse and pull merge the
		// default branch.
		report(stage, protocol.StateStart, "git branch --no-track "+branch+" origin/HEAD")
		if _, err := git(ctx, checkout, "branch", "--no-track", branch, "origin/HEAD"); err != nil {
			return a, fail(stage, err)
		}
		report(stage, protocol.StateDone, "branch "+branch+" from origin/HEAD")
	}
	entries, err := ListWorktrees(ctx, checkout)
	if err != nil {
		return a, fail(stage, err)
	}
	registered := false
	for _, e := range entries {
		if e.Root == checkout {
			if e.Branch == branch {
				return a, fail(stage, fmt.Errorf("branch %s is checked out in the main checkout %s", branch, checkout))
			}
			continue
		}
		switch {
		case e.Root == a.Root && e.Branch == branch:
			registered = true
		case e.Root == a.Root:
			return a, fail(stage, fmt.Errorf("%s is a worktree on %s, not %s", a.Root, branchOrDetached(e), branch))
		case e.Branch == branch:
			return a, fail(stage, fmt.Errorf("branch %s is checked out at %s", branch, e.Root))
		}
	}
	if registered {
		report(stage, protocol.StateSkip, "worktree registered at "+a.Root)
	} else {
		if err := os.MkdirAll(filepath.Dir(a.Root), 0o755); err != nil {
			return a, fail(stage, err)
		}
		report(stage, protocol.StateStart, "git worktree add "+a.Root+" "+branch)
		if _, err := git(ctx, checkout, "worktree", "add", "--", a.Root, branch); err != nil {
			return a, fail(stage, err)
		}
		// Publish the root as git registered it, symlinks resolved, so the
		// pane option and the worktree record agree.
		entries, err := ListWorktrees(ctx, checkout)
		if err != nil {
			return a, fail(stage, err)
		}
		for _, e := range entries {
			if e.Branch == branch && s.Owns(e.Root) {
				a.Root = e.Root
			}
		}
		report(stage, protocol.StateDone, "worktree at "+a.Root)
	}

	// copy and setup read the worktree's own .laatmux.yaml, so a branch
	// carries its own setup.
	setup, err := config.LoadSetup(a.Root)
	if err != nil {
		return a, fail(protocol.StageCopy, err)
	}

	stage = protocol.StageCopy
	for _, rel := range setup.Copy {
		if err := copyFile(ctx, checkout, a.Root, rel, report); err != nil {
			return a, fail(stage, err)
		}
	}

	stage = protocol.StageSetup
	if len(setup.Setup) > 0 {
		markers, err := markerDir(ctx, a.Root)
		if err != nil {
			return a, fail(stage, err)
		}
		for i, cmd := range setup.Setup {
			marker := filepath.Join(markers, "setup-"+strconv.Itoa(i)+"-"+hash(cmd))
			if _, err := os.Stat(marker); err == nil {
				report(stage, protocol.StateSkip, cmd+" (done before)")
				continue
			}
			report(stage, protocol.StateStart, cmd)
			if err := runStreaming(ctx, a.Root, report, stage, os.Environ(), "sh", "-c", cmd); err != nil {
				return a, fail(stage, fmt.Errorf("%s: %w", cmd, err))
			}
			if err := os.WriteFile(marker, []byte(cmd+"\n"), 0o644); err != nil {
				return a, fail(stage, err)
			}
			report(stage, protocol.StateDone, cmd)
		}
	}
	return a, nil
}

// checkBranch rejects names git would refuse, before anything is touched.
func checkBranch(ctx context.Context, branch string) error {
	if branch == "" {
		return errors.New("branch required")
	}
	if strings.HasPrefix(branch, "-") {
		return fmt.Errorf("branch %q must not start with -", branch)
	}
	if _, err := git(ctx, "", "check-ref-format", "--branch", branch); err != nil {
		return fmt.Errorf("%q is not a valid branch name", branch)
	}
	return nil
}

func refExists(ctx context.Context, checkout, ref string) bool {
	_, err := git(ctx, checkout, "rev-parse", "--verify", "--quiet", ref)
	return err == nil
}

func branchOrDetached(e Entry) string {
	if e.Branch == "" {
		return "a detached HEAD"
	}
	return "branch " + e.Branch
}

// copyFile copies one entry from the main checkout into the worktree,
// through a temporary file in the target directory renamed into place, so
// the target can only exist complete. Skipped when the target exists, and
// when the source is not in the checkout.
func copyFile(ctx context.Context, checkout, root, rel string, report Reporter) error {
	stage := protocol.StageCopy
	dst := filepath.Join(root, rel)
	if _, err := os.Lstat(dst); err == nil {
		report(stage, protocol.StateSkip, rel+" exists")
		return nil
	}
	src := filepath.Join(checkout, rel)
	fi, err := os.Stat(src)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			report(stage, protocol.StateSkip, rel+" not in "+checkout)
			return nil
		}
		return err
	}
	if !fi.Mode().IsRegular() {
		return fmt.Errorf("%s: not a regular file", src)
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	// A fixed temporary name: a copy that crashed halfway leaves it behind,
	// and the retry overwrites it rather than adding another.
	tmp := filepath.Join(filepath.Dir(dst), ".laatmux-copy-"+filepath.Base(dst))
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, fi.Mode().Perm())
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(tmp)
		return err
	}
	if err := out.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, dst); err != nil {
		os.Remove(tmp)
		return err
	}
	report(stage, protocol.StateDone, rel)
	return nil
}

// markerDir is <git-dir>/laatmux for the worktree: what git reports as the
// worktree's git directory, never derived from the branch name, so the
// markers die with the worktree.
func markerDir(ctx context.Context, root string) (string, error) {
	out, err := git(ctx, root, "rev-parse", "--absolute-git-dir")
	if err != nil {
		return "", err
	}
	dir := filepath.Join(strings.TrimSpace(out), "laatmux")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	return dir, nil
}

// runStreaming runs a command in dir with stdout and stderr merged, each
// line reported as output for the stage. The error carries the last lines
// of output, since the client may have seen them scroll by.
func runStreaming(ctx context.Context, dir string, report Reporter, stage string, env []string, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	cmd.Env = env
	cmd.Stdin = nil
	pr, pw := io.Pipe()
	cmd.Stdout = pw
	cmd.Stderr = pw
	if err := cmd.Start(); err != nil {
		pw.Close()
		return err
	}
	var tail []string
	done := make(chan struct{})
	go func() {
		defer close(done)
		streamLines(pr, func(line string) {
			report(stage, protocol.StateOutput, line)
			tail = append(tail, line)
			if len(tail) > 5 {
				tail = tail[1:]
			}
		})
	}()
	werr := cmd.Wait()
	pw.Close()
	<-done
	if werr != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		msg := werr.Error()
		if len(tail) > 0 {
			msg += ": " + strings.Join(tail, " | ")
		}
		return errors.New(msg)
	}
	return nil
}
