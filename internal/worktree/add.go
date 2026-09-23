package worktree

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math/rand/v2"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"

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
	if err := CheckBranch(ctx, branch); err != nil {
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
			if e.Branch == branch && !e.Prunable && e.Root != checkout && s.Owns(e.Root) {
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
		// A symlink already at <worktrees>/<name> could carry the new
		// worktree outside the directory, where it would never be
		// published or removable; Owns resolves the existing prefix.
		if !s.Owns(a.Root) {
			return a, fail(stage, fmt.Errorf("%s resolves outside the worktrees directory %s", a.Root, s.Dirs.Worktrees))
		}
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
		placed := false
		for _, e := range entries {
			if e.Branch == branch && e.Root != checkout && s.Owns(e.Root) {
				a.Root, placed = e.Root, true
			}
		}
		if !placed {
			return a, fail(stage, fmt.Errorf("git registered no worktree for %s under %s", branch, s.Dirs.Worktrees))
		}
		report(stage, protocol.StateDone, "worktree at "+a.Root)
	}

	// copy and setup read the worktree's own .laatmux.yaml, so a branch
	// carries its own setup.
	setup, err := config.LoadSetup(a.Root)
	if err != nil {
		// The file is read once for both stages; an invalid entry fails
		// the stage it belongs to.
		var fe *config.SetupFieldError
		if errors.As(err, &fe) && fe.Field == "setup" {
			return a, fail(protocol.StageSetup, err)
		}
		return a, fail(protocol.StageCopy, err)
	}

	// The committed steps first, then this host's for the repository,
	// then this host's for every worktree; a glob names what the main
	// checkout has that matches it.
	stage = protocol.StageCopy
	var rules []string
	rules = append(rules, setup.Copy...)
	rules = append(rules, repo.Copy...)
	rules = append(rules, s.Copy...)
	var listed []string
	listedOnce := false // an empty listing is a listing too
	for _, entry := range rules {
		if !config.IsGlob(entry) {
			if err := copyFile(ctx, checkout, a.Root, entry, report); err != nil {
				return a, fail(stage, err)
			}
			continue
		}
		if !listedOnce {
			if listed, err = listFiles(ctx, checkout); err != nil {
				return a, fail(stage, err)
			}
			listedOnce = true
		}
		matched := 0
		for _, rel := range listed {
			if !MatchGlob(entry, rel) {
				continue
			}
			// A glob names whatever git lists: a submodule, a symlink,
			// a directory are not files to copy and are passed over,
			// where a literal entry naming one is an error; a file gone
			// since the listing is passed over too, anything else the
			// lookup says is a failure like a literal copy's.
			fi, err := os.Lstat(filepath.Join(checkout, rel))
			if err != nil {
				if errors.Is(err, os.ErrNotExist) {
					continue
				}
				return a, fail(stage, err)
			}
			if !fi.Mode().IsRegular() {
				continue
			}
			matched++
			if err := copyFile(ctx, checkout, a.Root, rel, report); err != nil {
				return a, fail(stage, err)
			}
		}
		if matched == 0 {
			report(stage, protocol.StateSkip, entry+" matches nothing in "+checkout)
		}
	}

	// The committed commands, then this host's for the repository. Each
	// list numbers its own markers, so a committed list that grows does
	// not move a personal command onto another's marker.
	stage = protocol.StageSetup
	if len(setup.Setup) > 0 || len(repo.Setup) > 0 {
		markers, err := markerDir(ctx, a.Root)
		if err != nil {
			return a, fail(stage, err)
		}
		type step struct{ cmd, marker string }
		var steps []step
		for i, cmd := range setup.Setup {
			steps = append(steps, step{cmd, "setup-" + strconv.Itoa(i) + "-" + hash(cmd)})
		}
		for i, cmd := range repo.Setup {
			steps = append(steps, step{cmd, "setup-repo-" + strconv.Itoa(i) + "-" + hash(cmd)})
		}
		for _, st := range steps {
			cmd := st.cmd
			marker := filepath.Join(markers, st.marker)
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

// CheckBranch rejects names git would refuse, before anything is touched.
// The dashboard runs it on the laptop before sending an add; the daemon
// runs it again on the host.
func CheckBranch(ctx context.Context, branch string) error {
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
// when the source is not in the checkout. Every operation goes through
// os.Root handles on the checkout and the worktree, which resolve the
// relative path under the handle and refuse a symlink that leads out, at
// the moment of the operation rather than in a check before it: a
// symlink to a file elsewhere is not read, since the file was never the
// repository's, and no symlink in the worktree leads a directory or a
// write out of it.
func copyFile(ctx context.Context, checkout, root, rel string, report Reporter) error {
	stage := protocol.StageCopy
	rel = filepath.Clean(rel)
	co, err := os.OpenRoot(checkout)
	if err != nil {
		return err
	}
	defer co.Close()
	wt, err := os.OpenRoot(root)
	if err != nil {
		return err
	}
	defer wt.Close()
	if _, err := wt.Lstat(rel); err == nil {
		report(stage, protocol.StateSkip, rel+" exists")
		return nil
	}
	// Nonblocking, so a source that is a pipe with no writer does not
	// hold the stage and the repository lock; the check below on what
	// was opened rejects it.
	in, err := co.OpenFile(rel, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		switch {
		case errors.Is(err, os.ErrNotExist):
			report(stage, protocol.StateSkip, rel+" not in "+checkout)
			return nil
		case isEscape(err):
			return fmt.Errorf("%s resolves outside the checkout %s", rel, checkout)
		}
		return err
	}
	defer in.Close()
	fi, err := in.Stat()
	if err != nil {
		return err
	}
	if !fi.Mode().IsRegular() {
		return fmt.Errorf("%s: not a regular file", filepath.Join(checkout, rel))
	}
	dir := filepath.Dir(rel)
	if err := wt.MkdirAll(dir, 0o755); err != nil {
		if isEscape(err) {
			return fmt.Errorf("%s: its directory resolves outside the worktree %s", rel, root)
		}
		return err
	}
	// The temporary file is created exclusively with a random suffix, so
	// it can never truncate a file the repository happens to contain. A
	// copy that crashed halfway leaves its temporary behind; the retry
	// removes those first, and only those: names of exactly the form
	// tempName produces, read from the directory literally.
	base := filepath.Base(rel)
	removeStaleTemps(wt, dir, base)
	var out *os.File
	var tmp string
	for i := 0; ; i++ {
		tmp = filepath.Join(dir, tempName(base))
		out, err = wt.OpenFile(tmp, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
		if err == nil {
			break
		}
		if !errors.Is(err, os.ErrExist) || i >= 100 {
			if isEscape(err) {
				return fmt.Errorf("%s: its directory resolves outside the worktree %s", rel, root)
			}
			return err
		}
	}
	fail := func(err error) error {
		out.Close()
		wt.Remove(tmp)
		return err
	}
	if err := out.Chmod(fi.Mode().Perm()); err != nil {
		return fail(err)
	}
	if _, err := io.Copy(out, in); err != nil {
		return fail(err)
	}
	if err := out.Close(); err != nil {
		wt.Remove(tmp)
		return err
	}
	if err := wt.Rename(tmp, rel); err != nil {
		wt.Remove(tmp)
		return err
	}
	report(stage, protocol.StateDone, rel)
	return nil
}

// isEscape reports whether an os.Root operation refused a path for
// leading outside the root.
func isEscape(err error) bool {
	var pe *os.PathError
	return errors.As(err, &pe) && strings.Contains(pe.Err.Error(), "escapes from parent")
}

// tempName is a temporary file name beside base with a random numeric
// suffix, the shape removeStaleTemps recognises.
func tempName(base string) string {
	return ".laatmux-copy-" + base + "." + strconv.FormatUint(uint64(rand.Uint32()), 10)
}

// removeStaleTemps deletes leftovers of crashed copies of base in dir,
// under the worktree root: regular files named
// .laatmux-copy-<base>.<digits>, the shape tempName gives them. Anything
// else, such as a .backup a user kept under a similar name, is not
// laatmux's and stays. The directory is read, not globbed, so its name
// is taken literally.
func removeStaleTemps(wt *os.Root, dir, base string) {
	entries, err := fs.ReadDir(wt.FS(), filepath.ToSlash(dir))
	if err != nil {
		return
	}
	prefix := ".laatmux-copy-" + base + "."
	for _, e := range entries {
		suffix, ok := strings.CutPrefix(e.Name(), prefix)
		if !ok || !e.Type().IsRegular() || suffix == "" || strings.Trim(suffix, "0123456789") != "" {
			continue
		}
		wt.Remove(filepath.Join(dir, e.Name()))
	}
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
		StreamLines(pr, func(line string) {
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

// listFiles is what git knows of the main checkout, for a glob to match
// against: the files it tracks and the untracked files it does not
// ignore, plus the ignored files, which is where a personal env cache
// sits, with ignored directories collapsed to one entry each, so a **
// never walks node_modules and nothing inside an ignored directory is
// matched. Directories are left out; a glob names files.
func listFiles(ctx context.Context, checkout string) ([]string, error) {
	var out []string
	for _, args := range [][]string{
		{"ls-files", "-z", "--cached", "--others", "--exclude-standard"},
		{"ls-files", "-z", "--others", "--ignored", "--exclude-standard", "--directory"},
	} {
		res, err := git(ctx, checkout, args...)
		if err != nil {
			return nil, err
		}
		for _, p := range strings.Split(res, "\x00") {
			if p == "" || strings.HasSuffix(p, "/") {
				continue
			}
			out = append(out, p)
		}
	}
	sort.Strings(out)
	return out, nil
}

// MatchGlob matches a slash-separated path against a copy glob: ** is a
// whole segment standing for zero or more segments, every other segment
// is path.Match syntax and matches one segment. Neither * nor ? crosses
// a slash.
func MatchGlob(pattern, p string) bool {
	return matchSegments(strings.Split(pattern, "/"), strings.Split(p, "/"))
}

func matchSegments(pat, segs []string) bool {
	for len(pat) > 0 {
		if pat[0] == "**" {
			for i := 0; i <= len(segs); i++ {
				if matchSegments(pat[1:], segs[i:]) {
					return true
				}
			}
			return false
		}
		if len(segs) == 0 {
			return false
		}
		if ok, err := path.Match(pat[0], segs[0]); err != nil || !ok {
			return false
		}
		pat, segs = pat[1:], segs[1:]
	}
	return len(segs) == 0
}
