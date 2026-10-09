package daemon

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/laat/laatmux/internal/protocol"
	"github.com/laat/laatmux/internal/tmux"
	"github.com/laat/laatmux/internal/worktree"
)

// pruneCheckout clones the store's remote as its main checkout, with an
// identity for commits, and returns a func that makes a worktree on a
// new branch from origin/main under the worktrees directory.
func pruneCheckout(t *testing.T, store *worktree.Store, remote string) (string, func(branch string) string) {
	t.Helper()
	co := filepath.Join(store.Dirs.Repos[0], "proj")
	mkdirs(t, store.Dirs.Repos[0])
	sh(t, store.Dirs.Repos[0], "git", "clone", "-q", remote, co)
	sh(t, co, "git", "config", "user.email", "t@example.com")
	sh(t, co, "git", "config", "user.name", "t")
	return co, func(branch string) string {
		t.Helper()
		root := store.Dirs.Worktree("proj", branch)
		sh(t, co, "git", "worktree", "add", "-q", "-b", branch, root, "origin/main")
		return root
	}
}

// The facts message against real repositories: a clean worktree at
// main with an ignored file, one with a commit, one dirty, one pushed,
// one whose branch is gone from origin and a locked one, each as git
// has it, a root sent with a trailing slash read as its worktree's and
// answered as sent; a detached one has no branch; the main checkout and
// a directory that is no worktree have an error each, and the rest are
// read all the same, in the order asked. A daemon with no store
// refuses the message.
func TestFacts(t *testing.T) {
	store, remote := newStore(t)
	co, add := pruneCheckout(t, store, remote)
	clean, ahead, dirty, pushed, gone, locked := add("clean"), add("ahead"), add("dirty"), add("pushed"), add("gone"), add("locked")
	for _, root := range []string{ahead, pushed, gone} {
		sh(t, root, "git", "commit", "-q", "--allow-empty", "-m", "work")
	}
	os.WriteFile(filepath.Join(dirty, "new.txt"), []byte("x\n"), 0o644)
	os.WriteFile(filepath.Join(co, ".git", "info", "exclude"), []byte(".env\n"), 0o644)
	os.WriteFile(filepath.Join(clean, ".env"), []byte("x\n"), 0o644)
	sh(t, pushed, "git", "push", "-q", "origin", "pushed")
	sh(t, gone, "git", "push", "-q", "origin", "gone")
	sh(t, gone, "git", "push", "-q", "origin", "--delete", "gone")
	sh(t, co, "git", "worktree", "lock", "--reason", "keep me", locked)
	detached := store.Dirs.Worktree("proj", "detached")
	sh(t, co, "git", "worktree", "add", "-q", "--detach", detached, "origin/main")
	outside := t.TempDir()

	d, _ := addDaemon(t, store)
	if !protocol.Has(d.capabilities(), protocol.CapPrune) {
		t.Fatalf("caps %v", d.capabilities())
	}
	pc := conn(t, d)
	roots := []string{clean, ahead, dirty, pushed, gone, locked, detached + "/", co, outside}
	pc.Write(protocol.Message{Type: protocol.TypeFacts, ID: "f1", Roots: roots})
	res, _ := result(t, pc, "f1")
	if !res.OK || len(res.Facts) != len(roots) {
		t.Fatalf("result %+v", res)
	}
	main := gitOut(t, co, "rev-parse", "origin/main")
	want := []protocol.RootFacts{
		{Root: clean, Branch: "clean", Head: main, Base: "origin/main", Ignored: 1},
		{Root: ahead, Branch: "ahead", Head: gitOut(t, ahead, "rev-parse", "HEAD"), Base: "origin/main", Ahead: 1},
		{Root: dirty, Branch: "dirty", Head: main, Base: "origin/main", Changed: 1},
		{Root: pushed, Branch: "pushed", Head: gitOut(t, pushed, "rev-parse", "HEAD"), Base: "origin/main", Ahead: 1, OnOrigin: true, Pushed: true},
		{Root: gone, Branch: "gone", Head: gitOut(t, gone, "rev-parse", "HEAD"), Base: "origin/main", Ahead: 1},
		{Root: locked, Branch: "locked", Head: main, Base: "origin/main", Locked: true, LockReason: "keep me"},
		{Root: detached + "/", Head: main, Base: "origin/main"},
	}
	for i, w := range want {
		if res.Facts[i] != w {
			t.Errorf("facts %d\n got %+v\nwant %+v", i, res.Facts[i], w)
		}
	}
	for _, f := range res.Facts[len(want):] {
		if !strings.Contains(f.Error, "is not a worktree under the worktrees directory") || f.Head != "" || f.Branch != "" {
			t.Errorf("not a worktree: %+v", f)
		}
	}
	if n := len(want); res.Facts[n].Root != co || res.Facts[n+1].Root != outside {
		t.Errorf("roots out of order: %+v", res.Facts[n:])
	}

	bare := New(Config{Targets: managed(onePane(tmux.Pane{}, nil))})
	pc = conn(t, bare)
	pc.Write(protocol.Message{Type: protocol.TypeFacts, ID: "f2", Roots: roots})
	if res, _ := result(t, pc, "f2"); res.OK || !strings.Contains(res.Error, "no repos and worktrees directories") {
		t.Errorf("no store: %+v", res)
	}
}

// The facts reads end with the connection that asked, so a prune
// interrupted leaves no git running for it here: the connection's
// context ends when it does, and reads waiting on the slots every
// connection shares give up then, each root with the context's error.
func TestFactsEndWithConnection(t *testing.T) {
	c := &clientConn{ctx: context.Background(), quit: make(chan struct{})}
	ctx, cancel := c.context()
	defer cancel()
	close(c.quit)
	select {
	case <-ctx.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("the context outlived its connection")
	}

	// Every slot taken, by another connection's reads say: this one's
	// waits, and gives up when its context ends.
	store, remote := newStore(t)
	_, add := pruneCheckout(t, store, remote)
	root := add("slow")
	slots := make(chan struct{}, 1)
	slots <- struct{}{}
	rctx, rcancel := context.WithCancel(context.Background())
	done := make(chan []protocol.RootFacts, 1)
	d, _ := addDaemon(t, store)
	go func() { done <- d.tasks.readFacts(rctx, slots, []string{root, root}) }()
	select {
	case got := <-done:
		t.Fatalf("read with every slot taken: %+v", got)
	case <-time.After(200 * time.Millisecond):
	}
	rcancel()
	select {
	case got := <-done:
		for _, f := range got {
			if f.Root != root || f.Error != context.Canceled.Error() || f.Head != "" {
				t.Errorf("facts after the context ended: %+v", f)
			}
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the read waited on past its context")
	}
	if len(slots) != 1 {
		t.Errorf("slots taken: %d", len(slots))
	}
}

// rm with head removes only a worktree still at that commit, and with
// delete_branch deletes its branch once the worktree is gone, said in
// the branch stage; a run that removes no worktree, one gone already,
// keeps the branch, since which clone it was in is not known; and
// delete_branch alone is refused before anything is done.
func TestRmHeadAndBranch(t *testing.T) {
	store, remote := newStore(t)
	co, add := pruneCheckout(t, store, remote)
	done, kept := add("done"), add("kept")
	main := gitOut(t, co, "rev-parse", "origin/main")
	d, _ := addDaemon(t, store)
	pc := conn(t, d)

	pc.Write(protocol.Message{Type: protocol.TypeRm, ID: "r0", Repo: remote, Branch: "done", Root: done, DeleteBranch: true})
	if res, _ := result(t, pc, "r0"); res.OK || !strings.Contains(res.Error, "only given the branch and the commit") {
		t.Fatalf("delete_branch without head: %+v", res)
	}
	sh(t, done, "git", "commit", "-q", "--allow-empty", "-m", "since")
	pc.Write(protocol.Message{Type: protocol.TypeRm, ID: "r1", Repo: remote, Branch: "done", Root: done, Head: main, DeleteBranch: true})
	if res, _ := result(t, pc, "r1"); res.OK || !strings.Contains(res.Error, "as it was read; not removed") {
		t.Fatalf("moved HEAD: %+v", res)
	}
	if _, err := os.Stat(done); err != nil {
		t.Fatalf("the worktree went: %v", err)
	}
	head := gitOut(t, done, "rev-parse", "HEAD")
	pc.Write(protocol.Message{Type: protocol.TypeRm, ID: "r2", Repo: remote, Branch: "done", Root: done, Head: head, DeleteBranch: true})
	res, progress := result(t, pc, "r2")
	if !res.OK || !hasProgress(progress, protocol.StageBranch, protocol.StateDone, "deleted branch done") {
		t.Fatalf("rm: %+v %+v", res, progress)
	}
	if _, err := os.Stat(done); err == nil {
		t.Fatal("the worktree is still there")
	}
	if out := gitOut(t, co, "branch", "--list", "done"); out != "" {
		t.Fatalf("branch left: %q", out)
	}

	// Removed by hand: this run removes nothing, and the branch stays.
	sh(t, co, "git", "worktree", "remove", kept)
	pc.Write(protocol.Message{Type: protocol.TypeRm, ID: "r3", Repo: remote, Branch: "kept", Root: kept, Head: main, DeleteBranch: true})
	res, progress = result(t, pc, "r3")
	if !res.OK || !hasProgress(progress, protocol.StageBranch, protocol.StateSkip, "branch kept kept: no worktree of it was removed here") {
		t.Fatalf("rm of a worktree gone: %+v %+v", res, progress)
	}
	if out := gitOut(t, co, "branch", "--list", "kept"); out == "" {
		t.Fatal("the branch went")
	}
}

// prune's rm with unused refuses a worktree something runs in when it
// looks, listing every watched server then rather than trusting the
// last poll: a shell session jump made at its root, also once the
// shell has cd'd out; a pane of the user's default server under it; a
// run; an add at it with no outcome yet; and a server whose panes
// cannot be listed. Each leaves the worktree, the branch and the
// session; the facts say the same as in_use. A pane in a root that
// only begins with this one's, or laatmux's own pane, is no use. A run
// that resolved before rm's look cannot register after it. With
// nothing there the worktree goes.
func TestRmUnused(t *testing.T) {
	store, remote := newStore(t)
	co, add := pruneCheckout(t, store, remote)
	root := add("idle")
	ft, dft := &fakeServer{}, &fakeServer{}
	d := New(Config{
		EnvironmentID: "env", Host: "box",
		Targets: []Target{{Label: "laatmux", Tmux: ft, Managed: true}, {Label: "default", Tmux: dft}},
		Procs:   &fakeProcs{tables: []procTable{{}}},
		Store:   store, Agents: map[string][]string{"claude": {"claude"}},
		Commands: t.TempDir(), Timings: testTimings,
	})
	pc := conn(t, d)
	facts := func() protocol.RootFacts {
		t.Helper()
		pc.Write(protocol.Message{Type: protocol.TypeFacts, ID: "f", Roots: []string{root}})
		res, _ := result(t, pc, "f")
		if !res.OK || len(res.Facts) != 1 || res.Facts[0].Error != "" {
			t.Fatalf("facts %+v", res)
		}
		return res.Facts[0]
	}
	f := facts()
	if f.Changed != 0 || f.Ahead != 0 || f.InUse != "" {
		t.Fatalf("facts %+v", f)
	}
	head := f.Head
	rm := func(id string) protocol.Message {
		t.Helper()
		pc.Write(protocol.Message{Type: protocol.TypeRm, ID: id, Repo: remote, Branch: "idle", Root: root, Head: head, Unused: true, DeleteBranch: true})
		res, _ := result(t, pc, id)
		return res
	}
	refused := func(id, by string) {
		t.Helper()
		if res := rm(id); res.OK || !strings.Contains(res.Error, by) {
			t.Fatalf("%s: %+v, want %q", id, res, by)
		}
		if _, err := os.Stat(root); err != nil {
			t.Fatalf("%s: the worktree went: %v", id, err)
		}
		if gitOut(t, co, "branch", "--list", "idle") == "" {
			t.Fatalf("%s: the branch went", id)
		}
		if len(ft.kills) != 0 {
			t.Fatalf("%s: killed %v", id, ft.kills)
		}
	}

	// The shell jump made at the root has cd'd out: still its session,
	// which rm would kill.
	ft.set(func() {
		ft.panes = []tmux.Pane{{Session: "proj/idle", SessionID: "$7", ID: "%7", Cwd: root, CurrentPath: "/tmp", Managed: true, ServerPID: 5, TTY: "/dev/null"}}
	})
	gen := d.tasks.runGen(root)
	if f := facts(); f.InUse != "pane %7 of session proj/idle on the laatmux server" {
		t.Errorf("facts' in_use: %q", f.InUse)
	}
	refused("r1", "is in use, by pane %7 of session proj/idle on the laatmux server; not removed")
	if err := d.tasks.registerRun(&runJob{root: root}, gen); err == nil || err.Error() != "worktree removed; retry" {
		t.Errorf("a run resolved before rm looked registered after: %v", err)
	}
	// The refusal reopened the root: a run registers, and a session is
	// made there.
	job := &runJob{root: root}
	if err := d.tasks.registerRun(job, d.tasks.runGen(root)); err != nil {
		t.Errorf("a run after a refused rm: %v", err)
	} else {
		d.mu.Lock()
		delete(d.tasks.runs, root)
		d.mu.Unlock()
	}
	pc.Write(protocol.Message{Type: protocol.TypeNew, ID: "n-after", Name: "after", Cwd: root})
	if res, _ := result(t, pc, "n-after"); !res.OK {
		t.Errorf("new after a refused rm: %+v", res)
	}
	// While rm has the root closed, a run that resolves then does not
	// register either.
	reopen := d.tasks.closeRoot(root)
	if err := d.tasks.registerRun(&runJob{root: root}, d.tasks.runGen(root)); err == nil || err.Error() != "worktree being removed; retry" {
		t.Errorf("a run registered in a closed root: %v", err)
	}
	reopen()
	ft.set(func() {
		ft.panes = []tmux.Pane{{Session: "proj/idle", SessionID: "$7", ID: "%7", CurrentPath: root + "/sub", ServerPID: 5, TTY: "/dev/null"}}
	})
	refused("r2", "by pane %7 of session proj/idle on the laatmux server")
	// Made at a symlink to the root, then cd'd out: the resolver,
	// which answers once it has asked the file system, places it.
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(root, alias); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(5 * time.Second); d.paths.resolve(alias) != root; time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("the resolver has not resolved %s", alias)
		}
	}
	ft.set(func() {
		ft.panes = []tmux.Pane{{Session: "proj/idle", SessionID: "$7", ID: "%7", Cwd: alias, CurrentPath: "/tmp", Managed: true, ServerPID: 5, TTY: "/dev/null"}}
	})
	refused("r2a", "by pane %7 of session proj/idle on the laatmux server")
	ft.set(func() { ft.panes = nil })

	dft.set(func() { dft.panes = []tmux.Pane{{Session: "work", ID: "%3", CurrentPath: root}} })
	refused("r3", "by pane %3 of session work on the default server")
	dft.set(func() { dft.listErr = errors.New("tmux: lost server") })
	refused("r4", "the default server's panes: tmux: lost server")
	dft.set(func() {
		dft.listErr = nil
		dft.panes = []tmux.Pane{{Session: "work", ID: "%4", CurrentPath: root + "-2"}, {Session: "work", ID: "%5", CurrentPath: root, Own: true}}
	})

	d.mu.Lock()
	d.tasks.runs[root] = map[*runJob]struct{}{newRunJob(): {}}
	d.mu.Unlock()
	refused("r5", "by a run")
	d.mu.Lock()
	delete(d.tasks.runs, root)
	d.mu.Unlock()
	if err := d.journal.create(entry{ID: "add-1", Root: root, Branch: "idle", Source: remote, Repo: "proj"}); err != nil {
		t.Fatal(err)
	}
	if f := facts(); f.InUse != "add add-1, which has no outcome yet" {
		t.Errorf("facts' in_use: %q", f.InUse)
	}
	refused("r6", "by add add-1, which has no outcome yet")
	if _, err := d.journal.update("add-1", func(e *entry) { e.Result = &protocol.Message{Type: protocol.TypeResult, OK: true} }); err != nil {
		t.Fatal(err)
	}

	// A watched server that is not running has nothing in the root.
	dft.set(func() { dft.listErr = &tmux.Error{Msg: "no server running on /tmp/lmxt/default"} })
	if f := facts(); f.InUse != "" {
		t.Errorf("facts' in_use with a server not running: %q", f.InUse)
	}
	if res := rm("r7"); !res.OK {
		t.Fatalf("rm of an unused worktree: %+v", res)
	}
	if _, err := os.Stat(root); err == nil {
		t.Fatal("the worktree is still there")
	}
}

// new in a worktree prune's rm has closed is refused, and a session it
// made while the root was closed, or once its directory went, is
// killed again and refused, never left in the home directory tmux
// would start it in; a session elsewhere, or in another worktree, is
// made as ever, without waiting on rm.
func TestNewInClosedWorktree(t *testing.T) {
	store, remote := newStore(t)
	_, add := pruneCheckout(t, store, remote)
	root, other := add("closing"), add("open")
	d, ft := addDaemon(t, store)
	pc := conn(t, d)
	newAt := func(id, name, cwd string) protocol.Message {
		t.Helper()
		pc.Write(protocol.Message{Type: protocol.TypeNew, ID: id, Name: name, Cwd: cwd})
		res, _ := result(t, pc, id)
		return res
	}
	reopen := d.tasks.closeRoot(root)
	if res := newAt("n1", "in", root+"/sub"); res.OK || !strings.Contains(res.Error, "is being removed; not made") {
		t.Fatalf("new in a closed worktree: %+v", res)
	}
	for _, k := range []struct{ name, cwd string }{{"elsewhere", t.TempDir()}, {"other", other}} {
		if res := newAt("n-"+k.name, k.name, k.cwd); !res.OK {
			t.Fatalf("new %s while a worktree is closed: %+v", k.name, res)
		}
	}
	reopen()
	if res := newAt("n2", "in", root); !res.OK {
		t.Fatalf("new in the worktree reopened: %+v", res)
	}
	if res := newAt("n3", "gone", store.Dirs.Worktree("proj", "gone")); res.OK || !strings.Contains(res.Error, "no such file or directory") {
		t.Fatalf("new in a worktree directory that is not there: %+v", res)
	}

	// By a symlink to the worktree: closed all the same, and made at
	// the root itself, which rm kills by.
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(root, alias); err != nil {
		t.Fatal(err)
	}
	reopen = d.tasks.closeRoot(root)
	if res := newAt("n4", "by-alias", alias); res.OK || !strings.Contains(res.Error, "is being removed; not made") {
		t.Fatalf("new by a symlink to a closed worktree: %+v", res)
	}
	reopen()
	if res := newAt("n5", "by-alias", alias); !res.OK {
		t.Fatalf("new by a symlink: %+v", res)
	}
	ft.mu.Lock()
	cwd := ft.panes[len(ft.panes)-1].Cwd
	ft.mu.Unlock()
	if cwd != root {
		t.Errorf("made by a symlink at %s, not the root %s", cwd, root)
	}

	// Closed while the session was being made, also when reopened
	// before it was: it goes again.
	for _, k := range []struct {
		name   string
		reopen bool
	}{{"raced", false}, {"reopened", true}} {
		made := 0
		_, err := d.tasks.newInWorktree(context.Background(), root, k.name, func() (tmux.Session, error) {
			s, err := ft.NewSession(context.Background(), tmux.NewSessionOpts{Name: k.name, Cwd: root})
			made++
			reopen = d.tasks.closeRoot(root)
			if k.reopen {
				reopen()
			}
			return s, err
		})
		reopen()
		if err == nil || !strings.Contains(err.Error(), "was closed for removal while its session was made; not made, retry") || made != 1 {
			t.Fatalf("%s: %v, made %d", k.name, err, made)
		}
		if !slices.Contains(ft.killed, k.name) {
			t.Errorf("%s: the session made is left: killed %v", k.name, ft.killed)
		}
	}
	// One whose pane went into another session meanwhile is left, with
	// that session.
	_, err := d.tasks.newInWorktree(context.Background(), root, "moved", func() (tmux.Session, error) {
		s, err := ft.NewSession(context.Background(), tmux.NewSessionOpts{Name: "moved", Cwd: root})
		ft.set(func() { ft.panes[len(ft.panes)-1].Session = "work" })
		d.tasks.closeRoot(root)
		return s, err
	})
	if err == nil || !strings.Contains(err.Error(), "the session made is left: its pane") || !strings.Contains(err.Error(), "is in session work now") {
		t.Fatalf("moved: %v", err)
	}
	if slices.Contains(ft.killed, "work") {
		t.Errorf("killed the session the pane went into: %v", ft.killed)
	}
}

// gitOut is git's output in dir, trimmed.
func gitOut(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %s: %v", strings.Join(args, " "), err)
	}
	return strings.TrimSpace(string(out))
}
