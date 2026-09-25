package daemon

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/laat/laatmux/internal/config"
	"github.com/laat/laatmux/internal/protocol"
	"github.com/laat/laatmux/internal/tmux"
	"github.com/laat/laatmux/internal/worktree"
)

// fakeServer is a managed tmux server with any number of panes. NewSession
// adds a managed pane tagged with the root, as the real one does;
// KillSession removes it.
type fakeServer struct {
	mu     sync.Mutex
	panes  []tmux.Pane
	next   int
	killed []string
	// pastes records every Paste: the buffer, pane and text; pasteErr
	// is returned instead when set; newErr fails NewSession; buffers
	// is what DeleteBuffers was asked to clear.
	pastes    []fakePaste
	pasteErr  error
	pasteHold chan struct{} // when set, Paste blocks until it closes
	newErr    error
	buffers   []string
	cmds      [][]string // the Cmd of every NewSession
	server    int        // ServerPID of the panes made, 5 by default
	screen    []string   // what Capture shows in every pane
	// keys records every SendKeys, pane and keys; onKeys, when set, is
	// called with the fake locked, to change the screen as the agent
	// would.
	keys   [][]string
	onKeys func(f *fakeServer, keys []string)
}

type fakePaste struct{ buffer, pane, text string }

// set changes the fake under its lock, as the tests must while the
// daemon polls it.
func (f *fakeServer) set(change func()) {
	f.mu.Lock()
	defer f.mu.Unlock()
	change()
}

func (f *fakeServer) ListPanes(context.Context) ([]tmux.Pane, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]tmux.Pane(nil), f.panes...), nil
}
func (f *fakeServer) Capture(context.Context, string, int) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.screen...), nil
}
func (f *fakeServer) EnsureConfigured(context.Context) error { return nil }
func (f *fakeServer) SendKeys(_ context.Context, pane string, keys ...string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.keys = append(f.keys, append([]string{pane}, keys...))
	if f.onKeys != nil {
		f.onKeys(f, keys)
	}
	return nil
}
func (f *fakeServer) NewSession(_ context.Context, o tmux.NewSessionOpts) (tmux.Session, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cmds = append(f.cmds, append([]string(nil), o.Cmd...))
	if f.newErr != nil {
		return tmux.Session{}, f.newErr
	}
	f.next++
	id := "%" + strconv.Itoa(f.next)
	server := f.server
	if server == 0 {
		server = 5
	}
	f.panes = append(f.panes, tmux.Pane{Session: o.Name, ID: id, Cwd: o.Cwd, CurrentPath: o.Cwd, Managed: true, Host: o.Host, ServerPID: server, TTY: "/dev/null"})
	return tmux.Session{PaneID: id, ServerPID: server}, nil
}
func (f *fakeServer) KillSession(_ context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.killed = append(f.killed, name)
	kept := f.panes[:0]
	for _, p := range f.panes {
		if p.Session != name {
			kept = append(kept, p)
		}
	}
	f.panes = kept
	return nil
}
func (f *fakeServer) Paste(_ context.Context, buffer, pane, text string) error {
	f.mu.Lock()
	hold := f.pasteHold
	f.mu.Unlock()
	if hold != nil {
		<-hold
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.pasteErr != nil {
		return f.pasteErr
	}
	f.pastes = append(f.pastes, fakePaste{buffer, pane, text})
	return nil
}
func (f *fakeServer) DeleteBuffers(_ context.Context, prefix string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.buffers = append(f.buffers, prefix)
	return nil
}

// newStore makes a bare remote with one commit and a store with empty
// repos and worktrees directories. The remote's .laatmux.yaml copies
// .envrc and runs one setup command.
func newStore(t *testing.T) (*worktree.Store, string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	base := t.TempDir()
	if runtime.GOOS == "darwin" {
		if real, err := filepath.EvalSymlinks(base); err == nil {
			base = real
		}
	}
	remote := filepath.Join(base, "remote.git")
	seed := filepath.Join(base, "seed")
	sh := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	sh(base, "git", "init", "-q", "--bare", "--initial-branch=main", remote)
	sh(base, "git", "init", "-q", "--initial-branch=main", seed)
	sh(seed, "git", "config", "user.email", "t@example.com")
	sh(seed, "git", "config", "user.name", "t")
	os.WriteFile(filepath.Join(seed, config.SetupFile), []byte("copy: [.envrc]\nsetup: [\"echo ran >> log\"]\n"), 0o644)
	sh(seed, "git", "add", ".")
	sh(seed, "git", "commit", "-q", "-m", "init")
	sh(seed, "git", "push", "-q", remote, "main")
	dirs := config.Dirs{Repos: filepath.Join(base, "repos"), Worktrees: filepath.Join(base, "worktrees")}
	return worktree.New(dirs, []config.Repo{{Source: remote, Name: "proj"}}), remote
}

// conn opens a client connection to d, hello done.
func conn(t *testing.T, d *Daemon) *protocol.Conn {
	t.Helper()
	server, client := net.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go d.HandleConn(ctx, server, func() { server.Close() })
	pc := protocol.NewConn(client)
	client.SetDeadline(time.Now().Add(60 * time.Second))
	if m, err := pc.Read(); err != nil || m.Type != protocol.TypeHello {
		t.Fatalf("hello: %+v %v", m, err)
	}
	return pc
}

// result reads until the result for id, collecting progress on the way.
func result(t *testing.T, pc *protocol.Conn, id string) (protocol.Message, []protocol.Message) {
	t.Helper()
	var progress []protocol.Message
	for {
		m, err := pc.Read()
		if err != nil {
			t.Fatalf("read: %v (after %d progress messages)", err, len(progress))
		}
		if m.ID != id {
			continue
		}
		switch m.Type {
		case protocol.TypeProgress:
			progress = append(progress, m)
		case protocol.TypeResult:
			return m, progress
		}
	}
}

func hasProgress(ps []protocol.Message, stage, state, detailPrefix string) bool {
	for _, p := range ps {
		if p.Stage == stage && p.State == state && strings.HasPrefix(p.Detail, detailPrefix) {
			return true
		}
	}
	return false
}

func newAddDaemon(t *testing.T) (*Daemon, *fakeServer, *worktree.Store, string) {
	store, remote := newStore(t)
	ft := &fakeServer{}
	d := New(Config{
		EnvironmentID: "env", Host: "box",
		Targets: []Target{{Label: "laatmux", Tmux: ft, Managed: true}},
		Procs:   &fakeProcs{tables: []procTable{{}}},
		Store:   store, Agents: map[string][]string{"claude": {"claude"}},
		Commands: t.TempDir(),
	})
	t.Cleanup(func() {
		// The trust watchers an add starts end with the test.
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		d.StopRuns(ctx)
	})
	return d, ft, store, remote
}

func TestCapabilitiesNeedStoreAndManaged(t *testing.T) {
	store, _ := newStore(t)
	d := New(Config{Store: store, Targets: unmanaged(&fakeTmux{})})
	caps := d.capabilities()
	if !protocol.Has(caps, protocol.CapWorktrees) || protocol.Has(caps, protocol.CapAdd) || protocol.Has(caps, protocol.CapRm) {
		t.Fatalf("caps %v", caps)
	}
	d = New(Config{Targets: managed(&fakeTmux{})})
	if caps := d.capabilities(); protocol.Has(caps, protocol.CapWorktrees) || protocol.Has(caps, protocol.CapAdd) {
		t.Fatalf("caps %v", caps)
	}
	d = New(Config{Store: store, Targets: managed(&fakeTmux{})})
	if caps := d.capabilities(); !protocol.Has(caps, protocol.CapAdd) || !protocol.Has(caps, protocol.CapRm) {
		t.Fatalf("caps %v", caps)
	}
}

func TestAddThenRm(t *testing.T) {
	d, ft, store, remote := newAddDaemon(t)
	ctx := context.Background()
	pc := conn(t, d)

	// add: every stage runs, the session is proj/<encoded branch>, and
	// the pane is tagged with the root git registered.
	if err := pc.Write(protocol.Message{Type: protocol.TypeAdd, ID: "c1", Repo: remote, Branch: "fix/v1.2", AgentName: "claude"}); err != nil {
		t.Fatal(err)
	}
	res, progress := result(t, pc, "c1")
	if !res.OK {
		t.Fatalf("add failed at %s: %s", res.Stage, res.Error)
	}
	root := store.Dirs.Worktree("proj", "fix/v1.2")
	if res.Root != root || res.Session != "proj/fix/v1%2e2" || res.PaneID != "%1" {
		t.Fatalf("result %+v", res)
	}
	if len(ft.panes) != 1 || ft.panes[0].Cwd != root || ft.panes[0].Host != "box" {
		t.Fatalf("panes %+v", ft.panes)
	}
	for _, want := range [][3]string{
		{protocol.StageResolve, protocol.StateDone, "checkout"},
		{protocol.StageClone, protocol.StateDone, "cloned"},
		{protocol.StageWorktree, protocol.StateDone, "worktree at " + root},
		{protocol.StageSetup, protocol.StateDone, "echo ran >> log"},
		{protocol.StageAgent, protocol.StateDone, "session proj/fix/v1%2e2 pane %1"},
	} {
		if !hasProgress(progress, want[0], want[1], want[2]) {
			t.Errorf("missing %v in %+v", want, progress)
		}
	}
	if !hasProgress(progress, protocol.StageSetup, protocol.StateOutput, "") {
		// echo writes to a file; the marker is what proves it ran.
		if _, err := os.Stat(filepath.Join(root, "log")); err != nil {
			t.Fatal("setup did not run")
		}
	}

	// The record follows: git lists the worktree and the managed pane
	// names its session, without waiting for the ticker.
	if err := d.poll(ctx); err != nil {
		t.Fatal(err)
	}
	d.pollWorktrees(ctx)
	wts := d.Worktrees()
	if len(wts) != 1 || wts[0].Root != root || wts[0].Branch != "fix/v1.2" || wts[0].Repo != "proj" || wts[0].Session != "proj/fix/v1%2e2" || wts[0].ID != "env/worktree/"+root {
		t.Fatalf("worktrees %+v", wts)
	}

	// A repeat with the same id replays the finished result; a repeat
	// with a new id skips every stage, the agent one by root.
	pc.Write(protocol.Message{Type: protocol.TypeAdd, ID: "c1", Repo: remote, Branch: "fix/v1.2", AgentName: "claude"})
	if again, _ := result(t, pc, "c1"); !again.OK || again.PaneID != "%1" {
		t.Fatalf("replay %+v", again)
	}
	pc.Write(protocol.Message{Type: protocol.TypeAdd, ID: "c2", Repo: "proj", Branch: "fix/v1.2", AgentName: "claude"})
	res2, progress2 := result(t, pc, "c2")
	if !res2.OK || res2.PaneID != "%1" || len(ft.panes) != 1 {
		t.Fatalf("second add %+v panes %+v", res2, ft.panes)
	}
	for _, p := range progress2 {
		// Only fetch runs again; it is never skipped.
		if p.State == protocol.StateStart && p.Stage != protocol.StageFetch {
			t.Errorf("second add started %+v", p)
		}
	}
	if !hasProgress(progress2, protocol.StageAgent, protocol.StateSkip, "session proj/fix/v1%2e2 runs in "+root) {
		t.Fatalf("agent stage not skipped: %+v", progress2)
	}

	// rm by repo and branch, root along. Setup left an untracked file, so
	// git refuses without force, with its own message, and the session
	// is left running; with force the worktree goes and the session whose
	// pane records the root is killed.
	pc.Write(protocol.Message{Type: protocol.TypeRm, ID: "r0", Repo: remote, Branch: "fix/v1.2", Root: root})
	if res, _ := result(t, pc, "r0"); res.OK || !strings.Contains(res.Error, "untracked files") || len(ft.panes) != 1 {
		t.Fatalf("unforced rm: %+v panes %+v", res, ft.panes)
	}
	pc.Write(protocol.Message{Type: protocol.TypeRm, ID: "r1", Repo: remote, Branch: "fix/v1.2", Root: root, Force: true})
	if res, _ := result(t, pc, "r1"); !res.OK {
		t.Fatalf("rm: %s", res.Error)
	}
	if _, err := os.Stat(root); err == nil {
		t.Fatal("root still exists")
	}
	if len(ft.killed) != 1 || ft.killed[0] != "proj/fix/v1%2e2" || len(ft.panes) != 0 {
		t.Fatalf("killed %v panes %+v", ft.killed, ft.panes)
	}
	d.poll(ctx)
	d.pollWorktrees(ctx)
	if wts := d.Worktrees(); len(wts) != 0 {
		t.Fatalf("worktrees after rm %+v", wts)
	}
	// A repeat rm is a no-op and ok.
	pc.Write(protocol.Message{Type: protocol.TypeRm, ID: "r2", Repo: remote, Branch: "fix/v1.2", Root: root})
	if res, _ := result(t, pc, "r2"); !res.OK {
		t.Fatalf("repeat rm: %s", res.Error)
	}
}

// A session with the intended name whose pane records another root is a
// name in use; nothing is adopted and the worktree is left for a retry.
func TestAddNameInUse(t *testing.T) {
	d, ft, store, remote := newAddDaemon(t)
	ft.panes = []tmux.Pane{{Session: "proj/task", ID: "%9", Cwd: "/elsewhere", Managed: true}}
	pc := conn(t, d)
	pc.Write(protocol.Message{Type: protocol.TypeAdd, ID: "c1", Repo: remote, Branch: "task", Cmd: []string{"sleep", "1"}})
	res, _ := result(t, pc, "c1")
	if res.OK || res.Stage != protocol.StageAgent || !strings.Contains(res.Error, "name in use") {
		t.Fatalf("result %+v", res)
	}
	if res.Root != store.Dirs.Worktree("proj", "task") {
		t.Fatalf("root %s", res.Root)
	}
	if _, err := os.Stat(res.Root); err != nil {
		t.Fatal("worktree not left for a retry")
	}
	// Unknown repository and agent fail at resolve before anything runs.
	pc.Write(protocol.Message{Type: protocol.TypeAdd, ID: "c2", Repo: "nope", Branch: "task", AgentName: "claude"})
	if res, _ := result(t, pc, "c2"); res.OK || res.Stage != protocol.StageResolve || !strings.Contains(res.Error, "unknown repository") {
		t.Fatalf("result %+v", res)
	}
	pc.Write(protocol.Message{Type: protocol.TypeAdd, ID: "c3", Repo: remote, Branch: "task", AgentName: "nope"})
	if res, _ := result(t, pc, "c3"); res.OK || res.Stage != protocol.StageResolve || !strings.Contains(res.Error, "unknown agent") {
		t.Fatalf("result %+v", res)
	}
}

// A second connection sending the id of a running add attaches to its
// stream and gets the whole of it; the command outlives the connection
// that started it.
func TestAddFollowsAcrossConnections(t *testing.T) {
	d, _, _, remote := newAddDaemon(t)
	first := conn(t, d)
	first.Write(protocol.Message{Type: protocol.TypeAdd, ID: "c1", Repo: remote, Branch: "task", Cmd: []string{"true"}})
	// Read one progress message, then walk away.
	if m, err := first.Read(); err != nil || m.Type != protocol.TypeProgress {
		t.Fatalf("first progress: %+v %v", m, err)
	}
	second := conn(t, d)
	second.Write(protocol.Message{Type: protocol.TypeAdd, ID: "c1", Repo: remote, Branch: "task", Cmd: []string{"true"}})
	res, progress := result(t, second, "c1")
	if !res.OK {
		t.Fatalf("add failed at %s: %s", res.Stage, res.Error)
	}
	if !hasProgress(progress, protocol.StageResolve, protocol.StateDone, "checkout") {
		t.Fatalf("replay missed the start: %+v", progress)
	}
}

// Worktree records come from git joined with the managed panes, and a
// worktree whose directory was deleted by hand is not published.
func TestWorktreeRecords(t *testing.T) {
	d, ft, store, remote := newAddDaemon(t)
	ctx := context.Background()
	repo, _ := store.Repo(remote)
	added, err := store.Add(ctx, repo, "task", nil)
	if err != nil {
		t.Fatal(err)
	}
	pc := conn(t, d)
	pc.Write(protocol.Message{Type: protocol.TypeSubscribe})
	// Snapshot waits for both the pane poll and the git poll.
	got := make(chan protocol.Message, 8)
	go func() {
		for {
			m, err := pc.Read()
			if err != nil {
				return
			}
			got <- m
		}
	}()
	select {
	case m := <-got:
		t.Fatalf("snapshot before discovery: %+v", m)
	case <-time.After(100 * time.Millisecond):
	}
	if err := d.poll(ctx); err != nil {
		t.Fatal(err)
	}
	d.markDiscovered(&d.panesDiscovered)
	d.pollWorktrees(ctx)
	snap := <-got
	if snap.Type != protocol.TypeSnapshot || len(snap.Worktrees) != 1 || snap.Worktrees[0].Root != added.Root || snap.Worktrees[0].Session != "" {
		t.Fatalf("snapshot %+v", snap)
	}
	// A managed pane on the root names the session, from the pane poll
	// alone.
	ft.panes = []tmux.Pane{{Session: "proj/task", ID: "%1", Cwd: added.Root, Managed: true, ServerPID: 5, TTY: "/dev/null"}}
	if err := d.poll(ctx); err != nil {
		t.Fatal(err)
	}
	up := <-got
	if up.Type != protocol.TypeUpsert || up.Worktree == nil || up.Worktree.Session != "proj/task" {
		t.Fatalf("upsert %+v", up)
	}
	// The pane goes: the session field clears.
	ft.panes = nil
	d.poll(ctx)
	if up := <-got; up.Worktree == nil || up.Worktree.Session != "" {
		t.Fatalf("upsert %+v", up)
	}
	// The directory goes: git calls it prunable and the record is removed.
	os.RemoveAll(added.Root)
	d.pollWorktrees(ctx)
	if rm := <-got; rm.Type != protocol.TypeRemove || rm.WorktreeID != "env/worktree/"+added.Root {
		t.Fatalf("remove %+v", rm)
	}
}

// rm by repo and branch only touches worktrees under the worktrees
// directory: a worktree the user made elsewhere on that branch is not
// the daemon's to remove, and the result is ok with nothing done.
func TestRmLeavesExternalWorktree(t *testing.T) {
	d, _, store, remote := newAddDaemon(t)
	ctx := context.Background()
	repo, _ := store.Repo(remote)
	if _, err := store.Add(ctx, repo, "first", nil); err != nil {
		t.Fatal(err)
	}
	checkout, _, _ := store.Checkout(ctx, repo)
	elsewhere := filepath.Join(filepath.Dir(store.Dirs.Repos), "elsewhere")
	cmd := exec.Command("git", "worktree", "add", "-q", "-b", "outside", elsewhere, "main")
	cmd.Dir = checkout
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	pc := conn(t, d)
	pc.Write(protocol.Message{Type: protocol.TypeRm, ID: "r1", Repo: remote, Branch: "outside", Force: true})
	if res, _ := result(t, pc, "r1"); !res.OK || res.Root != "" {
		t.Fatalf("rm: %+v", res)
	}
	if _, err := os.Stat(elsewhere); err != nil {
		t.Fatal("external worktree removed")
	}
	pc.Write(protocol.Message{Type: protocol.TypeRm, ID: "r2", Root: elsewhere, Force: true})
	if res, _ := result(t, pc, "r2"); res.OK || !strings.Contains(res.Error, "not under the worktrees directory") {
		t.Fatalf("rm by root: %+v", res)
	}
	if _, err := os.Stat(elsewhere); err != nil {
		t.Fatal("external worktree removed by root")
	}
}

// A finished command is forgotten after its TTL without another command
// arriving, and the id is then fresh again.
func TestCommandEviction(t *testing.T) {
	d, _, _, remote := newAddDaemon(t)
	d.commandTTL = 50 * time.Millisecond
	pc := conn(t, d)
	pc.Write(protocol.Message{Type: protocol.TypeAdd, ID: "c1", Repo: remote, Branch: "task", Cmd: []string{"true"}})
	if res, _ := result(t, pc, "c1"); !res.OK {
		t.Fatalf("add: %+v", res)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		d.mu.Lock()
		_, kept := d.cmds["c1"]
		d.mu.Unlock()
		if !kept {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("command not evicted after its TTL")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, fresh := d.command("c1", nil); !fresh {
		t.Fatal("evicted id not fresh")
	}
}

// repo, branch and root on rm must agree: a root registered for another
// branch is a mismatch, not a target, whether the branch is registered
// elsewhere or not at all.
func TestRmMismatchRefused(t *testing.T) {
	d, ft, store, remote := newAddDaemon(t)
	pc := conn(t, d)
	for _, b := range []string{"one", "two"} {
		pc.Write(protocol.Message{Type: protocol.TypeAdd, ID: "add-" + b, Repo: remote, Branch: b, Cmd: []string{"true"}})
		if res, _ := result(t, pc, "add-"+b); !res.OK {
			t.Fatalf("add %s: %+v", b, res)
		}
	}
	one, two := store.Dirs.Worktree("proj", "one"), store.Dirs.Worktree("proj", "two")
	pc.Write(protocol.Message{Type: protocol.TypeRm, ID: "r1", Repo: remote, Branch: "one", Root: two, Force: true})
	if res, _ := result(t, pc, "r1"); res.OK || !strings.Contains(res.Error, two+" is the worktree for branch two of proj, not one") {
		t.Fatalf("rm one at two: %+v", res)
	}
	// A root with no registration, for a branch registered elsewhere.
	none := store.Dirs.Worktree("proj", "none")
	pc.Write(protocol.Message{Type: protocol.TypeRm, ID: "r1b", Repo: remote, Branch: "one", Root: none, Force: true})
	if res, _ := result(t, pc, "r1b"); res.OK || !strings.Contains(res.Error, "checked out at "+one+", not "+none) {
		t.Fatalf("rm one at none: %+v", res)
	}
	pc.Write(protocol.Message{Type: protocol.TypeRm, ID: "r2", Repo: remote, Branch: "gone", Root: two, Force: true})
	if res, _ := result(t, pc, "r2"); res.OK || !strings.Contains(res.Error, "worktree for branch two") {
		t.Fatalf("rm gone at two: %+v", res)
	}
	for _, root := range []string{one, two} {
		if _, err := os.Stat(root); err != nil {
			t.Fatalf("%s removed by a mismatched rm", root)
		}
	}
	if len(ft.panes) != 2 {
		t.Fatalf("panes %+v", ft.panes)
	}
	// Agreeing fields remove; a repeat with the registration gone but
	// the root retained still finds nothing to kill and is ok.
	pc.Write(protocol.Message{Type: protocol.TypeRm, ID: "r3", Repo: remote, Branch: "one", Root: one, Force: true})
	if res, _ := result(t, pc, "r3"); !res.OK || res.Root != one {
		t.Fatalf("rm one: %+v", res)
	}
	pc.Write(protocol.Message{Type: protocol.TypeRm, ID: "r4", Repo: remote, Branch: "one", Root: one, Force: true})
	if res, _ := result(t, pc, "r4"); !res.OK || len(ft.panes) != 1 || ft.panes[0].Cwd != two {
		t.Fatalf("repeat rm one: %+v panes %+v", res, ft.panes)
	}
}

// A checkout git cannot read is a failed lookup, not an absent worktree:
// rm reports the error and leaves the session running, since nothing is
// killed until git has removed the worktree.
func TestRmLookupErrorKeepsSession(t *testing.T) {
	d, ft, store, remote := newAddDaemon(t)
	pc := conn(t, d)
	pc.Write(protocol.Message{Type: protocol.TypeAdd, ID: "a", Repo: remote, Branch: "task", Cmd: []string{"true"}})
	res, _ := result(t, pc, "a")
	if !res.OK {
		t.Fatalf("add: %+v", res)
	}
	repo, _ := store.Repo(remote)
	checkout, _, _ := store.Checkout(context.Background(), repo)
	if err := os.WriteFile(filepath.Join(checkout, ".git", "config"), []byte("[core\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, m := range []protocol.Message{
		{Type: protocol.TypeRm, ID: "r1", Root: res.Root},
		{Type: protocol.TypeRm, ID: "r2", Repo: remote, Branch: "task", Root: res.Root},
	} {
		pc.Write(m)
		if r, _ := result(t, pc, m.ID); r.OK || !strings.Contains(r.Error, "config") {
			t.Fatalf("rm %s: %+v", m.ID, r)
		}
	}
	if len(ft.panes) != 1 {
		t.Fatal("session killed after a failed lookup")
	}
}

// Retained output is bounded: past the budget, lines are dropped after
// one saying so, while step and result messages are always kept.
func TestCommandOutputBounded(t *testing.T) {
	c := newCommand("x")
	line := strings.Repeat("x", 1024)
	for i := 0; i < 2*maxOutput/len(line); i++ {
		c.emit(protocol.Message{Type: protocol.TypeProgress, Stage: "setup", State: protocol.StateOutput, Detail: line})
	}
	c.emit(protocol.Message{Type: protocol.TypeProgress, Stage: "setup", State: protocol.StateDone, Detail: "cmd"})
	c.emit(protocol.Message{Type: protocol.TypeResult, OK: true})
	n := len(c.events)
	if n != maxOutput/cost(line)+2 || !strings.Contains(c.events[n-2].Detail, "dropped") || c.events[n-1].State != protocol.StateDone || !c.done || c.result.Type != protocol.TypeResult {
		t.Fatalf("%d events, tail %+v", n, c.events[n-2:])
	}
	for i, e := range c.events {
		if e.N != uint64(i+1) {
			t.Fatalf("event %d numbered %d", i, e.N)
		}
	}
}

// A worktree whose directory was deleted by hand keeps a prunable
// registration and, here, a session. rm by repo and branch, with or
// without the root, prunes the registration and kills the session.
func TestRmPrunableWorktree(t *testing.T) {
	d, ft, _, remote := newAddDaemon(t)
	pc := conn(t, d)
	for _, b := range []string{"one", "two"} {
		pc.Write(protocol.Message{Type: protocol.TypeAdd, ID: "add-" + b, Repo: remote, Branch: b, Cmd: []string{"true"}})
		if res, _ := result(t, pc, "add-"+b); !res.OK {
			t.Fatalf("add %s: %+v", b, res)
		}
	}
	roots := map[string]string{}
	for _, p := range ft.panes {
		roots[p.Session] = p.Cwd
		os.RemoveAll(p.Cwd)
	}
	pc.Write(protocol.Message{Type: protocol.TypeRm, ID: "r1", Repo: remote, Branch: "one", Root: roots["proj/one"]})
	if res, _ := result(t, pc, "r1"); !res.OK || res.Root != roots["proj/one"] {
		t.Fatalf("rm one: %+v", res)
	}
	pc.Write(protocol.Message{Type: protocol.TypeRm, ID: "r2", Repo: remote, Branch: "two"})
	if res, _ := result(t, pc, "r2"); !res.OK || res.Root != roots["proj/two"] {
		t.Fatalf("rm two: %+v", res)
	}
	if len(ft.panes) != 0 || len(ft.killed) != 2 {
		t.Fatalf("panes %+v killed %v", ft.panes, ft.killed)
	}
	d.pollWorktrees(context.Background())
	if wts := d.Worktrees(); len(wts) != 0 {
		t.Fatalf("worktrees %+v", wts)
	}
}

// rm by a root outside the worktrees directory is refused before the
// session step: a session new made with that cwd is not rm's to kill.
func TestRmRootOutsideRefused(t *testing.T) {
	d, ft, store, remote := newAddDaemon(t)
	ft.panes = []tmux.Pane{{Session: "scratch", ID: "%7", Cwd: "/tmp/scratch", Managed: true}}
	pc := conn(t, d)
	for _, m := range []protocol.Message{
		{Type: protocol.TypeRm, ID: "r1", Root: "/tmp/scratch"},
		{Type: protocol.TypeRm, ID: "r2", Repo: remote, Branch: "x", Root: "/tmp/scratch"},
		{Type: protocol.TypeRm, ID: "r3", Root: store.Dirs.Worktrees + "/../scratch"},
		{Type: protocol.TypeRm, ID: "r4", Root: "relative"},
	} {
		pc.Write(m)
		if res, _ := result(t, pc, m.ID); res.OK || !strings.Contains(res.Error, "not under the worktrees directory") {
			t.Fatalf("rm %s: %+v", m.ID, res)
		}
	}
	if len(ft.panes) != 1 {
		t.Fatal("session outside the worktrees directory killed")
	}
}

// rm holds every repository's lock while it resolves, since the root
// checks ask every checkout: an add in flight on another repository
// blocks it until done.
func TestRmWaitsForOtherRepositories(t *testing.T) {
	d, _, store, remote := newAddDaemon(t)
	// An add holds another repository's lock, one no config lists, as
	// an add from a repository entry does.
	unlockOther := d.lockRepo("/nowhere/other.git", "other")
	pc := conn(t, d)
	pc.Write(protocol.Message{Type: protocol.TypeRm, ID: "r1", Repo: remote, Branch: "task", Root: store.Dirs.Worktree("proj", "task")})
	got := make(chan protocol.Message, 1)
	go func() {
		res, _ := result(t, pc, "r1")
		got <- res
	}()
	select {
	case res := <-got:
		t.Fatalf("rm finished while another repository was locked: %+v", res)
	case <-time.After(300 * time.Millisecond):
	}
	unlockOther()
	select {
	case res := <-got:
		if !res.OK {
			t.Fatalf("rm: %+v", res)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("rm did not finish after the lock was released")
	}
}

// An add that brings its repository entry runs for a repository this
// host's config does not list: cloned under the entry's name, with the
// entry's setup, listed by its checkout, and removed by rm naming its
// source. An entry that does not match the add, or whose name could
// not place a directory, is refused at resolve; without an entry the
// host's own config decides, as before.
func TestAddFromRepoEntry(t *testing.T) {
	d, _, store, remote := newAddDaemon(t)
	store.Repos = nil
	ctx := context.Background()
	pc := conn(t, d)
	if !protocol.Has(d.capabilities(), protocol.CapRepoEntry) {
		t.Fatalf("caps %v", d.capabilities())
	}
	for id, m := range map[string]protocol.Message{
		"none":     {Repo: remote},
		"mismatch": {Repo: remote, RepoEntry: &protocol.RepoEntry{Source: "/elsewhere.git", Name: "sent"}},
		"name":     {Repo: remote, RepoEntry: &protocol.RepoEntry{Source: remote, Name: "../sent"}},
		"copy":     {Repo: remote, RepoEntry: &protocol.RepoEntry{Source: remote, Name: "sent", Copy: []string{"../x"}}},
		"setup":    {Repo: remote, RepoEntry: &protocol.RepoEntry{Source: remote, Name: "sent", Setup: []string{" "}}},
	} {
		m.Type, m.ID, m.Branch, m.AgentName = protocol.TypeAdd, id, "task", "claude"
		pc.Write(m)
		if res, _ := result(t, pc, id); res.OK || res.Stage != protocol.StageResolve {
			t.Fatalf("%s: %+v", id, res)
		}
	}
	if _, err := os.Stat(store.Dirs.Repos); err == nil {
		t.Fatal("a refused add touched the repos directory")
	}

	entry := &protocol.RepoEntry{Source: remote, Name: "sent", Setup: []string{"echo sent >> log"}}
	pc.Write(protocol.Message{Type: protocol.TypeAdd, ID: "a1", Repo: remote, RepoEntry: entry, Branch: "task", AgentName: "claude"})
	res, _ := result(t, pc, "a1")
	root := store.Dirs.Worktree("sent", "task")
	if !res.OK || res.Root != root {
		t.Fatalf("add: %+v", res)
	}
	if b, _ := os.ReadFile(filepath.Join(root, "log")); string(b) != "ran\nsent\n" {
		t.Fatalf("setup: %q", b)
	}
	if _, err := os.Stat(filepath.Join(store.Dirs.Repos, "sent", ".git")); err != nil {
		t.Fatal("not cloned under the entry's name")
	}
	d.pollWorktrees(ctx)
	if wts := d.Worktrees(); len(wts) != 1 || wts[0].Root != root || wts[0].Repo != "sent" || wts[0].Source != remote {
		t.Fatalf("worktrees %+v", wts)
	}
	pc.Write(protocol.Message{Type: protocol.TypeRm, ID: "r1", Repo: remote, Branch: "task", Root: root, Force: true})
	if res, _ := result(t, pc, "r1"); !res.OK {
		t.Fatalf("rm: %+v", res)
	}
	if _, err := os.Stat(root); err == nil {
		t.Fatal("root still exists")
	}
}

// A repository the host's config lists is the host's own, entry or
// not: with no checkout yet, it is cloned from the host's source, the
// transport the host can use, under the host's name and with the host's
// steps, even when the entry names another form of it. An entry whose name is the host's
// name for another repository is refused.
func TestAddEntryForListedRepository(t *testing.T) {
	d, _, store, remote := newAddDaemon(t)
	https, ssh := "https://example.com/o/proj", "git@example.com:o/proj.git"
	global := filepath.Join(t.TempDir(), "gitconfig")
	// Only the host's form reaches the remote: a clone from the entry's
	// form fails.
	os.WriteFile(global, []byte(fmt.Sprintf("[url %q]\n\tinsteadOf = %s\n", remote, https)), 0o644)
	t.Setenv("GIT_CONFIG_GLOBAL", global)
	store.Repos = []worktree.Repo{{Source: https, Name: "proj", Setup: []string{"echo host >> log"}}}
	pc := conn(t, d)
	entry := &protocol.RepoEntry{Source: ssh, Name: "sent", Setup: []string{"echo sent >> log"}}
	pc.Write(protocol.Message{Type: protocol.TypeAdd, ID: "a1", Repo: ssh, RepoEntry: entry, Branch: "task", AgentName: "claude"})
	res, _ := result(t, pc, "a1")
	if !res.OK || res.Root != store.Dirs.Worktree("proj", "task") {
		t.Fatalf("add: %+v", res)
	}
	// The host's own steps ran, the entry's did not.
	if b, _ := os.ReadFile(filepath.Join(res.Root, "log")); string(b) != "ran\nhost\n" {
		t.Fatalf("setup: %q", b)
	}
	out, err := exec.Command("git", "-C", store.Dirs.Checkout("proj"), "config", "--get", "remote.origin.url").Output()
	if err != nil || strings.TrimSpace(string(out)) != https {
		t.Fatalf("origin %q %v", out, err)
	}
	pc.Write(protocol.Message{Type: protocol.TypeAdd, ID: "a2", Repo: "/elsewhere.git", RepoEntry: &protocol.RepoEntry{Source: "/elsewhere.git", Name: "proj"}, Branch: "task", AgentName: "claude"})
	if res, _ := result(t, pc, "a2"); res.OK || !strings.Contains(res.Error, "this host's name for "+https) {
		t.Fatalf("name taken: %+v", res)
	}
}

// rm of a repository no config lists: its root reaches the session
// when the checkout is gone, and with two clones of it the checkout
// that registers the root is the one git removes from, dirty
// protection and all.
func TestRmUnlistedRepository(t *testing.T) {
	d, ft, store, remote := newAddDaemon(t)
	store.Repos = nil
	pc := conn(t, d)
	entry := &protocol.RepoEntry{Source: remote, Name: "sent"}
	pc.Write(protocol.Message{Type: protocol.TypeAdd, ID: "a1", Repo: remote, RepoEntry: entry, Branch: "task", AgentName: "claude"})
	res, _ := result(t, pc, "a1")
	if !res.OK {
		t.Fatalf("add: %+v", res)
	}
	root := res.Root

	// A second clone of the repository, with a worktree of its own.
	second := filepath.Join(store.Dirs.Repos, "sent2")
	if out, err := exec.Command("git", "clone", "-q", remote, second).CombinedOutput(); err != nil {
		t.Fatalf("clone: %v %s", err, out)
	}
	topic := store.Dirs.Worktree("sent2", "topic")
	if out, err := exec.Command("git", "-C", second, "worktree", "add", "-q", "-b", "topic", topic).CombinedOutput(); err != nil {
		t.Fatalf("worktree: %v %s", err, out)
	}
	os.WriteFile(filepath.Join(topic, "dirty"), []byte("x"), 0o644)
	pc.Write(protocol.Message{Type: protocol.TypeRm, ID: "r1", Repo: remote, Branch: "topic", Root: topic})
	if res, _ := result(t, pc, "r1"); res.OK || !strings.Contains(res.Error, "untracked") {
		t.Fatalf("dirty rm in the second clone: %+v", res)
	}
	pc.Write(protocol.Message{Type: protocol.TypeRm, ID: "r2", Repo: remote, Branch: "topic", Root: topic, Force: true})
	if res, _ := result(t, pc, "r2"); !res.OK {
		t.Fatalf("rm in the second clone: %+v", res)
	}
	if _, err := os.Stat(topic); err == nil {
		t.Fatal("the second clone's worktree is still there")
	}

	// Both checkouts gone by hand: the root still finds the session.
	for _, dir := range []string{store.Dirs.Checkout("sent"), second} {
		if err := os.RemoveAll(dir); err != nil {
			t.Fatal(err)
		}
	}
	if len(ft.panes) != 1 || ft.panes[0].Cwd != root {
		t.Fatalf("panes %+v", ft.panes)
	}
	pc.Write(protocol.Message{Type: protocol.TypeRm, ID: "r3", Repo: remote, Branch: "task", Root: root, Force: true})
	if res, _ := result(t, pc, "r3"); !res.OK || len(ft.panes) != 0 {
		t.Fatalf("rm with no checkout: %+v panes %+v", res, ft.panes)
	}
	pc.Write(protocol.Message{Type: protocol.TypeRm, ID: "r4", Repo: remote, Branch: "task"})
	if res, _ := result(t, pc, "r4"); res.OK || !strings.Contains(res.Error, "no checkout of it here") {
		t.Fatalf("rm with no checkout and no root: %+v", res)
	}
}
