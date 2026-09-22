package daemon

import (
	"context"
	"errors"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/laat/laatmux/internal/client"
	"github.com/laat/laatmux/internal/protocol"
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
	down  error      // Dial fails with it when set
	conns []net.Conn // the remote's end of every connection accepted
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

func (r *fakeRemote) dial(ctx context.Context, h client.Host) (*client.Conn, error) {
	r.mu.Lock()
	r.dials++
	down := r.down
	r.mu.Unlock()
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
	d.seq++
	d.broadcastLocked(protocol.Message{Type: protocol.TypeUpsert, Seq: d.seq, Agent: &a})
}

func discovered(d *Daemon) { d.discoveredOnce.Do(func() { close(d.discovered) }) }

// hostsList is a host set a test changes between subscriptions.
type hostsList struct {
	mu    sync.Mutex
	hosts []client.Host
	err   error
}

func (h *hostsList) get() ([]client.Host, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]client.Host(nil), h.hosts...), h.err
}

func (h *hostsList) set(hosts ...client.Host) {
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
	rd := New(Config{EnvironmentID: "renv", Version: "remote"})
	rd.mu.Lock()
	rd.agents["laatmux/%1"] = protocol.Agent{ID: "renv/laatmux/%1", EnvironmentID: "renv", Session: "proj/x", Activity: protocol.Working}
	rd.mu.Unlock()
	discovered(rd)
	remote := newFakeRemote(t, ctx, rd)
	hosts := &hostsList{hosts: []client.Host{{Name: "here"}, {Name: "vm", SSH: "vm"}}}
	ld := New(Config{EnvironmentID: "lenv", Version: "local", Hosts: hosts.get, Dial: remote.dial, Sessions: sessions,
		MergedIdle: 100 * time.Millisecond, SessionInterval: 10 * time.Millisecond, ReconnectMin: 10 * time.Millisecond})
	ld.mu.Lock()
	ld.agents["laatmux/%7"] = protocol.Agent{ID: "lenv/laatmux/%7", EnvironmentID: "lenv", Session: "proj/y", Activity: protocol.Idle}
	ld.mu.Unlock()
	discovered(ld)
	return &mergedFixture{local: ld, remote: remote, hosts: hosts}
}

// subscribe opens a merged subscription to the local daemon and returns
// the connection and the snapshot.
func (f *mergedFixture) subscribe(t *testing.T, ctx context.Context) (net.Conn, *protocol.Conn, protocol.Message) {
	t.Helper()
	server, cl := net.Pipe()
	go f.local.HandleConn(ctx, server, func() { server.Close() })
	pc := protocol.NewConn(cl)
	if _, err := pc.Read(); err != nil {
		t.Fatal(err)
	}
	if err := pc.Write(protocol.Message{Type: protocol.TypeSubscribe, Merged: true}); err != nil {
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
	// Cached records are in the next snapshot, with the host down.
	c2, pc2, snap := f.subscribe(t, ctx)
	defer c2.Close()
	if h, _ := findHost(snap.Hosts, "vm"); h.Connected || h.Listed {
		t.Errorf("host record while down = %+v", h)
	}
	if len(snap.Agents) != 2 {
		t.Errorf("cached records missing while down: %+v", snap.Agents)
	}
	// The retry fails with ssh's message in the record.
	until(t, c2, pc2, hostStatus("vm", func(st protocol.HostStatus) bool {
		return st.Error == "ssh: connect to host vm port 22: Connection refused"
	}))

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

	f.hosts.set(client.Host{Name: "here"}, client.Host{Name: "box", SSH: "box"})
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
	if h.Connected || h.Listed || h.Error != "" {
		t.Errorf("host record after idle = %+v, want neither connected nor failed", h)
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
				f.hosts.set(client.Host{Name: "here"}, client.Host{Name: "vm", SSH: "vm"})
			} else {
				f.hosts.set(client.Host{Name: "here"}, client.Host{Name: "vm", SSH: "vm"}, client.Host{Name: "box", SSH: "box"})
			}
			c, _, _ := f.subscribe(t, ctx)
			c.Close()
		}(i)
	}
	wg.Wait()
	f.hosts.set(client.Host{Name: "here"}, client.Host{Name: "vm", SSH: "vm"})
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
