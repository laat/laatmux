package daemon

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"time"

	"github.com/laat/laatmux/internal/protocol"
	"github.com/laat/laatmux/internal/worktree"
)

// Git status: each listed worktree's git object, read beside the listing
// rather than in it, since the listing is serialized and stamps the
// listings that retire tasks, and a slow repository must not hold it. A
// pool of gitWorkers takes the worktrees due for a refresh: one with a
// home session every gitSessionEvery, one without every gitIdleEvery,
// and any at once when add or a run ending makes it due or an mtime the
// loop stats every gitTick changes: HEAD and index in its git dir, and in
// the common dir packed-refs and the loose refs of the branch and its
// base. No worktree is refreshed more than once per gitMinGap.
const (
	gitWorkers      = 2
	gitTick         = time.Second
	gitSessionEvery = 5 * time.Second
	gitIdleEvery    = 30 * time.Second
	gitMinGap       = 2 * time.Second
)

// The git reads, variables so a test can stand in for git.
var (
	gitStatusRead = worktree.Status
	gitHeadRead   = worktree.Head
)

// gitEntry is one worktree's refresh state. The cache is the running
// refresh's while running is set; the rest is under d.mu, but for the
// paths, which the running refresh reads without it, since only its own
// completion writes them. The listing
// drops an entry when its root goes or changes branch, so a refresh
// still running for it publishes nothing.
type gitEntry struct {
	branch  string
	cache   worktree.StatusCache
	paths   worktree.Paths
	mtimes  map[string]time.Time
	due     bool
	last    time.Time // when the last refresh began
	running bool
}

// watched is the files whose mtimes make an entry due.
func (e *gitEntry) watched() []string {
	if e.paths.GitDir == "" {
		return nil
	}
	return append([]string{
		e.paths.GitDir + "/HEAD", e.paths.GitDir + "/index",
	}, e.paths.Refs...)
}

func statMtimes(files []string) map[string]time.Time {
	out := make(map[string]time.Time, len(files))
	for _, f := range files {
		if fi, err := os.Stat(f); err == nil {
			out[f] = fi.ModTime()
		}
	}
	return out
}

func sameMtimes(a, b map[string]time.Time) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if w, ok := b[k]; !ok || !w.Equal(v) {
			return false
		}
	}
	return true
}

// gitDue makes a worktree due at once: add made it, or a run in it ended.
func (d *Daemon) gitDue(root string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if e := d.gits[root]; e != nil {
		e.due = true
	}
}

// runGitStatus refreshes the worktrees' git objects until ctx is done.
func (d *Daemon) runGitStatus(ctx context.Context) {
	t := time.NewTicker(gitTick)
	defer t.Stop()
	slots := make(chan struct{}, gitWorkers)
	for {
		d.gitRound(ctx, slots)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// gitRound is one tick: the entries follow the listing, the watched
// mtimes are statted, and the worktrees due start refreshing while a
// worker is free, the longest waiting first.
func (d *Daemon) gitRound(ctx context.Context, slots chan struct{}) {
	type check struct {
		root  string
		files []string
		was   map[string]time.Time
	}
	d.mu.Lock()
	for root, w := range d.worktrees {
		if e := d.gits[root]; e == nil || e.branch != w.Branch {
			d.gits[root] = &gitEntry{branch: w.Branch, due: true}
		}
	}
	var checks []check
	for root, e := range d.gits {
		if _, ok := d.worktrees[root]; !ok {
			delete(d.gits, root)
			continue
		}
		if !e.running && !e.due {
			checks = append(checks, check{root, e.watched(), e.mtimes})
		}
	}
	d.mu.Unlock()
	changed := map[string]bool{}
	for _, c := range checks {
		if len(c.files) > 0 && !sameMtimes(statMtimes(c.files), c.was) {
			changed[c.root] = true
		}
	}
	now := time.Now()
	d.mu.Lock()
	type job struct {
		root string
		e    *gitEntry
	}
	var due []job
	for root, e := range d.gits {
		if changed[root] {
			e.due = true
		}
		if e.running || now.Sub(e.last) < gitMinGap {
			continue
		}
		every := gitIdleEvery
		if d.worktrees[root].Session != "" {
			every = gitSessionEvery
		}
		if e.due || now.Sub(e.last) >= every {
			due = append(due, job{root, e})
		}
	}
	sort.Slice(due, func(i, j int) bool { return due[i].e.last.Before(due[j].e.last) })
	var start []job
	for _, j := range due {
		select {
		case slots <- struct{}{}:
		default:
			continue
		}
		j.e.running, j.e.due, j.e.last = true, false, now
		start = append(start, j)
	}
	d.mu.Unlock()
	for _, j := range start {
		go func() {
			defer func() { <-slots }()
			d.refreshGit(ctx, j.root, j.e)
		}()
	}
}

// refreshGit reads one worktree's git state and publishes it when a value
// changed. The result is dropped when the worktree left the listing, or
// its HEAD moved, while the refresh ran; the next one reads it again. A
// refresh that timed out keeps the last object and marks it stale.
func (d *Daemon) refreshGit(ctx context.Context, root string, e *gitEntry) {
	// The watched mtimes before the read are the baseline the loop
	// compares with: a change during the read, an index staged after
	// the diff say, makes the worktree due again. The first refresh has
	// none, and the loop's first look then refreshes once more.
	var before map[string]time.Time
	if files := e.watched(); len(files) > 0 {
		before = statMtimes(files)
	}
	st, head, paths, err := gitStatusRead(ctx, root, e.branch, &e.cache)
	after := ""
	if err == nil {
		after, err = gitHeadRead(ctx, root)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	e.running = false
	if ctx.Err() != nil || d.gits[root] != e {
		return
	}
	w, ok := d.worktrees[root]
	if !ok || w.Branch != e.branch {
		// Another branch at the root since: its own entry reads it.
		return
	}
	if paths.GitDir != "" {
		e.paths, e.mtimes = paths, before
	}
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		if w.Git == nil || w.Git.Stale {
			return
		}
		g := *w.Git
		g.Stale, g.ChangedAt = true, time.Now()
		w.Git = &g
	case err != nil:
		d.logOnce(&d.lastGitErr, "git status: %v", fmt.Errorf("%s: %w", root, err))
		return
	case head != after:
		e.due = true
		return
	default:
		if w.Git != nil && w.Git.Same(st) {
			return
		}
		st.ChangedAt = time.Now()
		w.Git = &st
	}
	d.worktrees[root] = w
	d.seq++
	d.broadcastLocked(protocol.Message{Type: protocol.TypeUpsert, Seq: d.seq, Worktree: &w})
}
