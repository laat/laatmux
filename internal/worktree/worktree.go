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
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/laat/laatmux/internal/config"
)

// Repo is a known repository: its source, the identity, and its label,
// which places new clones and worktrees.
type Repo struct {
	Source string
	Name   string
}

// Store is one host's checkouts and worktrees.
type Store struct {
	Dirs  config.Dirs // expanded for this host
	Repos []Repo

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
		s.Repos = append(s.Repos, Repo{Source: r.Source, Name: r.Name})
	}
	return s
}

// Repo finds a known repository by name, else by source: the same order as
// config.Config.Repo, since a bare local source can equal another entry's
// label.
func (s *Store) Repo(nameOrSource string) (Repo, bool) {
	for _, r := range s.Repos {
		if r.Name == nameOrSource {
			return r, true
		}
	}
	for _, r := range s.Repos {
		if r.Source == nameOrSource {
			return r, true
		}
	}
	return Repo{}, false
}

// Checkout finds the main checkout of repo under the repos directory: the
// direct child whose remote.origin.url is the source. Reads of origin are
// cached by the mtime and size of .git/config, so an idle poll spawns no
// git processes. Not found is ("", false, nil).
func (s *Store) Checkout(ctx context.Context, repo Repo) (string, bool, error) {
	entries, err := os.ReadDir(s.Dirs.Repos)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", false, nil
		}
		return "", false, err
	}
	for _, e := range entries {
		dir := filepath.Join(s.Dirs.Repos, e.Name())
		url, ok, err := s.origin(ctx, dir)
		if err != nil {
			return "", false, err
		}
		if ok && url == repo.Source {
			return dir, true, nil
		}
	}
	return "", false, nil
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
func ListWorktrees(ctx context.Context, checkout string) ([]Entry, error) {
	out, err := git(ctx, checkout, "worktree", "list", "--porcelain")
	if err != nil {
		return nil, err
	}
	return parseWorktrees(out), nil
}

func parseWorktrees(out string) []Entry {
	var entries []Entry
	var cur *Entry
	for _, line := range strings.Split(out, "\n") {
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

// List returns every worktree of every known repository that lives under
// the worktrees directory. Prunable entries, whose directory is gone, are
// left out. A repository without a checkout on this host has no worktrees.
// One checkout failing to list does not hide the others: its error is
// returned alongside what was listed.
func (s *Store) List(ctx context.Context) ([]Record, error) {
	var records []Record
	var errs []error
	for _, r := range s.Repos {
		checkout, ok, err := s.Checkout(ctx, r)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if !ok {
			continue
		}
		entries, err := ListWorktrees(ctx, checkout)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", checkout, err))
			continue
		}
		for _, e := range entries {
			if e.Prunable || e.Bare || !s.Owns(e.Root) {
				continue
			}
			records = append(records, Record{Repo: r.Name, Source: r.Source, Branch: e.Branch, Root: e.Root})
		}
	}
	sort.Slice(records, func(i, j int) bool { return records[i].Root < records[j].Root })
	return records, errors.Join(errs...)
}

// Owns reports whether root is inside the worktrees directory: the only
// worktrees the daemon publishes, adopts for a branch, or removes. Git
// registers real paths, so the directory is compared both as configured
// and with symlinks resolved.
func (s *Store) Owns(root string) bool {
	dirs := []string{s.Dirs.Worktrees}
	if real, err := filepath.EvalSymlinks(s.Dirs.Worktrees); err == nil && real != s.Dirs.Worktrees {
		dirs = append(dirs, real)
	}
	for _, d := range dirs {
		if strings.HasPrefix(root, strings.TrimSuffix(d, "/")+"/") {
			return true
		}
	}
	return false
}

// Find locates a registered worktree by root across every known
// repository's checkout, under the worktrees directory only. Used by rm
// on a root-only target; a worktree elsewhere is not the daemon's to
// remove.
func (s *Store) Find(ctx context.Context, root string) (Record, string, bool, error) {
	if !s.Owns(root) {
		return Record{}, "", false, nil
	}
	for _, r := range s.Repos {
		checkout, ok, err := s.Checkout(ctx, r)
		if err != nil {
			// A checkout that cannot be read is not absence: rm must not
			// take it as "already removed" and go on to kill the session.
			return Record{}, "", false, err
		}
		if !ok {
			continue
		}
		entries, err := ListWorktrees(ctx, checkout)
		if err != nil {
			return Record{}, "", false, err
		}
		for _, e := range entries {
			if e.Root == root && e.Root != checkout {
				return Record{Repo: r.Name, Source: r.Source, Branch: e.Branch, Root: e.Root}, checkout, true, nil
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
func (s *Store) ByBranch(ctx context.Context, repo Repo, branch string) (Record, string, bool, error) {
	checkout, ok, err := s.Checkout(ctx, repo)
	if err != nil || !ok {
		return Record{}, "", false, err
	}
	entries, err := ListWorktrees(ctx, checkout)
	if err != nil {
		return Record{}, checkout, false, err
	}
	for _, e := range entries {
		if e.Branch == branch && e.Root != checkout && s.Owns(e.Root) {
			return Record{Repo: repo.Name, Source: repo.Source, Branch: e.Branch, Root: e.Root}, checkout, true, nil
		}
	}
	return Record{}, checkout, false, nil
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

// streamLines feeds each line of r to fn, without the newline, and reads
// r to its end whatever the line lengths: a Scanner would stop at its
// buffer limit and leave the writer blocked on the pipe.
func streamLines(r io.Reader, fn func(string)) {
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
