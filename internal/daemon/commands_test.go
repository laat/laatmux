package daemon

import (
	"context"
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/laat/laatmux/internal/config"
	"github.com/laat/laatmux/internal/gittest"
	"github.com/laat/laatmux/internal/procs"
	"github.com/laat/laatmux/internal/protocol"
	"github.com/laat/laatmux/internal/tmux"
	"github.com/laat/laatmux/internal/worktree"
)

// fakeServer is a tmux server, the managed one or the user's, with any
// number of panes. NewSession adds a managed pane tagged with the root,
// as the real one does, under a pane id and a session id no listed pane
// has; KillSessionID removes the session's panes on the server it is
// given and records its name in killed.
type fakeServer struct {
	mu       sync.Mutex
	panes    []tmux.Pane
	next     int // the last pane number given
	sessions int // the last session number given
	killed   []string
	kills    []string // every id KillSessionID was asked for
	// pastes records every Paste: the buffer, pane and text; pasteErr
	// is returned instead when set; newErr fails NewSession; buffers
	// is what DeleteBuffers was asked to clear.
	pastes    []fakePaste
	pasteErr  error
	pasteHold chan struct{} // when set, Paste blocks until it closes
	newErr    error
	newHold   chan struct{} // when set, NewSession blocks until it closes or ctx ends
	buffers   []string
	selected  []string   // panes SelectPane was asked for
	cmds      [][]string // the Cmd of every NewSession
	server    int        // ServerPID of the panes made, 5 by default
	screen    []string   // what Capture shows in every pane
	// keys records every SendKeys, pane and keys; onKeys, when set, is
	// called with the fake locked, to change the screen as the agent
	// would.
	keys   [][]string
	onKeys func(f *fakeServer, keys []string)
	// listErr fails ListPanes and captureErr Capture when set; a
	// *tmux.HookError comes with the panes, as the real one's does.
	// configured counts EnsureConfigured.
	listErr    error
	captureErr error
	configured int
}

// fakePaste is one Paste, enter set, or PasteNoEnter.
type fakePaste struct {
	buffer, pane, text string
	enter              bool
}

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
	if f.listErr != nil && !tmux.HookOnly(f.listErr) {
		return nil, f.listErr
	}
	return append([]tmux.Pane(nil), f.panes...), f.listErr
}
func (f *fakeServer) Capture(context.Context, string, int) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.screen...), f.captureErr
}
func (f *fakeServer) EnsureConfigured(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.configured++
	return nil
}
func (f *fakeServer) SelectPane(_ context.Context, pane string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.selected = append(f.selected, pane)
	return nil
}

func (f *fakeServer) SendKeys(_ context.Context, pane string, keys ...string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.keys = append(f.keys, append([]string{pane}, keys...))
	if f.onKeys != nil {
		f.onKeys(f, keys)
	}
	return nil
}
func (f *fakeServer) NewSession(ctx context.Context, o tmux.NewSessionOpts) (tmux.Session, error) {
	f.mu.Lock()
	hold := f.newHold
	f.mu.Unlock()
	if hold != nil {
		select {
		case <-hold:
		case <-ctx.Done():
			return tmux.Session{}, ctx.Err()
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cmds = append(f.cmds, append([]string(nil), o.Cmd...))
	if f.newErr != nil {
		return tmux.Session{}, f.newErr
	}
	id := f.unused("%", &f.next, func(p tmux.Pane) string { return p.ID })
	sid := f.unused("$", &f.sessions, func(p tmux.Pane) string { return p.SessionID })
	server := f.server
	if server == 0 {
		server = 5
	}
	f.panes = append(f.panes, tmux.Pane{Session: o.Name, SessionID: sid, ID: id, Cwd: o.Cwd, CurrentPath: o.Cwd, Managed: true, Host: o.Host, ServerPID: server, TTY: "/dev/null"})
	return tmux.Session{PaneID: id, ServerPID: server}, nil
}

// unused is prefix and the first number after *n that no listed pane
// has for key; *n is advanced to it. The fake is locked.
func (f *fakeServer) unused(prefix string, n *int, key func(tmux.Pane) string) string {
	for {
		*n++
		id := prefix + strconv.Itoa(*n)
		if !slices.ContainsFunc(f.panes, func(p tmux.Pane) bool { return key(p) == id }) {
			return id
		}
	}
}

// KillSessionID refuses a name and a pid that is none, as the real one
// does, and records every id it is asked for in kills. A session no
// listed pane has under the id and the server pid is one gone, which is
// no error.
func (f *fakeServer) KillSessionID(_ context.Context, id string, serverPID int) error {
	if !strings.HasPrefix(id, "$") || serverPID <= 0 {
		return fmt.Errorf("tmux: session %q on server %d refused", id, serverPID)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.kills = append(f.kills, id)
	listed := func(p tmux.Pane) bool { return p.SessionID == id && p.ServerPID == serverPID }
	if i := slices.IndexFunc(f.panes, listed); i >= 0 {
		f.killed = append(f.killed, f.panes[i].Session)
		f.panes = slices.DeleteFunc(f.panes, listed)
	}
	return nil
}

// endSession removes the panes of the session with the name, as the
// session ending on its own does.
func (f *fakeServer) endSession(name string) {
	f.set(func() {
		f.panes = slices.DeleteFunc(f.panes, func(p tmux.Pane) bool { return p.Session == name })
	})
}
func (f *fakeServer) Paste(_ context.Context, buffer, pane, text string) error {
	return f.paste(fakePaste{buffer, pane, text, true})
}
func (f *fakeServer) PasteNoEnter(_ context.Context, buffer, pane, text string) error {
	return f.paste(fakePaste{buffer, pane, text, false})
}
func (f *fakeServer) paste(p fakePaste) error {
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
	f.pastes = append(f.pastes, p)
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
	dirs := config.Dirs{Repos: []string{filepath.Join(base, "repos")}, Worktrees: filepath.Join(base, "worktrees")}
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
	d, ft := addDaemon(t, store)
	return d, ft, store, remote
}

// addDaemon is a daemon over store with a fake managed server.
func addDaemon(t *testing.T, store *worktree.Store) (*Daemon, *fakeServer) {
	ft := &fakeServer{}
	d := New(Config{
		EnvironmentID: "env", Host: "box",
		Targets: []Target{{Label: "laatmux", Tmux: ft, Managed: true}},
		Procs:   &fakeProcs{tables: []procTable{{}}},
		Store:   store, Agents: map[string][]string{"claude": {"claude"}},
		Commands: t.TempDir(), Timings: testTimings,
	})
	t.Cleanup(func() {
		// The trust watchers an add starts end with the test.
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		d.StopRuns(ctx)
	})
	return d, ft
}

func TestCapabilitiesNeedStoreAndManaged(t *testing.T) {
	store, _ := newStore(t)
	d := New(Config{Store: store, Targets: unmanaged(onePane(tmux.Pane{}, nil))})
	caps := d.capabilities()
	if !protocol.Has(caps, protocol.CapWorktrees) || protocol.Has(caps, protocol.CapAdd) || protocol.Has(caps, protocol.CapRm) {
		t.Fatalf("caps %v", caps)
	}
	d = New(Config{Targets: managed(onePane(tmux.Pane{}, nil))})
	if caps := d.capabilities(); protocol.Has(caps, protocol.CapWorktrees) || protocol.Has(caps, protocol.CapAdd) {
		t.Fatalf("caps %v", caps)
	}
	d = New(Config{Store: store, Targets: managed(onePane(tmux.Pane{}, nil))})
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
	wts := d.worktreeRecords()
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
	if wts := d.worktreeRecords(); len(wts) != 0 {
		t.Fatalf("worktrees after rm %+v", wts)
	}
	// A repeat rm is a no-op and ok.
	pc.Write(protocol.Message{Type: protocol.TypeRm, ID: "r2", Repo: remote, Branch: "fix/v1.2", Root: root})
	if res, _ := result(t, pc, "r2"); !res.OK {
		t.Fatalf("repeat rm: %s", res.Error)
	}
}

// A session in the root made with the user's shell, as a jump makes one
// for a worktree with none, is not taken up while no agent runs in it:
// add refuses at the agent stage, saying how to start the agent there,
// and the worktree is left. No agent runs in a pane with no state or one
// not observed yet, one observed bare, one whose agent has gone, and one
// whose observation is of another server instance, a pane id reused
// since. One whose shell runs an agent the daemon has identified is
// taken up as any session in the root is.
func TestAddShellSession(t *testing.T) {
	d, ft, store, remote := newAddDaemon(t)
	root := store.Dirs.Worktree("proj", "task")
	ft.panes = []tmux.Pane{{Session: "proj/task", ID: "%9", Cwd: root, Managed: true, NoCmd: true, ServerPID: 7}}
	pc := conn(t, d)
	want := "session proj/task in " + root + " has a shell and no agent running; start claude in it, or exit that shell and add again"
	for i, st := range []*paneState{
		nil,
		{hasIdentity: true, obs: observation{serverPID: 7}},
		{observed: true, bare: true, obs: observation{serverPID: 7}},
		{observed: true, hasIdentity: true, gone: true, obs: observation{serverPID: 7, live: false}},
		{observed: true, hasIdentity: true, obs: observation{serverPID: 6, live: true}},
	} {
		d.mu.Lock()
		if st == nil {
			delete(d.panes, paneKey("laatmux", "%9"))
		} else {
			d.panes[paneKey("laatmux", "%9")] = st
		}
		d.mu.Unlock()
		id := fmt.Sprintf("c%d", i)
		pc.Write(protocol.Message{Type: protocol.TypeAdd, ID: id, Repo: remote, Branch: "task", AgentName: "claude"})
		if res, _ := result(t, pc, id); res.OK || res.Stage != protocol.StageAgent || !strings.Contains(res.Error, want) || res.Root != root {
			t.Fatalf("state %+v: result %+v, want %q", st, res, want)
		}
		if len(ft.panes) != 1 {
			t.Fatalf("panes %+v", ft.panes)
		}
	}
	d.mu.Lock()
	d.panes[paneKey("laatmux", "%9")] = &paneState{observed: true, hasIdentity: true, obs: observation{serverPID: 7, live: true}}
	d.mu.Unlock()
	pc.Write(protocol.Message{Type: protocol.TypeAdd, ID: "c9", Repo: remote, Branch: "task", AgentName: "claude"})
	res, progress := result(t, pc, "c9")
	if !res.OK || res.Session != "proj/task" || res.PaneID != "%9" || !hasProgress(progress, protocol.StageAgent, protocol.StateSkip, "session proj/task runs in "+root) {
		t.Fatalf("with an agent: result %+v progress %+v", res, progress)
	}
}

// add's look at whether an agent runs in a pane follows the polls: one
// that finds the agent says it runs, on the server instance the pane is
// listed on; the next, which finds it gone, says it does not.
func TestAgentRunsFollowsPolls(t *testing.T) {
	d := New(Config{EnvironmentID: "env", Targets: managed(onePane(pane, idleScr)), Procs: &fakeProcs{tables: []procTable{{procs: []procs.Proc{shell, wrapper, claude}}, {procs: []procs.Proc{shell}}}}})
	ctx := context.Background()
	if d.tasks.agentRuns(pane) {
		t.Fatal("an agent runs before any poll")
	}
	d.poll(ctx)
	if !d.tasks.agentRuns(pane) {
		t.Fatal("the agent found does not run")
	}
	moved := pane
	moved.ServerPID++
	if d.tasks.agentRuns(moved) {
		t.Fatal("the agent runs on another server instance")
	}
	d.poll(ctx)
	if d.tasks.agentRuns(pane) {
		t.Fatal("the agent gone still runs")
	}
}

// add's look at whether an agent runs in a pane reads what the poll
// wrote under the lock, beside polls that find the agent and lose it
// again: the race detector would see a read of the poll's own state.
func TestAgentRunsBesidePolls(t *testing.T) {
	var tables []procTable
	for i := 0; i < 10; i++ {
		tables = append(tables, procTable{procs: []procs.Proc{shell, wrapper, claude}}, procTable{procs: []procs.Proc{shell}})
	}
	d := New(Config{EnvironmentID: "env", Targets: managed(onePane(pane, idleScr)), Procs: &fakeProcs{tables: tables}})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < len(tables); i++ {
			d.poll(context.Background())
		}
	}()
	for {
		select {
		case <-done:
			return
		default:
			d.tasks.agentRuns(pane)
			runtime.Gosched()
		}
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
	// So does a command whose first word is an assignment, which an
	// older client sends unchecked: no worktree is made for it.
	pc.Write(protocol.Message{Type: protocol.TypeAdd, ID: "c4", Repo: remote, Branch: "assign", Cmd: []string{"FOO=1", "claude"}})
	if res, _ := result(t, pc, "c4"); res.OK || res.Stage != protocol.StageResolve || !strings.Contains(res.Error, "FOO=1 is an environment assignment") {
		t.Fatalf("result %+v", res)
	}
	if _, err := os.Stat(store.Dirs.Worktree("proj", "assign")); !os.IsNotExist(err) {
		t.Fatalf("worktree for a refused command: %v", err)
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
	// The pane, with no agent in it, is the worktree's pane record.
	// The worktree's own messages: not a pane record's, and not the
	// main checkout's, in use while the worktree is listed and out of
	// use with it, whose remove the map order can put first.
	worktreeMsg := func() protocol.Message {
		for m := range got {
			if m.Pane == nil && m.PaneRecordID == "" && !(m.Worktree != nil && m.Worktree.Main) && !strings.Contains(m.WorktreeID, "/checkout/") {
				return m
			}
		}
		return protocol.Message{}
	}
	up := worktreeMsg()
	if up.Type != protocol.TypeUpsert || up.Worktree == nil || up.Worktree.Session != "proj/task" {
		t.Fatalf("upsert %+v", up)
	}
	// The pane goes: the session field clears.
	ft.panes = nil
	d.poll(ctx)
	if up := worktreeMsg(); up.Worktree == nil || up.Worktree.Session != "" {
		t.Fatalf("upsert %+v", up)
	}
	// The directory goes: git calls it prunable and the record is removed.
	os.RemoveAll(added.Root)
	d.pollWorktrees(ctx)
	if rm := worktreeMsg(); rm.Type != protocol.TypeRemove || rm.WorktreeID != "env/worktree/"+added.Root || rm.RemovedIn == nil || rm.Listing != nil {
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
	elsewhere := filepath.Join(filepath.Dir(store.Dirs.Repos[0]), "elsewhere")
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
	setTiming(t, &testTimings.CommandTTL, 50*time.Millisecond)
	d, _, _, remote := newAddDaemon(t)
	pc := conn(t, d)
	pc.Write(protocol.Message{Type: protocol.TypeAdd, ID: "c1", Repo: remote, Branch: "task", Cmd: []string{"true"}})
	if res, _ := result(t, pc, "c1"); !res.OK {
		t.Fatalf("add: %+v", res)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		_, kept := d.tasks.cmds.lookup("c1")
		if !kept {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("command not evicted after its TTL")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, fresh := d.tasks.cmds.get("c1", nil); !fresh {
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

// A root with a tab and an ESC in it, here from the repos and worktrees
// directories' names, is in add's progress, in rm's and run's refusals
// and in a prompt's reasons as tmux.Printable shows it: raw, the tab
// would break the client's line and the ESC reach its terminal. The
// output lines of git and the setup commands are theirs, and raw.
func TestRootWithControlBytesQuoted(t *testing.T) {
	store, remote := newStore(t)
	dirs := store.Dirs
	base := filepath.Dir(dirs.Worktrees)
	dirs.Repos = []string{filepath.Join(base, "re\tpos\x1b[32m")}
	dirs.Worktrees = filepath.Join(base, "work\ttrees\x1b[31m")
	store = worktree.New(dirs, []config.Repo{{Source: remote, Name: "proj"}, {Source: "/nowhere/other.git", Name: "other"}})
	d, ft := addDaemon(t, store)
	pc := conn(t, d)
	raw := func(s string) bool { return strings.ContainsAny(s, "\t\x1b") }

	root, none := store.Dirs.Worktree("proj", "task"), store.Dirs.Worktree("proj", "none")
	q, qnone, qcheckout := strconv.Quote(root), strconv.Quote(none), strconv.Quote(store.Dirs.Checkout("proj"))
	pc.Write(protocol.Message{Type: protocol.TypeAdd, ID: "c1", Repo: remote, Branch: "task", Cmd: []string{"true"}})
	res, progress := result(t, pc, "c1")
	if !res.OK || res.Root != root {
		t.Fatalf("add: %+v", res)
	}
	// A second add finds the worktree registered and the session in it.
	pc.Write(protocol.Message{Type: protocol.TypeAdd, ID: "c2", Repo: remote, Branch: "task", Cmd: []string{"true"}})
	if res, p := result(t, pc, "c2"); !res.OK {
		t.Fatalf("second add: %+v", res)
	} else {
		progress = append(progress, p...)
	}
	for _, want := range [][3]string{
		{protocol.StageResolve, protocol.StateDone, "checkout " + qcheckout},
		{protocol.StageClone, protocol.StateStart, "git clone " + remote + " " + qcheckout},
		{protocol.StageWorktree, protocol.StateStart, "git worktree add " + q + " task"},
		{protocol.StageWorktree, protocol.StateDone, "worktree at " + q},
		{protocol.StageCopy, protocol.StateSkip, ".envrc not in " + qcheckout},
		{protocol.StageWorktree, protocol.StateSkip, "worktree registered at " + q},
		{protocol.StageAgent, protocol.StateSkip, "session proj/task runs in " + q},
	} {
		if !hasProgress(progress, want[0], want[1], want[2]) {
			t.Errorf("missing %q in %+v", want, progress)
		}
	}
	for _, p := range progress {
		if p.State != protocol.StateOutput && raw(p.Detail) {
			t.Errorf("progress with a raw control byte: %q", p.Detail)
		}
	}

	// The session with the name an add wants runs in a directory that
	// has them too.
	elsewhere := "/else\twhere\x1b[1m"
	ft.set(func() {
		ft.panes = append(ft.panes, tmux.Pane{Session: "proj/taken", ID: "%9", Cwd: elsewhere, Managed: true})
	})
	for _, c := range []struct {
		m    protocol.Message
		want string
	}{
		{protocol.Message{Type: protocol.TypeAdd, ID: "c3", Repo: remote, Branch: "taken", Cmd: []string{"true"}}, "session proj/taken runs in " + strconv.Quote(elsewhere) + ", not " + strconv.Quote(store.Dirs.Worktree("proj", "taken")) + "; name in use"},
		{protocol.Message{Type: protocol.TypeRm, ID: "r1", Repo: remote, Branch: "other", Root: root}, q + " is the worktree for branch task of proj, not other"},
		{protocol.Message{Type: protocol.TypeRm, ID: "r2", Root: elsewhere}, strconv.Quote(elsewhere) + " is not under the worktrees directory " + strconv.Quote(dirs.Worktrees)},
		{protocol.Message{Type: protocol.TypeRm, ID: "r3", Repo: "other", Root: root}, q + " is a worktree of proj, not other"},
		{protocol.Message{Type: protocol.TypeRm, ID: "r4", Repo: remote, Branch: "task", Root: none}, "branch task of proj is checked out at " + q + ", not " + qnone},
		{protocol.Message{Type: protocol.TypeRun, ID: "u1", Root: root, Branch: "other", Cmd: []string{"true"}}, q + " is the worktree for branch task, not other"},
		{protocol.Message{Type: protocol.TypeRun, ID: "u2", Root: none, Cmd: []string{"true"}}, qnone + " is not a worktree of a known repository"},
		{protocol.Message{Type: protocol.TypeRun, ID: "u3", Root: elsewhere, Cmd: []string{"true"}}, strconv.Quote(elsewhere) + " is not under the worktrees directory " + strconv.Quote(dirs.Worktrees)},
		{protocol.Message{Type: protocol.TypeRun, ID: "u4", Root: root, Repo: "other", Cmd: []string{"true"}}, q + " is a worktree of proj, not other"},
	} {
		pc.Write(c.m)
		if res, _ := result(t, pc, c.m.ID); res.OK || !strings.Contains(res.Error, c.want) || raw(res.Error) {
			t.Errorf("%s: %q, want %q", c.m.ID, res.Error, c.want)
		}
	}

	// A prompt's reasons: the root is gone, another repository's or
	// another branch's, no session runs in it, two do, the one that does
	// is unverified (its session renamed by hand), the server is down,
	// or the checkout cannot be read.
	ctx := context.Background()
	for _, c := range []struct{ got, want string }{
		{d.tasks.worktreeReplaced(ctx, entry{Root: none, Source: remote, Branch: "none"}), "worktree replaced: " + qnone + " is gone"},
		{d.tasks.worktreeReplaced(ctx, entry{Root: root, Source: "/nowhere/other.git", Branch: "task"}), "worktree replaced: " + q + " is now a worktree of proj"},
		{d.tasks.worktreeReplaced(ctx, entry{Root: root, Source: remote, Branch: "renamed"}), "worktree replaced: " + q + " is now on branch task, not renamed"},
		{func() string { _, r := d.tasks.adopt(ctx, none); return r }(), "no agent to deliver to: no managed session in " + qnone},
	} {
		if c.got != c.want {
			t.Errorf("reason %q, want %q", c.got, c.want)
		}
	}
	renamed := "odd\tsession\x1b[1m"
	ft.set(func() {
		ft.panes = append(ft.panes,
			tmux.Pane{Session: "proj/again", ID: "%8", Cwd: root, Managed: true},
			tmux.Pane{Session: renamed, ID: "%7", Cwd: none, Managed: true})
	})
	if _, r := d.tasks.adopt(ctx, root); r != "no agent to deliver to: 2 managed sessions in "+q {
		t.Errorf("reason %q", r)
	}
	if _, r := d.tasks.adopt(ctx, none); r != "no agent to deliver to: no verified agent in session "+strconv.Quote(renamed) {
		t.Errorf("reason %q", r)
	}
	ft.set(func() { ft.listErr = &tmux.Error{Args: []string{"list-panes"}, Msg: "no server running on /tmp/x"} })
	_, r := d.tasks.adopt(ctx, root)
	ft.set(func() { ft.listErr = nil })
	if r != "no agent to deliver to: no managed session in "+q {
		t.Errorf("reason %q", r)
	}
	// A checkout git cannot read: the reason names the root quoted, and
	// git's message after it is git's (#275).
	if err := os.WriteFile(filepath.Join(store.Dirs.Checkout("proj"), ".git", "config"), []byte("[core\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if r := d.tasks.worktreeReplaced(ctx, entry{Root: root, Source: remote, Branch: "task"}); !strings.HasPrefix(r, "worktree "+q+" could not be checked: ") {
		t.Errorf("reason %q", r)
	}
}

// A branch with a C1 control character, which git takes, is in add's
// progress, in rm's and run's refusals and in a prompt's reason as
// tmux.Printable shows it: raw, U+009B is a CSI to the client's
// terminal. The branch a client asks for is quoted the same. A byte
// that is not UTF-8 does not cross the connection, whose JSON has
// U+FFFD for it; the reason is built from the entry as the daemon
// holds it.
func TestBranchWithC1Quoted(t *testing.T) {
	d, _, store, remote := newAddDaemon(t)
	pc := conn(t, d)
	raw := func(s string) bool { return !utf8.ValidString(s) || strings.ContainsFunc(s, unicode.IsControl) }
	q := strconv.Quote
	odd, other, invalid := "a\u009b31mb", "b\u009b0m", "c\xffd"
	root, none := store.Dirs.Worktree("proj", odd), store.Dirs.Worktree("proj", "none")
	qroot := q(root)
	pc.Write(protocol.Message{Type: protocol.TypeAdd, ID: "c1", Repo: remote, Branch: odd, Cmd: []string{"true"}})
	res, progress := result(t, pc, "c1")
	if !res.OK || res.Root != root || res.Branch != odd {
		t.Fatalf("add: %+v", res)
	}
	// A generated add whose proposal is taken by then.
	pc.Write(protocol.Message{Type: protocol.TypeAdd, ID: "c2", Repo: remote, Branch: odd, Generated: true, Cmd: []string{"true"}})
	if res, p := result(t, pc, "c2"); !res.OK || res.Branch != odd+"-2" {
		t.Fatalf("generated add: %+v", res)
	} else {
		progress = append(progress, p...)
	}
	// The resend of a generated add whose name was allocated before the
	// daemon stopped.
	if err := d.journal.create(entry{ID: "c3", Source: remote, Repo: "proj", Branch: odd + "-3", Generated: true, Allocated: true, Stage: protocol.StageWorktree, FirstSeen: time.Now()}); err != nil {
		t.Fatal(err)
	}
	pc.Write(protocol.Message{Type: protocol.TypeAdd, ID: "c3", Repo: remote, Branch: odd, Generated: true, Cmd: []string{"true"}})
	if res, p := result(t, pc, "c3"); !res.OK || res.Branch != odd+"-3" {
		t.Fatalf("resent add: %+v", res)
	} else {
		progress = append(progress, p...)
	}
	for _, want := range [][3]string{
		{protocol.StageAllocate, protocol.StateSkip, "branch " + q(odd) + " given"},
		{protocol.StageWorktree, protocol.StateStart, "git branch --no-track " + q(odd) + " origin/HEAD"},
		{protocol.StageWorktree, protocol.StateDone, "branch " + q(odd) + " from origin/HEAD"},
		{protocol.StageWorktree, protocol.StateStart, "git worktree add " + qroot + " " + q(odd)},
		{protocol.StageAllocate, protocol.StateDone, "branch " + q(odd+"-2") + " for proposal " + q(odd)},
		{protocol.StageAllocate, protocol.StateSkip, "branch " + q(odd+"-3") + " allocated before"},
	} {
		if !hasProgress(progress, want[0], want[1], want[2]) {
			t.Errorf("missing %q in %+v", want, progress)
		}
	}
	for _, p := range progress {
		if p.State != protocol.StateOutput && raw(p.Detail) {
			t.Errorf("progress with a raw byte: %q", p.Detail)
		}
	}

	for _, c := range []struct {
		m    protocol.Message
		want string
	}{
		{protocol.Message{Type: protocol.TypeRm, ID: "r1", Repo: remote, Branch: "task", Root: root}, qroot + " is the worktree for branch " + q(odd) + " of proj, not task"},
		{protocol.Message{Type: protocol.TypeRm, ID: "r2", Repo: remote, Branch: other, Root: root}, qroot + " is the worktree for branch " + q(odd) + " of proj, not " + q(other)},
		{protocol.Message{Type: protocol.TypeRm, ID: "r3", Repo: remote, Branch: odd, Root: none}, "branch " + q(odd) + " of proj is checked out at " + qroot + ", not " + none},
		{protocol.Message{Type: protocol.TypeRun, ID: "u1", Root: root, Branch: "task", Cmd: []string{"true"}}, qroot + " is the worktree for branch " + q(odd) + ", not task"},
		{protocol.Message{Type: protocol.TypeRun, ID: "u2", Root: root, Branch: other, Cmd: []string{"true"}}, qroot + " is the worktree for branch " + q(odd) + ", not " + q(other)},
	} {
		pc.Write(c.m)
		if res, _ := result(t, pc, c.m.ID); res.OK || res.Error != c.want {
			t.Errorf("%s: %q, want %q", c.m.ID, res.Error, c.want)
		}
	}
	if r := d.tasks.worktreeReplaced(context.Background(), entry{Root: root, Source: remote, Branch: invalid}); r != "worktree replaced: "+qroot+" is now on branch "+q(odd)+", not "+q(invalid) {
		t.Errorf("reason %q", r)
	}
}

// A branch laatmux cannot carry (#304). An older client's add of one,
// and its rm of one by branch alone, arrive with U+FFFD for its byte and
// are refused rather than taken for another branch: the add before the
// checkout is cloned. A worktree checked out by hand on such a branch is
// listed with the branch as tmux.Printable shows it and marked display
// only, which survives the connection. A root sent with that shown name,
// as the dashboard's x and a workspace session's rm and run send it, is
// taken for that root and no other: another root, or the U+FFFD form,
// is refused, saying how the worktree is named. Without a root the
// shown name names nothing; with a gone root it finds the branch's
// worktree elsewhere and is refused, as a carriable name is, killing
// nothing. The root alone runs in
// it and removes it. A branch with a real U+FFFD, which an older laatmux
// made from such a name, is reached with its root and that name, and an
// rm of a gone worktree's root with a session's U+FFFD tag still kills
// the session there.
func TestBranchNotCarried(t *testing.T) {
	d, ft, store, remote := newAddDaemon(t)
	ctx := context.Background()
	pc := conn(t, d)
	pc.Write(protocol.Message{Type: protocol.TypeAdd, ID: "a1", Repo: remote, Branch: "a\xffb", Cmd: []string{"true"}})
	if res, _ := result(t, pc, "a1"); res.OK || !strings.Contains(res.Error, "branch \"a\ufffdb\" has U+FFFD") {
		t.Fatalf("add: %+v", res)
	}
	if _, err := os.Stat(store.Dirs.Checkout("proj")); !os.IsNotExist(err) {
		t.Fatalf("the add reached the clone: %v", err)
	}
	// A branch with a C1 control character is one laatmux carries: it is
	// listed as it is, and its quoted form names nothing.
	csi := "csi\u009b1"
	for id, b := range map[string]string{"a2": "first", "a3": csi} {
		pc.Write(protocol.Message{Type: protocol.TypeAdd, ID: id, Repo: remote, Branch: b, Cmd: []string{"true"}})
		if res, _ := result(t, pc, id); !res.OK {
			t.Fatalf("add %q: %+v", b, res)
		}
	}
	checkout, _, _ := store.Checkout(ctx, worktree.Repo{Source: remote})
	hand, hand2, fffd := store.Dirs.Worktree("proj", "hand"), store.Dirs.Worktree("proj", "hand2"), store.Dirs.Worktree("proj", "fffd")
	gittest.HandMadeWorktree(t, checkout, hand, "a\xffb")
	gittest.HandMadeWorktree(t, checkout, hand2, "c\xffd")
	gittest.HandMadeWorktree(t, checkout, fffd, "x\ufffdy")
	shown, shown2 := strconv.Quote("a\xffb"), strconv.Quote("c\xffd")
	named := "; laatmux cannot carry that branch, so the worktree is named by its root, with the branch as listed or none"
	// A session left at the root the worktree on a\xffb was at before
	// it moved, which r8 must not kill.
	moved := store.Dirs.Worktree("proj", "moved")
	ft.set(func() {
		ft.panes = append(ft.panes, tmux.Pane{Session: "proj/moved", SessionID: "$91", ID: "%91", Cwd: moved, Managed: true, ServerPID: 5})
	})

	for _, c := range []struct {
		m    protocol.Message
		want string
	}{
		{protocol.Message{Type: protocol.TypeRm, ID: "r1", Repo: remote, Branch: "a\xffb"}, "branch \"a\ufffdb\" has U+FFFD"},
		{protocol.Message{Type: protocol.TypeRm, ID: "r2", Repo: remote, Branch: "a\xffb", Root: hand}, hand + " is the worktree for branch " + shown + " of proj, not a\ufffdb" + named},
		{protocol.Message{Type: protocol.TypeRun, ID: "u1", Root: hand, Branch: "a\xffb", Cmd: []string{"true"}}, hand + " is the worktree for branch " + shown + ", not a\ufffdb" + named},
		{protocol.Message{Type: protocol.TypeRm, ID: "r3", Repo: remote, Branch: shown2, Root: hand}, hand + " is the worktree for branch " + shown + " of proj, not " + shown2 + named},
		{protocol.Message{Type: protocol.TypeRun, ID: "u2", Root: hand, Branch: shown2, Cmd: []string{"true"}}, hand + " is the worktree for branch " + shown + ", not " + shown2 + named},
		{protocol.Message{Type: protocol.TypeRun, ID: "u6", Root: store.Dirs.Worktree("proj", csi), Branch: strconv.Quote(csi), Cmd: []string{"true"}}, strconv.Quote(store.Dirs.Worktree("proj", csi)) + " is the worktree for branch " + strconv.Quote(csi) + ", not " + strconv.Quote(csi)},
		// The shown name with no root names nothing; with a gone root it
		// finds the branch's worktree elsewhere, as a carriable name does.
		{protocol.Message{Type: protocol.TypeRm, ID: "r7", Repo: remote, Branch: shown}, shown + ` has a \, as no branch git takes has: a quoted branch a listing shows names its worktree only with the root`},
		{protocol.Message{Type: protocol.TypeRm, ID: "r8", Repo: remote, Branch: shown, Root: moved}, "branch " + shown + " of proj is checked out at " + hand + ", not " + moved},
	} {
		pc.Write(c.m)
		if res, _ := result(t, pc, c.m.ID); res.OK || !strings.HasPrefix(res.Error, c.want) {
			t.Errorf("%s: %q, want %q", c.m.ID, res.Error, c.want)
		}
	}
	for _, root := range []string{hand, hand2, fffd} {
		if _, err := os.Stat(root); err != nil {
			t.Fatalf("refused commands touched %s: %v", root, err)
		}
	}
	ft.mu.Lock()
	killedMoved := slices.Contains(ft.killed, "proj/moved")
	ft.mu.Unlock()
	if killedMoved {
		t.Fatal("a refused rm killed the session at the old root")
	}

	// Listed over a connection, the shown name intact.
	sub := conn(t, d)
	sub.Write(protocol.Message{Type: protocol.TypeSubscribe})
	if err := d.poll(ctx); err != nil {
		t.Fatal(err)
	}
	d.markDiscovered(&d.panesDiscovered)
	d.pollWorktrees(ctx)
	var snap protocol.Message
	for snap.Type != protocol.TypeSnapshot {
		m, err := sub.Read()
		if err != nil {
			t.Fatal(err)
		}
		snap = m
	}
	got := map[string]protocol.Worktree{}
	for _, w := range snap.Worktrees {
		got[w.Root] = w
	}
	for root, want := range map[string]protocol.Worktree{
		hand:                                 {Branch: shown, BranchDisplayOnly: true},
		hand2:                                {Branch: shown2, BranchDisplayOnly: true},
		fffd:                                 {Branch: "x\ufffdy", BranchDisplayOnly: true},
		store.Dirs.Worktree("proj", "first"): {Branch: "first"},
		store.Dirs.Worktree("proj", csi):     {Branch: csi},
	} {
		if w := got[root]; w.Branch != want.Branch || w.BranchDisplayOnly != want.BranchDisplayOnly {
			t.Errorf("%s: record %+v", root, w)
		}
	}

	for _, m := range []protocol.Message{
		{Type: protocol.TypeRun, ID: "u3", Root: hand, Branch: shown, Cmd: []string{"true"}},
		{Type: protocol.TypeRun, ID: "u4", Root: hand, Cmd: []string{"true"}},
		{Type: protocol.TypeRun, ID: "u5", Root: fffd, Branch: "x\ufffdy", Cmd: []string{"true"}},
	} {
		pc.Write(m)
		if res, _ := result(t, pc, m.ID); !res.OK {
			t.Errorf("%s: %+v", m.ID, res)
		}
	}
	// The dashboard's x: the root with the shown name; rm --root: the
	// root alone.
	for _, m := range []protocol.Message{
		{Type: protocol.TypeRm, ID: "r4", Repo: remote, Branch: shown, Root: hand},
		{Type: protocol.TypeRm, ID: "r5", Root: hand2},
	} {
		pc.Write(m)
		if res, _ := result(t, pc, m.ID); !res.OK || res.Root != m.Root {
			t.Errorf("%s: %+v", m.ID, res)
		}
		if _, err := os.Stat(m.Root); !os.IsNotExist(err) {
			t.Errorf("%s: root after rm: %v", m.ID, err)
		}
	}
	// A gone worktree's root, with the U+FFFD tag of the session there.
	gone := store.Dirs.Worktree("proj", "gone")
	ft.set(func() {
		ft.panes = append(ft.panes, tmux.Pane{Session: "proj/gone", SessionID: "$90", ID: "%90", Cwd: gone, Managed: true, ServerPID: 5})
	})
	pc.Write(protocol.Message{Type: protocol.TypeRm, ID: "r6", Repo: remote, Branch: "a\ufffdb", Root: gone})
	if res, _ := result(t, pc, "r6"); !res.OK || res.Root != gone {
		t.Errorf("rm of a gone root: %+v", res)
	}
	ft.mu.Lock()
	killed := slices.Contains(ft.killed, "proj/gone")
	ft.mu.Unlock()
	if !killed {
		t.Errorf("the session at the gone root was not killed")
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
	if wts := d.worktreeRecords(); len(wts) != 0 {
		t.Fatalf("worktrees %+v", wts)
	}
}

// rm refuses a main checkout's root, also where the repos directory is
// under the worktrees one, so the root passes Owns: the checkout stays
// and so does a session new made in it.
func TestRmRefusesMainCheckout(t *testing.T) {
	store, remote := newStore(t)
	store.Dirs.Repos = []string{filepath.Join(store.Dirs.Worktrees, "checkouts")}
	main := filepath.Join(store.Dirs.Repos[0], "proj")
	mkdirs(t, store.Dirs.Repos[0])
	sh(t, store.Dirs.Repos[0], "git", "clone", "-q", remote, main)
	d, ft := addDaemon(t, store)
	ft.set(func() {
		ft.panes = []tmux.Pane{{Session: "notes", SessionID: "$1", ID: "%1", Managed: true, Cwd: main, CurrentPath: main}}
	})
	pc := conn(t, d)
	pc.Write(protocol.Message{Type: protocol.TypeRm, ID: "r1", Repo: remote, Branch: "main", Root: main})
	if res, _ := result(t, pc, "r1"); res.OK || !strings.Contains(res.Error, "is a main checkout; rm removes worktrees") {
		t.Fatalf("rm of the main checkout: %+v", res)
	}
	if _, err := os.Stat(filepath.Join(main, ".git")); err != nil || len(ft.kills) != 0 {
		t.Fatalf("the checkout or its session went: %v, kills %v", err, ft.kills)
	}
}

// rm kills each managed session at the root by its id, on the server
// it was listed on, and once when it has two panes there: a session
// made by hand can be called c:d, which tmux 3.7 keeps and no target
// reaches by name, or $1, which a target reads as the id of another
// session, here one at another root that rm leaves. By name, c:d was
// refused after git had removed the worktree, and rm failed.
func TestRmKillsSessionsByID(t *testing.T) {
	d, ft, store, remote := newAddDaemon(t)
	ctx := context.Background()
	repo, _ := store.Repo(remote)
	added, err := store.Add(ctx, repo, "task", nil)
	if err != nil {
		t.Fatal(err)
	}
	root, other := added.Root, store.Dirs.Worktree("proj", "other")
	ft.set(func() {
		ft.panes = []tmux.Pane{
			{Session: "c:d", SessionID: "$7", ID: "%1", Cwd: root, CurrentPath: root, Managed: true, ServerPID: 5, TTY: "/dev/null"},
			{Session: "c:d", SessionID: "$7", ID: "%2", Cwd: root, CurrentPath: root, Managed: true, ServerPID: 5, TTY: "/dev/null"},
			{Session: "$1", SessionID: "$8", ID: "%3", Cwd: root, CurrentPath: root, Managed: true, ServerPID: 5, TTY: "/dev/null"},
			{Session: "proj/other", SessionID: "$1", ID: "%4", Cwd: other, CurrentPath: other, Managed: true, ServerPID: 5, TTY: "/dev/null"},
		}
	})
	pc := conn(t, d)
	pc.Write(protocol.Message{Type: protocol.TypeRm, ID: "r1", Repo: remote, Branch: "task", Root: root, Force: true})
	if res, _ := result(t, pc, "r1"); !res.OK {
		t.Fatalf("rm: %+v", res)
	}
	if _, err := os.Stat(root); err == nil {
		t.Fatal("root still exists")
	}
	ft.mu.Lock()
	defer ft.mu.Unlock()
	if !slices.Equal(ft.killed, []string{"c:d", "$1"}) || !slices.Equal(ft.kills, []string{"$7", "$8"}) || len(ft.panes) != 1 || ft.panes[0].ID != "%4" {
		t.Fatalf("killed %q by %q, panes left %+v", ft.killed, ft.kills, ft.panes)
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
	unhold := d.tasks.holdRepos()
	unlockRepo := d.tasks.lockRepo("/nowhere/other.git", "other")
	unlockOther := func() { unlockRepo(); unhold() }
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
// not place a directory or is not a label, is refused at resolve, the
// name's refusal with the label rule; without an entry the host's own
// config decides, as before.
func TestAddFromRepoEntry(t *testing.T) {
	d, _, store, remote := newAddDaemon(t)
	store.SetRepos(nil)
	ctx := context.Background()
	pc := conn(t, d)
	if !protocol.Has(d.capabilities(), protocol.CapRepoEntry) {
		t.Fatalf("caps %v", d.capabilities())
	}
	for id, m := range map[string]protocol.Message{
		"none":     {Repo: remote},
		"mismatch": {Repo: remote, RepoEntry: &protocol.RepoEntry{Source: "/elsewhere.git", Name: "sent"}},
		"name":     {Repo: remote, RepoEntry: &protocol.RepoEntry{Source: remote, Name: "../sent"}},
		"dash":     {Repo: remote, RepoEntry: &protocol.RepoEntry{Source: remote, Name: "--"}},
		"copy":     {Repo: remote, RepoEntry: &protocol.RepoEntry{Source: remote, Name: "sent", Copy: []string{"../x"}}},
		"setup":    {Repo: remote, RepoEntry: &protocol.RepoEntry{Source: remote, Name: "sent", Setup: []string{" "}}},
	} {
		m.Type, m.ID, m.Branch, m.AgentName = protocol.TypeAdd, id, "task", "claude"
		pc.Write(m)
		if res, _ := result(t, pc, id); res.OK || res.Stage != protocol.StageResolve {
			t.Fatalf("%s: %+v", id, res)
		} else if id == "dash" && !strings.Contains(res.Error, `"--" is not a valid label (`+config.LabelRule+`)`) {
			t.Fatalf("dash: the refusal should name the label and the rule: %q", res.Error)
		}
	}
	if _, err := os.Stat(store.Dirs.Repos[0]); err == nil {
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
	if _, err := os.Stat(filepath.Join(store.Dirs.Repos[0], "sent", ".git")); err != nil {
		t.Fatal("not cloned under the entry's name")
	}
	d.pollWorktrees(ctx)
	if wts := d.worktreeRecords(); len(wts) != 1 || wts[0].Root != root || wts[0].Repo != "sent" || wts[0].Source != remote {
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
	store.SetRepos([]worktree.Repo{{Source: https, Name: "proj", Setup: []string{"echo host >> log"}}})
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
	store.SetRepos(nil)
	pc := conn(t, d)
	entry := &protocol.RepoEntry{Source: remote, Name: "sent"}
	pc.Write(protocol.Message{Type: protocol.TypeAdd, ID: "a1", Repo: remote, RepoEntry: entry, Branch: "task", AgentName: "claude"})
	res, _ := result(t, pc, "a1")
	if !res.OK {
		t.Fatalf("add: %+v", res)
	}
	root := res.Root

	// A second clone of the repository, with a worktree of its own.
	second := filepath.Join(store.Dirs.Repos[0], "sent2")
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

// A checkout no config lists, cloned by hand under a name with a tab
// and an ESC, is named by its label in rm's and run's refusals, and the
// label names it in a request: no raw byte of the directory's name
// reaches a client.
func TestUnlistedCheckoutLabelInRefusals(t *testing.T) {
	d, _, store, remote := newAddDaemon(t)
	pc := conn(t, d)
	root := addWorktree(t, pc, remote, "task")
	const label = "hand_made__31m"
	hand := filepath.Join(store.Dirs.Repos[0], "hand\tmade\x1b[31m")
	side := store.Dirs.Worktree("hand", "side")
	for _, args := range [][]string{
		{"clone", "-q", remote, hand},
		{"-C", hand, "remote", "set-url", "origin", "/elsewhere/hand.git"},
		{"-C", hand, "worktree", "add", "-q", "-b", "side", side},
	} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	for _, c := range []struct {
		m    protocol.Message
		want string
	}{
		{protocol.Message{Type: protocol.TypeRm, ID: "rm1", Repo: remote, Root: side}, side + " is a worktree of " + label + ", not proj"},
		{protocol.Message{Type: protocol.TypeRm, ID: "rm2", Repo: label, Root: root}, root + " is a worktree of proj, not " + label},
		{protocol.Message{Type: protocol.TypeRun, ID: "run1", Repo: remote, Root: side, Cmd: []string{"true"}}, side + " is a worktree of " + label + ", not proj"},
		{protocol.Message{Type: protocol.TypeRun, ID: "run2", Repo: label, Root: root, Cmd: []string{"true"}}, root + " is a worktree of proj, not " + label},
	} {
		pc.Write(c.m)
		res, _ := result(t, pc, c.m.ID)
		if res.OK || !strings.Contains(res.Error, c.want) || strings.ContainsAny(res.Error, "\t\x1b") {
			t.Fatalf("%s: %+v, want %q", c.m.ID, res, c.want)
		}
	}
}

// The select command makes a pane current on the managed server and
// answers; a daemon without one refuses, and so does an empty pane id.
func TestSelectCommand(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	srv := &fakeServer{}
	d := New(Config{EnvironmentID: "env", Targets: []Target{{Label: "laatmux", Tmux: srv, Managed: true}}})
	if !protocol.Has(d.capabilities(), protocol.CapSelect) {
		t.Fatal("no select capability")
	}
	server, cl := net.Pipe()
	go d.HandleConn(ctx, server, func() { server.Close() })
	pc := protocol.NewConn(cl)
	if _, err := pc.Read(); err != nil {
		t.Fatal(err)
	}
	pc.Write(protocol.Message{Type: protocol.TypeSelect, ID: "s1", PaneID: "%7"})
	res := next(t, cl, pc)
	srv.mu.Lock()
	selected := append([]string(nil), srv.selected...)
	srv.mu.Unlock()
	if !res.OK || res.ID != "s1" || len(selected) != 1 || selected[0] != "%7" {
		t.Errorf("select: %+v, selected %v", res, selected)
	}
	pc.Write(protocol.Message{Type: protocol.TypeSelect, ID: "s2"})
	if res := next(t, cl, pc); res.OK || res.Error == "" {
		t.Errorf("select without a pane: %+v", res)
	}
	unmanaged := New(Config{EnvironmentID: "env", Targets: []Target{{Label: "default", Tmux: srv}}})
	if protocol.Has(unmanaged.capabilities(), protocol.CapSelect) {
		t.Error("select advertised without a managed server")
	}
}

// A listing error is logged and published once per change of its text,
// not once per poll, with the text on the listing record; a listing
// that works again is published once, with the error cleared, after
// the host's repositories, which the first listing that works has.
func TestListingErrorOnce(t *testing.T) {
	dir := t.TempDir()
	repos := filepath.Join(dir, "repos")
	// A file where the repos directory should be: every list fails.
	if err := os.WriteFile(repos, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	var logged strings.Builder
	d := New(Config{EnvironmentID: "lenv", Version: "local", Logger: log.New(&logged, "", 0),
		Store: worktree.New(config.Dirs{Repos: []string{repos}, Worktrees: filepath.Join(dir, "wt")}, nil)})
	state := func() (uint64, string) { d.mu.Lock(); defer d.mu.Unlock(); return d.seq, d.listErr }
	for range 3 {
		d.pollWorktrees(context.Background())
	}
	if n, e := state(); n != 1 || e == "" || !strings.Contains(logged.String(), e) || strings.Count(logged.String(), "worktrees: ") != 1 {
		t.Fatalf("seq %d, listErr %q, log:\n%s", n, e, logged.String())
	}
	os.Remove(repos)
	d.pollWorktrees(context.Background())
	if n, e := state(); n != 3 || e != "" {
		t.Fatalf("after recovery: seq %d, listErr %q", n, e)
	}
}
