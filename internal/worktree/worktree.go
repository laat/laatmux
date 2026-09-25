// Package worktree is the git side of a host's workspaces: the checkouts
// under the host's repos directory, the worktrees under its worktrees
// directory, and the stages of add that touch git and the filesystem.
//
// Git is the source of truth. A checkout is found under <repos> by its
// origin, never by its directory name; a worktree is found by asking the
// checkout's `git worktree list`, never by computing a path. Labels place
// new things only, so a renamed repository keeps its existing paths.
package worktree

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/laat/laatmux/internal/config"
)

// Repo is a known repository: its source, the identity, and its label,
// which places new clones and worktrees; Copy and Setup are the personal
// steps for its worktrees, run after the committed ones and before the
// store's own Copy. They come from this host's config, or from the add's
// repository entry for a repository the config does not list.
type Repo struct {
	Source string
	Name   string
	Copy   []string
	Setup  []string
}

// Store is one host's checkouts and worktrees. Copy is the host's own
// copy rules for every worktree, from its config, applied after a
// repository's own.
type Store struct {
	Dirs  config.Dirs // expanded for this host
	Repos []Repo
	Copy  []string

	mu      sync.Mutex
	origins map[string]originEntry // by checkout directory
}

type originEntry struct {
	mtime time.Time
	size  int64
	url   string
}

// New makes a store for the host's directories and known repositories.
func New(dirs config.Dirs, repos []config.Repo) *Store {
	s := &Store{Dirs: dirs, origins: map[string]originEntry{}}
	for _, r := range repos {
		s.Repos = append(s.Repos, Repo{Source: r.Source, Name: r.Name, Copy: r.Copy, Setup: r.Setup})
	}
	return s
}

// Repo finds a repository this host's config lists by name, else by
// source, in any form config.SameSource takes as one: the same order as
// config.Config.Repo, since a bare local source can equal another
// entry's label.
func (s *Store) Repo(nameOrSource string) (Repo, bool) {
	for _, r := range s.Repos {
		if r.Name == nameOrSource {
			return r, true
		}
	}
	return s.BySource(nameOrSource)
}

// BySource finds a repository this host's config lists by its source,
// in any form config.SameSource takes as one.
func (s *Store) BySource(source string) (Repo, bool) {
	for _, r := range s.Repos {
		if r.Source == source {
			return r, true
		}
	}
	for _, r := range s.Repos {
		if config.SameSource(r.Source, source) {
			return r, true
		}
	}
	return Repo{}, false
}

// Known finds a repository this host has, as List labels it: by label,
// the config's name or the directory of a checkout the config does not
// list, else by source, the config's or a checkout's origin. rm and run
// name a repository by what a listing said, which covers checkouts the
// config does not list. A label two repositories answer to is an error
// rather than a guess: the source tells them apart.
func (s *Store) Known(ctx context.Context, nameOrSource string) (Repo, bool, error) {
	cos, err := s.scan(ctx)
	if err != nil {
		return Repo{}, false, err
	}
	var named []Repo
	for _, r := range s.Repos {
		if r.Name == nameOrSource {
			named = append(named, r)
		}
	}
	for _, co := range cos {
		if filepath.Base(co.dir) != nameOrSource {
			continue
		}
		if _, listed := s.BySource(co.origin); !listed {
			named = append(named, s.label(co))
		}
	}
	for i := 1; i < len(named); i++ {
		if !config.SameSource(named[i].Source, named[0].Source) {
			return Repo{}, false, fmt.Errorf("%q names both %s and %s on this host; name the repository by its source", nameOrSource, named[0].Source, named[i].Source)
		}
	}
	if len(named) > 0 {
		return named[0], true, nil
	}
	if r, ok := s.BySource(nameOrSource); ok {
		return r, true, nil
	}
	for _, co := range cos {
		if config.SameSource(co.origin, nameOrSource) {
			return s.label(co), true, nil
		}
	}
	return Repo{}, false, nil
}

// Checkout finds the main checkout of repo under the repos directory: the
// direct child whose remote.origin.url is the source, in any form
// config.SameSource takes as one, so each host fetches over the
// transport its checkout was cloned with. Reads of origin are cached by
// the mtime and size of .git/config, so an idle poll spawns no git
// processes. Not found is ("", false, nil).
func (s *Store) Checkout(ctx context.Context, repo Repo) (string, bool, error) {
	checkouts, err := s.Checkouts(ctx)
	if err != nil {
		return "", false, err
	}
	dir, ok := checkouts[config.SourceKey(repo.Source)]
	return dir, ok, nil
}

// Checkouts scans the repos directory once and maps each origin found,
// by config.SourceKey, to its checkout, the first in directory order
// when two share an origin. One scan serves every repository in a poll.
func (s *Store) Checkouts(ctx context.Context) (map[string]string, error) {
	cos, err := s.scan(ctx)
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, co := range cos {
		key := config.SourceKey(co.origin)
		if _, dup := out[key]; !dup {
			out[key] = co.dir
		}
	}
	return out, nil
}

// checkout is a main checkout under the repos directory and its origin.
type checkout struct{ dir, origin string }

// scan is every main checkout with an origin directly under the repos
// directory, in directory order.
func (s *Store) scan(ctx context.Context) ([]checkout, error) {
	entries, err := os.ReadDir(s.Dirs.Repos)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var out []checkout
	for _, e := range entries {
		dir := filepath.Join(s.Dirs.Repos, e.Name())
		url, ok, err := s.origin(ctx, dir)
		if err != nil {
			return nil, err
		}
		if ok {
			out = append(out, checkout{dir, url})
		}
	}
	return out, nil
}

// label is the repository a checkout holds: the config's entry for its
// origin, else the origin under the checkout's directory name.
func (s *Store) label(co checkout) Repo {
	if r, ok := s.BySource(co.origin); ok {
		return r
	}
	return Repo{Source: co.origin, Name: filepath.Base(co.dir)}
}

// linked reports whether git must be asked for a main checkout's
// worktrees: whether one of them may be under the worktrees directory.
// git keeps each linked worktree's administrative directory under
// .git/worktrees, prunable ones included, with a gitdir file naming the
// worktree's .git, so a checkout with none, or with all of them
// elsewhere, is not asked. That keeps a poll over a repos directory of
// many checkouts from running git for each. What cannot be read is
// asked.
func (s *Store) linked(dir string) bool {
	admin := filepath.Join(dir, ".git", "worktrees")
	entries, err := os.ReadDir(admin)
	if err != nil {
		return !errors.Is(err, os.ErrNotExist) && !errors.Is(err, syscall.ENOTDIR)
	}
	for _, e := range entries {
		b, err := os.ReadFile(filepath.Join(admin, e.Name(), "gitdir"))
		if err != nil {
			return true
		}
		gitdir := strings.TrimSpace(string(b))
		// A relative one, from worktree.useRelativePaths, is relative to
		// the entry's real directory, which a symlink on the way to the
		// checkout can make another place than it looks from here: git
		// is asked.
		if !filepath.IsAbs(gitdir) || s.Owns(filepath.Dir(gitdir)) {
			return true
		}
	}
	return false
}

// origin returns dir's remote.origin.url when dir is a main checkout (has
// a .git directory with a config file), cached.
func (s *Store) origin(ctx context.Context, dir string) (string, bool, error) {
	fi, err := os.Stat(filepath.Join(dir, ".git", "config"))
	if err != nil {
		// Only a missing file proves this is not a main checkout. A
		// permission or I/O failure on one that exists is an error: taken
		// as absence, polling would drop its records and rm would take
		// the worktree as already gone.
		if errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ENOTDIR) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("%s: %w", dir, err)
	}
	s.mu.Lock()
	c, cached := s.origins[dir]
	s.mu.Unlock()
	if cached && c.mtime.Equal(fi.ModTime()) && c.size == fi.Size() {
		return c.url, c.url != "", nil
	}
	out, err := git(ctx, dir, "config", "--get", "remote.origin.url")
	url := strings.TrimSpace(out)
	if err != nil {
		// Exit 1 from --get means the key is unset: a checkout with no
		// origin. Anything else is a real failure, reported once.
		var ee *exec.ExitError
		if !errors.As(err, &ee) || ee.ExitCode() != 1 {
			return "", false, fmt.Errorf("%s: %w", dir, err)
		}
		url = ""
	}
	s.mu.Lock()
	s.origins[dir] = originEntry{mtime: fi.ModTime(), size: fi.Size(), url: url}
	s.mu.Unlock()
	return url, url != "", nil
}

// Entry is one line group of `git worktree list --porcelain`.
type Entry struct {
	Root     string
	Branch   string // "" when detached
	Detached bool
	Prunable bool
	Bare     bool
}

// ListWorktrees asks a checkout for its worktrees, the main one first.
// The output is NUL-terminated (-z), since a path may contain a newline
// and porcelain prints paths verbatim.
func ListWorktrees(ctx context.Context, checkout string) ([]Entry, error) {
	out, err := git(ctx, checkout, "worktree", "list", "--porcelain", "-z")
	if err != nil {
		return nil, err
	}
	return parseWorktrees(out), nil
}

// parseWorktrees reads `git worktree list --porcelain -z`: each attribute
// line is NUL-terminated and an empty one ends a record.
func parseWorktrees(out string) []Entry {
	var entries []Entry
	var cur *Entry
	for _, line := range strings.Split(out, "\x00") {
		if line == "" {
			if cur != nil {
				entries = append(entries, *cur)
				cur = nil
			}
			continue
		}
		key, val, _ := strings.Cut(line, " ")
		switch key {
		case "worktree":
			if cur != nil {
				entries = append(entries, *cur)
			}
			cur = &Entry{Root: val}
		case "branch":
			if cur != nil {
				cur.Branch = strings.TrimPrefix(val, "refs/heads/")
			}
		case "detached":
			if cur != nil {
				cur.Detached = true
			}
		case "prunable":
			if cur != nil {
				cur.Prunable = true
			}
		case "bare":
			if cur != nil {
				cur.Bare = true
			}
		}
	}
	if cur != nil {
		entries = append(entries, *cur)
	}
	return entries
}

// Record is one worktree of a known repository under the worktrees
// directory, as the daemon publishes it.
type Record struct {
	Repo   string // label
	Source string
	Branch string // "" when detached
	Root   string
}

// List returns every worktree that lives under the worktrees directory
// of every main checkout under the repos directory, whether or not the
// config lists its repository: a checkout the config lists is labelled
// with the config's name and source, any other with its directory name
// and origin. Prunable entries, whose directory is gone, are left out,
// as is the main checkout, which is not a worktree even when the repos
// directory sits under the worktrees one. The repos directory is
// scanned once; one checkout failing to list does not hide the others:
// its error is returned alongside what was listed.
func (s *Store) List(ctx context.Context) ([]Record, error) {
	cos, err := s.scan(ctx)
	if err != nil {
		return nil, err
	}
	var records []Record
	var errs []error
	seen := map[string]bool{}
	for _, co := range cos {
		if !s.linked(co.dir) {
			continue
		}
		entries, err := ListWorktrees(ctx, co.dir)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", co.dir, err))
			continue
		}
		r := s.label(co)
		for _, e := range entries {
			// A root two checkouts register, one after the other's
			// directory was deleted by hand, is listed once, for the
			// checkout the worktree points back to, as Find finds it.
			if e.Prunable || e.Bare || e.Root == co.dir || seen[e.Root] || !s.Owns(e.Root) {
				continue
			}
			// A root whose owner cannot be told, its .git unreadable or
			// not a worktree's, is listed for the first checkout that
			// registers it: the listing fails as a whole on an error,
			// which would hold every other worktree's change, and a
			// wrong clone here is only a label. Find and ByBranch, which
			// rm removes through, refuse it instead.
			if mine, err := pointsBack(e.Root, co.dir); err == nil && !mine {
				continue
			}
			seen[e.Root] = true
			records = append(records, Record{Repo: r.Name, Source: r.Source, Branch: e.Branch, Root: e.Root})
		}
	}
	sort.Slice(records, func(i, j int) bool { return records[i].Root < records[j].Root })
	return records, errors.Join(errs...)
}

// Owns reports whether root is inside the worktrees directory: the only
// worktrees the daemon publishes, adopts for a branch, or removes. Both
// sides are compared with symlinks resolved, as git registers real paths,
// so a root reached through a link that leaves the directory is judged by
// where it lands. The check is by path component, not by string prefix,
// so ".." in a root a client sends counts too, and a relative root is
// never owned.
func (s *Store) Owns(root string) bool {
	if !filepath.IsAbs(root) {
		return false
	}
	root, ok := resolveExisting(filepath.Clean(root))
	if !ok {
		return false
	}
	dir, ok := resolveExisting(filepath.Clean(s.Dirs.Worktrees))
	if !ok {
		return false
	}
	rel, err := filepath.Rel(dir, root)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, "../") {
		return false
	}
	return true
}

// resolveExisting resolves symlinks in the longest existing prefix of p
// and keeps the rest as given, so a path whose worktree directory is
// already deleted still resolves through the links above it. It fails
// closed: a component is peeled only when Lstat proves it absent, and a
// prefix that exists but cannot be resolved, for a permission error, a
// dangling link or a loop, is reported as unresolvable rather than taken
// lexically, since Owns gates removal.
func resolveExisting(p string) (string, bool) {
	rest := ""
	for cur := p; ; {
		if _, err := os.Lstat(cur); err != nil {
			if !errors.Is(err, os.ErrNotExist) && !errors.Is(err, syscall.ENOTDIR) {
				return "", false
			}
			parent := filepath.Dir(cur)
			if parent == cur {
				return "", false
			}
			rest = filepath.Join(filepath.Base(cur), rest)
			cur = parent
			continue
		}
		real, err := filepath.EvalSymlinks(cur)
		if err != nil {
			return "", false
		}
		return filepath.Join(real, rest), true
	}
}

// pointsBack reports whether the worktree at root is the checkout's: its
// .git file names an administrative directory under the checkout's
// .git/worktrees. A checkout whose worktree directory was deleted by hand
// keeps its registration, and another checkout can then add a worktree
// at the same root; git lists the root in both, and only the one it
// points back to can remove it. A root that is gone is taken as the
// checkout's, so a worktree deleted by hand is still found for rm to
// prune; one pointing at an administrative directory that is gone is
// no checkout's. A .git that cannot be read or is not a worktree's is
// an error rather than a guess, which would hand the root to whichever
// clone comes first.
func pointsBack(root, checkout string) (bool, error) {
	dotgit := filepath.Join(root, ".git")
	b, err := os.ReadFile(dotgit)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ENOTDIR) {
			return true, nil
		}
		return false, fmt.Errorf("%s: %w", dotgit, err)
	}
	gitdir, ok := strings.CutPrefix(strings.TrimSpace(string(b)), "gitdir:")
	if !ok {
		return false, fmt.Errorf("%s is not a worktree's .git file", dotgit)
	}
	gitdir = strings.TrimSpace(gitdir)
	if !filepath.IsAbs(gitdir) {
		gitdir = filepath.Join(root, gitdir)
	}
	admin, err := filepath.EvalSymlinks(filepath.Dir(gitdir))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, fmt.Errorf("%s: %w", dotgit, err)
	}
	want, err := filepath.EvalSymlinks(filepath.Join(checkout, ".git", "worktrees"))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	return admin == want, nil
}

// Find locates a registered worktree by root across every main checkout
// under the repos directory, under the worktrees directory only. Used by
// rm on a root-only target; a worktree elsewhere is not the daemon's to
// remove.
func (s *Store) Find(ctx context.Context, root string) (Record, string, bool, error) {
	if !s.Owns(root) {
		return Record{}, "", false, nil
	}
	// A checkout that cannot be read is not absence: rm must not take it
	// as "already removed" and go on to kill the session.
	cos, err := s.scan(ctx)
	if err != nil {
		return Record{}, "", false, err
	}
	for _, co := range cos {
		if !s.linked(co.dir) {
			continue
		}
		entries, err := ListWorktrees(ctx, co.dir)
		if err != nil {
			return Record{}, "", false, err
		}
		for _, e := range entries {
			if e.Root != root || e.Root == co.dir {
				continue
			}
			if mine, err := pointsBack(root, co.dir); err != nil {
				return Record{}, "", false, err
			} else if mine {
				r := s.label(co)
				return Record{Repo: r.Name, Source: r.Source, Branch: e.Branch, Root: e.Root}, co.dir, true, nil
			}
		}
	}
	return Record{}, "", false, nil
}

// ByBranch locates the worktree for a branch of repo under the worktrees
// directory, for rm. A worktree on the branch elsewhere, the main checkout
// included, does not count. A prunable registration, whose directory was
// deleted by hand, does: it is what rm prunes, and its root is what finds
// the orphaned session. Not found is (Record{}, checkout, false, nil) with
// the checkout still reported when it exists.
//
// Every checkout of the repository is asked: two clones of one
// repository each have worktrees, and each is listed. When both have
// one for the branch, which is meant is not known, and that is an error
// naming both roots rather than a guess.
func (s *Store) ByBranch(ctx context.Context, repo Repo, branch string) (Record, string, bool, error) {
	cos, err := s.scan(ctx)
	if err != nil {
		return Record{}, "", false, err
	}
	first := ""
	type match struct {
		rec      Record
		checkout string
		prunable bool
	}
	var matches []match
	for _, co := range cos {
		if !config.SameSource(co.origin, repo.Source) {
			continue
		}
		if first == "" {
			first = co.dir
		}
		if !s.linked(co.dir) {
			continue
		}
		entries, err := ListWorktrees(ctx, co.dir)
		if err != nil {
			return Record{}, co.dir, false, err
		}
		for _, e := range entries {
			if e.Branch != branch || e.Root == co.dir || !s.Owns(e.Root) {
				continue
			}
			mine, err := pointsBack(e.Root, co.dir)
			if err != nil {
				return Record{}, co.dir, false, err
			}
			if mine && !slices.ContainsFunc(matches, func(m match) bool { return m.rec.Root == e.Root }) {
				// A root two checkouts still register once its directory
				// is gone is one worktree: the first checkout's
				// registration removes it, the other is prunable.
				matches = append(matches, match{Record{Repo: repo.Name, Source: repo.Source, Branch: e.Branch, Root: e.Root}, co.dir, e.Prunable})
			}
		}
	}
	if len(matches) > 1 {
		// A registration whose directory was deleted by hand does not
		// compete with a live worktree for the branch in another clone.
		live := slices.DeleteFunc(slices.Clone(matches), func(m match) bool { return m.prunable })
		if len(live) > 0 {
			matches = live
		}
	}
	switch len(matches) {
	case 0:
		return Record{}, first, false, nil
	case 1:
		return matches[0].rec, matches[0].checkout, true, nil
	}
	return Record{}, first, false, fmt.Errorf("branch %s of %s has worktrees at %s and %s, in two clones of it; name the worktree by its root", branch, repo.Name, matches[0].rec.Root, matches[1].rec.Root)
}

// Remove unregisters and deletes a worktree through git, which is the
// judge of whether it may go: without force a dirty, locked or submodule
// worktree is refused with git's message. A registration whose directory
// is already gone is removed the same way: git drops just that entry,
// where prune would sweep every stale one. Skips when root is not a
// registered worktree of the checkout, so a retry is a no-op.
func Remove(ctx context.Context, checkout, root string, force bool) (removed bool, err error) {
	entries, err := ListWorktrees(ctx, checkout)
	if err != nil {
		return false, err
	}
	registered := false
	for _, e := range entries {
		if e.Root == root && e.Root != checkout {
			registered = true
		}
	}
	if !registered {
		return false, nil
	}
	args := []string{"worktree", "remove"}
	if force {
		args = append(args, "--force", "--force")
	}
	args = append(args, root)
	if _, err := git(ctx, checkout, args...); err != nil {
		return false, err
	}
	return true, nil
}

// git runs a git command in dir and returns its stdout. On failure the
// error carries git's stderr.
func git(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = gitEnv()
	var out, errb strings.Builder
	cmd.Stdout = &out
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(errb.String())
		if msg == "" {
			msg = err.Error()
		}
		return out.String(), &gitError{args: args, msg: msg, err: err}
	}
	return out.String(), nil
}

type gitError struct {
	args []string
	msg  string
	err  error
}

func (e *gitError) Error() string { return "git " + strings.Join(e.args, " ") + ": " + e.msg }
func (e *gitError) Unwrap() error { return e.err }

// gitEnv is the daemon's environment with prompts disabled: a fetch that
// needs credentials must fail, not hang the stage.
func gitEnv() []string {
	return append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "LC_ALL=C")
}

// hash is the marker suffix for a setup command: a changed command has a
// new hash and runs again.
func hash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:6])
}

// maxLine caps what one reported output line keeps; the rest of a longer
// line is read and dropped, so the producer never blocks on the pipe.
const maxLine = 64 * 1024

// StreamLines feeds each line of r to fn, without the newline, and reads
// r to its end whatever the line lengths: a Scanner would stop at its
// buffer limit and leave the writer blocked on the pipe. A partial last
// line is delivered at the end. The daemon streams a run's output with
// it too.
func StreamLines(r io.Reader, fn func(string)) {
	br := bufio.NewReader(r)
	var line []byte
	truncated := false
	for {
		part, isPrefix, err := br.ReadLine()
		if err != nil {
			if len(line) > 0 {
				fn(string(line))
			}
			return
		}
		if len(line) < maxLine {
			line = append(line, part...)
			if len(line) > maxLine {
				line = line[:maxLine]
				truncated = true
			}
		} else {
			truncated = true
		}
		if isPrefix {
			continue
		}
		if truncated {
			line = append(line, "..."...)
		}
		fn(string(line))
		line, truncated = line[:0], false
	}
}
