// Package worktree is the git side of a host's workspaces: the checkouts
// under the host's repos directories, the worktrees under its worktrees
// directory, and the stages of add that touch git and the filesystem.
//
// Git is the source of truth. A checkout is found under a <repos> by its
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
	"io/fs"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/laat/laatmux/internal/config"
	"github.com/laat/laatmux/internal/gitenv"
	"github.com/laat/laatmux/internal/source"
	"github.com/laat/laatmux/internal/tmux"
)

// Repo is a known repository: its source, the identity, and its label,
// which places new clones and worktrees; Copy and Setup are the personal
// steps for its worktrees, run after the committed ones and before the
// host's own copy rules for every worktree (Listed.Copy). They come
// from this host's config, or from the add's repository entry for a
// repository the config does not list.
type Repo struct {
	Source string
	Name   string
	Copy   []string
	Setup  []string
}

// Store is one host's checkouts and worktrees. Log, when set, says why
// a checkout's label has a hash (see labels) and that a checkout's HEAD
// cannot be read.
type Store struct {
	Dirs config.Dirs // expanded for this host
	Log  *log.Logger

	// listed is what this host's config lists for the store, replaced
	// whole when the daemon reads the file again (SetListed).
	listed atomic.Pointer[Listed]

	mu      sync.Mutex
	origins map[string]originEntry // by checkout directory
	logged  map[string]bool        // the lines logged, by key: a collision by directory and holder, a HEAD by directory
}

type originEntry struct {
	mtime time.Time
	size  int64
	url   string
}

// New makes a store for the host's directories and known repositories,
// with no copy rules for every worktree; SetListed sets both.
func New(dirs config.Dirs, repos []config.Repo) *Store {
	s := &Store{Dirs: dirs, origins: map[string]originEntry{}}
	s.SetListed(Listed{Repos: FromConfig(repos)})
	return s
}

// Listed is what a host's config lists for its store, from one read of
// the file: the repositories, and Copy, the host's own copy rules for
// every worktree it makes, applied after a repository's own. An add
// takes one, so its repository's steps and the rules after them are of
// one read.
type Listed struct {
	Repos Repos
	Copy  []string
}

// ListedFrom is the config's repositories and copy rules as the store
// keeps them.
func ListedFrom(cfg config.Config) Listed {
	return Listed{Repos: FromConfig(cfg.Repos), Copy: cfg.Copy}
}

// FromConfig is the config's repositories as the store keeps them.
func FromConfig(repos []config.Repo) []Repo {
	out := make([]Repo, 0, len(repos))
	for _, r := range repos {
		out = append(out, Repo{Source: r.Source, Name: r.Name, Copy: r.Copy, Setup: r.Setup})
	}
	return out
}

// Listed is what this host's config lists, as last set; its slices are
// the store's and are not to be changed.
func (s *Store) Listed() Listed {
	if p := s.listed.Load(); p != nil {
		return *p
	}
	return Listed{}
}

// SetListed replaces what this host's config lists, for a daemon that
// read the file again: a lookup, a listing or an add under way finishes
// on what it started with.
func (s *Store) SetListed(l Listed) { s.listed.Store(&l) }

// Repos is the repositories this host's config lists, as last set; the
// slice is the store's and is not to be changed. A caller that looks a
// repository up more than once takes it once, so every lookup is on
// one list.
func (s *Store) Repos() Repos { return s.Listed().Repos }

// SetRepos replaces the repositories this host's config lists and keeps
// its copy rules, in one step against a SetListed at the same time.
func (s *Store) SetRepos(repos []Repo) {
	for {
		old := s.listed.Load()
		l := Listed{Repos: repos}
		if old != nil {
			l.Copy = old.Copy
		}
		if s.listed.CompareAndSwap(old, &l) {
			return
		}
	}
}

// Repos is a list of the repositories a host's config lists.
type Repos []Repo

// Repo finds a repository this host's config lists by name, else by
// source, in any form source.Same takes as one: the same order as
// config.Config.Repo, since a bare local source can equal another
// entry's label.
func (s *Store) Repo(nameOrSource string) (Repo, bool) { return s.Repos().Find(nameOrSource) }

// BySource finds a repository this host's config lists by its source,
// in any form source.Same takes as one.
func (s *Store) BySource(src string) (Repo, bool) { return s.Repos().BySource(src) }

// Find is Store.Repo on the list.
func (rs Repos) Find(nameOrSource string) (Repo, bool) {
	for _, r := range rs {
		if r.Name == nameOrSource {
			return r, true
		}
	}
	return rs.BySource(nameOrSource)
}

// BySource is Store.BySource on the list.
func (rs Repos) BySource(src string) (Repo, bool) {
	for _, r := range rs {
		if r.Source == src {
			return r, true
		}
	}
	for _, r := range rs {
		if source.Same(r.Source, src) {
			return r, true
		}
	}
	return Repo{}, false
}

// Known finds a repository this host has, as List labels it: by label,
// the config's name or, for a checkout the config does not list, the
// label labels gives it; else by source, the config's or a checkout's
// origin. rm and run name a repository by what a listing said, which
// covers checkouts the config does not list. A label two repositories
// answer to, the config's name and a checkout's, is an error rather
// than a guess: the source tells them apart.
func (s *Store) Known(ctx context.Context, nameOrSource string) (Repo, bool, error) {
	cos, err := s.scan(ctx)
	if err != nil {
		return Repo{}, false, err
	}
	repos := s.Repos()
	var named []Repo
	for _, r := range repos {
		if r.Name == nameOrSource {
			named = append(named, r)
		}
	}
	labels, _ := s.labels(repos, cos)
	for _, co := range cos {
		if _, listed := repos.BySource(co.origin); listed {
			continue
		}
		if r := labels[co.dir]; r.Name == nameOrSource {
			named = append(named, r)
		}
	}
	for i := 1; i < len(named); i++ {
		if !source.Same(named[i].Source, named[0].Source) {
			return Repo{}, false, fmt.Errorf("%q names both %s and %s on this host; name the repository by its source", nameOrSource, named[0].Source, named[i].Source)
		}
	}
	if len(named) > 0 {
		return named[0], true, nil
	}
	if r, ok := repos.BySource(nameOrSource); ok {
		return r, true, nil
	}
	for _, co := range cos {
		if source.Same(co.origin, nameOrSource) {
			return labels[co.dir], true, nil
		}
	}
	return Repo{}, false, nil
}

// Checkout finds the main checkout of repo under the repos directories:
// the direct child of one whose remote.origin.url is the source, in any form
// source.Same takes as one, so each host fetches over the
// transport its checkout was cloned with. Reads of origin are cached by
// the mtime and size of .git/config, so an idle poll spawns no git
// processes. Not found is ("", false, nil).
func (s *Store) Checkout(ctx context.Context, repo Repo) (string, bool, error) {
	checkouts, err := s.Checkouts(ctx)
	if err != nil {
		return "", false, err
	}
	dir, ok := checkouts[source.Key(repo.Source)]
	return dir, ok, nil
}

// Checkouts scans the repos directories once and maps each origin found,
// by source.Key, to its checkout, the first in scan order (scan)
// when two share an origin. One scan serves every repository in a poll.
func (s *Store) Checkouts(ctx context.Context) (map[string]string, error) {
	cos, err := s.scan(ctx)
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, co := range cos {
		key := source.Key(co.origin)
		if _, dup := out[key]; !dup {
			out[key] = co.dir
		}
	}
	return out, nil
}

// checkout is a main checkout under a repos directory and its origin.
type checkout struct{ dir, origin string }

// scan is every main checkout with an origin directly under one of the
// repos directories, in scan order: the directories in the config's
// order, each in directory order. A directory that does not exist has
// no checkouts. A checkout reached twice, by a directory listed twice,
// as written or through a symlink, or by a symlink in a repos directory
// to another's checkout, a link left from before the list say, is
// listed once: by its own path where a repos directory has it as an
// entry that is no symlink, else by the path scanned first. A symlink
// leading to it would otherwise label it by the link's name, which the
// config's name for it does not hold (labels). Only a symlink is
// resolved per entry, so a poll over many checkouts costs no more than
// it did.
func (s *Store) scan(ctx context.Context) ([]checkout, error) {
	type entry struct {
		dir, real string
		link      bool
	}
	var entries []entry
	own := map[string]bool{} // by real path, the entries that are no symlink
	for _, repos := range s.Dirs.Repos {
		repos = filepath.Clean(repos)
		des, err := os.ReadDir(repos)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return nil, tmux.PrintablePath(err)
		}
		real := realPath(repos)
		for _, e := range des {
			en := entry{dir: filepath.Join(repos, e.Name()), real: filepath.Join(real, e.Name())}
			if e.Type()&fs.ModeSymlink != 0 {
				en.real, en.link = realPath(en.dir), true
			} else {
				own[en.real] = true
			}
			entries = append(entries, en)
		}
	}
	var out []checkout
	found := map[string]bool{} // by real path
	for _, en := range entries {
		if found[en.real] || en.link && own[en.real] {
			continue
		}
		url, ok, err := s.origin(ctx, en.dir)
		if err != nil {
			return nil, err
		}
		if ok {
			found[en.real] = true
			out = append(out, checkout{en.dir, url})
		}
	}
	return out, nil
}

// realPath is p with its symlinks resolved, as resolveExisting resolves
// them, or p as it is where they cannot be.
func realPath(p string) string {
	if real, ok := resolveExisting(p); ok {
		return real
	}
	return p
}

// dirLabel is the label of a checkout the config does not list, from
// its directory's name: the name itself when config.ValidLabel takes
// it, else the name with every character the rule does not take, a byte
// that is not UTF-8 and a leading - included, made a _: next.js is
// next_js. A label is printed by ls and in rm's and run's refusals,
// names the repository in jump's target, and is the repository's part
// of the local session name a jump gives a worktree in a session other
// than its home, which a control byte or a ":" would break, and a "."
// before tmux 3.7. laatmux clones under a label, but a checkout cloned
// into the repos directory by hand can be named anything, next.js by
// git's default say. Such a checkout is labelled rather than left out:
// an add finds a checkout by its origin, whatever its name, and
// leaving it out would hide the worktrees made in it.
func dirLabel(name string) string {
	if config.ValidLabel(name) {
		return name
	}
	out := []byte(strings.Map(func(r rune) rune {
		if 'A' <= r && r <= 'Z' || 'a' <= r && r <= 'z' || '0' <= r && r <= '9' || r == '_' || r == '-' {
			return r
		}
		return '_'
	}, name))
	if out[0] == '-' {
		out[0] = '_'
	}
	return string(out)
}

// labels is the repository each checkout of a scan holds, by directory,
// as List, Find and Known name it: the config's entry for its origin,
// else the origin under the label dirLabel makes of its directory's
// name. A label made of a name that is not one, next_js of next.js,
// does not take one another repository has: the config's name for it,
// or the label of a checkout the config does not list, whose directory
// is named so or which comes first in scan order, next_js or
// next:js say. The made label then has a - and a hash of its origin
// after it, so every clone of one repository that loses the plain
// label gets the same one; held names, by the directory of each such,
// the holder of its plain label, a checkout, or the config's entry with
// no directory. Every checkout is labelled, so a collision that appears
// next to a checkout with worktrees never takes them out of the
// listing, which the daemon would take for their removal. A directory
// whose own name is the config's name for another repository keeps it,
// as the user named it: Known refuses that label, naming both sources.
// A name that is a label is the checkout's own but where a checkout of
// another repository in another repos directory is named so too, as one
// directory cannot have two: a checkout of the config's repository
// named as the config names it keeps the plain label, else the first in
// scan order, and the other's label has the hash after it.
func (s *Store) labels(repos Repos, cos []checkout) (out map[string]Repo, held map[string]checkout) {
	out = make(map[string]Repo, len(cos))
	held = map[string]checkout{}
	holders := map[string]checkout{} // by label, what has it
	for _, r := range repos {
		holders[r.Name] = checkout{origin: r.Source}
	}
	// By name, the checkout named so that keeps it against one named so
	// in another repos directory: a checkout of the config's repository
	// whose directory is named as the config names it, wherever it is,
	// else the first in scan order.
	named := map[string]checkout{}
	for _, co := range cos {
		if r, listed := repos.BySource(co.origin); listed && filepath.Base(co.dir) == r.Name {
			if _, ok := named[r.Name]; !ok {
				named[r.Name] = co
			}
		}
	}
	var made []checkout // unlisted checkouts whose name is not a label
	for _, co := range cos {
		if r, listed := repos.BySource(co.origin); listed {
			out[co.dir] = r
			continue
		}
		name := filepath.Base(co.dir)
		if !config.ValidLabel(name) {
			made = append(made, co)
			continue
		}
		if f, ok := named[name]; !ok {
			named[name] = co
		} else if !source.Same(f.origin, co.origin) {
			// A checkout of another repository named so in another
			// repos directory: one directory cannot have two.
			held[co.dir] = f
			out[co.dir] = Repo{Source: co.origin, Name: hashed(name, co.origin)}
			continue
		}
		out[co.dir] = Repo{Source: co.origin, Name: name}
		if _, taken := holders[name]; !taken {
			holders[name] = co
		}
	}
	for _, co := range made {
		label := dirLabel(filepath.Base(co.dir))
		h, taken := holders[label]
		switch {
		case !taken:
			holders[label] = co
		case !source.Same(h.origin, co.origin):
			held[co.dir] = h
			label = hashed(label, co.origin)
		}
		out[co.dir] = Repo{Source: co.origin, Name: label}
	}
	return out, held
}

// hashed is a label that lost its plain form to another repository's:
// a - and six hex digits of a hash of the origin after it, the same for
// every clone of one repository.
func hashed(label, origin string) string {
	sum := sha256.Sum256([]byte(source.Key(origin)))
	return label + "-" + hex.EncodeToString(sum[:3])
}

// collided logs, once per pair, that a checkout's label has a hash
// because another repository has its plain label, and what settles it.
func (s *Store) collided(co, holder checkout, label string) {
	key := co.dir + "\x00" + holder.dir + "\x00" + holder.origin
	if holder.dir == "" {
		s.logOnce(key, "worktrees: %s is labelled %s: the label its name makes is this host's config's name for %s; a name for it in the config settles it", tmux.Printable(co.dir), label, tmux.Printable(holder.origin))
		return
	}
	s.logOnce(key, "worktrees: %s is labelled %s: the label its name makes is %s's, of another repository; a name for either in this host's config settles it", tmux.Printable(co.dir), label, tmux.Printable(holder.dir))
}

// forget lets logOnce log under the key again.
func (s *Store) forget(key string) {
	s.mu.Lock()
	delete(s.logged, key)
	s.mu.Unlock()
}

// logOnce logs a line the first time its key is seen.
func (s *Store) logOnce(key, format string, args ...any) {
	s.mu.Lock()
	seen := s.logged[key]
	if !seen {
		if s.logged == nil {
			s.logged = map[string]bool{}
		}
		s.logged[key] = true
	}
	s.mu.Unlock()
	if seen || s.Log == nil {
		return
	}
	s.Log.Printf(format, args...)
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
		return "", false, fmt.Errorf("%s: %w", tmux.Printable(dir), tmux.PrintablePath(err))
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
			return "", false, fmt.Errorf("%s: %w", tmux.Printable(dir), err)
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
// directory, as the daemon publishes it, or with Main a main checkout
// under a repos directory, as ListAll lists them.
type Record struct {
	Repo   string // label
	Source string
	Branch string // "" when detached
	Root   string
	// Main is a main checkout's record: Root is its directory under
	// a repos directory and Branch what its HEAD has checked out.
	// Configured is that this host's config lists its repository;
	// Linked that a worktree of this checkout is in the listing;
	// Unread that its HEAD could not be read, so it is not to be
	// published, though its directory still bounds what a worktree
	// around it is said to hold.
	Main       bool
	Configured bool
	Linked     bool
	Unread     bool
}

// List returns every worktree that lives under the worktrees directory
// of every main checkout under the repos directories, whether or not the
// config lists its repository: a checkout the config lists is labelled
// with the config's name and source, any other with its directory name,
// made a label by dirLabel, and origin, a hash after it when another
// repository has that label, logged once (see labels). Prunable entries,
// whose directory is gone, are left out, as is the main checkout, which
// is not a worktree even when the repos directory sits under the
// worktrees one. The repos directories are scanned once; one checkout
// failing to list does not hide the others: its error is returned
// alongside what was listed.
func (s *Store) List(ctx context.Context) ([]Record, error) {
	records, _, err := s.ListAll(ctx)
	return records, err
}

// ListAll is List's worktrees and a record of every main checkout the
// scan finds, by root, labelled as its worktrees are, its branch read
// from its HEAD (headBranch): the daemon publishes those in use. A
// checkout whose HEAD cannot be read is marked Unread, and the error is
// logged once rather than returned. git finds no repository there, so
// this holds for a checkout with no worktree, while its origin is
// cached: one with a worktree fails git worktree list, an error as
// before, and an origin not read yet leaves it unscanned.
func (s *Store) ListAll(ctx context.Context) (records, checkouts []Record, err error) {
	cos, err := s.scan(ctx)
	if err != nil {
		return nil, nil, err
	}
	repos := s.Repos()
	labels, held := s.labels(repos, cos)
	var errs []error
	seen := map[string]bool{}
	for _, co := range cos {
		r := labels[co.dir]
		_, configured := repos.BySource(co.origin)
		main := Record{Repo: r.Name, Source: r.Source, Root: co.dir, Main: true, Configured: configured}
		if s.linked(co.dir) {
			if h, ok := held[co.dir]; ok {
				s.collided(co, h, r.Name)
			}
			recs, err := s.worktreesOf(ctx, co, r, seen)
			if err != nil {
				errs = append(errs, err)
			}
			records = append(records, recs...)
			main.Linked = len(recs) > 0
		}
		branch, err := headBranch(ctx, co.dir)
		if err != nil {
			// Not the listing's failure: one checkout of many, in no use
			// as a rule, would hold every worktree's change. It is
			// marked unread, published by no one, until its HEAD reads
			// again; the error is logged once until then, as a git
			// status error is.
			s.logOnce("head\x00"+co.dir, "worktrees: %v", err)
			main.Unread = true
		} else {
			s.forget("head\x00" + co.dir)
			main.Branch = branch
		}
		checkouts = append(checkouts, main)
	}
	sort.Slice(records, func(i, j int) bool { return records[i].Root < records[j].Root })
	return records, checkouts, errors.Join(errs...)
}

// worktreesOf is a checkout's worktrees under the worktrees directory,
// labelled as the checkout is, but for a root seen holds, which another
// checkout listed; it adds the roots it lists to seen.
func (s *Store) worktreesOf(ctx context.Context, co checkout, r Repo, seen map[string]bool) ([]Record, error) {
	entries, err := ListWorktrees(ctx, co.dir)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", tmux.Printable(co.dir), err)
	}
	var records []Record
	for i, e := range entries {
		// git lists the main worktree first, by its real path, which a
		// symlink on the way to the repos directory makes another than
		// the checkout's as scanned: it is the main checkout's record.
		// A root two checkouts register, one after the other's
		// directory was deleted by hand, is listed once, for the
		// checkout the worktree points back to, as Find finds it.
		if i == 0 || e.Prunable || e.Bare || e.Root == co.dir || seen[e.Root] || !s.Owns(e.Root) {
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
	return records, nil
}

// headBranch is the branch a main checkout has checked out, read from
// its .git/HEAD, which costs no process on a poll over many checkouts:
// "" for a detached HEAD. A checkout on the reftable backend keeps a
// stub there, refs/heads/.invalid, and git is asked. Only ASCII white
// space is trimmed, as git trims it: a branch can end in U+0085 or
// U+00A0, which strings.TrimSpace would take.
func headBranch(ctx context.Context, dir string) (string, error) {
	b, err := os.ReadFile(filepath.Join(dir, ".git", "HEAD"))
	if err != nil {
		return "", fmt.Errorf("%s: %w", tmux.Printable(dir), tmux.PrintablePath(err))
	}
	const space = " \t\n\v\f\r"
	ref, ok := strings.CutPrefix(strings.TrimRight(string(b), space), "ref:")
	if !ok {
		return "", nil
	}
	ref = strings.TrimLeft(ref, space)
	if ref == "refs/heads/.invalid" {
		out, err := git(ctx, dir, "symbolic-ref", "--quiet", "HEAD")
		var ee *exec.ExitError
		switch {
		case errors.As(err, &ee) && ee.ExitCode() == 1:
			// Detached.
			return "", nil
		case err != nil:
			return "", fmt.Errorf("%s: %w", tmux.Printable(dir), err)
		}
		ref = strings.TrimRight(out, space)
	}
	branch, ok := strings.CutPrefix(ref, "refs/heads/")
	if !ok {
		return "", nil
	}
	return branch, nil
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

// IsCheckout reports whether root is a main checkout under a repos
// directory, as written or with symlinks resolved: rm refuses one even
// where the repos directory sits under the worktrees one, so Owns takes
// it, since its sessions are new's, not a worktree's.
func (s *Store) IsCheckout(ctx context.Context, root string) (bool, error) {
	cos, err := s.scan(ctx)
	if err != nil {
		return false, err
	}
	root = filepath.Clean(root)
	real, resolved := resolveExisting(root)
	for _, co := range cos {
		if co.dir == root {
			return true, nil
		}
		if dir, ok := resolveExisting(co.dir); ok && resolved && dir == real {
			return true, nil
		}
	}
	return false, nil
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
		return false, fmt.Errorf("%s: %w", tmux.Printable(dotgit), tmux.PrintablePath(err))
	}
	gitdir, ok := strings.CutPrefix(strings.TrimSpace(string(b)), "gitdir:")
	if !ok {
		return false, fmt.Errorf("%s is not a worktree's .git file", tmux.Printable(dotgit))
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
		return false, fmt.Errorf("%s: %w", tmux.Printable(dotgit), tmux.PrintablePath(err))
	}
	want, err := filepath.EvalSymlinks(filepath.Join(checkout, ".git", "worktrees"))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, tmux.PrintablePath(err)
	}
	return admin == want, nil
}

// Find locates a registered worktree by root across every main checkout
// under the repos directories, under the worktrees directory only. Used by
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
				labels, _ := s.labels(s.Repos(), cos)
				r := labels[co.dir]
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
// the checkout still reported when it exists. The branch is matched as
// BranchIs matches it, so a branch laatmux cannot carry is found by the
// name a listing shows for it.
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
		if !source.Same(co.origin, repo.Source) {
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
			if !BranchIs(e.Branch, branch) || e.Root == co.dir || !s.Owns(e.Root) {
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
	return Record{}, first, false, fmt.Errorf("branch %s of %s has worktrees at %s and %s, in two clones of it; name the worktree by its root", tmux.Printable(branch), repo.Name, tmux.Printable(matches[0].rec.Root), tmux.Printable(matches[1].rec.Root))
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

// Clean reports whether the worktree at root has nothing modified,
// staged or untracked, which is what git's worktree remove refuses
// without force; rm reads it to know whether what runs in the root can
// go before the removal, which takes seconds on a large worktree. A
// root that is not there, a registration whose directory went by hand,
// has nothing to lose and is clean.
func Clean(ctx context.Context, root string) (bool, error) {
	if _, err := os.Stat(root); errors.Is(err, fs.ErrNotExist) {
		return true, nil
	}
	out, err := git(ctx, root, "status", "--porcelain", "--untracked-files=all", "-z")
	if err != nil {
		return false, err
	}
	return out == "", nil
}

// git runs a git command in dir and returns its stdout. On failure the
// error carries git's stderr.
func git(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = GitEnv()
	var out, errb strings.Builder
	cmd.Stdout = &out
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(errb.String())
		if msg == "" {
			// The exit status, or os's error when git could not be
			// started, which names a dir that cannot be entered.
			msg = tmux.PrintablePath(err).Error()
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

// Error names the command with each argument as tmux.Printable shows
// it, and git's message with each of its lines so: a root goes into
// the arguments as it is, and git's message can repeat it or the
// directory git ran in. git turns most control bytes in its fatal and
// error lines into ?, but not a tab or a C1 character, and not in its
// hints; a message of several lines keeps them.
func (e *gitError) Error() string {
	a := make([]string, len(e.args))
	for i, v := range e.args {
		a[i] = tmux.Printable(v)
	}
	return "git " + strings.Join(a, " ") + ": " + tmux.PrintableLines(e.msg)
}
func (e *gitError) Unwrap() error { return e.err }

// GitEnv is the environment for a git command whose output or error is
// read: this process's, with prompts disabled, so a fetch that needs
// credentials fails rather than hangs the stage, and with the C locale,
// so what is matched is git's English whatever the user's locale. The
// variables of gitenv.Local are dropped: an exported GIT_DIR, or what a
// hook's environment has, would have git act on that repository and not
// on the one of the directory it is given. The config the environment
// gives every git is kept, GIT_CONFIG_PARAMETERS (git -c) and
// GIT_CONFIG_COUNT with its keys and values, as git keeps it when it
// starts a git for another repository.
func GitEnv() []string {
	env := slices.DeleteFunc(os.Environ(), func(kv string) bool {
		k, _, _ := strings.Cut(kv, "=")
		return slices.Contains(gitenv.Local, k) && k != "GIT_CONFIG_PARAMETERS" && k != "GIT_CONFIG_COUNT"
	})
	return append(env, "GIT_TERMINAL_PROMPT=0", "LC_ALL=C")
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
