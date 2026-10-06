package daemon

import (
	"context"
	"errors"
	"log"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/laat/laatmux/internal/protocol"
	"github.com/laat/laatmux/internal/worktree"
)

// fakeGit stands in for the git reads: a status and a head per root, a
// head after the read that may differ, a block, and a count of reads.
type fakeGit struct {
	mu     sync.Mutex
	status map[string]protocol.GitStatus
	head   map[string]string
	after  map[string]string // HEAD read after the status, when set
	err    map[string]error
	block  chan struct{} // reads wait on it when set
	reads  int
}

func installFakeGit(t *testing.T) *fakeGit {
	f := &fakeGit{status: map[string]protocol.GitStatus{}, head: map[string]string{}, after: map[string]string{}, err: map[string]error{}}
	oldS, oldH := gitStatusRead, gitHeadRead
	gitStatusRead = func(ctx context.Context, root, branch string, c *worktree.StatusCache) (protocol.GitStatus, string, worktree.Paths, error) {
		f.mu.Lock()
		block := f.block
		f.reads++
		f.mu.Unlock()
		if block != nil {
			<-block
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		return f.status[root], f.head[root], worktree.Paths{}, f.err[root]
	}
	gitHeadRead = func(ctx context.Context, root string) (string, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if h, ok := f.after[root]; ok {
			return h, nil
		}
		return f.head[root], nil
	}
	t.Cleanup(func() { gitStatusRead, gitHeadRead = oldS, oldH })
	return f
}

// gitDaemon is a daemon with one listed worktree at /w/a and a
// subscriber on its own stream.
func gitDaemon(t *testing.T) (*Daemon, *subscriber) {
	d := New(Config{EnvironmentID: "env"})
	s := &subscriber{ch: make(chan protocol.Message, 64)}
	d.mu.Lock()
	d.subs[s] = struct{}{}
	d.worktrees["/w/a"] = protocol.Worktree{ID: d.worktreeID("/w/a"), EnvironmentID: "env", Repo: "proj", Branch: "a", Root: "/w/a", Session: "proj/a"}
	d.mu.Unlock()
	return d, s
}

// upserts drains the worktree upserts sent so far.
func upserts(s *subscriber) []protocol.Worktree {
	var out []protocol.Worktree
	for {
		select {
		case m := <-s.ch:
			if m.Worktree != nil {
				out = append(out, *m.Worktree)
			}
		default:
			return out
		}
	}
}

// refresh runs one round and waits for its refreshes.
func refresh(t *testing.T, d *Daemon) {
	t.Helper()
	slots := make(chan struct{}, gitWorkers)
	d.gitRound(context.Background(), slots)
	idle(t, d)
}

// idle waits until no refresh runs.
func idle(t *testing.T, d *Daemon) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		d.mu.Lock()
		busy := false
		for _, e := range d.gits {
			busy = busy || e.running
		}
		d.mu.Unlock()
		if !busy {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("refresh did not finish")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// makeDue makes every entry due, as a trigger would, past the minimum gap.
func makeDue(d *Daemon) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, e := range d.gits {
		e.due, e.last = true, time.Time{}
	}
}

// A refresh upserts the record with its git object; one that changes
// nothing upserts nothing; a changed value does, with a new ChangedAt.
func TestGitRefreshPublishes(t *testing.T) {
	f := installFakeGit(t)
	d, s := gitDaemon(t)
	f.status["/w/a"] = protocol.GitStatus{Base: "origin/main", Committed: [2]int{4, 1}, Ahead: 2}
	f.head["/w/a"] = "h1"
	refresh(t, d)
	ups := upserts(s)
	if len(ups) != 1 || ups[0].Git == nil || ups[0].Git.Committed != [2]int{4, 1} || ups[0].Git.ChangedAt.IsZero() {
		t.Fatalf("first refresh: %+v", ups)
	}
	first := ups[0].Git.ChangedAt
	makeDue(d)
	refresh(t, d)
	if ups := upserts(s); len(ups) != 0 {
		t.Errorf("an unchanged refresh upserted: %+v", ups)
	}
	f.mu.Lock()
	f.status["/w/a"] = protocol.GitStatus{Base: "origin/main", Committed: [2]int{4, 1}, Ahead: 2, Dirty: true}
	f.mu.Unlock()
	makeDue(d)
	refresh(t, d)
	if ups := upserts(s); len(ups) != 1 || !ups[0].Git.Dirty || !ups[0].Git.ChangedAt.After(first) {
		t.Errorf("a change: %+v", ups)
	}
}

// A result whose HEAD moved while the refresh ran is dropped and the
// worktree due again; one for a worktree gone from the listing too; a
// timeout keeps the last object, marked stale.
func TestGitRefreshDrops(t *testing.T) {
	f := installFakeGit(t)
	d, s := gitDaemon(t)
	f.status["/w/a"] = protocol.GitStatus{Base: "origin/main", Ahead: 1}
	f.head["/w/a"], f.after["/w/a"] = "h1", "h2"
	refresh(t, d)
	if ups := upserts(s); len(ups) != 0 {
		t.Fatalf("a result after HEAD moved was published: %+v", ups)
	}
	d.mu.Lock()
	due := d.gits["/w/a"].due
	d.mu.Unlock()
	if !due {
		t.Error("not due again after a dropped result")
	}
	f.mu.Lock()
	delete(f.after, "/w/a")
	f.mu.Unlock()
	makeDue(d)
	refresh(t, d)
	if ups := upserts(s); len(ups) != 1 {
		t.Fatalf("the next refresh: %+v", ups)
	}

	f.mu.Lock()
	f.err["/w/a"] = context.DeadlineExceeded
	f.mu.Unlock()
	makeDue(d)
	refresh(t, d)
	if ups := upserts(s); len(ups) != 1 || !ups[0].Git.Stale || ups[0].Git.Ahead != 1 {
		t.Errorf("a timeout: %+v", ups)
	}

	// Gone while the read ran.
	f.mu.Lock()
	f.err["/w/a"] = nil
	f.block = make(chan struct{})
	block := f.block
	f.mu.Unlock()
	makeDue(d)
	d.gitRound(context.Background(), make(chan struct{}, gitWorkers))
	d.mu.Lock()
	delete(d.worktrees, "/w/a")
	d.mu.Unlock()
	close(block)
	idle(t, d)
	if ups := upserts(s); len(ups) != 0 {
		t.Errorf("a gone worktree was published: %+v", ups)
	}
}

// A git error is logged once per worktree while it persists, and two
// worktrees' errors apart. A read that works clears it, so the same error
// coming back is logged again: one that finds the object it last
// published, and one whose HEAD moved, too. A timeout marking the object
// stale does not clear it.
func TestGitErrorLoggedOnce(t *testing.T) {
	f := installFakeGit(t)
	var logged strings.Builder
	d := New(Config{EnvironmentID: "env", Logger: log.New(&logged, "", 0)})
	d.mu.Lock()
	for _, r := range []string{"/w/a", "/w/b"} {
		d.worktrees[r] = protocol.Worktree{ID: d.worktreeID(r), EnvironmentID: "env", Branch: r, Root: r}
	}
	d.mu.Unlock()
	f.head["/w/a"], f.head["/w/b"] = "h1", "h1"
	setErr := func(root string, err error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.err[root] = err
	}
	rounds := func(n int) {
		for range n {
			makeDue(d)
			refresh(t, d)
		}
	}
	git := func() protocol.GitStatus {
		t.Helper()
		d.mu.Lock()
		g := d.worktrees["/w/a"].Git
		d.mu.Unlock()
		if g == nil {
			t.Fatal("no object published for /w/a")
		}
		return *g
	}
	want := func(a, b int, when string) {
		t.Helper()
		d.mu.Lock()
		got := logged.String()
		d.mu.Unlock()
		if n, m := strings.Count(got, "a broke"), strings.Count(got, "b broke"); n != a || m != b {
			t.Fatalf("%s: a logged %d times and b %d, want %d and %d; log:\n%s", when, n, m, a, b, got)
		}
	}
	rounds(1) // both publish an object
	published := git()
	setErr("/w/a", errors.New("a broke"))
	setErr("/w/b", errors.New("b broke"))
	rounds(3)
	want(1, 1, "three rounds")
	setErr("/w/a", nil)
	rounds(1)
	if g := git(); !g.ChangedAt.Equal(published.ChangedAt) {
		t.Fatalf("the read that worked published %+v, want the object it last published", g)
	}
	setErr("/w/a", errors.New("a broke"))
	rounds(2)
	want(2, 1, "a read that found the object it last published")
	setErr("/w/a", context.DeadlineExceeded)
	rounds(1)
	if !git().Stale {
		t.Fatal("the timeout did not mark the object stale")
	}
	setErr("/w/a", errors.New("a broke"))
	rounds(1)
	want(2, 1, "a timeout")
	f.mu.Lock()
	f.err["/w/a"], f.after["/w/a"] = nil, "h2"
	f.mu.Unlock()
	rounds(1)
	if !git().Stale {
		t.Fatal("the read whose HEAD moved published its result")
	}
	f.mu.Lock()
	delete(f.after, "/w/a")
	f.mu.Unlock()
	setErr("/w/a", errors.New("a broke"))
	rounds(1)
	want(3, 1, "a read whose HEAD moved")
}

// The listing carries the git object across its rebuild, so a session
// appearing neither drops the stats nor waits for a refresh; a new branch
// at the root starts without them.
func TestGitCarriedAcrossListing(t *testing.T) {
	installFakeGit(t)
	d, s := gitDaemon(t)
	g := &protocol.GitStatus{Base: "origin/main", Ahead: 3}
	d.mu.Lock()
	w := d.worktrees["/w/a"]
	w.Git = g
	d.worktrees["/w/a"] = w
	d.lastList = []worktree.Record{{Repo: "proj", Branch: "a", Root: "/w/a"}}
	d.managedRoots = map[string]string{"/w/a": "proj/a2"}
	d.publishWorktreesLocked(time.Now())
	d.mu.Unlock()
	if ups := upserts(s); len(ups) != 1 || ups[0].Git == nil || ups[0].Git.Ahead != 3 {
		t.Errorf("session change: %+v", ups)
	}
	d.mu.Lock()
	d.lastList = []worktree.Record{{Repo: "proj", Branch: "b", Root: "/w/a"}}
	d.publishWorktreesLocked(time.Now())
	d.mu.Unlock()
	if ups := upserts(s); len(ups) != 1 || ups[0].Git != nil {
		t.Errorf("new branch: %+v", ups)
	}
}

// A slow repository holds neither the listing nor the other worktrees
// beyond the pool: with two stuck, a third waits, and the listing's lock
// is free meanwhile.
func TestGitSlowRepository(t *testing.T) {
	f := installFakeGit(t)
	d, _ := gitDaemon(t)
	d.mu.Lock()
	for _, r := range []string{"/w/b", "/w/c"} {
		d.worktrees[r] = protocol.Worktree{ID: d.worktreeID(r), EnvironmentID: "env", Branch: r, Root: r}
	}
	d.mu.Unlock()
	f.mu.Lock()
	f.block = make(chan struct{})
	block := f.block
	f.mu.Unlock()
	slots := make(chan struct{}, gitWorkers)
	d.gitRound(context.Background(), slots)
	time.Sleep(20 * time.Millisecond)
	f.mu.Lock()
	reads := f.reads
	f.mu.Unlock()
	if reads != gitWorkers {
		t.Errorf("%d reads started, want %d", reads, gitWorkers)
	}
	locked := make(chan struct{})
	go func() {
		d.pollMu.Lock()
		d.mu.Lock()
		d.mu.Unlock()
		d.pollMu.Unlock()
		close(locked)
	}()
	select {
	case <-locked:
	case <-time.After(time.Second):
		t.Error("the listing's locks are held by a slow refresh")
	}
	close(block)
	idle(t, d)
}

// The capability: a host with worktrees and a merging daemon have it.
func TestGitStatusCapability(t *testing.T) {
	hosts := &hostsList{}
	d := New(Config{EnvironmentID: "env", Hosts: hosts.get})
	if !protocol.Has(d.capabilities(), protocol.CapGitStatus) {
		t.Error("a merging daemon without worktrees lacks git-status")
	}
	if protocol.Has(New(Config{EnvironmentID: "env"}).capabilities(), protocol.CapGitStatus) {
		t.Error("a daemon with neither has git-status")
	}
}

// A merging daemon forwards a host's git objects in its snapshot and
// upserts, unchanged.
func TestGitForwarded(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f := newMergedFixture(t, ctx, nil)
	g := &protocol.GitStatus{Base: "origin/main", Committed: [2]int{5, 1}, Ahead: 1}
	f.remote.d.mu.Lock()
	f.remote.d.worktrees["/r/x"] = protocol.Worktree{ID: "renv/worktree//r/x", EnvironmentID: "renv", Repo: "proj", Branch: "x", Root: "/r/x", Git: g}
	f.remote.d.mu.Unlock()
	c, pc, _ := f.subscribe(t, ctx)
	defer c.Close()
	msgs := until(t, c, pc, func(m protocol.Message) bool {
		return m.Worktree != nil && m.Worktree.ID == "renv/worktree//r/x"
	})
	if w := msgs[len(msgs)-1].Worktree; w.Git == nil || w.Git.Committed != [2]int{5, 1} {
		t.Errorf("forwarded: %+v", w)
	}
}

// A refresh whose root changed branch, or went and came
// back, while it ran publishes nothing; a watched file changed during the
// read makes the worktree due again.
func TestGitRefreshAcrossListing(t *testing.T) {
	f := installFakeGit(t)
	d, s := gitDaemon(t)
	f.status["/w/a"] = protocol.GitStatus{Base: "origin/main", Ahead: 1}
	f.head["/w/a"] = "h1"
	for _, change := range []func(){
		func() { // another branch at the root
			d.lastList = []worktree.Record{{Repo: "proj", Branch: "b", Root: "/w/a"}}
			d.publishWorktreesLocked(time.Now())
		},
		func() { // gone and back
			d.lastList = nil
			d.publishWorktreesLocked(time.Now())
			d.lastList = []worktree.Record{{Repo: "proj", Branch: "b", Root: "/w/a"}}
			d.publishWorktreesLocked(time.Now())
		},
	} {
		f.mu.Lock()
		f.block = make(chan struct{})
		block := f.block
		f.mu.Unlock()
		makeDue(d)
		d.gitRound(context.Background(), make(chan struct{}, gitWorkers))
		d.mu.Lock()
		change()
		d.mu.Unlock()
		upserts(s)
		close(block)
		time.Sleep(20 * time.Millisecond)
		for _, w := range upserts(s) {
			if w.Git != nil {
				t.Errorf("a stale refresh published: %+v", w)
			}
		}
		f.mu.Lock()
		f.block = nil
		f.mu.Unlock()
	}
}

func TestGitTriggerDuringRead(t *testing.T) {
	f := installFakeGit(t)
	d, _ := gitDaemon(t)
	dir := t.TempDir()
	index := dir + "/index"
	os.WriteFile(index, []byte("1"), 0o644)
	f.head["/w/a"] = "h1"
	orig := gitStatusRead
	gitStatusRead = func(ctx context.Context, root, branch string, c *worktree.StatusCache) (protocol.GitStatus, string, worktree.Paths, error) {
		st, h, _, err := orig(ctx, root, branch, c)
		return st, h, worktree.Paths{GitDir: dir, CommonDir: dir}, err
	}
	refresh(t, d) // learns the paths
	makeDue(d)
	f.mu.Lock()
	f.block = make(chan struct{})
	block := f.block
	f.mu.Unlock()
	d.gitRound(context.Background(), make(chan struct{}, gitWorkers))
	time.Sleep(20 * time.Millisecond)
	later := time.Now().Add(time.Hour)
	os.Chtimes(index, later, later) // staged while the diff was read
	close(block)
	idle(t, d)
	d.mu.Lock()
	// Past the minimum gap, short of the cadence: only the trigger can
	// make it due.
	d.gits["/w/a"].last = time.Now().Add(-gitMinGap - time.Second)
	d.mu.Unlock()
	f.mu.Lock()
	f.block = nil
	n := f.reads
	f.mu.Unlock()
	refresh(t, d)
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.reads == n {
		t.Error("a change during the read did not make the worktree due")
	}
}
