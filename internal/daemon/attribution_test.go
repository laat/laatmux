package daemon

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/laat/laatmux/internal/procs"
	"github.com/laat/laatmux/internal/protocol"
	"github.com/laat/laatmux/internal/tmux"
)

// realTemp is a temporary directory with its symlinks resolved, as git
// registers roots: on macOS the temporary directory is behind /var.
func realTemp(t *testing.T) string {
	t.Helper()
	base := t.TempDir()
	if runtime.GOOS == "darwin" {
		if real, err := filepath.EvalSymlinks(base); err == nil {
			base = real
		}
	}
	return base
}

func mkdirs(t *testing.T, dirs ...string) {
	t.Helper()
	for _, d := range dirs {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
}

func TestAttributionTable(t *testing.T) {
	base := realTemp(t)
	foo := filepath.Join(base, "worktrees", "proj", "foo")
	foo2 := filepath.Join(base, "worktrees", "proj", "foo-2")
	nested := filepath.Join(foo, "vendor", "lib")
	main := filepath.Join(base, "repos", "proj")
	mkdirs(t, filepath.Join(foo, "src"), foo2, nested, main)
	link := filepath.Join(base, "link")
	if err := os.Symlink(foo, link); err != nil {
		t.Fatal(err)
	}
	d := New(Config{EnvironmentID: "env"})
	d.mu.Lock()
	d.roots = d.resolveRoots([]string{foo, foo2, nested})
	d.mu.Unlock()
	id := func(root string) string { return "env/worktree/" + root }
	for _, c := range []struct {
		name string
		pane tmux.Pane
		want string
	}{
		{"managed pane at the root", tmux.Pane{Managed: true, Cwd: foo, CurrentPath: "/elsewhere"}, id(foo)},
		{"pane in a subdirectory", tmux.Pane{CurrentPath: filepath.Join(foo, "src")}, id(foo)},
		{"sibling root with a common prefix", tmux.Pane{CurrentPath: foo2}, id(foo2)},
		{"checkout nested in a worktree", tmux.Pane{CurrentPath: nested}, id(nested)},
		{"main checkout", tmux.Pane{CurrentPath: main}, ""},
		{"no path", tmux.Pane{}, ""},
		{"path reaching the root through a symlink", tmux.Pane{CurrentPath: filepath.Join(link, "src")}, id(foo)},
		{"a path that is gone", tmux.Pane{CurrentPath: filepath.Join(foo, "gone")}, id(foo)},
	} {
		d.mu.Lock()
		got := d.worktreeOfLocked(d.resolve(panePath(c.pane)))
		d.mu.Unlock()
		if got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
}

func TestHomeSessions(t *testing.T) {
	root := "/w/proj/foo"
	resolve := func(p string) string { return p }
	for _, c := range []struct {
		name  string
		panes []tmux.Pane
		want  string
	}{
		{"single managed pane", []tmux.Pane{{Session: "s", Managed: true, Cwd: root}}, "s"},
		{"a split inside the root keeps the home", []tmux.Pane{
			{Session: "s", Managed: true, Cwd: root},
			{Session: "s", CurrentPath: root + "/src"},
		}, "s"},
		{"a split elsewhere takes it away", []tmux.Pane{
			{Session: "s", Managed: true, Cwd: root},
			{Session: "s", CurrentPath: "/tmp"},
		}, ""},
		{"a split in a sibling with a common prefix takes it away", []tmux.Pane{
			{Session: "s", Managed: true, Cwd: root},
			{Session: "s", CurrentPath: root + "-2"},
		}, ""},
		{"a pane not made by laatmux is no home", []tmux.Pane{{Session: "s", Cwd: root}}, ""},
		{"the lexically first of two", []tmux.Pane{
			{Session: "t", Managed: true, Cwd: root},
			{Session: "s", Managed: true, Cwd: root},
		}, "s"},
		{"one session broken, the other whole", []tmux.Pane{
			{Session: "a", Managed: true, Cwd: root},
			{Session: "a", CurrentPath: "/tmp"},
			{Session: "b", Managed: true, Cwd: root},
		}, "b"},
	} {
		if got := homeSessions(c.panes, resolve)[root]; got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
}

// ttyProcs identifies an agent by the pane's tty.
type ttyProcs struct {
	mu  sync.Mutex
	ids map[string]procs.Identity
}

func (f *ttyProcs) Find(tty string) (procs.Identity, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	id, ok := f.ids[tty]
	return id, ok, nil
}

func (f *ttyProcs) Exists(tty string, id procs.Identity) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	cur, ok := f.ids[tty]
	return ok && cur.Same(id), nil
}

// attrFixture is a daemon watching a managed and a default server, with
// agents identified by tty, and a subscriber.
type attrFixture struct {
	d        *Daemon
	laatmux  *fakeServer
	def      *fakeServer
	got      chan protocol.Message
	foo, bar string
}

func newAttrFixture(t *testing.T) *attrFixture {
	t.Helper()
	base := realTemp(t)
	f := &attrFixture{laatmux: &fakeServer{}, def: &fakeServer{}, got: make(chan protocol.Message, 64),
		foo: filepath.Join(base, "worktrees", "proj", "foo"), bar: filepath.Join(base, "worktrees", "proj", "bar")}
	mkdirs(t, filepath.Join(f.foo, "src"), f.bar)
	start := time.Now().Add(-time.Hour)
	f.d = New(Config{
		EnvironmentID: "env",
		Targets:       []Target{{Label: "laatmux", Tmux: f.laatmux, Managed: true}, {Label: "default", Tmux: f.def}},
		Procs: &ttyProcs{ids: map[string]procs.Identity{
			"/dev/a1": {Agent: "claude", PID: 11, Start: start, Comm: "claude"},
			"/dev/a2": {Agent: "codex", PID: 12, Start: start, Comm: "codex"},
		}},
	})
	f.d.discoveredOnce.Do(func() { close(f.d.discovered) })
	pc := conn(t, f.d)
	pc.Write(protocol.Message{Type: protocol.TypeSubscribe})
	if m, err := pc.Read(); err != nil || m.Type != protocol.TypeSnapshot {
		t.Fatalf("snapshot %+v %v", m, err)
	}
	go func() {
		for {
			m, err := pc.Read()
			if err != nil {
				return
			}
			f.got <- m
		}
	}()
	return f
}

// list makes git's listing the given roots.
func (f *attrFixture) list(roots ...string) {
	f.d.mu.Lock()
	defer f.d.mu.Unlock()
	f.d.setRootsLocked(f.d.resolveRoots(roots), time.Now())
}

// drain returns what the subscriber got until the stream is quiet.
func (f *attrFixture) drain(t *testing.T) []protocol.Message {
	t.Helper()
	var out []protocol.Message
	for {
		select {
		case m := <-f.got:
			out = append(out, m)
		case <-time.After(200 * time.Millisecond):
			return out
		}
	}
}

func (f *attrFixture) poll(t *testing.T) []protocol.Message {
	t.Helper()
	if err := f.d.poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	return f.drain(t)
}

func agentUpserts(ms []protocol.Message) map[string]protocol.Agent {
	out := map[string]protocol.Agent{}
	for _, m := range ms {
		if m.Agent != nil {
			out[m.Agent.PaneID] = *m.Agent
		}
	}
	return out
}

func TestAttributionAgents(t *testing.T) {
	f := newAttrFixture(t)
	f.list(f.foo, f.bar)
	fooID := "env/worktree/" + f.foo
	// Two agents in one worktree: the one add made at its root, and one
	// on the default server in a subdirectory.
	f.laatmux.set(func() {
		f.laatmux.panes = []tmux.Pane{{Session: "proj/foo", ID: "%1", TTY: "/dev/a1", Managed: true, Cwd: f.foo, CurrentPath: f.foo}}
	})
	f.def.set(func() {
		f.def.panes = []tmux.Pane{{Session: "work", ID: "%7", TTY: "/dev/a2", CurrentPath: filepath.Join(f.foo, "src")}}
	})
	got := agentUpserts(f.poll(t))
	if got["%1"].WorktreeID != fooID || got["%7"].WorktreeID != fooID {
		t.Fatalf("agents %+v", got)
	}
	// Nothing changed: nothing published.
	if ms := f.poll(t); len(ms) != 0 {
		t.Fatalf("unchanged poll published %+v", ms)
	}
	// The default server's pane leaves the worktree, then enters another.
	f.def.set(func() { f.def.panes[0].CurrentPath = "/" })
	if got := agentUpserts(f.poll(t)); len(got) != 1 || got["%7"].WorktreeID != "" {
		t.Fatalf("left %+v", got)
	}
	f.def.set(func() { f.def.panes[0].CurrentPath = f.bar })
	if got := agentUpserts(f.poll(t)); len(got) != 1 || got["%7"].WorktreeID != "env/worktree/"+f.bar {
		t.Fatalf("entered %+v", got)
	}
	// The worktree goes from the listing: its agent loses the id at once,
	// without a pane poll.
	f.list(f.foo)
	if got := agentUpserts(f.drain(t)); len(got) != 1 || got["%7"].WorktreeID != "" {
		t.Fatalf("after removal %+v", got)
	}
}

// A worktree listed after its pane was polled: the pane gains the id on
// the listing, not only once it changes.
func TestAttributionListedAfterPoll(t *testing.T) {
	f := newAttrFixture(t)
	f.laatmux.set(func() {
		f.laatmux.panes = []tmux.Pane{
			{Session: "proj/foo", ID: "%1", TTY: "/dev/a1", Managed: true, Cwd: f.foo, CurrentPath: f.foo},
			{Session: "proj/foo", ID: "%2", TTY: "/dev/s1", CurrentCommand: "zsh", PID: 40, CurrentPath: f.foo},
		}
	})
	ms := f.poll(t)
	if got := agentUpserts(ms); got["%1"].WorktreeID != "" {
		t.Fatalf("before listing %+v", got)
	}
	for _, m := range ms {
		if m.Pane != nil {
			t.Fatalf("pane record outside every listed root: %+v", m.Pane)
		}
	}
	f.list(f.foo)
	ms = f.drain(t)
	if got := agentUpserts(ms); got["%1"].WorktreeID != "env/worktree/"+f.foo {
		t.Fatalf("after listing %+v", got)
	}
	var pane *protocol.Pane
	for _, m := range ms {
		if m.Pane != nil {
			pane = m.Pane
		}
	}
	if pane == nil || pane.ID != "env/pane/laatmux/%2" || pane.WorktreeID != "env/worktree/"+f.foo || pane.Command != "zsh" || pane.PID != 40 {
		t.Fatalf("pane record %+v", pane)
	}
}

func TestAttributionPaneRecords(t *testing.T) {
	f := newAttrFixture(t)
	f.list(f.foo)
	f.def.set(func() {
		f.def.panes = []tmux.Pane{
			{Session: "work", ID: "%3", TTY: "/dev/s1", CurrentCommand: "zsh", PID: 30, CurrentPath: f.foo},
			{Session: "work", ID: "%4", TTY: "/dev/s2", CurrentCommand: "zsh", PID: 31, CurrentPath: "/"},
		}
	})
	panes := func(ms []protocol.Message) (up []protocol.Pane, rm []string) {
		for _, m := range ms {
			if m.Pane != nil {
				up = append(up, *m.Pane)
			}
			if m.PaneRecordID != "" {
				rm = append(rm, m.PaneRecordID)
			}
			if m.Agent != nil {
				t.Fatalf("agent record for a shell: %+v", m.Agent)
			}
		}
		return up, rm
	}
	// Only the pane inside a root is published.
	if up, rm := panes(f.poll(t)); len(up) != 1 || up[0].PaneID != "%3" || up[0].Server != "default" || up[0].Command != "zsh" || len(rm) != 0 {
		t.Fatalf("first poll %+v %v", up, rm)
	}
	// An unchanged poll publishes nothing; the command changing does.
	if up, rm := panes(f.poll(t)); len(up) != 0 || len(rm) != 0 {
		t.Fatalf("unchanged %+v %v", up, rm)
	}
	f.def.set(func() { f.def.panes[0].CurrentCommand = "go" })
	if up, _ := panes(f.poll(t)); len(up) != 1 || up[0].Command != "go" {
		t.Fatalf("command changed %+v", up)
	}
	// The path changing within the worktree is an upsert; out of it a
	// remove.
	f.def.set(func() { f.def.panes[0].CurrentPath = filepath.Join(f.foo, "src") })
	if up, _ := panes(f.poll(t)); len(up) != 1 || up[0].Cwd != filepath.Join(f.foo, "src") {
		t.Fatalf("path changed %+v", up)
	}
	f.def.set(func() { f.def.panes[0].CurrentPath = "/" })
	if up, rm := panes(f.poll(t)); len(up) != 0 || len(rm) != 1 || rm[0] != "env/pane/default/%3" {
		t.Fatalf("left %+v %v", up, rm)
	}
	// A pane that goes takes its record with it.
	f.def.set(func() {
		f.def.panes = []tmux.Pane{{Session: "work", ID: "%5", TTY: "/dev/s3", CurrentCommand: "zsh", CurrentPath: f.foo}}
	})
	f.poll(t)
	f.def.set(func() { f.def.panes = nil })
	var gone bool
	for _, m := range f.poll(t) {
		gone = gone || m.PaneRecordID == "env/pane/default/%5"
	}
	if !gone {
		t.Fatal("pane record kept after the pane went")
	}
	d := f.d
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.paneRecs) != 0 {
		t.Fatalf("records left %+v", d.paneRecs)
	}
}

// A pane that is a pane record in one poll and has an agent in the next
// has its record removed before the agent's upsert.
func TestAttributionPaneBecomesAgent(t *testing.T) {
	f := newAttrFixture(t)
	f.list(f.foo)
	f.def.set(func() {
		f.def.panes = []tmux.Pane{{Session: "work", ID: "%3", TTY: "/dev/s1", CurrentCommand: "zsh", CurrentPath: f.foo}}
	})
	f.poll(t)
	f.def.set(func() { f.def.panes[0].TTY = "/dev/a2" })
	ms := f.poll(t)
	if len(ms) != 2 || ms[0].PaneRecordID != "env/pane/default/%3" || ms[1].Agent == nil || ms[1].Agent.WorktreeID != "env/worktree/"+f.foo {
		t.Fatalf("messages %+v", ms)
	}
}

// A run is published while it runs and removed when it ends, by itself
// or by rm.
func TestRunRecords(t *testing.T) {
	d, _, _, remote := newAddDaemon(t)
	d.discoveredOnce.Do(func() { close(d.discovered) })
	pc := conn(t, d)
	root := addWorktree(t, pc, remote, "task")
	sub := conn(t, d)
	sub.Write(protocol.Message{Type: protocol.TypeSubscribe})
	if m, err := sub.Read(); err != nil || m.Type != protocol.TypeSnapshot {
		t.Fatalf("snapshot %+v %v", m, err)
	}
	await := func(want func(protocol.Message) bool) protocol.Message {
		t.Helper()
		for {
			m, err := sub.Read()
			if err != nil {
				t.Fatal(err)
			}
			if want(m) {
				return m
			}
		}
	}
	pc.Write(protocol.Message{Type: protocol.TypeRun, ID: "r1", Root: root, Cmd: []string{"true"}})
	if res, _ := result(t, pc, "r1"); !res.OK {
		t.Fatalf("run %+v", res)
	}
	up := await(func(m protocol.Message) bool { return m.Run != nil })
	if r := up.Run; r.ID != "env/run/r1" || r.Root != root || r.WorktreeID != "env/worktree/"+root || len(r.Cmd) != 1 || r.Cmd[0] != "true" || r.StartedAt.IsZero() {
		t.Fatalf("run record %+v", r)
	}
	await(func(m protocol.Message) bool { return m.RunID == "env/run/r1" })

	pc.Write(protocol.Message{Type: protocol.TypeRun, ID: "r2", Root: root, Cmd: []string{"sh", "-c", "sleep 30"}})
	await(func(m protocol.Message) bool { return m.Run != nil && m.Run.ID == "env/run/r2" })
	late := conn(t, d)
	late.Write(protocol.Message{Type: protocol.TypeSubscribe})
	if m, err := late.Read(); err != nil || len(m.Runs) != 1 || m.Runs[0].ID != "env/run/r2" {
		t.Fatalf("snapshot while running %+v %v", m, err)
	}
	other := conn(t, d)
	other.Write(protocol.Message{Type: protocol.TypeRm, ID: "rm1", Repo: remote, Branch: "task", Root: root, Force: true})
	if res, _ := result(t, other, "rm1"); !res.OK {
		t.Fatalf("rm: %+v", res)
	}
	await(func(m protocol.Message) bool { return m.RunID == "env/run/r2" })
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.runRecs) != 0 {
		t.Fatalf("run records left %+v", d.runRecs)
	}
}
