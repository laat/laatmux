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
	"unicode"
	"unicode/utf8"

	"github.com/laat/laatmux/internal/config"
	"github.com/laat/laatmux/internal/protocol"
	"github.com/laat/laatmux/internal/tmux"
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
// the worktree in place for a retry. The daemon runs the three parts
// itself, with its allocate stage between Prepare and Place; Add is the
// composition for a branch decided in advance, with the store's copy
// rules for every worktree as it starts.
func (s *Store) Add(ctx context.Context, repo Repo, branch string, report Reporter) (Added, error) {
	if report == nil {
		report = func(string, string, string) {}
	}
	hostCopy := s.Listed().Copy
	if err := CheckBranch(ctx, branch); err != nil {
		return Added{}, fail(protocol.StageResolve, err)
	}
	p, err := s.Prepare(ctx, repo, report)
	if err != nil {
		return Added{}, err
	}
	root, err := s.Place(ctx, p, repo, branch)
	if err != nil {
		return Added{Checkout: p.Checkout}, fail(protocol.StageResolve, err)
	}
	return s.Materialize(ctx, p.Checkout, repo, branch, root, hostCopy, report)
}

// Prepared is what the resolve, clone and fetch stages leave: the main
// checkout, fresh, whether it was found or made.
type Prepared struct {
	Checkout string
	Found    bool // the checkout was there before
}

// Prepare runs resolve, clone and fetch: the main checkout is found by
// its origin in any of the repos directories or cloned into the first,
// then fetched, so what follows decides against current branches.
// Nothing here depends on the branch.
func (s *Store) Prepare(ctx context.Context, repo Repo, report Reporter) (Prepared, error) {
	if report == nil {
		report = func(string, string, string) {}
	}
	var p Prepared

	// resolve
	stage := protocol.StageResolve
	checkout, found, err := s.Checkout(ctx, repo)
	if err != nil {
		return p, fail(stage, err)
	}
	if !found {
		checkout = s.Dirs.Checkout(repo.Name)
	}
	p.Checkout, p.Found = checkout, found
	report(stage, protocol.StateDone, "checkout "+tmux.Printable(checkout))

	// clone
	stage = protocol.StageClone
	if found {
		report(stage, protocol.StateSkip, "checkout exists")
	} else {
		if _, err := os.Stat(checkout); err == nil {
			url, hasOrigin, _ := s.origin(ctx, checkout)
			switch {
			case hasOrigin:
				return p, fail(stage, fmt.Errorf("%s exists with origin %s, not %s", tmux.Printable(checkout), url, repo.Source))
			default:
				return p, fail(stage, fmt.Errorf("%s exists and is not a checkout of %s", tmux.Printable(checkout), repo.Source))
			}
		}
		if err := os.MkdirAll(s.Dirs.Clones(), 0o755); err != nil {
			return p, fail(stage, tmux.PrintablePath(err))
		}
		report(stage, protocol.StateStart, "git clone "+repo.Source+" "+tmux.Printable(checkout))
		if err := runStreaming(ctx, s.Dirs.Clones(), report, stage, GitEnv(), "git", "clone", "--", repo.Source, checkout); err != nil {
			return p, fail(stage, err)
		}
		report(stage, protocol.StateDone, "cloned")
	}

	// fetch: never skipped; the branch base must be fresh.
	stage = protocol.StageFetch
	report(stage, protocol.StateStart, "git fetch origin")
	if err := runStreaming(ctx, checkout, report, stage, GitEnv(), "git", "fetch", "origin"); err != nil {
		return p, fail(stage, err)
	}
	report(stage, protocol.StateDone, "fetched")
	return p, nil
}

// Place is the root the worktree for branch has, or will have: the one
// git registers for the branch under the worktrees directory when there
// is one, else the label's place. Only a worktree under the worktrees
// directory is the worktree for the branch; one elsewhere fails the
// worktree stage in Materialize.
func (s *Store) Place(ctx context.Context, p Prepared, repo Repo, branch string) (string, error) {
	root := s.Dirs.Worktree(repo.Name, branch)
	if !p.Found {
		return root, nil
	}
	entries, err := ListWorktrees(ctx, p.Checkout)
	if err != nil {
		return "", err
	}
	for _, e := range entries {
		if e.Branch == branch && !e.Prunable && e.Root != p.Checkout && s.Owns(e.Root) {
			root = e.Root
		}
	}
	return root, nil
}

// Materialize runs the worktree, copy and setup stages for branch at
// root in the prepared checkout. hostCopy is the host's copy rules for
// every worktree, of the Listed the add took repo from.
func (s *Store) Materialize(ctx context.Context, checkout string, repo Repo, branch, root string, hostCopy []string, report Reporter) (Added, error) {
	if report == nil {
		report = func(string, string, string) {}
	}
	a := Added{Checkout: checkout, Root: root}

	// worktree
	stage := protocol.StageWorktree
	if _, err := git(ctx, checkout, "remote", "set-head", "origin", "--auto"); err != nil {
		return a, fail(stage, err)
	}
	if _, err := git(ctx, checkout, "worktree", "prune"); err != nil {
		return a, fail(stage, err)
	}
	report(stage, protocol.StateDone, "origin/HEAD refreshed, stale worktrees pruned")
	// The root was placed before the prune; a prunable entry for the
	// branch elsewhere is gone now and the label's place stands.
	// The branch is printed as tmux.Printable shows it: git takes a C1
	// control character and a byte that is not UTF-8 in one.
	b := tmux.Printable(branch)
	switch {
	case refExists(ctx, checkout, "refs/heads/"+branch):
		report(stage, protocol.StateSkip, "branch "+b+" exists, used as is")
	case refExists(ctx, checkout, "refs/remotes/origin/"+branch):
		report(stage, protocol.StateStart, "git branch --track "+b+" "+tmux.Printable("origin/"+branch))
		if _, err := git(ctx, checkout, "branch", "--track", branch, "origin/"+branch); err != nil {
			return a, fail(stage, err)
		}
		report(stage, protocol.StateDone, "branch "+b+" tracks "+tmux.Printable("origin/"+branch))
		setBase(ctx, checkout, branch, report)
	default:
		// --no-track: the new branch has no remote counterpart yet, and an
		// upstream of origin/HEAD would make push refuse and pull merge the
		// default branch.
		report(stage, protocol.StateStart, "git branch --no-track "+b+" origin/HEAD")
		if _, err := git(ctx, checkout, "branch", "--no-track", branch, "origin/HEAD"); err != nil {
			return a, fail(stage, err)
		}
		report(stage, protocol.StateDone, "branch "+b+" from origin/HEAD")
		setBase(ctx, checkout, branch, report)
	}
	entries, err := ListWorktrees(ctx, checkout)
	if err != nil {
		return a, fail(stage, err)
	}
	registered := false
	for _, e := range entries {
		if e.Root == checkout {
			if e.Branch == branch {
				// The default branch of a fresh clone is: what to give
				// instead is said, since the first add of a repository
				// new to the config names it as often as not.
				return a, fail(stage, fmt.Errorf("branch %s is checked out in the main checkout %s; a worktree needs another branch: a new name, or a prompt to propose one", b, tmux.Printable(checkout)))
			}
			continue
		}
		switch {
		case e.Root == a.Root && e.Branch == branch:
			registered = true
		case e.Root == a.Root:
			return a, fail(stage, fmt.Errorf("%s is a worktree on %s, not %s", tmux.Printable(a.Root), branchOrDetached(e), b))
		case e.Branch == branch:
			return a, fail(stage, fmt.Errorf("branch %s is checked out at %s", b, tmux.Printable(e.Root)))
		}
	}
	if registered {
		report(stage, protocol.StateSkip, "worktree registered at "+tmux.Printable(a.Root))
	} else {
		// A symlink already at <worktrees>/<name> could carry the new
		// worktree outside the directory, where it would never be
		// published or removable; Owns resolves the existing prefix.
		if !s.Owns(a.Root) {
			return a, fail(stage, fmt.Errorf("%s resolves outside the worktrees directory %s", tmux.Printable(a.Root), tmux.Printable(s.Dirs.Worktrees)))
		}
		if err := os.MkdirAll(filepath.Dir(a.Root), 0o755); err != nil {
			return a, fail(stage, tmux.PrintablePath(err))
		}
		report(stage, protocol.StateStart, "git worktree add "+tmux.Printable(a.Root)+" "+b)
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
			return a, fail(stage, fmt.Errorf("git registered no worktree for %s under %s", b, tmux.Printable(s.Dirs.Worktrees)))
		}
		report(stage, protocol.StateDone, "worktree at "+tmux.Printable(a.Root))
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
	rules = append(rules, hostCopy...)
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
				return a, fail(stage, tmux.PrintablePath(err))
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
			report(stage, protocol.StateSkip, tmux.Printable(entry)+" matches nothing in "+tmux.Printable(checkout))
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
				return a, fail(stage, tmux.PrintablePath(err))
			}
			report(stage, protocol.StateDone, cmd)
		}
	}
	return a, nil
}

// setBase records a new branch's base for the views' diff stats. A
// failure is reported and does not stop the add: the stats then compare
// with origin/HEAD, main or master.
func setBase(ctx context.Context, checkout, branch string, report Reporter) {
	if err := SetBase(ctx, checkout, branch); err != nil {
		report(protocol.StageWorktree, protocol.StateOutput, "base not recorded: "+err.Error())
	}
}

// CheckBranch rejects names git would refuse, before anything is touched,
// and names CheckWire refuses. The dashboard runs it on the laptop before
// sending an add; the daemon runs it again on the host. git refuses a C0
// control character and DEL; a C1 control character it takes, and a
// branch someone pushed can have one, so such a name is not refused here
// but printed as tmux.Printable shows it wherever a message names it. A
// byte that is not UTF-8 git takes as well, but laatmux cannot carry it,
// and CheckWire refuses it.
func CheckBranch(ctx context.Context, branch string) error {
	if branch == "" {
		return errors.New("branch required")
	}
	if err := CheckWire(branch); err != nil {
		return err
	}
	if strings.HasPrefix(branch, "-") {
		return fmt.Errorf("branch %q must not start with -", branch)
	}
	if _, err := git(ctx, "", "check-ref-format", "--branch", branch); err != nil {
		return fmt.Errorf("%q is not a valid branch name", branch)
	}
	return nil
}

// CheckWire refuses a branch laatmux cannot carry between a client and
// a daemon. Their connection is JSON, which writes U+FFFD for every
// byte that is not part of valid UTF-8, so such a branch would arrive
// as another name: add would make that branch, and rm and run would
// look for one no worktree is on. git takes such a name; on macOS the
// filesystem refuses it, as a ref and as a worktree root. A branch with
// U+FFFD in it is refused too, since a daemon cannot tell it from one
// an older client sent with such a byte. A client runs it before it
// sends a branch, the daemon on every branch it receives.
func CheckWire(branch string) error {
	switch {
	case !utf8.ValidString(branch):
		return fmt.Errorf("branch %q is not valid UTF-8; laatmux cannot carry it between client and daemon", branch)
	case strings.ContainsRune(branch, utf8.RuneError):
		return fmt.Errorf("branch %q has U+FFFD, which a byte that is not UTF-8 becomes between laatmux's client and daemon; laatmux takes neither", branch)
	}
	return nil
}

// BranchIs reports whether a client's branch names got, a branch as git
// has it: the name, or for a branch CheckWire refuses, the name a
// listing shows for it (tmux.Printable), which is what a client has for
// it. That form is the name itself or a quoted one with a \ in it, which
// no branch git takes has, so it names no other branch.
func BranchIs(got, branch string) bool {
	return got == branch || CheckWire(got) != nil && branch == tmux.Printable(got)
}

func refExists(ctx context.Context, checkout, ref string) bool {
	_, err := git(ctx, checkout, "rev-parse", "--verify", "--quiet", ref)
	return err == nil
}

// Branches lists the checkout's local branches and its origin's remote
// branches by name, for a generated name to be allocated against.
func Branches(ctx context.Context, checkout string) (local, remote []string, err error) {
	out, err := git(ctx, checkout, "for-each-ref", "--format=%(refname)", "refs/heads/", "refs/remotes/origin/")
	if err != nil {
		return nil, nil, err
	}
	for _, ref := range strings.Split(out, "\n") {
		switch {
		case strings.HasPrefix(ref, "refs/heads/"):
			local = append(local, strings.TrimPrefix(ref, "refs/heads/"))
		case strings.HasPrefix(ref, "refs/remotes/origin/"):
			name := strings.TrimPrefix(ref, "refs/remotes/origin/")
			if name != "HEAD" {
				remote = append(remote, name)
			}
		}
	}
	return local, remote, nil
}

// RefConflict reports whether a branch named existing rules out one
// named candidate: the same name, or one a directory of the other in
// the ref namespace, since refs/heads/task cannot exist beside
// refs/heads/task/sub.
func RefConflict(existing, candidate string) bool {
	return existing == candidate || strings.HasPrefix(existing, candidate+"/") || strings.HasPrefix(candidate, existing+"/")
}

// Allocate is the first free of <name>, <name>-2, <name>-3 and on, where
// taken says what is not free: the local and remote branches, the
// registered worktrees, and the names other adds have allocated and
// not yet made into branches. A name whose ancestor in the ref
// namespace is a branch, feature/task beside feature, is never free
// however it is numbered; the search is bounded and says so.
func Allocate(name string, taken func(string) bool) (string, error) {
	if !taken(name) {
		return name, nil
	}
	for i := 2; i <= 1000; i++ {
		if c := name + "-" + strconv.Itoa(i); !taken(c) {
			return c, nil
		}
	}
	return "", fmt.Errorf("no free name for %s: every numbered form is taken, or a branch occupies a component of the name", tmux.Printable(name))
}

// ProposeBranch derives a branch name from a prompt: the first words,
// lowercased, runs of anything but letters and digits turned into one
// dash, trimmed, cut at forty characters on a word boundary. It is a
// proposal for the allocate stage to make unique, or for the user to
// replace; "" when the prompt has no letters or digits. The result
// passes CheckBranch: letters, digits and single dashes only, so no
// sequence git refuses can arise. Characters are counted, not bytes,
// so a name is never cut inside one.
func ProposeBranch(prompt string) string {
	const limit = 40
	var name []rune
	dash := false
	for _, r := range strings.ToLower(prompt) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			if dash && len(name) > 0 {
				name = append(name, '-')
			}
			dash = false
			name = append(name, r)
			continue
		}
		dash = true
	}
	if len(name) <= limit {
		return string(name)
	}
	cut := name[:limit]
	for i := len(cut) - 1; i > 0; i-- {
		if cut[i] == '-' {
			cut = cut[:i]
			break
		}
	}
	return strings.TrimRight(string(cut), "-")
}

func branchOrDetached(e Entry) string {
	if e.Branch == "" {
		return "a detached HEAD"
	}
	return "branch " + tmux.Printable(e.Branch)
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
// write out of it. An os error it returns as it is names the checkout,
// the worktree or rel, and is returned with that path as tmux.Printable
// shows it.
func copyFile(ctx context.Context, checkout, root, rel string, report Reporter) (err error) {
	defer func() { err = tmux.PrintablePath(err) }()
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
		report(stage, protocol.StateSkip, tmux.Printable(rel)+" exists")
		return nil
	}
	// Nonblocking, so a source that is a pipe with no writer does not
	// hold the stage and the repository lock; the check below on what
	// was opened rejects it.
	in, err := co.OpenFile(rel, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		switch {
		case errors.Is(err, os.ErrNotExist):
			report(stage, protocol.StateSkip, tmux.Printable(rel)+" not in "+tmux.Printable(checkout))
			return nil
		case isEscape(err):
			return fmt.Errorf("%s resolves outside the checkout %s", tmux.Printable(rel), tmux.Printable(checkout))
		}
		return err
	}
	defer in.Close()
	fi, err := in.Stat()
	if err != nil {
		return err
	}
	if !fi.Mode().IsRegular() {
		return fmt.Errorf("%s: not a regular file", tmux.Printable(filepath.Join(checkout, rel)))
	}
	dir := filepath.Dir(rel)
	if err := wt.MkdirAll(dir, 0o755); err != nil {
		if isEscape(err) {
			return fmt.Errorf("%s: its directory resolves outside the worktree %s", tmux.Printable(rel), tmux.Printable(root))
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
				return fmt.Errorf("%s: its directory resolves outside the worktree %s", tmux.Printable(rel), tmux.Printable(root))
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
	report(stage, protocol.StateDone, tmux.Printable(rel))
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
		return "", tmux.PrintablePath(err)
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
		// A dir that cannot be entered is named in the error.
		return tmux.PrintablePath(err)
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
			// The output lines went on as they are; in the error, one
			// line, each is as tmux.Printable shows it: clone's and
			// fetch's can name the checkout, and a setup command's can
			// be anything.
			for i, line := range tail {
				tail[i] = tmux.Printable(line)
			}
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
