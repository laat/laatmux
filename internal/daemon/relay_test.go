package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/laat/laatmux/internal/client"
	"github.com/laat/laatmux/internal/procs"
	"github.com/laat/laatmux/internal/protocol"
	"github.com/laat/laatmux/internal/worktree"
)

// relayFixture is a laptop daemon with the relay, dialling a host
// daemon with the task capability in-process.
type relayFixture struct {
	host   *Daemon
	ft     *fakeServer
	store  *worktree.Store
	remote *fakeRemote
	hosts  *hostsList
	local  *Daemon
	dir    string
	ctx    context.Context
}

func newRelayFixture(t *testing.T, screen []string) *relayFixture {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	store, remote := newStore(t)
	ft := &fakeServer{screen: screen}
	host := New(Config{
		EnvironmentID: "henv", Host: "vm", Version: "host",
		Targets: []Target{{Label: "laatmux", Tmux: ft, Managed: true}},
		Procs:   &fakeProcs{tables: []procTable{{procs: []procs.Proc{shell, claude}}}},
		Store:   store, Agents: map[string][]string{"claude": {"claude"}, "argv": {"claude", PromptPlaceholder}},
		Commands: t.TempDir(), WorktreeInterval: 50 * time.Millisecond,
	})
	go host.Run(ctx)
	fr := newFakeRemote(t, ctx, host)
	hosts := &hostsList{hosts: []client.Host{{Name: "vm", SSH: "vm"}}}
	dir := t.TempDir()
	local := New(Config{
		EnvironmentID: "lenv", Version: "local", Hosts: hosts.get, Dial: fr.dial, Pending: dir,
		MergedIdle: 200 * time.Millisecond, SessionInterval: 20 * time.Millisecond, ReconnectMin: 20 * time.Millisecond,
	})
	discovered(local)
	go local.Run(ctx)
	f := &relayFixture{host: host, ft: ft, store: store, remote: fr, hosts: hosts, local: local, dir: dir, ctx: ctx}
	_ = remote
	return f
}

// source is the host's repository source.
func (f *relayFixture) source() string { return f.store.Repos[0].Source }

// merged subscribes to the local daemon's merged stream.
func (f *relayFixture) merged(t *testing.T) (net.Conn, *protocol.Conn, protocol.Message) {
	t.Helper()
	server, cl := net.Pipe()
	go f.local.HandleConn(f.ctx, server, func() { server.Close() })
	pc := protocol.NewConn(cl)
	if _, err := pc.Read(); err != nil {
		t.Fatal(err)
	}
	if err := pc.Write(protocol.Message{Type: protocol.TypeSubscribe, Merged: true}); err != nil {
		t.Fatal(err)
	}
	cl.SetReadDeadline(time.Now().Add(30 * time.Second))
	snap, err := pc.Read()
	if err != nil || snap.Type != protocol.TypeSnapshot {
		t.Fatalf("snapshot %+v %v", snap, err)
	}
	return cl, pc, snap
}

// request sends one command to the local daemon and returns its result.
func (f *relayFixture) request(t *testing.T, m protocol.Message) protocol.Message {
	t.Helper()
	pc := conn(t, f.local)
	if err := pc.Write(m); err != nil {
		t.Fatal(err)
	}
	res, _ := result(t, pc, m.ID)
	return res
}

// awaitMerged reads the merged stream until pred holds, within wait.
func awaitMerged(t *testing.T, c net.Conn, pc *protocol.Conn, wait time.Duration, pred func(protocol.Message) bool) protocol.Message {
	t.Helper()
	deadline := time.Now().Add(wait)
	c.SetReadDeadline(deadline)
	for {
		m, err := pc.Read()
		if err != nil {
			t.Fatalf("merged stream: %v", err)
		}
		if pred(m) {
			return m
		}
	}
}

// readPending reads a record's file.
func readPending(t *testing.T, dir, id string) pendingFile {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, fileName(id)))
	if err != nil {
		t.Fatal(err)
	}
	var p pendingFile
	if err := json.Unmarshal(b, &p); err != nil {
		t.Fatal(err)
	}
	return p
}

// awaitRecord polls the relay until pred holds for the record.
func (f *relayFixture) awaitRecord(t *testing.T, id string, wait time.Duration, pred func(pendingFile) bool) pendingFile {
	t.Helper()
	deadline := time.Now().Add(wait)
	for {
		p, ok := f.local.relay.get(id)
		if ok && pred(p) {
			return p
		}
		if time.Now().After(deadline) {
			t.Fatalf("record %s: %+v", id, p)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// A relayed add: accepted once the file is on disk, prompt included;
// run against the host and followed to its result, the record in the
// merged stream as it goes; then, the listing after the result seen,
// handed over to the worktree row with the prompt scrubbed and the
// handoff kept.
func TestRelayAdd(t *testing.T) {
	f := newRelayFixture(t, nil)
	c, pc, snap := f.merged(t)
	defer c.Close()
	if !protocol.Has(f.local.capabilities(), protocol.CapRelay) || len(snap.Pendings) != 0 {
		t.Fatalf("caps %v pendings %d", f.local.capabilities(), len(snap.Pendings))
	}
	res := f.request(t, protocol.Message{Type: protocol.TypeAdd, ID: "t1", Relay: "vm", Repo: f.source(), Name: "proj", Branch: "task", Generated: true, AgentName: "argv", Prompt: "the secret", SubmittedAt: time.Now()})
	if !res.OK {
		t.Fatalf("accept: %+v", res)
	}
	if p := readPending(t, f.dir, "t1"); p.PromptText != "the secret" || p.Host != "vm" || p.Done {
		t.Fatalf("file after accept: %+v", p)
	}
	// The record shows up, then progresses, then is removed into its
	// worktree row.
	var last protocol.Pending
	rm := awaitMerged(t, c, pc, 30*time.Second, func(m protocol.Message) bool {
		if m.Type == protocol.TypeUpsert && m.Pending != nil && m.Pending.ID == "t1" {
			last = *m.Pending
		}
		return m.Type == protocol.TypeRemove && m.PendingID == "t1"
	})
	root := f.store.Dirs.Worktree("proj", "task")
	if rm.ReplacedBy != "henv/worktree/"+root {
		t.Fatalf("remove %+v", rm)
	}
	if !last.Done || !last.OK || last.Prompt != protocol.DeliveryDelivered || last.Root != root || last.Branch != "task" || last.EnvironmentID != "henv" || !last.Taken || !last.Reachable {
		t.Fatalf("last record %+v", last)
	}
	p := readPending(t, f.dir, "t1")
	if p.PromptText != "" || p.ReplacedBy != rm.ReplacedBy || p.RetiredAt.IsZero() || !p.Listed {
		t.Fatalf("file after handoff: %+v", p)
	}
	c2, _, snap2 := f.merged(t)
	defer c2.Close()
	if len(snap2.Pendings) != 0 || len(snap2.Handoffs) != 1 || snap2.Handoffs[0].ReplacedBy != rm.ReplacedBy {
		t.Fatalf("snapshot after handoff: pendings %d handoffs %+v", len(snap2.Pendings), snap2.Handoffs)
	}
	// The host journaled it under the client's id, with the prompt on
	// the argv and nothing of it in the journal.
	e := readEntry(t, f.host, "t1")
	if e.Delivery != protocol.DeliveryDelivered || !e.ArgvPrompt {
		t.Fatalf("host entry %+v", e)
	}
	// A second submit under the same id is accepted again and runs
	// nothing.
	if res := f.request(t, protocol.Message{Type: protocol.TypeAdd, ID: "t1", Relay: "vm", Repo: f.source(), Branch: "task", AgentName: "argv"}); !res.OK {
		t.Fatalf("resubmit %+v", res)
	}
	// Swept after the retention.
	f.local.relay.sweep(time.Now().Add(handoffRetention + time.Second))
	if _, err := os.Stat(filepath.Join(f.dir, fileName("t1"))); err == nil {
		t.Fatal("retired file kept past the retention")
	}
}

// A record that needs the user stays: the add succeeded but the prompt
// was not delivered, the prompt is retained, the listing is noted, and
// p delivers it later as an attempt the file holds first; then the
// record is complete and retires.
func TestRelayPromptLater(t *testing.T) {
	shortWait(t, 300*time.Millisecond)
	f := newRelayFixture(t, []string{"loading"})
	c, pc, _ := f.merged(t)
	defer c.Close()
	if res := f.request(t, protocol.Message{Type: protocol.TypeAdd, ID: "t2", Relay: "vm", Repo: f.source(), Name: "proj", Branch: "typed", AgentName: "claude", Prompt: "later", SubmittedAt: time.Now()}); !res.OK {
		t.Fatal(res.Error)
	}
	p := f.awaitRecord(t, "t2", 30*time.Second, func(p pendingFile) bool { return p.Done && p.Listed })
	if !p.OK || p.Prompt != protocol.DeliveryNotDelivered || p.PromptText != "later" || p.retired() || !strings.Contains(p.Error, "not ready") {
		t.Fatalf("record %+v", p)
	}
	// Dismiss is refused while an attempt is open; p on it delivers.
	f.ft.set(func() { f.ft.screen = idleScreen })
	res := f.request(t, protocol.Message{Type: protocol.TypePrompt, ID: "t2"})
	if !res.OK || res.Prompt != protocol.DeliveryDelivered || res.Attempt != 1 {
		t.Fatalf("prompt %+v", res)
	}
	rm := awaitMerged(t, c, pc, 30*time.Second, func(m protocol.Message) bool { return m.Type == protocol.TypeRemove && m.PendingID == "t2" })
	if rm.ReplacedBy == "" {
		t.Fatalf("remove %+v", rm)
	}
	if p := readPending(t, f.dir, "t2"); p.PromptText != "" || p.Attempt != 1 || p.AttemptOpen {
		t.Fatalf("file %+v", p)
	}
	if e := readEntry(t, f.host, "t2"); len(e.Attempts) != 1 || e.Attempts[0].State != protocol.DeliveryDelivered {
		t.Fatalf("host entry %+v", e)
	}
	// p again is refused: the prompt is delivered and gone.
	if res := f.request(t, protocol.Message{Type: protocol.TypePrompt, ID: "t2"}); res.OK {
		t.Fatalf("second p %+v", res)
	}
}

// A daemon that starts picks up the files it finds: an add without an
// outcome is run, one that may have reached the host is followed and
// resent when the host never saw it, an open attempt is followed and
// sent when the host never saw it.
func TestRelayResumesFiles(t *testing.T) {
	f := newRelayFixture(t, idleScreen)
	// Records written as a previous daemon left them.
	write := func(p pendingFile) {
		t.Helper()
		p.UpdatedAt = time.Now()
		if p.SubmittedAt.IsZero() {
			p.SubmittedAt = time.Now()
		}
		b, _ := json.Marshal(p)
		if err := os.WriteFile(filepath.Join(f.dir, fileName(p.ID)), b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(pendingFile{Pending: protocol.Pending{ID: "r1", Host: "vm", Source: f.source(), Repo: "proj", Branch: "one", Agent: "argv"}, PromptText: "one"})
	write(pendingFile{Pending: protocol.Pending{ID: "r2", Host: "vm", Source: f.source(), Repo: "proj", Branch: "two", Agent: "argv"}, PromptText: "two", Sent: true})
	// r3: a finished add on the host whose prompt was not delivered,
	// with an attempt the last daemon opened and the host never saw.
	pc := conn(t, f.host)
	pc.Write(protocol.Message{Type: protocol.TypeAdd, ID: "r3", Repo: f.source(), Branch: "three", AgentName: "claude"})
	three, _ := result(t, pc, "r3")
	if !three.OK {
		t.Fatal(three.Error)
	}
	f.host.journal.update("r3", func(e *entry) {
		e.HasPrompt, e.Delivery, e.DeliveryError = true, protocol.DeliveryNotDelivered, "session existed"
	})
	write(pendingFile{Pending: protocol.Pending{ID: "r3", Host: "vm", EnvironmentID: "henv", Source: f.source(), Repo: "proj", Branch: "three", Agent: "claude",
		Taken: true, Done: true, OK: true, Root: three.Root, Prompt: protocol.DeliveryNotDelivered, Error: "session existed", Attempt: 1, AttemptOpen: true, Listed: true}, PromptText: "three", Sent: true})
	// A new daemon on the same files.
	local := New(Config{
		EnvironmentID: "lenv", Version: "local", Hosts: f.hosts.get, Dial: f.remote.dial, Pending: f.dir,
		MergedIdle: 200 * time.Millisecond, ReconnectMin: 20 * time.Millisecond,
	})
	discovered(local)
	go local.Run(f.ctx)
	f.local = local
	for _, id := range []string{"r1", "r2"} {
		p := f.awaitRecord(t, id, 30*time.Second, func(p pendingFile) bool { return p.retired() })
		if p.Prompt != protocol.DeliveryDelivered {
			t.Fatalf("%s: %+v", id, p)
		}
	}
	p := f.awaitRecord(t, "r3", 30*time.Second, func(p pendingFile) bool { return !p.AttemptOpen })
	if p.Prompt != protocol.DeliveryDelivered || p.PromptText != "" {
		t.Fatalf("r3: %+v", p)
	}
	if e := readEntry(t, f.host, "r3"); len(e.Attempts) != 1 || e.Attempts[0].State != protocol.DeliveryDelivered {
		t.Fatalf("host entry %+v", e)
	}
}

// Connectivity is not outcome: a host that cannot be reached leaves
// the record waiting, saying so, until it can; a lost connection is
// followed.
func TestRelayUnreachable(t *testing.T) {
	f := newRelayFixture(t, nil)
	f.remote.mu.Lock()
	f.remote.down = errors.New("ssh: connect refused")
	f.remote.mu.Unlock()
	if res := f.request(t, protocol.Message{Type: protocol.TypeAdd, ID: "u1", Relay: "vm", Repo: f.source(), Name: "proj", Branch: "up", AgentName: "argv", Prompt: "p", SubmittedAt: time.Now()}); !res.OK {
		t.Fatal(res.Error)
	}
	p := f.awaitRecord(t, "u1", 5*time.Second, func(p pendingFile) bool { return p.Unreachable != "" })
	if p.Reachable || p.Taken || p.Done || !strings.Contains(p.Unreachable, "connect refused") {
		t.Fatalf("record %+v", p)
	}
	f.remote.mu.Lock()
	f.remote.down = nil
	f.remote.mu.Unlock()
	p = f.awaitRecord(t, "u1", 30*time.Second, func(p pendingFile) bool { return p.retired() })
	if !p.Reachable || p.Unreachable != "" || p.Prompt != protocol.DeliveryDelivered {
		t.Fatalf("record %+v", p)
	}
}

// Refusals and dismiss: a host not in the config, a host whose daemon
// is known to lack the task capability, a dismiss of a running add; a
// dismiss of a record that needs the user removes it.
func TestRelayRefusalsAndDismiss(t *testing.T) {
	shortWait(t, 200*time.Millisecond)
	f := newRelayFixture(t, []string{"loading"})
	c, pc, _ := f.merged(t)
	defer c.Close()
	if res := f.request(t, protocol.Message{Type: protocol.TypeAdd, ID: "x1", Relay: "nope", Repo: f.source(), Branch: "b"}); res.OK || !strings.Contains(res.Error, "not in the config") {
		t.Fatalf("unknown host %+v", res)
	}
	// The host row says the daemon has no task capability.
	awaitMerged(t, c, pc, 10*time.Second, func(m protocol.Message) bool {
		return m.Type == protocol.TypeUpsert && m.HostStatus != nil && m.HostStatus.Name == "vm" && m.HostStatus.EnvironmentID != ""
	})
	f.local.mu.Lock()
	mh := f.local.mhosts["vm"]
	caps := mh.status.Capabilities
	mh.status.Capabilities = []string{protocol.CapStatus, protocol.CapAdd}
	f.local.mu.Unlock()
	if res := f.request(t, protocol.Message{Type: protocol.TypeAdd, ID: "x2", Relay: "vm", Repo: f.source(), Branch: "b"}); res.OK || !strings.Contains(res.Error, "tasks not supported") {
		t.Fatalf("no task cap %+v", res)
	}
	f.local.mu.Lock()
	mh.status.Capabilities = caps
	f.local.mu.Unlock()
	if res := f.request(t, protocol.Message{Type: protocol.TypeAdd, ID: "x3", Relay: "vm", Repo: f.source(), Name: "proj", Branch: "b", AgentName: "claude", Prompt: "p", SubmittedAt: time.Now()}); !res.OK {
		t.Fatal(res.Error)
	}
	if res := f.request(t, protocol.Message{Type: protocol.TypeDismiss, ID: "x3"}); res.OK || !strings.Contains(res.Error, "still running") {
		t.Fatalf("dismiss running %+v", res)
	}
	f.awaitRecord(t, "x3", 30*time.Second, func(p pendingFile) bool { return p.Done && p.Listed })
	if res := f.request(t, protocol.Message{Type: protocol.TypeDismiss, ID: "x3"}); !res.OK {
		t.Fatalf("dismiss %+v", res)
	}
	rm := awaitMerged(t, c, pc, 5*time.Second, func(m protocol.Message) bool { return m.Type == protocol.TypeRemove && m.PendingID == "x3" })
	if rm.ReplacedBy != "" {
		t.Fatalf("dismiss remove %+v", rm)
	}
	if _, err := os.Stat(filepath.Join(f.dir, fileName("x3"))); err == nil {
		t.Fatal("file kept after dismiss")
	}
	if res := f.request(t, protocol.Message{Type: protocol.TypeDismiss, ID: "x3"}); res.OK {
		t.Fatal("dismissed twice")
	}
}

// The lifetime and a removed host: an add older than seven days is not
// sent and is outcome unknown; a record whose host left the config
// waits, saying so, and runs once the host is back.
func TestRelayLifetimeAndHostRemoved(t *testing.T) {
	f := newRelayFixture(t, nil)
	if res := f.request(t, protocol.Message{Type: protocol.TypeAdd, ID: "old", Relay: "vm", Repo: f.source(), Name: "proj", Branch: "old", AgentName: "argv", SubmittedAt: time.Now().Add(-8 * 24 * time.Hour)}); !res.OK {
		t.Fatal(res.Error)
	}
	p := f.awaitRecord(t, "old", 5*time.Second, func(p pendingFile) bool { return p.Done })
	if p.OK || !strings.Contains(p.Error, relayOutcomeUnknown) || p.Sent {
		t.Fatalf("old %+v", p)
	}
	if _, ok := f.host.journal.get("old"); ok {
		t.Fatal("an expired add reached the host")
	}
	f.hosts.set()
	if res := f.request(t, protocol.Message{Type: protocol.TypeAdd, ID: "gone", Relay: "vm", Repo: f.source(), Branch: "b"}); res.OK {
		t.Fatal("accepted for a host not in the config")
	}
	// Accepted while the host was configured, then the host leaves.
	f.hosts.set(client.Host{Name: "vm", SSH: "vm"})
	f.remote.mu.Lock()
	f.remote.down = errors.New("down")
	f.remote.mu.Unlock()
	if res := f.request(t, protocol.Message{Type: protocol.TypeAdd, ID: "h1", Relay: "vm", Repo: f.source(), Name: "proj", Branch: "back", AgentName: "argv", SubmittedAt: time.Now()}); !res.OK {
		t.Fatal(res.Error)
	}
	f.hosts.set()
	p = f.awaitRecord(t, "h1", 5*time.Second, func(p pendingFile) bool { return strings.Contains(p.Unreachable, "host removed") })
	f.hosts.set(client.Host{Name: "vm", SSH: "vm"})
	f.remote.mu.Lock()
	f.remote.down = nil
	f.remote.mu.Unlock()
	if p = f.awaitRecord(t, "h1", 30*time.Second, func(p pendingFile) bool { return p.Done }); !p.OK {
		t.Fatalf("h1 %+v", p)
	}
}

// The listing after a success that has no worktree at the root is
// done, worktree gone: a record kept for the user to dismiss.
func TestRelayWorktreeGone(t *testing.T) {
	f := newRelayFixture(t, nil)
	f.host.mu.Lock()
	gen := f.host.generation
	f.host.mu.Unlock()
	p := pendingFile{Pending: protocol.Pending{ID: "g1", Host: "vm", EnvironmentID: "henv", Source: f.source(), Repo: "proj", Branch: "b", Taken: true, Done: true, OK: true,
		Root: "/nowhere/b", Prompt: protocol.DeliveryNone, SubmittedAt: time.Now(), UpdatedAt: time.Now()}, Barrier: &protocol.Listing{Generation: gen, Revision: 0}}
	if _, err := f.local.relay.create(p); err != nil {
		t.Fatal(err)
	}
	go f.local.retire(f.ctx, "g1")
	got := f.awaitRecord(t, "g1", 10*time.Second, func(p pendingFile) bool { return p.Listed })
	if !got.Gone || got.retired() {
		t.Fatalf("record %+v", got)
	}
}
