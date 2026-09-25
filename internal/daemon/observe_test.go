package daemon

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/laat/laatmux/internal/procs"
	"github.com/laat/laatmux/internal/protocol"
	"github.com/laat/laatmux/internal/tmux"
)

// fakeProcs serves scripted process tables per call, or an error.
type fakeProcs struct {
	tables []procTable
	i      int
}

type procTable struct {
	procs []procs.Proc
	err   error
}

func (f *fakeProcs) next() procTable {
	t := f.tables[min(f.i, len(f.tables)-1)]
	f.i++
	return t
}

func (f *fakeProcs) Find(tty string) (procs.Identity, bool, error) {
	t := f.next()
	if t.err != nil {
		return procs.Identity{}, false, t.err
	}
	id, ok := procs.FindIn(t.procs)
	return id, ok, nil
}

func (f *fakeProcs) Exists(tty string, id procs.Identity) (bool, error) {
	t := f.next()
	if t.err != nil {
		return false, t.err
	}
	return procs.ExistsIn(t.procs, id), nil
}

// fakeTmux returns one pane and a scripted screen; records configuration.
type fakeTmux struct {
	pane       tmux.Pane
	screen     []string
	captureErr error
	configured int
	listErr    error
}

func (f *fakeTmux) ListPanes(context.Context) ([]tmux.Pane, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	return []tmux.Pane{f.pane}, nil
}
func (f *fakeTmux) Capture(context.Context, string, int) ([]string, error) {
	return f.screen, f.captureErr
}
func (f *fakeTmux) EnsureConfigured(context.Context) error { f.configured++; return nil }
func (f *fakeTmux) NewSession(context.Context, tmux.NewSessionOpts) (tmux.Session, error) {
	return tmux.Session{PaneID: "%0", ServerPID: 1}, nil
}
func (f *fakeTmux) KillSession(context.Context, string) error           { return nil }
func (f *fakeTmux) Paste(context.Context, string, string, string) error { return nil }
func (f *fakeTmux) DeleteBuffers(context.Context, string) error         { return nil }
func (f *fakeTmux) SendKeys(context.Context, string, ...string) error   { return nil }

// managed and unmanaged wrap a fake as the daemon's targets.
func managed(ft *fakeTmux) []Target   { return []Target{{Label: "laatmux", Tmux: ft, Managed: true}} }
func unmanaged(ft *fakeTmux) []Target { return []Target{{Label: "default", Tmux: ft}} }

var (
	t0      = time.Unix(1_700_000_000, 0)
	shell   = procs.Proc{PID: 10, PPID: 1, PGID: 10, TPGID: 10, Comm: "zsh", Start: t0}
	wrapper = procs.Proc{PID: 100, PPID: 10, PGID: 100, TPGID: 100, Comm: "bash", Start: t0.Add(time.Second), Argv: []string{"bash", "-c", "claude; sleep 60"}}
	claude  = procs.Proc{PID: 101, PPID: 100, PGID: 100, TPGID: 100, Comm: "claude", Start: t0.Add(2 * time.Second)}
	hinted  = procs.Proc{PID: 100, PPID: 10, PGID: 100, TPGID: 100, Comm: "agentd", Start: t0.Add(time.Second), Env: []string{"LAATMUX_AGENT=claude"}}
	pane    = tmux.Pane{ID: "%1", Session: "s", TTY: "/dev/pts/9", Title: "✳ x", ServerPID: 5}
	idleScr = []string{"────", "❯ ", "────", "  ⏸ manual mode on"}
	blkScr  = []string{"────", " Bash command", "   rm -f /tmp/x", " Do you want to proceed?", " ❯ 1. Yes", "   2. No", " Esc to cancel · Tab to amend"}
)

func run(t *testing.T, fp *fakeProcs, ft *fakeTmux, polls int) []protocol.Agent {
	t.Helper()
	d := New(Config{EnvironmentID: "env", Targets: managed(ft), Procs: fp})
	for i := 0; i < polls; i++ {
		if err := d.poll(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	_, agents := d.Snapshot()
	return agents
}

// Review 2 finding 1: a shell -c wrapper observed before its child must not
// be identified; the child is picked up when it appears. A pane with no
// identified agent is not published at all.
func TestObserveWrapperBeforeChild(t *testing.T) {
	fp := &fakeProcs{tables: []procTable{
		{procs: []procs.Proc{shell, wrapper}},
		{procs: []procs.Proc{shell, wrapper, claude}},
	}}
	ft := &fakeTmux{pane: pane, screen: idleScr}
	if ag := run(t, fp, ft, 1); len(ag) != 0 {
		t.Fatalf("wrapper identified: %+v", ag)
	}
	ag := run(t, &fakeProcs{tables: fp.tables[1:]}, ft, 1)
	if len(ag) != 1 {
		t.Fatalf("agents = %d", len(ag))
	}
	a := ag[0]
	if a.Agent != "claude" || a.Identity == nil || a.Identity.PID != 101 || a.Liveness != protocol.Alive {
		t.Fatalf("child not identified: %+v", a)
	}
	if a.ID != "env/laatmux/%1" || a.Server != "laatmux" {
		t.Fatalf("id/server: %q %q", a.ID, a.Server)
	}
}

// Issue 3, option B: panes without an identified agent are never published,
// so a shell pane appearing and disappearing produces no traffic at all.
func TestUnidentifiedPaneIsSilent(t *testing.T) {
	fp := &fakeProcs{tables: []procTable{{procs: []procs.Proc{shell}}}}
	ft := &fakeTmux{pane: pane, screen: idleScr}
	d := New(Config{EnvironmentID: "env", Targets: unmanaged(ft), Procs: fp})
	d.poll(context.Background())
	d.poll(context.Background())
	if _, ag := d.Snapshot(); len(ag) != 0 {
		t.Fatalf("shell published: %+v", ag)
	}
	if len(d.panes) != 1 {
		t.Fatalf("pane not tracked: %d", len(d.panes))
	}
	ft.listErr = &tmux.Error{Msg: "no server running"}
	d.poll(context.Background())
	if d.seq != 0 || len(d.panes) != 0 {
		t.Fatalf("seq %d panes %d after unpublished pane left", d.seq, len(d.panes))
	}
}

// Issue 3, option B: two servers with the same pane id are two agents, only
// the managed one is configured, and a server going away removes only its
// own agents.
func TestTwoServers(t *testing.T) {
	fp := &fakeProcs{tables: []procTable{{procs: []procs.Proc{shell, claude}}}}
	m := &fakeTmux{pane: pane, screen: idleScr}
	u := &fakeTmux{pane: pane, screen: blkScr}
	d := New(Config{EnvironmentID: "env", Targets: append(managed(m), unmanaged(u)...), Procs: fp})
	if got := d.capabilities(); !protocol.Has(got, protocol.CapNew) {
		t.Fatalf("caps %v", got)
	}
	for i := 0; i < 2; i++ {
		if err := d.poll(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if m.configured != 1 || u.configured != 0 {
		t.Fatalf("configured managed=%d unmanaged=%d", m.configured, u.configured)
	}
	_, ag := d.Snapshot()
	if len(ag) != 2 {
		t.Fatalf("agents = %d: %+v", len(ag), ag)
	}
	byID := map[string]protocol.Agent{}
	for _, a := range ag {
		byID[a.ID] = a
	}
	ma, ok1 := byID["env/laatmux/%1"]
	ua, ok2 := byID["env/default/%1"]
	if !ok1 || !ok2 {
		t.Fatalf("ids: %v", byID)
	}
	if ma.Server != "laatmux" || ma.Activity != protocol.Idle || ua.Server != "default" || ua.Activity != protocol.Blocked {
		t.Fatalf("records mixed up: %+v %+v", ma, ua)
	}

	sub, _ := d.subscribe(nil)
	u.listErr = &tmux.Error{Msg: "no server running"}
	if err := d.poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case msg := <-sub.ch:
		if msg.Type != protocol.TypeRemove || msg.AgentID != "env/default/%1" {
			t.Fatalf("got %+v", msg)
		}
	default:
		t.Fatal("no remove for the vanished server")
	}
	_, ag = d.Snapshot()
	if len(ag) != 1 || ag[0].ID != "env/laatmux/%1" {
		t.Fatalf("after removal: %+v", ag)
	}
	// One server failing hard keeps discovery pending but the other still polls.
	u.listErr = &tmux.Error{Msg: "permission denied"}
	if err := d.poll(context.Background()); err == nil {
		t.Fatal("hard error swallowed")
	}
	if _, ag = d.Snapshot(); len(ag) != 1 {
		t.Fatalf("managed server not polled past the failing one: %+v", ag)
	}
}

// A daemon that does not watch the managed server does not offer new.
func TestNoManagedServerNoNew(t *testing.T) {
	d := New(Config{EnvironmentID: "env", Targets: unmanaged(&fakeTmux{pane: pane})})
	if protocol.Has(d.capabilities(), protocol.CapNew) {
		t.Fatal("new offered without the managed server")
	}
}

// A tentative (hinted) identity is replaced by a verified child.
func TestObserveTentativeReplacedByVerified(t *testing.T) {
	child := claude
	child.PPID = 100
	fp := &fakeProcs{tables: []procTable{
		{procs: []procs.Proc{shell, hinted}},
		{procs: []procs.Proc{shell, hinted, child}},
	}}
	d := New(Config{EnvironmentID: "env", Targets: managed(&fakeTmux{pane: pane, screen: idleScr}), Procs: fp})
	d.poll(context.Background())
	_, ag := d.Snapshot()
	if ag[0].Identity == nil || ag[0].Identity.PID != 100 {
		t.Fatalf("tentative not used: %+v", ag[0])
	}
	d.poll(context.Background())
	_, ag = d.Snapshot()
	if ag[0].Identity.PID != 101 {
		t.Fatalf("verified child did not replace tentative: %+v", ag[0])
	}
}

// Review 2 finding 1 (second reproduction): when Claude exits under a
// surviving wrapper the record keeps Claude's pid as gone, and does not
// become an alive agent at the wrapper's pid.
func TestObserveExitUnderSurvivingWrapper(t *testing.T) {
	fp := &fakeProcs{tables: []procTable{
		{procs: []procs.Proc{shell, wrapper, claude}},
		{procs: []procs.Proc{shell, wrapper, claude}}, // exists check
		{procs: []procs.Proc{shell, wrapper}},         // exists: gone
		{procs: []procs.Proc{shell, wrapper}},         // find: nothing
	}}
	ft := &fakeTmux{pane: pane, screen: idleScr}
	d := New(Config{EnvironmentID: "env", Targets: managed(ft), Procs: fp})
	for i := 0; i < 4; i++ {
		d.poll(context.Background())
	}
	_, ag := d.Snapshot()
	a := ag[0]
	if a.Liveness != protocol.Gone || a.Identity == nil || a.Identity.PID != 101 || a.Agent != "claude" {
		t.Fatalf("got %+v", a)
	}
	if a.Activity != protocol.Idle {
		t.Fatalf("last activity not retained: %s", a.Activity)
	}
}

// Review 2 finding 3: a process-read error is not absence, and the same
// instance found again is alive without a reset.
func TestObserveTransientReadError(t *testing.T) {
	fp := &fakeProcs{tables: []procTable{
		{procs: []procs.Proc{shell, claude}},
		{err: errors.New("sysctl: EAGAIN")},
		{procs: []procs.Proc{shell, claude}},
	}}
	ft := &fakeTmux{pane: pane, screen: idleScr}
	d := New(Config{EnvironmentID: "env", Targets: managed(ft), Procs: fp})
	d.poll(context.Background())
	_, ag := d.Snapshot()
	first := ag[0]
	d.poll(context.Background())
	_, ag = d.Snapshot()
	if ag[0].Liveness != protocol.Alive {
		t.Fatalf("read error marked agent gone: %+v", ag[0])
	}
	d.poll(context.Background())
	_, ag = d.Snapshot()
	if ag[0].Liveness != protocol.Alive || ag[0].Identity.PID != first.Identity.PID || !ag[0].ActivityAt.Equal(first.ActivityAt) {
		t.Fatalf("instance reset after transient error: %+v vs %+v", ag[0], first)
	}
	// A transient omission from the table, then reappearance, also restores.
	fp2 := &fakeProcs{tables: []procTable{
		{procs: []procs.Proc{shell, claude}},
		{procs: []procs.Proc{shell}},         // exists: false -> gone
		{procs: []procs.Proc{shell, claude}}, // find: same instance
	}}
	d = New(Config{EnvironmentID: "env", Targets: managed(ft), Procs: fp2})
	for i := 0; i < 3; i++ {
		d.poll(context.Background())
	}
	_, ag = d.Snapshot()
	if ag[0].Liveness != protocol.Alive || ag[0].Identity.PID != 101 {
		t.Fatalf("reappeared instance not restored: %+v", ag[0])
	}
}

// Review 2 finding 5: a failed capture keeps the last activity.
func TestObserveCaptureFailureKeepsBlocked(t *testing.T) {
	fp := &fakeProcs{tables: []procTable{{procs: []procs.Proc{shell, claude}}}}
	ft := &fakeTmux{pane: pane, screen: blkScr}
	d := New(Config{EnvironmentID: "env", Targets: managed(ft), Procs: fp})
	d.poll(context.Background())
	_, ag := d.Snapshot()
	if ag[0].Activity != protocol.Blocked {
		t.Fatalf("setup: %+v", ag[0])
	}
	ft.captureErr = errors.New("capture-pane: pane busy")
	for i := 0; i < 5; i++ {
		d.poll(context.Background())
	}
	_, ag = d.Snapshot()
	if ag[0].Activity != protocol.Blocked {
		t.Fatalf("capture failure changed activity: %+v", ag[0])
	}
	ft.captureErr = nil
	ft.screen = idleScr
	d.poll(context.Background())
	_, ag = d.Snapshot()
	if ag[0].Activity != protocol.Idle {
		t.Fatalf("recovery: %+v", ag[0])
	}
}

// Review 2 finding 2: discovery of a managed server reconciles it, once per
// server instance, including a server that appears after startup.
func TestDiscoveryConfiguresManagedServer(t *testing.T) {
	fp := &fakeProcs{tables: []procTable{{procs: []procs.Proc{shell}}}}
	ft := &fakeTmux{pane: pane, screen: idleScr, listErr: &tmux.Error{Msg: "no server running"}}
	d := New(Config{EnvironmentID: "env", Targets: managed(ft), Procs: fp})
	d.poll(context.Background())
	if ft.configured != 0 {
		t.Fatal("configured with no server")
	}
	ft.listErr = nil
	d.poll(context.Background())
	d.poll(context.Background())
	if ft.configured != 1 {
		t.Fatalf("configured %d times, want 1", ft.configured)
	}
	ft.pane.ServerPID = 6 // server restarted by hand
	d.poll(context.Background())
	if ft.configured != 2 {
		t.Fatalf("restarted server not reconciled: %d", ft.configured)
	}
	ftu := &fakeTmux{pane: pane, screen: idleScr}
	d = New(Config{EnvironmentID: "env", Targets: unmanaged(ftu), Procs: fp})
	d.poll(context.Background())
	if ftu.configured != 0 {
		t.Fatal("unmanaged server configured")
	}
}
