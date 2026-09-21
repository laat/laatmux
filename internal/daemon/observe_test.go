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
	managed    bool
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
func (f *fakeTmux) NewSession(context.Context, tmux.NewSessionOpts) (string, error) {
	return "%0", nil
}
func (f *fakeTmux) Managed() bool { return f.managed }

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

func run(t *testing.T, fp *fakeProcs, ft *fakeTmux, polls int) protocol.Agent {
	t.Helper()
	d := New(Config{EnvironmentID: "env", Tmux: ft, Procs: fp})
	for i := 0; i < polls; i++ {
		if err := d.poll(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	_, agents := d.Snapshot()
	if len(agents) != 1 {
		t.Fatalf("agents = %d", len(agents))
	}
	return agents[0]
}

// Review 2 finding 1: a shell -c wrapper observed before its child must not
// be identified; the child is picked up when it appears.
func TestObserveWrapperBeforeChild(t *testing.T) {
	fp := &fakeProcs{tables: []procTable{
		{procs: []procs.Proc{shell, wrapper}},
		{procs: []procs.Proc{shell, wrapper, claude}},
	}}
	ft := &fakeTmux{pane: pane, screen: idleScr}
	a := run(t, fp, ft, 1)
	if a.Agent != "" || a.Liveness != protocol.None {
		t.Fatalf("wrapper identified: %+v", a)
	}
	a = run(t, &fakeProcs{tables: fp.tables[1:]}, ft, 1)
	if a.Agent != "claude" || a.Identity == nil || a.Identity.PID != 101 || a.Liveness != protocol.Alive {
		t.Fatalf("child not identified: %+v", a)
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
	d := New(Config{EnvironmentID: "env", Tmux: &fakeTmux{pane: pane, screen: idleScr}, Procs: fp})
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
	d := New(Config{EnvironmentID: "env", Tmux: ft, Procs: fp})
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
	d := New(Config{EnvironmentID: "env", Tmux: ft, Procs: fp})
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
	d = New(Config{EnvironmentID: "env", Tmux: ft, Procs: fp2})
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
	d := New(Config{EnvironmentID: "env", Tmux: ft, Procs: fp})
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
	ft := &fakeTmux{pane: pane, screen: idleScr, managed: true, listErr: &tmux.Error{Msg: "no server running"}}
	d := New(Config{EnvironmentID: "env", Tmux: ft, Procs: fp})
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
	ftu := &fakeTmux{pane: pane, screen: idleScr, managed: false}
	d = New(Config{EnvironmentID: "env", Tmux: ftu, Procs: fp})
	d.poll(context.Background())
	if ftu.configured != 0 {
		t.Fatal("unmanaged server configured")
	}
}
