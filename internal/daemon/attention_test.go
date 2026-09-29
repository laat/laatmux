package daemon

import (
	"context"
	"net"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/laat/laatmux/internal/client"
	"github.com/laat/laatmux/internal/protocol"
)

// attnDaemon is a merging daemon with attention, this machine mac and a
// remote host vm, the file under dir.
func attnDaemon(t *testing.T, dir string, hosts ...client.Host) *Daemon {
	t.Helper()
	if len(hosts) == 0 {
		hosts = []client.Host{{Name: "mac"}, {Name: "vm", SSH: "vm"}}
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
	return protocol.Agent{ID: "venv/laatmux/" + pane, EnvironmentID: "venv", Session: session, PaneID: pane, Agent: "claude",
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
	d.markSeenLocked(views)
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
	a := protocol.Agent{ID: "menv/laatmux/%1", EnvironmentID: "menv", Session: "proj/x", PaneID: "%1", Agent: "claude",
		Activity: protocol.Working, ActivityAt: t0, Liveness: protocol.Alive, Managed: true, Identity: &protocol.Identity{PID: 1}}
	publish(d, "laatmux/%1", a)
	a.Activity, a.ActivityAt = protocol.Idle, t0.Add(time.Second)
	publish(d, "laatmux/%1", a)
	d.mu.Lock()
	d.reconcileHostsLocked([]client.Host{{Name: "laptop"}, {Name: "vm", SSH: "vm"}})
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
// default server, one on another observed server. This machine's default
// server is, through the agent's own pane.
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
	d.reconcileHostsLocked([]client.Host{{Name: "mac"}})
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
	hosts := &hostsList{hosts: []client.Host{{Name: "mac"}}}
	d := New(Config{EnvironmentID: "menv", Host: "mac", Hosts: hosts.get, Attention: filepath.Join(t.TempDir(), "a.json"),
		Clients: func(context.Context) ([]ClientView, error) { listings.Add(1); return nil, nil }})
	if !protocol.Has(d.capabilities(), protocol.CapAttention) {
		t.Fatal("no attention capability")
	}
	discovered(d)
	go d.runSeen(ctx)
	a := protocol.Agent{ID: "menv/laatmux/%1", EnvironmentID: "menv", Session: "s", Activity: protocol.Working, ActivityAt: time.Now(), Identity: &protocol.Identity{PID: 1}}
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
