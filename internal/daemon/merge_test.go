package daemon

import (
	"context"
	"errors"
	"log"
	"net"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/laat/laatmux/internal/client"
	"github.com/laat/laatmux/internal/peer"
	"github.com/laat/laatmux/internal/protocol"
	"github.com/laat/laatmux/internal/tmux"
)

// A merging daemon under test dials a second daemon in-process over
// loopback TCP, which is what a remote host is once ssh and bridge are
// out of the way. A pipe would not do: both sides write their hello before
// reading, which a socket buffers and net.Pipe deadlocks on. The remote
// records every connection so a test can drop them.

type fakeRemote struct {
	d     *Daemon
	ln    net.Listener
	mu    sync.Mutex
	down  error         // Dial fails with it when set
	hold  chan struct{} // Dial waits for it to close when set, as ssh starting a machine
	conns []net.Conn    // the remote's end of every connection accepted
	dials int
}

func newFakeRemote(t *testing.T, ctx context.Context, d *Daemon) *fakeRemote {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	r := &fakeRemote{d: d, ln: ln}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			r.mu.Lock()
			r.conns = append(r.conns, c)
			r.mu.Unlock()
			go func() {
				defer c.Close()
				d.HandleConn(ctx, c, func() { c.Close() })
			}()
		}
	}()
	return r
}

func (r *fakeRemote) dial(ctx context.Context, h peer.Host) (*client.Conn, error) {
	r.mu.Lock()
	r.dials++
	down, hold := r.down, r.hold
	r.mu.Unlock()
	if hold != nil {
		select {
		case <-hold:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if down != nil {
		return nil, down
	}
	c, err := net.Dial("tcp", r.ln.Addr().String())
	if err != nil {
		return nil, err
	}
	return client.Connect(ctx, h, c, c, func() { c.Close() })
}

// dropAll closes the remote's end of every connection, as a lost network
// or a restarted daemon would.
func (r *fakeRemote) dropAll() {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, c := range r.conns {
		c.Close()
	}
	r.conns = nil
}

func (r *fakeRemote) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.dials
}

// publish adds or changes an agent on a daemon and broadcasts it, as a
// poll would.
func publish(d *Daemon, key string, a protocol.Agent) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.agents[key] = a
	d.broadcastLocked(protocol.Message{Type: protocol.TypeUpsert, Agent: &a})
}

// discovered completes both sides of a daemon's first poll.
func discovered(d *Daemon) {
	d.markDiscovered(&d.panesDiscovered)
	d.markDiscovered(&d.worktreesDiscovered)
}

// hostsList is a host set a test changes between subscriptions.
type hostsList struct {
	mu    sync.Mutex
	hosts []peer.Host
	err   error
	// flip, when positive, answers that many reads with no hosts and
	// then the list again: a host gone and back between two reads.
	flip int
	// reads counts every read, a flipped one included.
	reads int
}

// setFlip sets flip under the lock.
func (h *hostsList) setFlip(n int) {
	h.mu.Lock()
	h.flip = n
	h.mu.Unlock()
}

// readCount is reads under the lock.
func (h *hostsList) readCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.reads
}

func (h *hostsList) get() ([]peer.Host, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.reads++
	if h.flip > 0 {
		h.flip--
		return nil, h.err
	}
	return append([]peer.Host(nil), h.hosts...), h.err
}

func (h *hostsList) set(hosts ...peer.Host) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.hosts = hosts
}

type mergedFixture struct {
	local  *Daemon
	remote *fakeRemote
	hosts  *hostsList
}

func newMergedFixture(t *testing.T, ctx context.Context, sessions func(context.Context) ([]protocol.Session, error)) *mergedFixture {
	t.Helper()
	f := newUndiscoveredFixture(t, ctx, sessions)
	discovered(f.local)
	return f
}

// newUndiscoveredFixture is the fixture with the local daemon's first
// poll still pending.
func newUndiscoveredFixture(t *testing.T, ctx context.Context, sessions func(context.Context) ([]protocol.Session, error)) *mergedFixture {
	t.Helper()
	rd := New(Config{EnvironmentID: "renv", Version: "remote"})
	rd.mu.Lock()
	rd.agents["laatmux/%1"] = protocol.Agent{ID: "renv/laatmux/%1", EnvironmentID: "renv", Session: "proj/x", Activity: protocol.Working}
	rd.mu.Unlock()
	discovered(rd)
	remote := newFakeRemote(t, ctx, rd)
	hosts := &hostsList{hosts: []peer.Host{{Name: "here"}, {Name: "vm", SSH: "vm"}}}
	ld := New(Config{EnvironmentID: "lenv", Version: "local", Hosts: hosts.get, Dial: remote.dial, Sessions: sessions,
		MergedIdle: 100 * time.Millisecond, SessionInterval: 10 * time.Millisecond, ReconnectMin: 10 * time.Millisecond})
	ld.mu.Lock()
	ld.agents["laatmux/%7"] = protocol.Agent{ID: "lenv/laatmux/%7", EnvironmentID: "lenv", Session: "proj/y", Activity: protocol.Idle}
	ld.mu.Unlock()
	return &mergedFixture{local: ld, remote: remote, hosts: hosts}
}

// A merged subscription does not wait for the local daemon's first poll:
// the snapshot comes at once with the local host unlisted, the remote
// host is listed on its own, and the local host follows on discovery. A
// plain subscription still waits.
func TestMergedDoesNotWaitForLocalDiscovery(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f := newUndiscoveredFixture(t, ctx, nil)
	c, pc, snap := f.subscribe(t, ctx)
	defer c.Close()
	if h, _ := findHost(snap.Hosts, "here"); !h.Connected || h.Listed {
		t.Errorf("local host before discovery = %+v", h)
	}
	until(t, c, pc, hostStatus("vm", listed))
	f.local.markDiscovered(&f.local.panesDiscovered)
	f.local.markDiscovered(&f.local.worktreesDiscovered)
	until(t, c, pc, hostStatus("here", listed))
	c2, _, snap := f.subscribe(t, ctx)
	defer c2.Close()
	if h, _ := findHost(snap.Hosts, "here"); !h.Listed {
		t.Errorf("local host after discovery = %+v", h)
	}
}

// An idle callback that fired before a subscriber came and went does
// nothing: the subscriber's leaving set a newer timer, whose grace time
// is the one that counts.
func TestMergedStaleIdleCallbackIgnored(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f := newMergedFixture(t, ctx, nil)
	f.local.cfg.MergedIdle = time.Hour
	c, pc, _ := f.subscribe(t, ctx)
	until(t, c, pc, hostStatus("vm", listed))
	c.Close()
	gen := idleTimerGen(t, f.local, 0)
	c2, _, _ := f.subscribe(t, ctx) // stops the timer
	c2.Close()                      // sets a new one, once the connection's goroutine unsubscribes
	idleTimerGen(t, f.local, gen)
	f.local.mergedIdle(gen) // the old callback, late
	f.local.mu.Lock()
	active, timer := f.local.mctx != nil, f.local.midle != nil
	f.local.mu.Unlock()
	if !active || !timer {
		t.Fatalf("stale callback acted: active=%v timer=%v", active, timer)
	}
}

// idleTimerGen waits for an idle timer of a generation other than not
// and returns its generation.
func idleTimerGen(t *testing.T, d *Daemon, not uint64) uint64 {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		d.mu.Lock()
		gen, set := d.midleGen, d.midle != nil
		d.mu.Unlock()
		if set && gen != not {
			return gen
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("no idle timer after the last subscriber left")
	return 0
}

// subscribe opens a merged subscription to the local daemon and returns
// the connection and the snapshot.
func (f *mergedFixture) subscribe(t *testing.T, ctx context.Context) (net.Conn, *protocol.Conn, protocol.Message) {
	t.Helper()
	return f.subscribeAsking(t, ctx, false)
}

// subscribeAsking is subscribe, asking for the main checkouts' records
// or not.
func (f *mergedFixture) subscribeAsking(t *testing.T, ctx context.Context, checkouts bool) (net.Conn, *protocol.Conn, protocol.Message) {
	t.Helper()
	server, cl := net.Pipe()
	go f.local.HandleConn(ctx, server, func() { server.Close() })
	pc := protocol.NewConn(cl)
	if _, err := pc.Read(); err != nil {
		t.Fatal(err)
	}
	if err := pc.Write(protocol.Message{Type: protocol.TypeSubscribe, Merged: true, Checkouts: checkouts}); err != nil {
		t.Fatal(err)
	}
	snap := next(t, cl, pc)
	if snap.Type != protocol.TypeSnapshot {
		t.Fatalf("expected snapshot, got %+v", snap)
	}
	return cl, pc, snap
}

// next reads one message within two seconds.
func next(t *testing.T, c net.Conn, pc *protocol.Conn) protocol.Message {
	t.Helper()
	c.SetReadDeadline(time.Now().Add(2 * time.Second))
	m, err := pc.Read()
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	return m
}

// until reads messages until pred holds, failing after two seconds. The
// messages read are returned, the matching one last.
func until(t *testing.T, c net.Conn, pc *protocol.Conn, pred func(protocol.Message) bool) []protocol.Message {
	t.Helper()
	var seen []protocol.Message
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		c.SetReadDeadline(deadline)
		m, err := pc.Read()
		if err != nil {
			t.Fatalf("read: %v (after %d messages: %+v)", err, len(seen), seen)
		}
		seen = append(seen, m)
		if pred(m) {
			return seen
		}
	}
	t.Fatalf("condition not met; saw %+v", seen)
	return nil
}

func hostStatus(name string, pred func(protocol.HostStatus) bool) func(protocol.Message) bool {
	return func(m protocol.Message) bool {
		return m.Type == protocol.TypeUpsert && m.HostStatus != nil && m.HostStatus.Name == name && pred(*m.HostStatus)
	}
}

func listed(st protocol.HostStatus) bool  { return st.Connected && st.Listed }
func dropped(st protocol.HostStatus) bool { return !st.Connected && st.Error != "" }
func hasAgent(id string) func(protocol.Message) bool {
	return func(m protocol.Message) bool {
		return m.Type == protocol.TypeUpsert && m.Agent != nil && m.Agent.ID == id
	}
}

func findHost(hosts []protocol.HostStatus, name string) (protocol.HostStatus, bool) {
	for _, h := range hosts {
		if h.Name == name {
			return h, true
		}
	}
	return protocol.HostStatus{}, false
}

// The merged snapshot has a record per configured host and the local
// daemon's own records at once; the remote host's records and its listed
// bit follow as its connection comes up, and its upserts are forwarded
// on the merged sequence.
func TestMergedStreamHostsAndRecords(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f := newMergedFixture(t, ctx, nil)
	c, pc, snap := f.subscribe(t, ctx)
	defer c.Close()

	if len(snap.Hosts) != 2 || snap.Hosts[0].Name != "here" || snap.Hosts[1].Name != "vm" {
		t.Fatalf("hosts = %+v", snap.Hosts)
	}
	if h := snap.Hosts[0]; !h.Connected || !h.Listed || h.EnvironmentID != "lenv" || !protocol.Has(h.Capabilities, protocol.CapMerged) || !h.Local() {
		t.Errorf("local host record = %+v", h)
	}
	if h := snap.Hosts[1]; h.Connected || h.Listed || h.Error != "" || h.SSH != "vm" {
		t.Errorf("remote host record before connecting = %+v", h)
	}
	if len(snap.Agents) != 1 || snap.Agents[0].ID != "lenv/laatmux/%7" {
		t.Errorf("snapshot agents = %+v, want the local one alone", snap.Agents)
	}

	msgs := until(t, c, pc, hostStatus("vm", listed))
	var sawAgent bool
	var last uint64 = snap.Seq
	for _, m := range msgs {
		if m.Seq <= last {
			t.Errorf("seq %d after %d", m.Seq, last)
		}
		last = m.Seq
		if hasAgent("renv/laatmux/%1")(m) {
			sawAgent = true
		}
	}
	if !sawAgent {
		t.Errorf("remote agent not forwarded before listed: %+v", msgs)
	}
	if st := msgs[len(msgs)-1].HostStatus; st.EnvironmentID != "renv" || st.Version != "remote" {
		t.Errorf("listed host record = %+v", st)
	}

	// A change on the remote is forwarded; so is one of the local daemon's.
	publish(f.remote.d, "laatmux/%1", protocol.Agent{ID: "renv/laatmux/%1", EnvironmentID: "renv", Session: "proj/x", Activity: protocol.Blocked})
	m := next(t, c, pc)
	if !hasAgent("renv/laatmux/%1")(m) || m.Agent.Activity != protocol.Blocked || m.Seq != last+1 {
		t.Fatalf("forwarded remote upsert = %+v", m)
	}
	publish(f.local, "laatmux/%7", protocol.Agent{ID: "lenv/laatmux/%7", EnvironmentID: "lenv", Session: "proj/y", Activity: protocol.Working})
	m = next(t, c, pc)
	if !hasAgent("lenv/laatmux/%7")(m) || m.Agent.Activity != protocol.Working || m.Seq != last+2 {
		t.Fatalf("forwarded local upsert = %+v", m)
	}
}

// A host that drops is marked so, its records staying, and comes back
// listed after a reconnect with its records replaced in one step.
func TestMergedHostDownAndBack(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f := newMergedFixture(t, ctx, nil)
	c, pc, _ := f.subscribe(t, ctx)
	defer c.Close()
	until(t, c, pc, hostStatus("vm", listed))

	f.remote.mu.Lock()
	f.remote.down = errors.New("ssh: connect to host vm port 22: Connection refused")
	f.remote.mu.Unlock()
	f.remote.dropAll()
	msgs := until(t, c, pc, hostStatus("vm", dropped))
	for _, m := range msgs {
		if m.Type == protocol.TypeRemove {
			t.Errorf("record removed on drop: %+v", m)
		}
	}
	// The drop says a dial is coming, so a client waits for it, and
	// keeps the host's identity, so the clients keep its cached records
	// attributed to it.
	if st := *msgs[len(msgs)-1].HostStatus; !st.Reconnecting || st.Error != "disconnected" || st.EnvironmentID != "renv" || st.Version != "remote" || !protocol.Has(st.Capabilities, protocol.CapStatus) {
		t.Errorf("drop not marked reconnecting with the identity kept: %+v", st)
	}
	// Cached records are in the next snapshot, with the host down.
	c2, pc2, snap := f.subscribe(t, ctx)
	defer c2.Close()
	if h, _ := findHost(snap.Hosts, "vm"); h.Connected || h.Listed {
		t.Errorf("host record while down = %+v", h)
	}
	if len(snap.Agents) != 2 {
		t.Errorf("cached records missing while down: %+v", snap.Agents)
	}
	// The retry fails with ssh's message in the record, and the
	// reconnect is over: the host is down for a client to stop on, its
	// identity kept so the clients keep its cached records its own.
	msgs = until(t, c2, pc2, hostStatus("vm", func(st protocol.HostStatus) bool {
		return st.Error == "ssh: connect to host vm port 22: Connection refused" && !st.Reconnecting
	}))
	if st := *msgs[len(msgs)-1].HostStatus; st.EnvironmentID != "renv" || st.Version != "remote" || !protocol.Has(st.Capabilities, protocol.CapStatus) {
		t.Errorf("the failed redial dropped the host's identity: %+v", st)
	}

	// Back: the remote has changed meanwhile; the snapshot replaces its
	// records, removing what is gone, then marks it listed.
	f.remote.d.mu.Lock()
	delete(f.remote.d.agents, "laatmux/%1")
	f.remote.d.agents["laatmux/%2"] = protocol.Agent{ID: "renv/laatmux/%2", EnvironmentID: "renv", Session: "proj/z"}
	f.remote.d.mu.Unlock()
	f.remote.mu.Lock()
	f.remote.down = nil
	f.remote.mu.Unlock()
	msgs = until(t, c, pc, hostStatus("vm", listed))
	if st := *msgs[len(msgs)-1].HostStatus; st.Reconnecting || st.Error != "" {
		t.Errorf("back but still marked down: %+v", st)
	}
	var removed, added bool
	for _, m := range msgs {
		if m.Type == protocol.TypeRemove && m.AgentID == "renv/laatmux/%1" {
			removed = true
		}
		if hasAgent("renv/laatmux/%2")(m) {
			added = true
		}
	}
	if !removed || !added {
		t.Errorf("records not replaced on reconnect: removed=%v added=%v %+v", removed, added, msgs)
	}
}

// A host removed from the config is gone from the stream on the next
// subscription, records first; one added shows up.
func TestMergedHostsFollowConfig(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f := newMergedFixture(t, ctx, nil)
	c, pc, _ := f.subscribe(t, ctx)
	defer c.Close()
	until(t, c, pc, hostStatus("vm", listed))

	f.hosts.set(peer.Host{Name: "here"}, peer.Host{Name: "box", SSH: "box"})
	c2, pc2, snap := f.subscribe(t, ctx)
	defer c2.Close()
	if _, ok := findHost(snap.Hosts, "vm"); ok {
		t.Errorf("removed host still in snapshot: %+v", snap.Hosts)
	}
	if _, ok := findHost(snap.Hosts, "box"); !ok {
		t.Errorf("added host not in snapshot: %+v", snap.Hosts)
	}
	for _, a := range snap.Agents {
		if a.EnvironmentID == "renv" {
			t.Errorf("removed host's record in snapshot: %+v", a)
		}
	}
	msgs := until(t, c, pc, func(m protocol.Message) bool { return m.Type == protocol.TypeRemove && m.HostName == "vm" })
	var recordRemoved bool
	for _, m := range msgs {
		if m.Type == protocol.TypeRemove && m.AgentID == "renv/laatmux/%1" {
			recordRemoved = true
		}
	}
	if !recordRemoved {
		t.Errorf("host's records not removed before the host: %+v", msgs)
	}
	until(t, c, pc, hostStatus("box", func(st protocol.HostStatus) bool { return true }))
	until(t, c2, pc2, hostStatus("box", listed))
}

// A host removed from the config, or added, reaches the subscribers
// already there once the daemon sees the file changed, with no new
// subscription: a view that stays up follows the file.
func TestMergedHostsFollowConfigChange(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f := newMergedFixture(t, ctx, nil)
	var mu sync.Mutex
	changed := true
	f.local.cfg.Reread = func() (ConfigRead, bool, error) {
		mu.Lock()
		defer mu.Unlock()
		c := changed
		changed = false
		return ConfigRead{}, c, nil
	}
	f.local.readConfig(ctx) // Run's first read
	c, pc, _ := f.subscribe(t, ctx)
	defer c.Close()
	until(t, c, pc, hostStatus("vm", listed))
	f.hosts.set(peer.Host{Name: "here"}, peer.Host{Name: "box", SSH: "box"})
	f.local.readConfig(ctx)
	if n := f.hosts.readCount(); n != 1 {
		t.Fatalf("the hosts read %d times with the file unchanged", n)
	}
	mu.Lock()
	changed = true
	mu.Unlock()
	f.local.readConfig(ctx)
	until(t, c, pc, func(m protocol.Message) bool { return m.Type == protocol.TypeRemove && m.HostName == "vm" })
	until(t, c, pc, hostStatus("box", listed))
	// A read of the hosts that fails, a file being written say, keeps
	// the host set.
	f.hosts.mu.Lock()
	f.hosts.hosts, f.hosts.err = nil, errors.New("the config file is empty while it is being written")
	f.hosts.mu.Unlock()
	mu.Lock()
	changed = true
	mu.Unlock()
	f.local.readConfig(ctx)
	f.local.mu.Lock()
	_, kept := f.local.mhosts["box"]
	f.local.mu.Unlock()
	if !kept {
		t.Fatal("a failed read of the hosts dropped box")
	}
	// The read is made again at the next look, the file unchanged to
	// the watch, as one that settles empty is.
	f.hosts.mu.Lock()
	f.hosts.hosts, f.hosts.err = []peer.Host{{Name: "here"}, {Name: "box", SSH: "box"}, {Name: "vm", SSH: "vm"}}, nil
	f.hosts.mu.Unlock()
	f.local.readConfig(ctx)
	until(t, c, pc, hostStatus("vm", listed))
	reads := f.hosts.readCount()
	f.local.readConfig(ctx)
	if n := f.hosts.readCount(); n != reads {
		t.Fatalf("the hosts read again after a read that succeeded: %d", n-reads)
	}
}

// A host paused in the config is never dialled, and is a record that
// says so, with none of its own; resumed, once the daemon sees the file
// changed, it is dialled and listed with its records; paused again, its
// records are removed and its record with them, as for a host removed
// from the config, and the record that says it is paused takes its
// place, for the subscribers already there and for a new one alike.
func TestMergedHostPaused(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f := newMergedFixture(t, ctx, nil)
	// The merging daemon says it takes paused; one that merges nothing
	// has no hosts to pause.
	if !protocol.Has(f.local.capabilities(), protocol.CapPause) || protocol.Has(f.remote.d.capabilities(), protocol.CapPause) {
		t.Fatalf("capabilities %v, the remote's %v", f.local.capabilities(), f.remote.d.capabilities())
	}
	vm, paused := peer.Host{Name: "vm", SSH: "vm"}, peer.Host{Name: "vm", SSH: "vm", Paused: true}
	f.hosts.set(peer.Host{Name: "here"}, paused)
	var mu sync.Mutex
	changed := true
	f.local.cfg.Reread = func() (ConfigRead, bool, error) {
		mu.Lock()
		defer mu.Unlock()
		c := changed
		changed = false
		return ConfigRead{}, c, nil
	}
	f.local.readConfig(ctx) // Run's first read
	edit := func(hosts ...peer.Host) {
		f.hosts.set(hosts...)
		mu.Lock()
		changed = true
		mu.Unlock()
		f.local.readConfig(ctx)
	}
	isPaused := func(st protocol.HostStatus) bool { return st.Paused && !st.Connected && !st.Listed && st.Error == "" }
	noDials := func(want int) {
		t.Helper()
		// Ten of the fixture's reconnect waits.
		time.Sleep(100 * time.Millisecond)
		if n := f.remote.count(); n != want {
			t.Fatalf("a paused host dialled %d times", n-want)
		}
	}
	c, pc, snap := f.subscribe(t, ctx)
	defer c.Close()
	if st, ok := findHost(snap.Hosts, "vm"); !ok || !isPaused(st) {
		t.Fatalf("snapshot hosts %+v", snap.Hosts)
	}
	noDials(0)

	edit(peer.Host{Name: "here"}, vm)
	msgs := until(t, c, pc, hostStatus("vm", listed))
	if !slices.ContainsFunc(msgs, hasAgent("renv/laatmux/%1")) {
		t.Fatalf("resumed host's records not listed: %+v", msgs)
	}

	edit(peer.Host{Name: "here"}, paused)
	msgs = until(t, c, pc, hostStatus("vm", isPaused))
	recordRemoved := slices.IndexFunc(msgs, func(m protocol.Message) bool { return m.Type == protocol.TypeRemove && m.AgentID == "renv/laatmux/%1" })
	hostRemoved := slices.IndexFunc(msgs, func(m protocol.Message) bool { return m.Type == protocol.TypeRemove && m.HostName == "vm" })
	if recordRemoved < 0 || hostRemoved < recordRemoved {
		t.Fatalf("paused host's records not removed before its record: %+v", msgs)
	}
	dials := f.remote.count()
	c2, _, snap := f.subscribe(t, ctx)
	defer c2.Close()
	if st, ok := findHost(snap.Hosts, "vm"); !ok || !isPaused(st) {
		t.Fatalf("snapshot hosts after the pause %+v", snap.Hosts)
	}
	for _, a := range snap.Agents {
		if a.EnvironmentID == "renv" {
			t.Fatalf("paused host's record in the snapshot: %+v", a)
		}
	}
	noDials(dials)
}

// Remote subscriptions are dropped once no merged subscriber has been
// around for the idle time, and taken up again by the next one, which
// sees the cached records with the host neither connected nor failed.
func TestMergedIdleDrop(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f := newMergedFixture(t, ctx, nil)
	c, pc, _ := f.subscribe(t, ctx)
	until(t, c, pc, hostStatus("vm", listed))
	dials := f.remote.count()
	c.Close()

	// The remote's end of the connection sees EOF once the follow stops.
	f.remote.mu.Lock()
	rc := f.remote.conns[len(f.remote.conns)-1]
	f.remote.mu.Unlock()
	rc.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := rc.Read(make([]byte, 1)); err == nil {
		t.Fatal("remote connection still open after idle")
	} else if ne, ok := err.(net.Error); ok && ne.Timeout() {
		t.Fatal("remote connection not closed within the idle time")
	}
	time.Sleep(50 * time.Millisecond)
	if n := f.remote.count(); n != dials {
		t.Errorf("dialled %d times after idle, want none", n-dials)
	}

	c2, pc2, snap := f.subscribe(t, ctx)
	defer c2.Close()
	h, _ := findHost(snap.Hosts, "vm")
	if h.Connected || h.Listed || h.Error != "" || h.EnvironmentID != "renv" {
		t.Errorf("host record after idle = %+v, want neither connected nor failed, its identity kept", h)
	}
	if len(snap.Agents) != 2 {
		t.Errorf("cached records missing after idle: %+v", snap.Agents)
	}
	until(t, c2, pc2, hostStatus("vm", listed))
	if f.remote.count() != dials+1 {
		t.Errorf("dials = %d, want one reconnect", f.remote.count()-dials)
	}
}

// The local sessions are in the snapshot, listed on the subscription
// itself, and their changes are published by the poll.
func TestMergedSessions(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var mu sync.Mutex
	sessions := []protocol.Session{{Name: "vm/proj/x", Key: "renv//r/x", Host: "vm"}}
	var listErr error
	list := func(context.Context) ([]protocol.Session, error) {
		mu.Lock()
		defer mu.Unlock()
		return append([]protocol.Session(nil), sessions...), listErr
	}
	f := newMergedFixture(t, ctx, list)
	c, pc, snap := f.subscribe(t, ctx)
	defer c.Close()
	if len(snap.Sessions) != 1 || snap.Sessions[0].Name != "vm/proj/x" || snap.SessionsError != "" {
		t.Fatalf("snapshot sessions = %+v %q", snap.Sessions, snap.SessionsError)
	}
	mu.Lock()
	sessions = []protocol.Session{{Name: "vm/proj/x", Key: "renv//r/x", Host: "vm", Settled: true}, {Name: "here/w", Attach: "here/w", Host: "here"}}
	mu.Unlock()
	var settled, added bool
	until(t, c, pc, func(m protocol.Message) bool {
		if m.Type == protocol.TypeUpsert && m.LocalSession != nil {
			switch {
			case m.LocalSession.Name == "vm/proj/x" && m.LocalSession.Settled:
				settled = true
			case m.LocalSession.Name == "here/w":
				added = true
			}
		}
		return settled && added
	})
	mu.Lock()
	sessions = sessions[:1]
	mu.Unlock()
	until(t, c, pc, func(m protocol.Message) bool { return m.Type == protocol.TypeRemove && m.LocalSessionName == "here/w" })

	// A listing that fails keeps the records, tells the subscriber, and
	// says so in the next snapshot; a listing that works again clears it.
	mu.Lock()
	listErr = errors.New("tmux: permission denied")
	mu.Unlock()
	until(t, c, pc, func(m protocol.Message) bool {
		return m.Type == protocol.TypeUpsert && m.SessionsError == "tmux: permission denied"
	})
	c2, pc2, snap := f.subscribe(t, ctx)
	defer c2.Close()
	_ = pc2
	if snap.SessionsError != "tmux: permission denied" || len(snap.Sessions) != 1 {
		t.Errorf("snapshot after failed listing = %+v %q", snap.Sessions, snap.SessionsError)
	}
	mu.Lock()
	listErr = nil
	mu.Unlock()
	until(t, c, pc, func(m protocol.Message) bool { return m.Type == protocol.TypeUpsert && m.SessionsListed })
}

// A merged subscriber dropped for falling behind counts as gone: when it
// was the last, the idle timer runs and the remote connection closes.
func TestMergedOverflowStartsIdle(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f := newMergedFixture(t, ctx, nil)
	c, pc, _ := f.subscribe(t, ctx)
	defer c.Close()
	until(t, c, pc, hostStatus("vm", listed))
	f.remote.mu.Lock()
	rc := f.remote.conns[len(f.remote.conns)-1]
	f.remote.mu.Unlock()

	// Overflow without reading: the pipe is unbuffered, so the writer
	// blocks on the first message and the channel fills.
	f.local.mu.Lock()
	for i := 0; i < subscriberBuffer+8; i++ {
		f.local.mbroadcastLocked(protocol.Message{Type: protocol.TypeUpsert, Agent: &protocol.Agent{ID: "x"}})
	}
	n := len(f.local.msubs)
	f.local.mu.Unlock()
	if n != 0 {
		t.Fatalf("%d merged subscribers after overflow, want none", n)
	}
	rc.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := rc.Read(make([]byte, 1)); err == nil {
		t.Fatal("remote connection still open after overflow and idle")
	} else if ne, ok := err.(net.Error); ok && ne.Timeout() {
		t.Fatal("remote connection not closed within the idle time after an overflow")
	}
}

// Subscriptions arriving together, with the config changing under them,
// leave the host set as the last read had it; run under -race.
func TestMergedConcurrentSubscriptions(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f := newMergedFixture(t, ctx, nil)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if i%2 == 0 {
				f.hosts.set(peer.Host{Name: "here"}, peer.Host{Name: "vm", SSH: "vm"})
			} else {
				f.hosts.set(peer.Host{Name: "here"}, peer.Host{Name: "vm", SSH: "vm"}, peer.Host{Name: "box", SSH: "box"})
			}
			c, _, _ := f.subscribe(t, ctx)
			c.Close()
		}(i)
	}
	wg.Wait()
	f.hosts.set(peer.Host{Name: "here"}, peer.Host{Name: "vm", SSH: "vm"})
	c, _, snap := f.subscribe(t, ctx)
	defer c.Close()
	if len(snap.Hosts) != 2 {
		t.Errorf("hosts after the last read = %+v", snap.Hosts)
	}
}

// A daemon without hosts in its config has no merged capability and says
// so to a merged subscribe.
func TestMergedRefusedWithoutHosts(t *testing.T) {
	d := newTestDaemon()
	discovered(d)
	if protocol.Has(d.capabilities(), protocol.CapMerged) {
		t.Fatal("merged advertised without hosts")
	}
	server, cl := net.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go d.HandleConn(ctx, server, func() { server.Close() })
	pc := protocol.NewConn(cl)
	next(t, cl, pc) // hello
	pc.Write(protocol.Message{Type: protocol.TypeSubscribe, Merged: true})
	if m := next(t, cl, pc); m.Type != protocol.TypeError {
		t.Fatalf("got %+v, want error", m)
	}
}

// The daemon's own records reach plain subscribers as before, and the
// merged stream does not: a plain subscribe is this host alone.
func TestPlainSubscribeUnchanged(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f := newMergedFixture(t, ctx, nil)
	server, cl := net.Pipe()
	go f.local.HandleConn(ctx, server, func() { server.Close() })
	pc := protocol.NewConn(cl)
	next(t, cl, pc)
	pc.Write(protocol.Message{Type: protocol.TypeSubscribe})
	snap := next(t, cl, pc)
	if snap.Type != protocol.TypeSnapshot || len(snap.Hosts) != 0 || len(snap.Agents) != 1 {
		t.Fatalf("plain snapshot = %+v", snap)
	}
	if f.remote.count() != 0 {
		t.Error("a plain subscribe dialled a remote host")
	}
}

// A host's pane and run records travel in the merged stream as its
// agents do: in its snapshot, as upserts and removes, and out with the
// host when it leaves the config.
func TestMergedPaneAndRunRecords(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f := newMergedFixture(t, ctx, nil)
	rd := f.remote.d
	rd.mu.Lock()
	rd.paneRecs["default/%3"] = protocol.Pane{ID: "renv/pane/default/%3", EnvironmentID: "renv", WorktreeID: "renv/worktree//w/a", Command: "zsh"}
	rd.mu.Unlock()
	c, pc, _ := f.subscribe(t, ctx)
	defer c.Close()
	msgs := until(t, c, pc, hostStatus("vm", listed))
	var sawPane bool
	for _, m := range msgs {
		sawPane = sawPane || (m.Pane != nil && m.Pane.ID == "renv/pane/default/%3")
	}
	if !sawPane {
		t.Fatalf("remote pane record not forwarded: %+v", msgs)
	}
	rd.runStarted(&runJob{id: "r1", root: "/w/a", argv: []string{"make"}}, time.Now())
	if m := next(t, c, pc); m.Run == nil || m.Run.ID != "renv/run/r1" || m.Run.WorktreeID != "renv/worktree//w/a" {
		t.Fatalf("run upsert %+v", m)
	}
	c2, _, snap := f.subscribe(t, ctx)
	c2.Close()
	if len(snap.Panes) != 1 || len(snap.Runs) != 1 {
		t.Fatalf("snapshot panes %+v runs %+v", snap.Panes, snap.Runs)
	}
	rd.mu.Lock()
	rd.runEndedLocked(&runJob{id: "r1"})
	rd.mu.Unlock()
	if m := next(t, c, pc); m.Type != protocol.TypeRemove || m.RunID != "renv/run/r1" {
		t.Fatalf("run remove %+v", m)
	}
	f.hosts.set(peer.Host{Name: "here"})
	c3, _, _ := f.subscribe(t, ctx)
	c3.Close()
	got := until(t, c, pc, func(m protocol.Message) bool { return m.PaneRecordID == "renv/pane/default/%3" })
	if m := got[len(got)-1]; m.Type != protocol.TypeRemove {
		t.Fatalf("pane remove %+v", m)
	}
}

// The merging daemon asks its hosts for their main checkouts' records
// and forwards them, in the snapshot and as upserts, to a merged
// subscriber that asked for them, and to no other, which still gets the
// worktree and the agent naming the checkout.
func TestMergedMainCheckouts(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f := newMergedFixture(t, ctx, nil)
	rd := f.remote.d
	main := protocol.Worktree{ID: "renv/checkout//r/proj", EnvironmentID: "renv", Repo: "proj", Branch: "main", Root: "/r/proj", Main: true}
	rd.mu.Lock()
	rd.worktrees["/r/proj"] = main
	rd.worktrees["/w/a"] = protocol.Worktree{ID: "renv/worktree//w/a", EnvironmentID: "renv", Repo: "proj", Branch: "a", Root: "/w/a"}
	rd.mu.Unlock()
	hasMain := func(ws []protocol.Worktree) bool {
		return slices.ContainsFunc(ws, func(w protocol.Worktree) bool { return w.Main })
	}
	c, pc, _ := f.subscribeAsking(t, ctx, true)
	defer c.Close()
	msgs := until(t, c, pc, hostStatus("vm", listed))
	if !slices.ContainsFunc(msgs, func(m protocol.Message) bool { return m.Worktree != nil && m.Worktree.ID == main.ID }) {
		t.Fatalf("the main checkout not forwarded: %+v", msgs)
	}
	// Subscribed once the host is listed, so its snapshot is checked
	// below with the others, and its upserts from here on.
	c2, pc2, _ := f.subscribeAsking(t, ctx, false)
	defer c2.Close()
	c3, _, snap := f.subscribeAsking(t, ctx, true)
	c3.Close()
	c4, _, snap2 := f.subscribeAsking(t, ctx, false)
	c4.Close()
	if !hasMain(snap.Worktrees) || hasMain(snap2.Worktrees) || len(snap2.Worktrees) != 1 {
		t.Fatalf("snapshots: asked %+v, not %+v", snap.Worktrees, snap2.Worktrees)
	}
	// An upsert of the checkout, then the agent in it.
	main.Branch = "feature"
	a := protocol.Agent{ID: "renv/default/%9", EnvironmentID: "renv", Server: "default", Session: "work", WorktreeID: main.ID}
	rd.mu.Lock()
	rd.worktrees["/r/proj"] = main
	rd.broadcastLocked(protocol.Message{Type: protocol.TypeUpsert, Worktree: &main})
	rd.agents["default/%9"] = a
	rd.broadcastLocked(protocol.Message{Type: protocol.TypeUpsert, Agent: &a})
	rd.mu.Unlock()
	got := until(t, c, pc, hasAgent(a.ID))
	if !slices.ContainsFunc(got, func(m protocol.Message) bool { return m.Worktree != nil && m.Worktree.Branch == "feature" }) || got[len(got)-1].Agent.WorktreeID != main.ID {
		t.Fatalf("the checkout's upsert and the agent in it: %+v", got)
	}
	got = until(t, c2, pc2, hasAgent(a.ID))
	for _, m := range got {
		if m.Worktree != nil {
			t.Fatalf("forwarded to a subscriber that did not ask: %+v", m)
		}
	}
	// The agent is one of no worktree there, as before, in the upsert
	// and in a snapshot.
	if got[len(got)-1].Agent.WorktreeID != "" {
		t.Fatalf("the attribution forwarded to a subscriber that did not ask: %+v", got[len(got)-1].Agent)
	}
	c5, _, snap3 := f.subscribeAsking(t, ctx, false)
	c5.Close()
	if i := slices.IndexFunc(snap3.Agents, func(x protocol.Agent) bool { return x.ID == a.ID }); i < 0 || snap3.Agents[i].WorktreeID != "" {
		t.Fatalf("the attribution in a snapshot that did not ask: %+v", snap3.Agents)
	}
	// This machine dropped from the hosts: its main checkout's record
	// goes by its own id.
	lmain := protocol.Worktree{ID: "lenv/checkout//code/x", EnvironmentID: "lenv", Repo: "x", Branch: "main", Root: "/code/x", Main: true}
	f.local.mu.Lock()
	f.local.worktrees["/code/x"] = lmain
	f.local.mu.Unlock()
	f.hosts.set(peer.Host{Name: "vm", SSH: "vm"})
	c6, _, _ := f.subscribeAsking(t, ctx, true)
	c6.Close()
	until(t, c, pc, func(m protocol.Message) bool { return m.Type == protocol.TypeRemove && m.WorktreeID == lmain.ID })
}

// A reconnect's snapshot is forwarded in the host's own order: the
// worktrees before what names them, and a shell that became an agent
// while the connection was down removed as a pane before it comes as an
// agent.
func TestMergedSnapshotOrder(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f := newMergedFixture(t, ctx, nil)
	rd := f.remote.d
	rd.mu.Lock()
	rd.paneRecs["default/%3"] = protocol.Pane{ID: "renv/pane/default/%3", EnvironmentID: "renv", WorktreeID: "renv/worktree//w/a"}
	rd.mu.Unlock()
	c, pc, _ := f.subscribe(t, ctx)
	defer c.Close()
	until(t, c, pc, hostStatus("vm", listed))
	rd.mu.Lock()
	delete(rd.paneRecs, "default/%3")
	rd.agents["default/%3"] = protocol.Agent{ID: "renv/default/%3", EnvironmentID: "renv", WorktreeID: "renv/worktree//w/b"}
	rd.worktrees["/w/b"] = protocol.Worktree{ID: "renv/worktree//w/b", EnvironmentID: "renv", Root: "/w/b"}
	rd.mu.Unlock()
	f.remote.dropAll()
	until(t, c, pc, hostStatus("vm", dropped))
	msgs := until(t, c, pc, hostStatus("vm", listed))
	at := map[string]int{}
	for i, m := range msgs {
		switch {
		case m.Worktree != nil && m.Worktree.ID == "renv/worktree//w/b":
			at["worktree"] = i
		case m.PaneRecordID == "renv/pane/default/%3":
			at["pane gone"] = i
		case m.Agent != nil && m.Agent.ID == "renv/default/%3":
			at["agent"] = i
		}
	}
	w, wok := at["worktree"]
	g, gok := at["pane gone"]
	a, aok := at["agent"]
	if !wok || !gok || !aok || !(w < a) || !(g < a) {
		t.Fatalf("order %v in %+v", at, msgs)
	}
}

// A listing error is logged and published once per change of its text,
// not once per poll; a listing that works again is published once.
func TestSessionsErrorOnce(t *testing.T) {
	var logged strings.Builder
	d := New(Config{EnvironmentID: "lenv", Version: "local", Logger: log.New(&logged, "", 0)})
	seq := func() uint64 { d.mu.Lock(); defer d.mu.Unlock(); return d.mseq }
	apply := func(err error) {
		d.mu.Lock()
		d.applySessionsLocked(nil, err)
		d.mu.Unlock()
	}
	denied := errors.New("tmux: permission denied")
	apply(denied)
	apply(denied)
	apply(errors.New("tmux: permission denied"))
	if n := seq(); n != 1 {
		t.Fatalf("%d messages for one error repeated, want 1", n)
	}
	apply(errors.New("tmux: no server"))
	if n := seq(); n != 2 {
		t.Fatalf("%d messages after a different error, want 2", n)
	}
	apply(nil)
	apply(nil)
	if n := seq(); n != 3 {
		t.Fatalf("%d messages after the listing works again, want 3", n)
	}
	if got := logged.String(); strings.Count(got, "permission denied") != 1 || strings.Count(got, "no server") != 1 || strings.Count(got, "listed again") != 1 {
		t.Fatalf("log:\n%s", got)
	}
}

// A sessions listing that a user's hook failed after is applied: its
// sessions are published and no error is, and after a failed listing it
// is the one listed again. The hook's error is logged once while it
// fails, and once more when it fails again after a listing that works,
// not after one that failed.
func TestSessionsHookError(t *testing.T) {
	var logged strings.Builder
	d := New(Config{EnvironmentID: "lenv", Version: "local", Logger: log.New(&logged, "", 0)})
	apply := func(recs []protocol.Session, err error) {
		d.mu.Lock()
		d.applySessionsLocked(recs, err)
		d.mu.Unlock()
	}
	hook := &tmux.HookError{Err: &tmux.Error{Args: []string{"list-sessions", "-F", "#{session_name}"}, Msg: "can't find session: nosuch"}}
	ws := []protocol.Session{{Name: "vm/proj/x", Key: "env1/w/x", Host: "vm"}}
	want := func(n int, when string) {
		t.Helper()
		if got := logged.String(); strings.Count(got, "sessions: tmux list-sessions -F #{session_name}: can't find session: nosuch (after") != n {
			t.Fatalf("%s: the hook's error not logged %d times; log:\n%s", when, n, got)
		}
	}
	apply(nil, errors.New("tmux: permission denied"))
	apply(ws, hook)
	apply(ws, hook)
	d.mu.Lock()
	seq, s, serr := d.mseq, d.msessions["vm/proj/x"], d.sessionsErr
	d.mu.Unlock()
	// The failure, its recovery and the session: three messages.
	if seq != 3 || s != ws[0] || serr != "" {
		t.Fatalf("after the hook's listings: %d messages, session %+v, error %q; want 3, %+v and none", seq, s, serr, ws[0])
	}
	if strings.Count(logged.String(), "listed again") != 1 {
		t.Fatalf("the hook's listing not the one listed again; log:\n%s", logged.String())
	}
	want(1, "two listings")
	apply(ws, nil)
	want(1, "a listing that works")
	apply(ws, hook)
	want(2, "the hook failing again")
	apply(nil, errors.New("tmux: permission denied"))
	apply(ws, hook)
	want(2, "a failed listing between")
}
