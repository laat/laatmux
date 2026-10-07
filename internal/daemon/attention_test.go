package daemon

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/laat/laatmux/internal/peer"
	"github.com/laat/laatmux/internal/protocol"
)

// attnDaemon is a merging daemon with attention, this machine mac and a
// remote host vm, the file under dir.
func attnDaemon(t *testing.T, dir string, hosts ...peer.Host) *Daemon {
	t.Helper()
	if len(hosts) == 0 {
		hosts = []peer.Host{{Name: "mac"}, {Name: "vm", SSH: "vm"}}
	}
	list := &hostsList{hosts: hosts}
	d := New(Config{EnvironmentID: "menv", Host: "mac", Hosts: list.get, Attention: filepath.Join(dir, "attention.json")})
	d.mu.Lock()
	d.reconcileHostsLocked(hosts)
	if mh := d.mhosts["vm"]; mh != nil {
		mh.status.EnvironmentID = "venv"
	}
	d.mu.Unlock()
	return d
}

// remoteAgent is vm's agent in a managed session, started at pid 100.
func remoteAgent(pane, session string, act protocol.Activity, at time.Time) protocol.Agent {
	return protocol.Agent{ID: "venv/laatmux/" + pane, EnvironmentID: "venv", Server: "laatmux", Session: session, PaneID: pane, Agent: "claude",
		Activity: act, ActivityAt: at, Liveness: protocol.Alive, Managed: true, Identity: &protocol.Identity{PID: 100, StartUnix: 1}}
}

// fromVM applies a record as vm's upsert.
func fromVM(d *Daemon, a protocol.Agent) {
	d.applyRemote(context.Background(), d.mhosts["vm"], protocol.Message{Type: protocol.TypeUpsert, Agent: &a})
}

func attnOf(d *Daemon, id string) (protocol.Attention, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	e, ok := d.attn.entries[id]
	if !ok {
		return protocol.Attention{}, false
	}
	return e.record(id), true
}

func done(d *Daemon, id string) bool {
	a, ok := attnOf(d, id)
	return ok && a.Done()
}

func look(d *Daemon, views ...ClientView) {
	d.mu.Lock()
	d.markSeenLocked(views, time.Now())
	d.mu.Unlock()
}

// attachTo is a client showing a live attach pane to a managed session.
func attachTo(host, session string) ClientView {
	return ClientView{Client: "/dev/ttys001", Pane: "%50", AttachPane: true, Target: session, Host: host}
}

// A finish while the user is elsewhere is done until a client shows the
// agent. One while a client shows it is seen by the listing the finish
// has made at once; an interrupt followed by a switch away before that
// listing is done, as the note says, since the user left before the
// finish was observed.
func TestAttentionFinishAndSeen(t *testing.T) {
	d := attnDaemon(t, t.TempDir())
	t0 := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
	id := "venv/laatmux/%1"
	fromVM(d, remoteAgent("%1", "proj/x", protocol.Working, t0))
	if _, ok := attnOf(d, id); !ok {
		t.Fatal("not tracked")
	}
	fromVM(d, remoteAgent("%1", "proj/x", protocol.Idle, t0.Add(time.Minute)))
	if !done(d, id) {
		t.Fatal("finish while elsewhere not done")
	}
	// A client sitting on another session, or on the shell window of a
	// workspace session, sees nothing.
	look(d, attachTo("vm", "proj/other"), ClientView{Pane: "%9", Workspace: "venv//w/x", Host: "vm"})
	if !done(d, id) {
		t.Fatal("seen from elsewhere")
	}
	look(d, attachTo("vm", "proj/x"))
	if done(d, id) {
		t.Fatal("not seen through the attach")
	}

	// Watching: the finish pokes, and the listing it makes sees it.
	fromVM(d, remoteAgent("%1", "proj/x", protocol.Working, t0.Add(2*time.Minute)))
	fromVM(d, remoteAgent("%1", "proj/x", protocol.Idle, t0.Add(3*time.Minute)))
	select {
	case <-d.attn.poke:
	default:
		t.Fatal("a finish did not poke")
	}
	look(d, attachTo("vm", "proj/x"))
	if done(d, id) {
		t.Error("finish while watched is done")
	}
	// An interrupt, then a switch away before the finish is observed:
	// the listing after it shows another session.
	fromVM(d, remoteAgent("%1", "proj/x", protocol.Working, t0.Add(4*time.Minute)))
	fromVM(d, remoteAgent("%1", "proj/x", protocol.Idle, t0.Add(5*time.Minute)))
	look(d, attachTo("vm", "proj/other"))
	if !done(d, id) {
		t.Error("interrupt then switch away is not done")
	}
}

// This machine's own agents are matched against the name the config
// gives it now: a rename, read on a subscription, is what the sessions a
// jump makes are tagged with.
func TestAttentionLocalRename(t *testing.T) {
	d := attnDaemon(t, t.TempDir())
	t0 := time.Now()
	a := protocol.Agent{ID: "menv/laatmux/%1", EnvironmentID: "menv", Server: "laatmux", Session: "proj/x", PaneID: "%1", Agent: "claude",
		Activity: protocol.Working, ActivityAt: t0, Liveness: protocol.Alive, Managed: true, Identity: &protocol.Identity{PID: 1}}
	publish(d, "laatmux/%1", a)
	a.Activity, a.ActivityAt = protocol.Idle, t0.Add(time.Second)
	publish(d, "laatmux/%1", a)
	d.mu.Lock()
	d.reconcileHostsLocked([]peer.Host{{Name: "laptop"}, {Name: "vm", SSH: "vm"}})
	d.mu.Unlock()
	if !done(d, a.ID) {
		t.Fatal("the rename forgot the local agent")
	}
	look(d, attachTo("mac", "proj/x"))
	if !done(d, a.ID) {
		t.Error("seen through the old name")
	}
	look(d, attachTo("laptop", "proj/x"))
	if done(d, a.ID) {
		t.Error("not seen through the new name")
	}

	// Finished again, then the daemon restarts under the new name: the
	// entry is the same agent's and keeps its finish.
	a.Activity, a.ActivityAt = protocol.Working, t0.Add(2*time.Second)
	publish(d, "laatmux/%1", a)
	a.Activity, a.ActivityAt = protocol.Idle, t0.Add(3*time.Second)
	publish(d, "laatmux/%1", a)
	list := &hostsList{hosts: []peer.Host{{Name: "laptop"}}}
	d = New(Config{EnvironmentID: "menv", Host: "laptop", Hosts: list.get, Attention: d.attn.path})
	publish(d, "laatmux/%1", a)
	if !done(d, a.ID) {
		t.Error("a restart under the new name lost the finish")
	}
}

// Only working to idle with a new mark is a finish: working, unknown,
// then idle is one too; blocked to idle is not; a record with another
// identity starts over; a host clock hours ahead changes nothing, the
// finish being dated on this machine.
func TestAttentionTransitions(t *testing.T) {
	d := attnDaemon(t, t.TempDir())
	ahead := time.Now().Add(5 * time.Hour)
	id := "venv/laatmux/%1"
	fromVM(d, remoteAgent("%1", "s", protocol.Working, ahead))
	fromVM(d, remoteAgent("%1", "s", protocol.Unknown, ahead.Add(time.Second)))
	before := time.Now()
	fromVM(d, remoteAgent("%1", "s", protocol.Idle, ahead.Add(2*time.Second)))
	a, _ := attnOf(d, id)
	if !a.Done() || a.FinishedAt.Before(before) || a.FinishedAt.After(time.Now()) {
		t.Errorf("working, unknown, idle: %+v", a)
	}

	fromVM(d, remoteAgent("%2", "s2", protocol.Blocked, ahead))
	fromVM(d, remoteAgent("%2", "s2", protocol.Idle, ahead.Add(time.Second)))
	if done(d, "venv/laatmux/%2") {
		t.Error("blocked to idle is done")
	}

	// A new agent in the same pane: no finish, and the old times go.
	fresh := remoteAgent("%1", "s", protocol.Idle, ahead.Add(time.Minute))
	fresh.Identity = &protocol.Identity{PID: 200, StartUnix: 2}
	fromVM(d, fresh)
	if a, ok := attnOf(d, id); !ok || !a.FinishedAt.IsZero() {
		t.Errorf("new identity kept the old finish: %+v", a)
	}
}

// Agents that can never be seen are not tracked: one on a remote host's
// default server, one on another observed server, one naming no server.
// This machine's default server is, through the agent's own pane.
func TestAttentionTracked(t *testing.T) {
	d := attnDaemon(t, t.TempDir())
	t0 := time.Now()
	a := remoteAgent("%3", "notes", protocol.Working, t0)
	a.Server, a.ID = "default", "venv/default/%3"
	fromVM(d, a)
	a.Activity, a.ActivityAt = protocol.Idle, t0.Add(time.Second)
	fromVM(d, a)
	if _, ok := attnOf(d, a.ID); ok {
		t.Error("remote default server tracked")
	}
	// A record naming no server is on none that can be seen: it is not
	// read as the managed server.
	a = remoteAgent("%6", "notes", protocol.Working, t0)
	a.Server, a.ID = "", "venv//%6"
	fromVM(d, a)
	if _, ok := attnOf(d, a.ID); ok {
		t.Error("serverless record tracked")
	}

	local := protocol.Agent{ID: "menv/default/%4", EnvironmentID: "menv", Server: "default", Session: "notes", PaneID: "%4",
		Agent: "claude", Activity: protocol.Working, ActivityAt: t0, Liveness: protocol.Alive, Identity: &protocol.Identity{PID: 7, StartUnix: 1}}
	publish(d, "default/%4", local)
	local.Activity, local.ActivityAt = protocol.Idle, t0.Add(time.Second)
	publish(d, "default/%4", local)
	if !done(d, local.ID) {
		t.Fatal("local default agent's finish with no subscriber not done")
	}
	look(d, ClientView{Pane: "%4"})
	if done(d, local.ID) {
		t.Error("local pane shown, not seen")
	}

	other := local
	other.ID, other.Server, other.PaneID = "menv//tmp/sock/%5", "/tmp/sock", "%5"
	publish(d, "/tmp/sock/%5", other)
	if _, ok := attnOf(d, other.ID); ok {
		t.Error("another observed server tracked")
	}
}

// What sees: a live attach pane to the agent's session on the agent's
// host. A dead one, one to the same session name on another host, and
// one from before the target was tagged, told apart by the plain
// attachment's tag or the workspace session's key.
func TestAttentionViews(t *testing.T) {
	d := attnDaemon(t, t.TempDir())
	t0 := time.Now()
	d.mu.Lock()
	d.mhosts["vm"].worktrees["venv/worktree//w/x"] = protocol.Worktree{ID: "venv/worktree//w/x", EnvironmentID: "venv", Root: "/w/x", Session: "proj/x"}
	d.mhosts["vm"].worktrees["venv/worktree//w/y"] = protocol.Worktree{ID: "venv/worktree//w/y", EnvironmentID: "venv", Root: "/w/y", Session: "proj/y"}
	d.mu.Unlock()
	finish := func(pane, session string) string {
		fromVM(d, remoteAgent(pane, session, protocol.Working, t0))
		fromVM(d, remoteAgent(pane, session, protocol.Idle, t0.Add(time.Second)))
		return "venv/laatmux/" + pane
	}
	x, y, n := finish("%1", "proj/x"), finish("%2", "proj/y"), finish("%3", "scratch")
	dead := attachTo("vm", "proj/x")
	dead.Dead = true
	look(d, dead, attachTo("mac", "proj/x"))
	if !done(d, x) {
		t.Error("seen through a dead attach pane or another host's session")
	}
	// Before #56: the workspace session's key names the worktree whose
	// home session is the target; two on one host told apart by it.
	look(d, ClientView{Pane: "%60", AttachPane: true, Host: "vm", Workspace: "venv//w/x"})
	if done(d, x) || !done(d, y) {
		t.Errorf("old workspace attach: x done %v, y done %v", done(d, x), done(d, y))
	}
	look(d, ClientView{Pane: "%61", AttachPane: true, Attach: "vm/scratch"})
	if done(d, n) {
		t.Error("old plain attachment did not see")
	}
}

// The state outlives the daemon: a restart does not bring back done on
// what was seen, a finish while the daemon was down is found in the
// host's next snapshot, and a cached record replayed is not a finish.
func TestAttentionAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	t0 := time.Now()
	d := attnDaemon(t, dir)
	fromVM(d, remoteAgent("%1", "a", protocol.Working, t0))
	fromVM(d, remoteAgent("%1", "a", protocol.Idle, t0.Add(time.Second)))
	look(d, attachTo("vm", "a"))
	fromVM(d, remoteAgent("%2", "b", protocol.Working, t0))

	d = attnDaemon(t, dir)
	snap := protocol.Message{Type: protocol.TypeSnapshot, Agents: []protocol.Agent{
		remoteAgent("%1", "a", protocol.Idle, t0.Add(time.Second)),
		remoteAgent("%2", "b", protocol.Idle, t0.Add(time.Hour)),
	}}
	d.applyRemote(context.Background(), d.mhosts["vm"], snap)
	if done(d, "venv/laatmux/%1") {
		t.Error("seen agent done after a restart")
	}
	if !done(d, "venv/laatmux/%2") {
		t.Error("finish while down not found in the snapshot")
	}
	// The same snapshot again, as on a reconnect: nothing new.
	look(d, attachTo("vm", "b"))
	d.applyRemote(context.Background(), d.mhosts["vm"], snap)
	if done(d, "venv/laatmux/%2") {
		t.Error("a replayed snapshot is a finish")
	}
}

// Entries go with their agent's remove, with a snapshot that lacks the
// agent, and with the host's removal from the config; each published one
// is removed from the stream.
func TestAttentionForget(t *testing.T) {
	d := attnDaemon(t, t.TempDir())
	t0 := time.Now()
	for _, p := range []string{"%1", "%2", "%3"} {
		fromVM(d, remoteAgent(p, "s"+p, protocol.Working, t0))
		fromVM(d, remoteAgent(p, "s"+p, protocol.Idle, t0.Add(time.Second)))
	}
	s := &subscriber{ch: make(chan protocol.Message, 16), merged: true}
	d.mu.Lock()
	d.msubs[s] = struct{}{}
	d.mu.Unlock()
	d.applyRemote(context.Background(), d.mhosts["vm"], protocol.Message{Type: protocol.TypeRemove, AgentID: "venv/laatmux/%1"})
	d.applyRemote(context.Background(), d.mhosts["vm"], protocol.Message{Type: protocol.TypeSnapshot, Agents: []protocol.Agent{remoteAgent("%3", "s%3", protocol.Idle, t0.Add(time.Second))}})
	if _, ok := attnOf(d, "venv/laatmux/%1"); ok {
		t.Error("kept after the remove")
	}
	if _, ok := attnOf(d, "venv/laatmux/%2"); ok {
		t.Error("kept after a snapshot without it")
	}
	d.mu.Lock()
	d.reconcileHostsLocked([]peer.Host{{Name: "mac"}})
	d.mu.Unlock()
	if _, ok := attnOf(d, "venv/laatmux/%3"); ok {
		t.Error("kept after the host left the config")
	}
	removed := map[string]bool{}
	for len(s.ch) > 0 {
		if m := <-s.ch; m.AttentionID != "" {
			removed[m.AttentionID] = true
		}
	}
	if len(removed) != 3 {
		t.Errorf("attention removes: %v", removed)
	}
}

// The merged snapshot carries the records, a poke has the clients listed
// at once, and a subscriber that knows no attention passes over them.
func TestAttentionStream(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var listings atomic.Int32
	hosts := &hostsList{hosts: []peer.Host{{Name: "mac"}}}
	d := New(Config{EnvironmentID: "menv", Host: "mac", Hosts: hosts.get, Attention: filepath.Join(t.TempDir(), "a.json"),
		Clients: func(context.Context) ([]ClientView, error) { listings.Add(1); return nil, nil }})
	if !protocol.Has(d.capabilities(), protocol.CapAttention) {
		t.Fatal("no attention capability")
	}
	discovered(d)
	go d.runSeen(ctx)
	a := protocol.Agent{ID: "menv/laatmux/%1", EnvironmentID: "menv", Server: "laatmux", Session: "s", Activity: protocol.Working, ActivityAt: time.Now(), Identity: &protocol.Identity{PID: 1}}
	publish(d, "laatmux/%1", a)
	a.Activity, a.ActivityAt = protocol.Idle, a.ActivityAt.Add(time.Second)
	publish(d, "laatmux/%1", a)

	server, cl := net.Pipe()
	go d.HandleConn(ctx, server, func() { server.Close() })
	pc := protocol.NewConn(cl)
	if _, err := pc.Read(); err != nil {
		t.Fatal(err)
	}
	pc.Write(protocol.Message{Type: protocol.TypeSubscribe, Merged: true})
	snap := next(t, cl, pc)
	if len(snap.Attentions) != 1 || snap.Attentions[0].AgentID != a.ID || !snap.Attentions[0].Done() {
		t.Errorf("snapshot attentions: %+v", snap.Attentions)
	}
	n := listings.Load()
	pc.Write(protocol.Message{Type: protocol.TypePoke})
	deadline := time.Now().Add(seenInterval / 2)
	for listings.Load() == n && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if listings.Load() == n {
		t.Error("a poke did not list the clients at once")
	}
}

// The seen loop lists once a second only while an agent is done and a
// view is open or left less than the idle time ago, and a finish lists at
// once, view or not, the listing that decides it. A finish watched with
// no view open is seen; one after the user left stays done, and a view
// opened later does not see it. A host's daemon, which no view of its
// own subscribes to, lists only on its finishes.
func TestAttentionSeenLoop(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	here, elsewhere := []ClientView{attachTo("mac", "s")}, []ClientView{attachTo("mac", "elsewhere")}
	var views atomic.Value
	views.Store(here)
	var listings atomic.Int32
	hosts := &hostsList{hosts: []peer.Host{{Name: "mac"}}}
	d := New(Config{EnvironmentID: "menv", Host: "mac", Hosts: hosts.get, Attention: filepath.Join(t.TempDir(), "a.json"), MergedIdle: time.Hour,
		Clients: func(context.Context) ([]ClientView, error) {
			// Read, then counted: a listing counted has taken its
			// views, and a store after that is the next one's.
			v := views.Load().([]ClientView)
			listings.Add(1)
			return v, nil
		}})
	go d.runSeen(ctx)
	wait := func(cond func() bool, what string) {
		t.Helper()
		deadline := time.Now().Add(3 * time.Second)
		for !cond() {
			if time.Now().After(deadline) {
				t.Fatal(what)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	listed := func(what string) {
		t.Helper()
		n := listings.Load()
		wait(func() bool { return listings.Load() > n }, what)
	}
	// quiet is no listing for longer than a tick.
	quiet := func(what string) {
		t.Helper()
		n := listings.Load()
		time.Sleep(seenInterval + seenInterval/2)
		if m := listings.Load(); m != n {
			t.Fatalf("%d listings %s", m-n, what)
		}
	}
	a := protocol.Agent{ID: "menv/laatmux/%1", EnvironmentID: "menv", Server: "laatmux", Session: "s", Activity: protocol.Working, ActivityAt: time.Now(), Identity: &protocol.Identity{PID: 1}}
	publish(d, "laatmux/%1", a)
	finish := func() {
		t.Helper()
		a.Activity, a.ActivityAt = protocol.Working, a.ActivityAt.Add(time.Second)
		publish(d, "laatmux/%1", a)
		n := listings.Load()
		a.Activity, a.ActivityAt = protocol.Idle, a.ActivityAt.Add(time.Second)
		publish(d, "laatmux/%1", a)
		wait(func() bool { return listings.Load() > n }, "the finish did not list")
	}
	finish()
	wait(func() bool { x, _ := attnOf(d, a.ID); return !x.FinishedAt.IsZero() && !x.Done() }, "a watched finish not seen")

	// The user leaves, the agent works and finishes again: done, and
	// with no view open nothing lists after the finish's own listing.
	views.Store(elsewhere)
	finish()
	if !done(d, a.ID) {
		t.Fatal("a finish after the user left is not done")
	}
	quiet("with no view open")

	// A view opens: the loop lists, and it sees the user come back. The
	// poke's listing comes after the tick's has moved what it moves.
	s, _ := d.mergedSubscribe(ctx, nil)
	listed("no listing with a view open")
	n := listings.Load()
	d.Poke()
	wait(func() bool { return listings.Load() > n }, "a poke did not list")
	if !done(d, a.ID) {
		t.Error("a view opened after the user left saw the finish")
	}
	views.Store(here)
	wait(func() bool { return !done(d, a.ID) }, "a visit with a view open not seen")
	quiet("with a view open and nothing done")

	// Done again, and the view closes: the loop lists through the idle
	// time, so a dashboard's jump is seen.
	views.Store(elsewhere)
	finish()
	d.mergedUnsubscribe(s)
	listed("no listing in the idle time after the view closed")
	// After it, nothing lists, and a visit then is not recorded.
	d.mu.Lock()
	d.midle.Stop()
	gen := d.midleGen
	d.mu.Unlock()
	d.mergedIdle(gen)
	views.Store(here)
	quiet("after the idle time")
	if !done(d, a.ID) {
		t.Error("a visit with no view open for the idle time was seen")
	}
}

// The daemon's own agents that went while it was down lose their entries
// at its first complete poll; entries of hosts gone from the config go at
// start.
func TestAttentionForgetAtStart(t *testing.T) {
	dir := t.TempDir()
	d := attnDaemon(t, dir)
	t0 := time.Now()
	publish(d, "laatmux/%1", protocol.Agent{ID: "menv/laatmux/%1", EnvironmentID: "menv", Server: "laatmux", Session: "a", Activity: protocol.Working, ActivityAt: t0, Identity: &protocol.Identity{PID: 1}})
	publish(d, "laatmux/%2", protocol.Agent{ID: "menv/laatmux/%2", EnvironmentID: "menv", Server: "laatmux", Session: "b", Activity: protocol.Working, ActivityAt: t0, Identity: &protocol.Identity{PID: 2}})
	fromVM(d, remoteAgent("%3", "c", protocol.Working, t0))

	list := &hostsList{hosts: []peer.Host{{Name: "mac"}}}
	d = New(Config{EnvironmentID: "menv", Host: "mac", Hosts: list.get, Attention: filepath.Join(dir, "attention.json"), Targets: []Target{}})
	d.mu.Lock()
	d.agents["laatmux/%1"] = protocol.Agent{ID: "menv/laatmux/%1"}
	d.forgetLocalLocked()
	d.forgetUnconfiguredLocked([]string{"mac"})
	d.mu.Unlock()
	for id, want := range map[string]bool{"menv/laatmux/%1": true, "menv/laatmux/%2": false, "venv/laatmux/%3": false} {
		if _, ok := attnOf(d, id); ok != want {
			t.Errorf("%s kept %v, want %v", id, ok, want)
		}
	}
}

// An agent the detector cannot read, unknown while working was kept,
// changes nothing and writes nothing.
func TestAttentionUnknownWritesNothing(t *testing.T) {
	d := attnDaemon(t, t.TempDir())
	t0 := time.Now()
	fromVM(d, remoteAgent("%1", "s", protocol.Working, t0))
	d.mu.Lock()
	d.attn.dirty = false
	d.mu.Unlock()
	u := remoteAgent("%1", "s", protocol.Unknown, t0)
	u.Title = "changed"
	fromVM(d, u)
	d.mu.Lock()
	e := *d.attn.entries["venv/laatmux/%1"]
	d.mu.Unlock()
	if e.Activity != protocol.Working {
		t.Errorf("kept activity %s", e.Activity)
	}
	info, err := os.Stat(filepath.Join(filepath.Dir(d.attn.path), "attention.json"))
	if err != nil {
		t.Fatal(err)
	}
	before := info.ModTime()
	time.Sleep(20 * time.Millisecond)
	fromVM(d, u)
	if info, _ := os.Stat(d.attn.path); !info.ModTime().Equal(before) {
		t.Error("the file was written for nothing")
	}
}

// A listing that began before a finish does not see it, whatever it
// shows: the user may have left while it ran.
func TestAttentionListingBeforeFinish(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	started, release := make(chan struct{}, 4), make(chan struct{})
	var calls atomic.Int32
	hosts := &hostsList{hosts: []peer.Host{{Name: "mac"}}}
	d := New(Config{EnvironmentID: "menv", Host: "mac", Hosts: hosts.get, Attention: filepath.Join(t.TempDir(), "a.json"),
		Clients: func(ctx context.Context) ([]ClientView, error) {
			if calls.Add(1) == 1 {
				started <- struct{}{}
				select {
				case <-release:
				case <-ctx.Done():
				}
				return []ClientView{attachTo("mac", "s")}, nil
			}
			return []ClientView{attachTo("mac", "elsewhere")}, nil
		}})
	a := protocol.Agent{ID: "menv/laatmux/%1", EnvironmentID: "menv", Server: "laatmux", Session: "s", Activity: protocol.Working, ActivityAt: time.Now(), Identity: &protocol.Identity{PID: 1}}
	publish(d, "laatmux/%1", a)
	go d.runSeen(ctx)
	d.Poke()
	<-started
	// The first listing shows the agent; while it runs the agent
	// finishes.
	a.Activity, a.ActivityAt = protocol.Idle, a.ActivityAt.Add(time.Second)
	publish(d, "laatmux/%1", a)
	close(release)
	deadline := time.Now().Add(2 * time.Second)
	for calls.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(20 * time.Millisecond)
	if !done(d, a.ID) {
		t.Error("a listing from before the finish saw it")
	}
}

// A write that fails keeps the file dirty and is tried again, so a visit
// is not lost to a full disk and a restart does not bring back done.
func TestAttentionWriteRetried(t *testing.T) {
	dir := t.TempDir()
	d := attnDaemon(t, dir)
	t0 := time.Now()
	fromVM(d, remoteAgent("%1", "s", protocol.Working, t0))
	fromVM(d, remoteAgent("%1", "s", protocol.Idle, t0.Add(time.Second)))
	// The directory made unwritable: the visit is not saved.
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o700) })
	look(d, attachTo("vm", "s"))
	d.mu.Lock()
	dirty := d.attn.dirty
	d.mu.Unlock()
	if !dirty {
		t.Fatal("a failed write cleared dirty")
	}
	os.Chmod(dir, 0o700)
	d.mu.Lock()
	d.flushAttentionLocked()
	d.mu.Unlock()
	d = attnDaemon(t, dir)
	if a, ok := attnOf(d, "venv/laatmux/%1"); !ok || a.Done() {
		t.Errorf("after a retried write and a restart: %+v", a)
	}
}
