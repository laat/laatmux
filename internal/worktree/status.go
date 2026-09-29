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
	"time"

	"github.com/laat/laatmux/internal/protocol"
)

// The git status of a worktree, as the host's daemon reads it for the
// views: see milestone five's note, Diff stats. Every call runs as `git
// --no-optional-locks`, so it never takes the index lock a user's git
// needs, and the diffs take --no-ext-diff and --no-textconv, so no diff
// driver of the user's runs.

// BaseKey is the branch config key add sets to the base a branch was
// made from, resolved: origin/main, not origin/HEAD.
const BaseKey = "laatmux-base"

// Untracked file limits: at most this many files are read, each up to
// this many bytes; past either the uncommitted count is a lower bound.
const (
	untrackedFiles = 200
	untrackedBytes = 1 << 20
)

// GitTimeout bounds each git call of a refresh.
const GitTimeout = 10 * time.Second

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

// Pair is the commits the committed stats depend on.
type Pair struct{ Base, Head string }

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
	baseAt    time.Time
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
	if base == "" || time.Since(cache.baseAt) > baseTTL {
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
			base = ""
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
	if base == "" {
		base, baseOID = resolveBase(ctx, root, branch)
		cache.base, cache.baseAt = base, time.Now()
	}
	st.Base = base
	if branch != "" {
		paths.Refs = append(paths.Refs, filepath.Join(paths.CommonDir, "refs", "heads", branch))
	}
	if base != "" {
		paths.Refs = append(paths.Refs, filepath.Join(paths.CommonDir, refPath(base)))
	}
	paths.Refs = append(paths.Refs, filepath.Join(paths.CommonDir, "packed-refs"))

	// The committed side, on anything but the base branch itself.
	if baseOID != "" && !onBase(base, branch) {
		pair := Pair{Base: baseOID, Head: head}
		if !cache.have || cache.pair != pair {
			c, err := readCommitted(ctx, root, base)
			if err != nil {
				return st, head, paths, err
			}
			cache.pair, cache.committed, cache.have = pair, c, true
		}
		st.Committed, st.Ahead, st.Behind, st.Conflict = cache.committed.Diff, cache.committed.Ahead, cache.committed.Behind, cache.committed.Conflict
	}

	// The uncommitted side, every refresh. Dirty is any path in the
	// diff against HEAD, a mode change or a binary file too, or any
	// untracked file: what `git status` would list, without its call.
	diff, err := g("diff", "--numstat", "--no-ext-diff", "--no-textconv", "HEAD")
	if err != nil {
		return st, head, paths, err
	}
	st.Uncommitted = numstat(diff)
	st.Dirty = strings.TrimSpace(diff) != ""
	others, err := g("ls-files", "--others", "--exclude-standard", "-z")
	if err != nil {
		return st, head, paths, err
	}
	lines, partial := cache.countUntracked(root, others)
	st.Uncommitted[0] += lines
	st.UncommittedPartial = partial
	if others != "" {
		st.Dirty = true
	}
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

// resolveBase is the base a branch is compared with, and its commit: the
// first that exists of the branch's laatmux-base key, origin/HEAD's
// branch, main, master. "" when none does.
func resolveBase(ctx context.Context, root, branch string) (name, oid string) {
	var candidates []string
	if branch != "" {
		if out, err := statusGit(ctx, root, "config", "--get", "branch."+branch+"."+BaseKey); err == nil {
			if b := strings.TrimSpace(out); b != "" {
				candidates = append(candidates, b)
			}
		}
	}
	if out, err := statusGit(ctx, root, "rev-parse", "--abbrev-ref", "origin/HEAD"); err == nil {
		if b := strings.TrimSpace(out); b != "" && b != "origin/HEAD" {
			candidates = append(candidates, b)
		}
	}
	candidates = append(candidates, "main", "master")
	for _, c := range candidates {
		out, err := statusGit(ctx, root, "rev-parse", "--verify", "--quiet", c+"^{commit}")
		if err == nil {
			if oid := strings.TrimSpace(out); oid != "" {
				return c, oid
			}
		}
	}
	return "", ""
}

// onBase reports whether the branch is its own base: main against main,
// or against origin/main.
func onBase(base, branch string) bool {
	if branch == "" {
		return false
	}
	return base == branch || strings.HasPrefix(base, "origin/") && strings.TrimPrefix(base, "origin/") == branch
}

// refPath is the loose ref file of a branch name as rev-parse takes it:
// origin/main is refs/remotes/origin/main, main refs/heads/main.
func refPath(name string) string {
	if strings.HasPrefix(name, "refs/") {
		return name
	}
	if strings.Contains(name, "/") {
		return filepath.Join("refs", "remotes", name)
	}
	return filepath.Join("refs", "heads", name)
}

// readCommitted is what depends on the commit pair: the branch's diff
// against its merge base with base, ahead and behind, and whether a
// merge would conflict. merge-tree --write-tree writes objects, which is
// why this runs once per pair.
func readCommitted(ctx context.Context, root, base string) (Committed, error) {
	var c Committed
	diff, err := statusGit(ctx, root, "diff", "--numstat", "--no-ext-diff", "--no-textconv", base+"...HEAD")
	if err != nil {
		return c, err
	}
	c.Diff = numstat(diff)
	counts, err := statusGit(ctx, root, "rev-list", "--left-right", "--count", base+"...HEAD")
	if err != nil {
		return c, err
	}
	if f := strings.Fields(counts); len(f) == 2 {
		c.Behind, _ = strconv.Atoi(f[0])
		c.Ahead, _ = strconv.Atoi(f[1])
	}
	_, err = statusGit(ctx, root, "merge-tree", "--write-tree", base, "HEAD")
	var ee *exec.ExitError
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

// countUntracked counts the lines of the untracked files ls-files -z
// listed, reading only those whose size or mtime changed since the last
// refresh; past the limits the count is a lower bound.
func (c *StatusCache) countUntracked(root, listed string) (lines int, partial bool) {
	if c.untracked == nil {
		c.untracked, c.keys = map[string]untrackedCount{}, map[string]untrackedKey{}
	}
	seen := map[string]bool{}
	n := 0
	for _, p := range strings.Split(listed, "\x00") {
		if p == "" {
			continue
		}
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
			c.untracked[p] = countFile(filepath.Join(root, p))
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
func countFile(p string) untrackedCount {
	f, err := os.Open(p)
	if err != nil {
		return untrackedCount{}
	}
	defer f.Close()
	buf, err := io.ReadAll(io.LimitReader(f, untrackedBytes+1))
	if err != nil {
		return untrackedCount{}
	}
	partial := len(buf) > untrackedBytes
	if partial {
		buf = buf[:untrackedBytes]
	}
	if bytes.IndexByte(buf[:min(len(buf), 8000)], 0) >= 0 {
		return untrackedCount{}
	}
	n := bytes.Count(buf, []byte{'\n'})
	if len(buf) > 0 && buf[len(buf)-1] != '\n' && !partial {
		n++
	}
	return untrackedCount{lines: n, partial: partial}
}

// statusGit runs one git call of a refresh, with the timeout and without
// the optional locks.
func statusGit(ctx context.Context, dir string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, GitTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", append([]string{"--no-optional-locks"}, args...)...)
	cmd.Dir = dir
	cmd.Env = gitEnv()
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
