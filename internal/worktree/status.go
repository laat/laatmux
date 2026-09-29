package worktree

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/laat/laatmux/internal/protocol"
)

// The git status of a worktree, as the host's daemon reads it for the
// views: see milestone five's note, Diff stats. Every call runs as `git
// --no-optional-locks`, and the diff against HEAD with
// diff.autoRefreshIndex off, so no call takes the index lock a user's
// git needs or rewrites the index; the diffs take --no-ext-diff and
// --no-textconv, so no diff driver of the user's runs. merge-tree runs
// the repository's merge drivers, as any merge would.

// BaseKey is the branch config key add sets to the base a branch was
// made from, resolved: origin/main, not origin/HEAD.
const BaseKey = "laatmux-base"

// Untracked file limits: at most this many files are read, each up to
// this many bytes; past either the uncommitted count is a lower bound.
const (
	untrackedFiles = 200
	untrackedBytes = 1 << 20
)

// GitTimeout bounds each git call of a refresh; gitWaitDelay how long
// its output is drained after that, since a child that keeps git's
// pipes open, a merge driver's, would otherwise hold the call. Variables
// for tests.
var (
	GitTimeout   = 10 * time.Second
	gitWaitDelay = time.Second
)

// baseTTL is how long a resolved base name is kept: a key set by hand,
// or a new origin/HEAD, shows within it.
const baseTTL = time.Minute

// Paths are what a refresh finds about where a worktree's git lives, for
// the daemon's mtime triggers: its own git dir, the common dir, and the
// refs of the branch and its base.
type Paths struct {
	GitDir    string // .git/worktrees/<name> of the main checkout
	CommonDir string
	Refs      []string // loose ref files of the branch and the base, which may not exist
}

// Pair is what the committed stats depend on: the two commits, and the
// shallow boundary, the mtime of the common dir's shallow file, zero
// when there is none, since deepening a shallow history can find a merge
// base without moving either commit.
type Pair struct {
	Base, Head string
	Shallow    time.Time
}

// Committed is what depends on the commit pair alone.
type Committed struct {
	Diff          [2]int
	Ahead, Behind int
	Conflict      *bool
}

// untrackedKey is what an untracked file's count is kept by.
type untrackedKey struct {
	size  int64
	mtime time.Time
}

type untrackedCount struct {
	lines   int
	partial bool
}

// StatusCache is one worktree's cache across refreshes: the committed
// stats by commit pair, and the line counts of untracked files by path,
// size and mtime. A refresh reads only what changed.
type StatusCache struct {
	// base is the base resolved at baseAt; it is resolved again after
	// baseTTL, or when its commit cannot be read.
	base      string
	baseRef   string // its full ref name, for the mtime trigger
	baseAt    time.Time
	resolved  bool // baseAt is a resolution's, one that found none too
	pair      Pair
	committed Committed
	have      bool
	untracked map[string]untrackedCount
	keys      map[string]untrackedKey
}

// Status reads a worktree's git state. branch is the worktree's branch,
// "" when detached. The head it returns is HEAD's commit when the read
// began, so the caller can drop a result whose HEAD has moved since.
func Status(ctx context.Context, root, branch string, cache *StatusCache) (st protocol.GitStatus, head string, paths Paths, err error) {
	g := func(args ...string) (string, error) { return statusGit(ctx, root, args...) }
	// One call for where git lives, HEAD, and the cached base's commit;
	// the base is resolved again when it is old or cannot be read.
	var baseOID string
	base := cache.base
	fresh := cache.resolved && time.Since(cache.baseAt) <= baseTTL
	if !fresh {
		base = ""
	}
	for {
		args := []string{"rev-parse", "--absolute-git-dir", "--git-common-dir", "HEAD"}
		if base != "" {
			args = append(args, base+"^{commit}")
		}
		out, err := g(args...)
		f := strings.Split(strings.TrimSpace(out), "\n")
		if err != nil && base != "" && ctx.Err() == nil && !errors.Is(err, context.DeadlineExceeded) {
			// The base is gone: resolved again.
			base, fresh = "", false
			continue
		}
		if err != nil {
			return st, "", paths, err
		}
		if len(f) != len(args)-1 {
			return st, "", paths, errors.New("git rev-parse: unexpected output")
		}
		paths.GitDir, paths.CommonDir, head = f[0], f[1], f[2]
		if base != "" {
			baseOID = f[3]
		}
		break
	}
	if !filepath.IsAbs(paths.CommonDir) {
		paths.CommonDir = filepath.Join(root, paths.CommonDir)
	}
	if !fresh {
		// A repository with no base at all is not asked again for the
		// minute either.
		var ref string
		base, ref, baseOID, err = resolveBase(ctx, root, branch)
		if err != nil {
			return st, head, paths, err
		}
		cache.base, cache.baseRef, cache.baseAt, cache.resolved = base, ref, time.Now(), true
	}
	st.Base = base
	if branch != "" {
		paths.Refs = append(paths.Refs, filepath.Join(paths.CommonDir, "refs", "heads", branch))
	}
	if cache.baseRef != "" {
		paths.Refs = append(paths.Refs, filepath.Join(paths.CommonDir, filepath.FromSlash(cache.baseRef)))
	}
	paths.Refs = append(paths.Refs, filepath.Join(paths.CommonDir, "packed-refs"), filepath.Join(paths.CommonDir, "shallow"))

	// The committed side, on anything but the base branch itself.
	if baseOID != "" && !onBase(base, branch) {
		pair := Pair{Base: baseOID, Head: head}
		if fi, err := os.Stat(filepath.Join(paths.CommonDir, "shallow")); err == nil {
			pair.Shallow = fi.ModTime()
		}
		if !cache.have || cache.pair != pair {
			c, err := readCommitted(ctx, root, pair)
			if err != nil {
				return st, head, paths, err
			}
			cache.pair, cache.committed, cache.have = pair, c, true
		}
		st.Committed, st.Ahead, st.Behind, st.Conflict = cache.committed.Diff, cache.committed.Ahead, cache.committed.Behind, cache.committed.Conflict
	}

	// The uncommitted side, every refresh: git status for dirty, a
	// staged change the working tree undoes included, and for the
	// untracked files that are not ignored; the diff against HEAD for
	// the counts.
	status, err := g("status", "--porcelain=v2", "-z", "--untracked-files=all")
	if err != nil {
		return st, head, paths, err
	}
	var untracked []string
	entries := strings.Split(status, "\x00")
	for i := 0; i < len(entries); i++ {
		e := entries[i]
		switch {
		case e == "" || strings.HasPrefix(e, "# "):
		case strings.HasPrefix(e, "? "):
			untracked = append(untracked, e[2:])
			st.Dirty = true
		case strings.HasPrefix(e, "2 "):
			// A rename or copy: its original path is the next entry.
			st.Dirty = true
			i++
		default:
			st.Dirty = true
		}
	}
	diff, err := g("-c", "diff.autoRefreshIndex=false", "diff", "--numstat", "--no-ext-diff", "--no-textconv", "HEAD")
	if err != nil {
		return st, head, paths, err
	}
	st.Uncommitted = numstat(diff)
	lines, partial := cache.countUntracked(root, untracked)
	st.Uncommitted[0] += lines
	st.UncommittedPartial = partial
	for _, d := range []string{"rebase-merge", "rebase-apply"} {
		if fi, err := os.Stat(filepath.Join(paths.GitDir, d)); err == nil && fi.IsDir() {
			st.Rebasing = true
		}
	}
	return st, head, paths, nil
}

// Head is HEAD's commit, for the check that a refresh's HEAD has not
// moved.
func Head(ctx context.Context, root string) (string, error) {
	out, err := statusGit(ctx, root, "rev-parse", "HEAD")
	return strings.TrimSpace(out), err
}

// resolveBase is the base a branch is compared with, its full ref name
// and its commit: the first that exists of the branch's laatmux-base key,
// origin/HEAD's branch, main, master. "" when none does. A call that
// times out is an error, not a missing ref, so a slow repository keeps
// its last stats rather than switching base.
func resolveBase(ctx context.Context, root, branch string) (name, ref, oid string, err error) {
	failed := func(err error) bool {
		return errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled)
	}
	var candidates []string
	if branch != "" {
		out, err := statusGit(ctx, root, "config", "--get", "branch."+branch+"."+BaseKey)
		if failed(err) {
			return "", "", "", err
		}
		if b := strings.TrimSpace(out); err == nil && b != "" {
			candidates = append(candidates, b)
		}
	}
	out, err := statusGit(ctx, root, "rev-parse", "--abbrev-ref", "origin/HEAD")
	if failed(err) {
		return "", "", "", err
	}
	if b := strings.TrimSpace(out); err == nil && b != "" && b != "origin/HEAD" {
		candidates = append(candidates, b)
	}
	candidates = append(candidates, "main", "master")
	for _, c := range candidates {
		out, err := statusGit(ctx, root, "rev-parse", "--verify", "--quiet", "--symbolic-full-name", c)
		if failed(err) {
			return "", "", "", err
		}
		full := strings.TrimSpace(out)
		if err != nil || full == "" {
			continue
		}
		out, err = statusGit(ctx, root, "rev-parse", "--verify", "--quiet", c+"^{commit}")
		if failed(err) {
			return "", "", "", err
		}
		if oid := strings.TrimSpace(out); err == nil && oid != "" {
			return c, full, oid, nil
		}
	}
	return "", "", "", nil
}

// onBase reports whether the branch is its own base: main against main,
// or against origin/main.
func onBase(base, branch string) bool {
	if branch == "" {
		return false
	}
	return base == branch || strings.HasPrefix(base, "origin/") && strings.TrimPrefix(base, "origin/") == branch
}

// readCommitted is what depends on the commit pair: the branch's diff
// against its merge base with base, ahead and behind, and whether a
// merge would conflict. It runs once per pair: a merge-tree without
// --quiet writes objects.
// noQuietMerge is that this machine's git has no merge-tree --quiet.
var noQuietMerge atomic.Bool

func readCommitted(ctx context.Context, root string, pair Pair) (Committed, error) {
	var c Committed
	// The commits, not the names: a fetch or a commit between the calls
	// cannot mix two pairs under one key.
	span := pair.Base + "..." + pair.Head
	// A branch with no merge base with its base, an orphan or a
	// shallow history, has no diff against it and no merge to try:
	// ahead and behind alone.
	_, err := statusGit(ctx, root, "merge-base", pair.Base, pair.Head)
	var ee *exec.ExitError
	switch {
	case err == nil:
	case errors.As(err, &ee) && ee.ExitCode() == 1:
		counts, err := statusGit(ctx, root, "rev-list", "--left-right", "--count", span)
		if err != nil {
			return c, err
		}
		c.Behind, c.Ahead = leftRight(counts)
		return c, nil
	default:
		return c, err
	}
	diff, err := statusGit(ctx, root, "diff", "--numstat", "--no-ext-diff", "--no-textconv", span)
	if err != nil {
		return c, err
	}
	c.Diff = numstat(diff)
	counts, err := statusGit(ctx, root, "rev-list", "--left-right", "--count", span)
	if err != nil {
		return c, err
	}
	c.Behind, c.Ahead = leftRight(counts)
	// --quiet stops at the first conflict and writes no objects; a git
	// without it, before 2.50, exits 129 and gets the plain call, which
	// writes the merge's objects once per pair. The 129 is remembered.
	if !noQuietMerge.Load() {
		_, err = statusGit(ctx, root, "merge-tree", "--write-tree", "--quiet", pair.Base, pair.Head)
		if errors.As(err, &ee) && ee.ExitCode() == 129 {
			noQuietMerge.Store(true)
		}
	}
	if noQuietMerge.Load() {
		_, err = statusGit(ctx, root, "merge-tree", "--write-tree", pair.Base, pair.Head)
	}
	switch {
	case err == nil:
		no := false
		c.Conflict = &no
	case errors.As(err, &ee) && ee.ExitCode() == 1:
		yes := true
		c.Conflict = &yes
	case errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled):
		return c, err
	default:
		// git older than 2.38 has no --write-tree: the field is left out.
	}
	return c, nil
}

// leftRight reads rev-list --left-right --count: the base's side, then
// HEAD's.
func leftRight(out string) (behind, ahead int) {
	if f := strings.Fields(out); len(f) == 2 {
		behind, _ = strconv.Atoi(f[0])
		ahead, _ = strconv.Atoi(f[1])
	}
	return behind, ahead
}

// numstat sums `git diff --numstat` output; a binary file counts 0.
func numstat(out string) [2]int {
	var n [2]int
	for _, line := range strings.Split(out, "\n") {
		f := strings.SplitN(line, "\t", 3)
		if len(f) < 3 {
			continue
		}
		a, errA := strconv.Atoi(f[0])
		d, errD := strconv.Atoi(f[1])
		if errA != nil || errD != nil {
			continue
		}
		n[0] += a
		n[1] += d
	}
	return n
}

// countUntracked counts the lines of the untracked files git status
// listed, reading only those whose size or mtime changed since the last
// refresh; past the limits the count is a lower bound.
func (c *StatusCache) countUntracked(root string, listed []string) (lines int, partial bool) {
	if c.untracked == nil {
		c.untracked, c.keys = map[string]untrackedCount{}, map[string]untrackedKey{}
	}
	seen := map[string]bool{}
	n := 0
	for _, p := range listed {
		if n == untrackedFiles {
			partial = true
			break
		}
		n++
		seen[p] = true
		fi, err := os.Lstat(filepath.Join(root, p))
		if err != nil || !fi.Mode().IsRegular() {
			continue
		}
		key := untrackedKey{size: fi.Size(), mtime: fi.ModTime()}
		if k, ok := c.keys[p]; !ok || k != key {
			cnt, ok := countFile(filepath.Join(root, p))
			if !ok {
				// Unreadable now: not kept, so a read that works later
				// counts it, and the count is a lower bound meanwhile.
				delete(c.untracked, p)
				delete(c.keys, p)
				partial = true
				continue
			}
			c.untracked[p] = cnt
			c.keys[p] = key
		}
		cnt := c.untracked[p]
		lines += cnt.lines
		partial = partial || cnt.partial
	}
	for p := range c.untracked {
		if !seen[p] {
			delete(c.untracked, p)
			delete(c.keys, p)
		}
	}
	return lines, partial
}

// countFile is a file's line count, up to untrackedBytes of it; a binary
// file, one with a NUL in its first 8000 bytes as git judges it, counts 0.
func countFile(p string) (untrackedCount, bool) {
	f, err := os.Open(p)
	if err != nil {
		return untrackedCount{}, false
	}
	defer f.Close()
	buf, err := io.ReadAll(io.LimitReader(f, untrackedBytes+1))
	if err != nil {
		return untrackedCount{}, false
	}
	partial := len(buf) > untrackedBytes
	if partial {
		buf = buf[:untrackedBytes]
	}
	if bytes.IndexByte(buf[:min(len(buf), 8000)], 0) >= 0 {
		return untrackedCount{}, true
	}
	n := bytes.Count(buf, []byte{'\n'})
	if len(buf) > 0 && buf[len(buf)-1] != '\n' && !partial {
		n++
	}
	return untrackedCount{lines: n, partial: partial}, true
}

// statusGit runs one git call of a refresh, with the timeout and without
// the optional locks.
func statusGit(ctx context.Context, dir string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, GitTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", append([]string{"--no-optional-locks"}, args...)...)
	cmd.Dir = dir
	// A partial clone does not fetch the blobs a merge-tree or a diff
	// lacks: a refresh never goes to the network. GIT_NO_LAZY_FETCH is
	// git 2.45's; for an older git no transport is allowed, so a lazy
	// fetch fails rather than connects. The conflict is then left out
	// for the pair.
	cmd.Env = append(gitEnv(), "GIT_NO_LAZY_FETCH=1", "GIT_ALLOW_PROTOCOL=none")
	// Its own process group, killed whole at the timeout: a merge
	// driver or a hook git started goes with it.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = gitWaitDelay
	var out, errb strings.Builder
	cmd.Stdout = &out
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return out.String(), ctx.Err()
		}
		return out.String(), err
	}
	return out.String(), nil
}

// SetBase records the base a new branch was made from, resolved, unless
// the branch already has one: a later change of the default branch does
// not move it.
func SetBase(ctx context.Context, checkout, branch string) error {
	if out, err := git(ctx, checkout, "config", "--get", "branch."+branch+"."+BaseKey); err == nil && strings.TrimSpace(out) != "" {
		return nil
	}
	out, err := git(ctx, checkout, "rev-parse", "--abbrev-ref", "origin/HEAD")
	if err != nil {
		return err
	}
	base := strings.TrimSpace(out)
	if base == "" || base == "origin/HEAD" {
		return nil
	}
	_, err = git(ctx, checkout, "config", "branch."+branch+"."+BaseKey, base)
	return err
}
