package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/laat/laatmux/internal/client"
	"github.com/laat/laatmux/internal/procs"
	"github.com/laat/laatmux/internal/protocol"
	"github.com/laat/laatmux/internal/tmux"
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
	locals []*Daemon // every laptop daemon made, for the cleanup
	dir    string
	ctx    context.Context
}

func newRelayFixture(t *testing.T, screen []string) *relayFixture {
	t.Helper()
	// The store's directories and the pending directory are made before
	// the context, so the cleanup cancels the daemons and waits for the
	// relay's goroutines before the directories go.
	store, remote := newStore(t)
	dir := t.TempDir()
	hostCommands := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	f := &relayFixture{store: store, dir: dir, ctx: ctx}
	t.Cleanup(func() {
		cancel()
		f.awaitQuiet(t)
	})
	ft := &fakeServer{screen: screen}
	host := New(Config{
		EnvironmentID: "henv", Host: "vm", Version: "host",
		Targets: []Target{{Label: "laatmux", Tmux: ft, Managed: true}},
		Procs:   &fakeProcs{tables: []procTable{{procs: []procs.Proc{shell, claude}}}},
		Store:   store, Agents: map[string][]string{"claude": {"claude"}, "argv": {"claude", PromptPlaceholder}},
		Commands: hostCommands, WorktreeInterval: 50 * time.Millisecond, Interval: 30 * time.Millisecond,
	})
	go host.Run(ctx)
	fr := newFakeRemote(t, ctx, host)
	hosts := &hostsList{hosts: []client.Host{{Name: "vm", SSH: "vm"}}}
	local := New(Config{
		EnvironmentID: "lenv", Version: "local", Hosts: hosts.get, Dial: fr.dial, Pending: dir,
		MergedIdle: 200 * time.Millisecond, SessionInterval: 20 * time.Millisecond, ReconnectMin: 20 * time.Millisecond,
	})
	discovered(local)
	go local.Run(ctx)
	f.host, f.ft, f.remote, f.hosts = host, ft, fr, hosts
	f.setLocal(local)
	_ = remote
	return f
}

// setLocal makes d the fixture's laptop daemon, remembered for the
// cleanup's wait.
func (f *relayFixture) setLocal(d *Daemon) {
	f.local = d
	f.locals = append(f.locals, d)
}

// awaitQuiet waits for every laptop daemon's relay goroutines to have
// returned, bounded, so nothing writes into a directory being removed.
func (f *relayFixture) awaitQuiet(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for _, d := range f.locals {
		if d.relay == nil {
			continue
		}
		for {
			d.relay.mu.Lock()
			n := len(d.relay.runners)
			d.relay.mu.Unlock()
			if n == 0 || time.Now().After(deadline) {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	// The host's add goroutines end with the context too; a moment for
	// their last writes.
	time.Sleep(50 * time.Millisecond)
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
	b, err := os.ReadFile(filepath.Join(dir, FileName(id)))
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
	// Two submits of one id in flight start one runner.
	f.remote.mu.Lock()
	dials := f.remote.dials
	f.remote.mu.Unlock()
	for _, i := range []int{1, 2} {
		_ = i
		if res := f.request(t, protocol.Message{Type: protocol.TypeAdd, ID: "t1b", Relay: "vm", Repo: f.source(), Name: "proj", Branch: "twice", AgentName: "argv", SubmittedAt: time.Now()}); !res.OK {
			t.Fatal(res.Error)
		}
	}
	f.awaitRecord(t, "t1b", 30*time.Second, func(p pendingFile) bool { return p.retired() })
	f.remote.mu.Lock()
	added := f.remote.dials - dials
	f.remote.mu.Unlock()
	if added != 2 { // the add's connection and the listing's
		t.Fatalf("%d dials for one task", added)
	}
	// A retired record is not dismissed: its handoff is kept.
	if res := f.request(t, protocol.Message{Type: protocol.TypeDismiss, ID: "t1"}); res.OK || !strings.Contains(res.Error, "handed over") {
		t.Fatalf("dismiss retired %+v", res)
	}
	if _, err := os.Stat(filepath.Join(f.dir, FileName("t1"))); err != nil {
		t.Fatal("retired file dismissed")
	}
	// Swept after the retention.
	f.local.relay.sweep(time.Now().Add(handoffRetention + time.Second))
	if _, err := os.Stat(filepath.Join(f.dir, FileName("t1"))); err == nil {
		t.Fatal("retired file kept past the retention")
	}
}

// A record that needs the user stays: the add succeeded but the prompt
// was not delivered, the prompt is retained, the listing is noted, and
// p delivers it later as an attempt the file holds first; then the
// record is complete and retires.
func TestRelayPromptLater(t *testing.T) {
	shortWait(t, time.Second)
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
		if err := os.WriteFile(filepath.Join(f.dir, FileName(p.ID)), b, 0o600); err != nil {
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
	f.setLocal(local)
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
	// Well past the three attempts a foreground client gives up after.
	for deadline := time.Now().Add(10 * time.Second); ; {
		f.remote.mu.Lock()
		n := f.remote.dials
		f.remote.mu.Unlock()
		if n >= 6 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d dials", n)
		}
		time.Sleep(20 * time.Millisecond)
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
	shortWait(t, time.Second)
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
	// Once the host has the add, dismiss waits for its outcome.
	f.awaitRecord(t, "x3", 30*time.Second, func(p pendingFile) bool { return p.Taken })
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
	if _, err := os.Stat(filepath.Join(f.dir, FileName("x3"))); err == nil {
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

// A daemon that starts on a complete, listed record whose handoff was
// never written hands it over; a failed add keeps the host's delivery
// state and the prompt, and delivers no more once delivered.
func TestRelaySettleAndFailedAdd(t *testing.T) {
	f := newRelayFixture(t, nil)
	c, pc, _ := f.merged(t)
	defer c.Close()
	if res := f.request(t, protocol.Message{Type: protocol.TypeAdd, ID: "s1", Relay: "vm", Repo: f.source(), Name: "proj", Branch: "settle", AgentName: "argv", Prompt: "p", SubmittedAt: time.Now()}); !res.OK {
		t.Fatal(res.Error)
	}
	awaitMerged(t, c, pc, 30*time.Second, func(m protocol.Message) bool { return m.Type == protocol.TypeRemove && m.PendingID == "s1" })
	// As a daemon that died between the listing and the handoff left it.
	p := readPending(t, f.dir, "s1")
	p.ReplacedBy, p.RetiredAt = "", time.Time{}
	b, _ := json.Marshal(p)
	os.WriteFile(filepath.Join(f.dir, FileName("s1")), b, 0o600)
	local := New(Config{
		EnvironmentID: "lenv", Version: "local", Hosts: f.hosts.get, Dial: f.remote.dial, Pending: f.dir,
		MergedIdle: 200 * time.Millisecond, ReconnectMin: 20 * time.Millisecond,
	})
	discovered(local)
	go local.Run(f.ctx)
	f.setLocal(local)
	if got := f.awaitRecord(t, "s1", 10*time.Second, func(p pendingFile) bool { return p.retired() }); got.ReplacedBy != p.WorktreeID() {
		t.Fatalf("settled %+v", got)
	}
	// A launch that fails after new-session was submitted: the add
	// fails, the delivery is unknown, the prompt stays.
	f.ft.set(func() { f.ft.newErr = &tmux.SubmittedError{Err: errors.New("tmux: set-option failed")} })
	if res := f.request(t, protocol.Message{Type: protocol.TypeAdd, ID: "s2", Relay: "vm", Repo: f.source(), Name: "proj", Branch: "failed", AgentName: "argv", Prompt: "keep", SubmittedAt: time.Now()}); !res.OK {
		t.Fatal(res.Error)
	}
	got := f.awaitRecord(t, "s2", 30*time.Second, func(p pendingFile) bool { return p.Done })
	if got.OK || got.Prompt != protocol.DeliveryUnknown || got.PromptText != "keep" || !strings.Contains(got.Error, "failed at agent") {
		t.Fatalf("failed add %+v", got)
	}
	f.ft.set(func() { f.ft.newErr = nil })
	// p on a failed add is refused: the prompt is the user's to paste.
	if res := f.request(t, protocol.Message{Type: protocol.TypePrompt, ID: "s2"}); res.OK || !strings.Contains(res.Error, "the add failed") {
		t.Fatalf("prompt on failed add %+v", res)
	}
	if got := readPending(t, f.dir, "s2"); got.PromptText != "keep" || got.Attempt != 0 {
		t.Fatalf("file %+v", got)
	}
}

// An add that fails before the agent stage has no delivery state, and
// its prompt is kept for the user; an attempt on a host that cannot be
// reached stays open, is refused to dismiss, and resolves when the
// host is back.
func TestRelayEarlyFailureAndOpenAttempt(t *testing.T) {
	shortWait(t, time.Second)
	f := newRelayFixture(t, []string{"loading"})
	if res := f.request(t, protocol.Message{Type: protocol.TypeAdd, ID: "e1", Relay: "vm", Repo: f.source(), Name: "proj", Branch: "early", AgentName: "nope", Prompt: "keep me", SubmittedAt: time.Now()}); !res.OK {
		t.Fatal(res.Error)
	}
	p := f.awaitRecord(t, "e1", 30*time.Second, func(p pendingFile) bool { return p.Done })
	if p.OK || p.Prompt != "" || p.PromptText != "keep me" || p.Delivered() || !strings.Contains(p.Error, "failed at resolve") {
		t.Fatalf("early failure %+v", p)
	}
	// A typed add whose prompt was not delivered; then the host goes
	// away and p is pressed.
	if res := f.request(t, protocol.Message{Type: protocol.TypeAdd, ID: "e2", Relay: "vm", Repo: f.source(), Name: "proj", Branch: "open", AgentName: "claude", Prompt: "later", SubmittedAt: time.Now()}); !res.OK {
		t.Fatal(res.Error)
	}
	f.awaitRecord(t, "e2", 30*time.Second, func(p pendingFile) bool { return p.Done && p.Listed })
	f.remote.mu.Lock()
	f.remote.down = errors.New("down")
	f.remote.mu.Unlock()
	res := f.request(t, protocol.Message{Type: protocol.TypePrompt, ID: "e2"})
	if res.OK || !strings.Contains(res.Error, "attempt 1 is open") || res.Attempt != 1 {
		t.Fatalf("p while down %+v", res)
	}
	if p := readPending(t, f.dir, "e2"); !p.AttemptOpen || p.Attempt != 1 {
		t.Fatalf("file %+v", p)
	}
	if res := f.request(t, protocol.Message{Type: protocol.TypeDismiss, ID: "e2"}); res.OK || !strings.Contains(res.Error, "unresolved") {
		t.Fatalf("dismiss with an attempt open %+v", res)
	}
	// A second p while the attempt is followed in the background is
	// answered that it is open, not queued to open the next.
	if res := f.request(t, protocol.Message{Type: protocol.TypePrompt, ID: "e2"}); res.OK || !strings.Contains(res.Error, "attempt 1 is open") {
		t.Fatalf("second p %+v", res)
	}
	f.ft.set(func() { f.ft.screen = idleScreen })
	f.remote.mu.Lock()
	f.remote.down = nil
	f.remote.mu.Unlock()
	p = f.awaitRecord(t, "e2", 30*time.Second, func(p pendingFile) bool { return !p.AttemptOpen })
	if p.Prompt != protocol.DeliveryDelivered || p.Attempt != 1 {
		t.Fatalf("resolved %+v", p)
	}
	if e := readEntry(t, f.host, "e2"); len(e.Attempts) != 1 {
		t.Fatalf("host entry %+v", e)
	}
}

// A capability refusal after the add was sent is connectivity, not an
// outcome: the relay waits for the host and follows the add once it is
// back.
func TestRelayRefusalAfterSend(t *testing.T) {
	f := newRelayFixture(t, nil)
	// A host that answers without task: the fake remote's daemon with
	// its journal gone.
	if res := f.request(t, protocol.Message{Type: protocol.TypeAdd, ID: "c1", Relay: "vm", Repo: f.source(), Name: "proj", Branch: "cap", AgentName: "argv", Prompt: "p", SubmittedAt: time.Now()}); !res.OK {
		t.Fatal(res.Error)
	}
	f.awaitRecord(t, "c1", 30*time.Second, func(p pendingFile) bool { return p.retired() })
	// As a record left unfinished after an interrupted answer, to be
	// sent again: taken by the host once, not sent now. The host that
	// answers is one without task, the same environment, as a daemon
	// restarted without its journal directory would be.
	p := readPending(t, f.dir, "c1")
	p.Done, p.OK, p.Listed, p.ReplacedBy, p.RetiredAt, p.Sent, p.Taken = false, false, false, "", time.Time{}, false, true
	b, _ := json.Marshal(p)
	os.WriteFile(filepath.Join(f.dir, FileName("c1")), b, 0o600)
	bare := New(Config{EnvironmentID: "henv", Host: "vm", Version: "bare", Targets: []Target{{Label: "laatmux", Tmux: &fakeServer{}, Managed: true}}, Store: f.store})
	discovered(bare)
	bareRemote := newFakeRemote(t, f.ctx, bare)
	var mu sync.Mutex
	current := bareRemote
	dial := func(ctx context.Context, h client.Host) (*client.Conn, error) {
		mu.Lock()
		r := current
		mu.Unlock()
		return r.dial(ctx, h)
	}
	local := New(Config{
		EnvironmentID: "lenv", Version: "local", Hosts: f.hosts.get, Dial: dial, Pending: f.dir,
		MergedIdle: 200 * time.Millisecond, ReconnectMin: 20 * time.Millisecond,
	})
	discovered(local)
	go local.Run(f.ctx)
	f.setLocal(local)
	got := f.awaitRecord(t, "c1", 5*time.Second, func(p pendingFile) bool { return strings.Contains(p.Unreachable, "tasks not supported") })
	if got.Done {
		t.Fatalf("done on a refusal after the send: %+v", got)
	}
	mu.Lock()
	current = f.remote
	mu.Unlock()
	if got := f.awaitRecord(t, "c1", 30*time.Second, func(p pendingFile) bool { return p.Done }); !got.OK || got.Prompt != protocol.DeliveryDelivered {
		t.Fatalf("after the host is back: %+v", got)
	}
}

// An attempt the host could not record keeps its number on the relay:
// the request answers that it is open, and once the host can record
// again the same number is delivered.
func TestRelayAttemptNotRecorded(t *testing.T) {
	shortWait(t, time.Second)
	f := newRelayFixture(t, []string{"loading"})
	if res := f.request(t, protocol.Message{Type: protocol.TypeAdd, ID: "n1", Relay: "vm", Repo: f.source(), Name: "proj", Branch: "nr", AgentName: "claude", Prompt: "later", SubmittedAt: time.Now()}); !res.OK {
		t.Fatal(res.Error)
	}
	f.awaitRecord(t, "n1", 30*time.Second, func(p pendingFile) bool { return p.Done && p.Listed })
	if err := os.Chmod(f.host.journal.dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(f.host.journal.dir, 0o700) })
	res := f.request(t, protocol.Message{Type: protocol.TypePrompt, ID: "n1"})
	if res.OK || !strings.Contains(res.Error, "attempt 1 is open") || !strings.Contains(res.Error, protocol.ErrAttemptNotRecorded) {
		t.Fatalf("p on an unrecordable attempt %+v", res)
	}
	f.ft.set(func() { f.ft.screen = idleScreen })
	os.Chmod(f.host.journal.dir, 0o700)
	p := f.awaitRecord(t, "n1", 30*time.Second, func(p pendingFile) bool { return !p.AttemptOpen })
	if p.Attempt != 1 || p.Prompt != protocol.DeliveryDelivered {
		t.Fatalf("resolved %+v", p)
	}
	if e := readEntry(t, f.host, "n1"); len(e.Attempts) != 1 || e.Attempts[0].N != 1 {
		t.Fatalf("host entry %+v", e.Attempts)
	}
}

// The host's daemon restarted mid-add: a new host daemon on the same
// journal answers the follow interrupted, the relay resends under the
// same id, the host resumes with the allocated branch, and the record
// ends delivered.
func TestRelayHostRestartMidAdd(t *testing.T) {
	f := newRelayFixture(t, nil)
	c, pc, _ := f.merged(t)
	defer c.Close()
	if res := f.request(t, protocol.Message{Type: protocol.TypeAdd, ID: "h1", Relay: "vm", Repo: f.source(), Name: "proj", Branch: "restart", Generated: true, AgentName: "argv", Prompt: "p", SubmittedAt: time.Now()}); !res.OK {
		t.Fatal(res.Error)
	}
	awaitMerged(t, c, pc, 30*time.Second, func(m protocol.Message) bool { return m.Type == protocol.TypeRemove && m.PendingID == "h1" })
	// The host's journal as a daemon that died at the worktree stage
	// left it, its branch allocated; the relay's record as sent and
	// not done; a new host daemon on the same directories.
	e := readEntry(t, f.host, "h1")
	e.Result, e.TerminalAt, e.Launch, e.PaneID, e.Delivery, e.Stage = nil, time.Time{}, "", "", "", protocol.StageWorktree
	b, _ := json.Marshal(e)
	os.WriteFile(filepath.Join(f.host.journal.dir, FileName("h1")), b, 0o600)
	p := readPending(t, f.dir, "h1")
	p.Done, p.OK, p.Listed, p.ReplacedBy, p.RetiredAt, p.Sent, p.PromptText = false, false, false, "", time.Time{}, true, "p"
	b, _ = json.Marshal(p)
	os.WriteFile(filepath.Join(f.dir, FileName("h1")), b, 0o600)
	ft := &fakeServer{}
	host2 := New(Config{
		EnvironmentID: "henv", Host: "vm", Version: "host2",
		Targets: []Target{{Label: "laatmux", Tmux: ft, Managed: true}},
		Procs:   &fakeProcs{tables: []procTable{{procs: []procs.Proc{shell, claude}}}},
		Store:   f.store, Agents: map[string][]string{"argv": {"claude", PromptPlaceholder}},
		Commands: f.host.journal.dir, WorktreeInterval: 50 * time.Millisecond, Interval: 30 * time.Millisecond,
	})
	go host2.Run(f.ctx)
	remote2 := newFakeRemote(t, f.ctx, host2)
	local := New(Config{
		EnvironmentID: "lenv", Version: "local", Hosts: f.hosts.get, Dial: remote2.dial, Pending: f.dir,
		MergedIdle: 200 * time.Millisecond, ReconnectMin: 20 * time.Millisecond,
	})
	discovered(local)
	go local.Run(f.ctx)
	f.setLocal(local)
	got := f.awaitRecord(t, "h1", 30*time.Second, func(p pendingFile) bool { return p.retired() })
	if got.Branch != e.Branch || got.Prompt != protocol.DeliveryDelivered || len(ft.cmds) != 1 {
		t.Fatalf("resumed %+v cmds %q", got, ft.cmds)
	}
	if e2 := readEntry(t, host2, "h1"); e2.Branch != e.Branch || e2.Delivery != protocol.DeliveryDelivered {
		t.Fatalf("host entry %+v", e2)
	}
}

// A host that answers as another environment gets nothing: the record
// waits, done false, and runs once the right machine answers.
func TestRelayEnvironmentMismatchWaits(t *testing.T) {
	f := newRelayFixture(t, nil)
	other := New(Config{EnvironmentID: "elsewhere", Host: "vm", Version: "other", Targets: []Target{{Label: "laatmux", Tmux: &fakeServer{}, Managed: true}}, Store: f.store, Commands: t.TempDir()})
	discovered(other)
	otherRemote := newFakeRemote(t, f.ctx, other)
	var mu sync.Mutex
	current := otherRemote
	dial := func(ctx context.Context, h client.Host) (*client.Conn, error) {
		mu.Lock()
		r := current
		mu.Unlock()
		return r.dial(ctx, h)
	}
	local := New(Config{
		EnvironmentID: "lenv", Version: "local", Hosts: f.hosts.get, Dial: dial, Pending: f.dir,
		MergedIdle: 200 * time.Millisecond, ReconnectMin: 20 * time.Millisecond,
	})
	discovered(local)
	go local.Run(f.ctx)
	f.setLocal(local)
	// Pinned at accept to the host row's environment.
	f.local.mu.Lock()
	f.local.mhosts["vm"] = &mergedHost{host: client.Host{Name: "vm", SSH: "vm"}, status: protocol.HostStatus{Name: "vm", EnvironmentID: "henv", Capabilities: []string{protocol.CapTask}}, agents: map[string]protocol.Agent{}, worktrees: map[string]protocol.Worktree{}}
	f.local.mu.Unlock()
	if res := f.request(t, protocol.Message{Type: protocol.TypeAdd, ID: "m1", Relay: "vm", Repo: f.source(), Name: "proj", Branch: "moved", AgentName: "argv", SubmittedAt: time.Now()}); !res.OK {
		t.Fatal(res.Error)
	}
	p := f.awaitRecord(t, "m1", 5*time.Second, func(p pendingFile) bool { return strings.Contains(p.Unreachable, "answers as environment") })
	if p.Done || p.Sent {
		t.Fatalf("record %+v", p)
	}
	if _, ok := other.journal.get("m1"); ok {
		t.Fatal("the add reached the wrong machine")
	}
	mu.Lock()
	current = f.remote
	mu.Unlock()
	if p := f.awaitRecord(t, "m1", 30*time.Second, func(p pendingFile) bool { return p.Done }); !p.OK {
		t.Fatalf("after the right host answers: %+v", p)
	}
}

// A client gone before it read the accepted answer leaves a task that
// runs anyway: the acceptance is the file.
func TestRelayLostAcceptedAnswer(t *testing.T) {
	f := newRelayFixture(t, nil)
	server, cl := net.Pipe()
	go f.local.HandleConn(f.ctx, server, func() { server.Close() })
	pc := protocol.NewConn(cl)
	if _, err := pc.Read(); err != nil {
		t.Fatal(err)
	}
	if err := pc.Write(protocol.Message{Type: protocol.TypeAdd, ID: "l1", Relay: "vm", Repo: f.source(), Name: "proj", Branch: "lost", AgentName: "argv", SubmittedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	// Gone without reading: the daemon's write of the answer fails.
	cl.Close()
	if p := f.awaitRecord(t, "l1", 30*time.Second, func(p pendingFile) bool { return p.Done }); !p.OK {
		t.Fatalf("record %+v", p)
	}
}

// A record the host will never answer for can be dismissed: one never
// sent nor taken, to a host that is down for good, and one whose host
// left the config with an attempt open; the goroutines end first.
func TestRelayDismissAbandoned(t *testing.T) {
	shortWait(t, time.Second)
	f := newRelayFixture(t, []string{"loading"})
	f.remote.mu.Lock()
	f.remote.down = errors.New("gone for good")
	f.remote.mu.Unlock()
	if res := f.request(t, protocol.Message{Type: protocol.TypeAdd, ID: "d1", Relay: "vm", Repo: f.source(), Name: "proj", Branch: "dead", AgentName: "argv", SubmittedAt: time.Now()}); !res.OK {
		t.Fatal(res.Error)
	}
	f.awaitRecord(t, "d1", 5*time.Second, func(p pendingFile) bool { return p.Unreachable != "" })
	if res := f.request(t, protocol.Message{Type: protocol.TypeDismiss, ID: "d1"}); !res.OK {
		t.Fatalf("dismiss unsent %+v", res)
	}
	if _, ok := f.local.relay.get("d1"); ok {
		t.Fatal("record kept")
	}
	f.local.relay.mu.Lock()
	n := len(f.local.relay.runners["d1"])
	f.local.relay.mu.Unlock()
	if n != 0 {
		t.Fatalf("%d runners after dismiss", n)
	}
	// A record with an attempt open whose host leaves the config.
	f.remote.mu.Lock()
	f.remote.down = nil
	f.remote.mu.Unlock()
	if res := f.request(t, protocol.Message{Type: protocol.TypeAdd, ID: "d2", Relay: "vm", Repo: f.source(), Name: "proj", Branch: "left", AgentName: "claude", Prompt: "p", SubmittedAt: time.Now()}); !res.OK {
		t.Fatal(res.Error)
	}
	f.awaitRecord(t, "d2", 30*time.Second, func(p pendingFile) bool { return p.Done && p.Listed })
	f.remote.mu.Lock()
	f.remote.down = errors.New("down")
	f.remote.mu.Unlock()
	if res := f.request(t, protocol.Message{Type: protocol.TypePrompt, ID: "d2"}); res.OK || !strings.Contains(res.Error, "attempt 1 is open") {
		t.Fatalf("p while down %+v", res)
	}
	if res := f.request(t, protocol.Message{Type: protocol.TypeDismiss, ID: "d2"}); res.OK {
		t.Fatal("dismissed with the host configured and an attempt open")
	}
	f.hosts.set()
	if res := f.request(t, protocol.Message{Type: protocol.TypeDismiss, ID: "d2"}); !res.OK {
		t.Fatalf("dismiss with the host gone %+v", res)
	}
}

// p is offered only on not delivered or unknown: an add that failed,
// or a worktree that is gone, keeps the prompt for tasks show and no
// attempt is opened.
func TestRelayPromptOnlyWhenUndelivered(t *testing.T) {
	f := newRelayFixture(t, nil)
	if res := f.request(t, protocol.Message{Type: protocol.TypeAdd, ID: "f1", Relay: "vm", Repo: f.source(), Name: "proj", Branch: "fail", AgentName: "nope", Prompt: "keep", SubmittedAt: time.Now()}); !res.OK {
		t.Fatal(res.Error)
	}
	f.awaitRecord(t, "f1", 30*time.Second, func(p pendingFile) bool { return p.Done })
	res := f.request(t, protocol.Message{Type: protocol.TypePrompt, ID: "f1"})
	if res.OK || !strings.Contains(res.Error, "the add failed") {
		t.Fatalf("p on a failed add %+v", res)
	}
	if p := readPending(t, f.dir, "f1"); p.Attempt != 0 || p.PromptText != "keep" {
		t.Fatalf("file %+v", p)
	}
}

// A worktree removed after the relay's listing showed it and before
// the merged stream did makes the record gone rather than a handoff
// that waits for a row that will never come.
func TestRelayHandoffFindsWorktreeGone(t *testing.T) {
	f := newRelayFixture(t, nil)
	c, pc, _ := f.merged(t)
	defer c.Close()
	// Keep the merged stream from ever showing the worktree: the
	// merged host's connection is a fake whose records never arrive,
	// which is what a dropped merged follow looks like.
	f.local.mu.Lock()
	f.local.mhosts["vm"].cancel()
	f.local.mhosts["vm"].cancel = func() {}
	f.local.mu.Unlock()
	if res := f.request(t, protocol.Message{Type: protocol.TypeAdd, ID: "g1", Relay: "vm", Repo: f.source(), Name: "proj", Branch: "gone", AgentName: "argv", SubmittedAt: time.Now()}); !res.OK {
		t.Fatal(res.Error)
	}
	p := f.awaitRecord(t, "g1", 30*time.Second, func(p pendingFile) bool { return p.Listed })
	// The worktree goes while the handoff waits for the stream.
	pc2 := conn(t, f.host)
	pc2.Write(protocol.Message{Type: protocol.TypeRm, ID: "r1", Repo: f.source(), Branch: "gone", Root: p.Root, Force: true})
	if rres, _ := result(t, pc2, "r1"); !rres.OK {
		t.Fatal(rres.Error)
	}
	got := f.awaitRecord(t, "g1", 30*time.Second, func(p pendingFile) bool { return p.Gone })
	if got.retired() {
		t.Fatalf("handed off a gone worktree: %+v", got)
	}
	_ = pc
}

// Recovery expired through the relay: a host journal swept before p
// refuses the attempt, which the record keeps apart from the add's
// outcome with its number rolled back, the next p is refused, and the
// prompt stays in the file for tasks show.
func TestRelayRecoveryExpired(t *testing.T) {
	shortWait(t, time.Second)
	f := newRelayFixture(t, []string{"loading"})
	if res := f.request(t, protocol.Message{Type: protocol.TypeAdd, ID: "x1", Relay: "vm", Repo: f.source(), Name: "proj", Branch: "swept", AgentName: "claude", Prompt: "keep", SubmittedAt: time.Now()}); !res.OK {
		t.Fatal(res.Error)
	}
	f.awaitRecord(t, "x1", 30*time.Second, func(p pendingFile) bool { return p.Done && p.Listed })
	f.host.journal.sweep(time.Now().Add(journalRetention + time.Hour))
	if _, ok := f.host.journal.get("x1"); ok {
		t.Fatal("entry not swept")
	}
	res := f.request(t, protocol.Message{Type: protocol.TypePrompt, ID: "x1"})
	if res.OK || res.Error != protocol.ErrRecoveryExpired {
		t.Fatalf("p after the sweep %+v", res)
	}
	p := readPending(t, f.dir, "x1")
	if p.AttemptError != protocol.ErrRecoveryExpired || p.Attempt != 0 || p.AttemptOpen || p.PromptText != "keep" || !strings.Contains(p.Error, "not ready") {
		t.Fatalf("file %+v", p)
	}
	if res := f.request(t, protocol.Message{Type: protocol.TypePrompt, ID: "x1"}); res.OK || !strings.Contains(res.Error, protocol.ErrRecoveryExpired) {
		t.Fatalf("second p %+v", res)
	}
}

// A host that refuses the add as submission expired makes the record
// outcome unknown.
func TestRelaySubmissionExpiredByHost(t *testing.T) {
	was := journalRetention
	journalRetention = time.Hour
	t.Cleanup(func() { journalRetention = was })
	f := newRelayFixture(t, nil)
	if res := f.request(t, protocol.Message{Type: protocol.TypeAdd, ID: "e1", Relay: "vm", Repo: f.source(), Name: "proj", Branch: "expired", AgentName: "argv", SubmittedAt: time.Now().Add(-2 * time.Hour)}); !res.OK {
		t.Fatal(res.Error)
	}
	p := f.awaitRecord(t, "e1", 10*time.Second, func(p pendingFile) bool { return p.Done })
	if p.OK || !strings.Contains(p.Error, relayOutcomeUnknown) || !strings.Contains(p.Error, protocol.ErrSubmissionExpired) {
		t.Fatalf("record %+v", p)
	}
}

// A laptop daemon restarted while the host still runs the add: the new
// daemon's follow attaches to the live stream and gets the result.
func TestRelayLaptopRestartDuringAdd(t *testing.T) {
	shortWait(t, 3*time.Second)
	f := newRelayFixture(t, []string{"loading"})
	// The host is running the add, in its typed wait.
	hpc := conn(t, f.host)
	hpc.Write(protocol.Message{Type: protocol.TypeAdd, ID: "l1", Repo: f.source(), Branch: "live", AgentName: "claude", Prompt: "p", SubmittedAt: time.Now()})
	for {
		m, err := hpc.Read()
		if err != nil {
			t.Fatal(err)
		}
		if m.Type == protocol.TypeProgress && strings.HasPrefix(m.Detail, "typing the prompt") {
			break
		}
	}
	// The record as the last laptop daemon left it: sent, not done.
	p := pendingFile{Pending: protocol.Pending{ID: "l1", Host: "vm", EnvironmentID: "henv", Source: f.source(), Repo: "proj", Branch: "live", Agent: "claude", SubmittedAt: time.Now(), UpdatedAt: time.Now()}, PromptText: "p", Sent: true}
	b, _ := json.Marshal(p)
	os.WriteFile(filepath.Join(f.dir, FileName("l1")), b, 0o600)
	local := New(Config{
		EnvironmentID: "lenv", Version: "local", Hosts: f.hosts.get, Dial: f.remote.dial, Pending: f.dir,
		MergedIdle: 200 * time.Millisecond, ReconnectMin: 20 * time.Millisecond,
	})
	discovered(local)
	go local.Run(f.ctx)
	f.setLocal(local)
	got := f.awaitRecord(t, "l1", 30*time.Second, func(p pendingFile) bool { return p.Done })
	if !got.OK || got.Prompt != protocol.DeliveryNotDelivered || !got.Taken {
		t.Fatalf("record %+v", got)
	}
	if res, _ := result(t, hpc, "l1"); !res.OK {
		t.Fatalf("host result %+v", res)
	}
	// The host ran the add once: one launch.
	if len(f.ft.cmds) != 1 {
		t.Fatalf("cmds %q", f.ft.cmds)
	}
}

// Attempt numbers that drifted from the host's are put back from its
// answer, and the next p delivers.
func TestRelayAttemptNumberResync(t *testing.T) {
	shortWait(t, time.Second)
	f := newRelayFixture(t, []string{"loading"})
	if res := f.request(t, protocol.Message{Type: protocol.TypeAdd, ID: "n1", Relay: "vm", Repo: f.source(), Name: "proj", Branch: "drift", AgentName: "claude", Prompt: "p", SubmittedAt: time.Now()}); !res.OK {
		t.Fatal(res.Error)
	}
	f.awaitRecord(t, "n1", 30*time.Second, func(p pendingFile) bool { return p.Done && p.Listed })
	f.local.setPending("n1", true, func(p *pendingFile) { p.Attempt = 3 })
	res := f.request(t, protocol.Message{Type: protocol.TypePrompt, ID: "n1"})
	if res.OK || !strings.Contains(res.Error, "the journal has 0") {
		t.Fatalf("drifted p %+v", res)
	}
	if p := readPending(t, f.dir, "n1"); p.Attempt != 0 || p.AttemptError == "" {
		t.Fatalf("file %+v", p)
	}
	f.ft.set(func() { f.ft.screen = idleScreen })
	if res := f.request(t, protocol.Message{Type: protocol.TypePrompt, ID: "n1"}); !res.OK || res.Attempt != 1 || res.Prompt != protocol.DeliveryDelivered {
		t.Fatalf("resynced p %+v", res)
	}
	if n, ok := journalHas("attempt 4 is not the next; the journal has 2"); !ok || n != 2 {
		t.Fatalf("journalHas %d %v", n, ok)
	}
	if _, ok := journalHas("removed"); ok {
		t.Fatal("journalHas on another message")
	}
}

// A merged stream that never shows the worktree holds the row for the
// patience, then the record hands over on the listing alone.
func TestRelayHandoffPatience(t *testing.T) {
	was := handoffPatience
	handoffPatience = 2 * time.Second
	t.Cleanup(func() { handoffPatience = was })
	f := newRelayFixture(t, nil)
	c, _, _ := f.merged(t)
	defer c.Close()
	f.local.mu.Lock()
	f.local.mhosts["vm"].cancel()
	f.local.mhosts["vm"].cancel = func() {}
	f.local.mu.Unlock()
	if res := f.request(t, protocol.Message{Type: protocol.TypeAdd, ID: "p1", Relay: "vm", Repo: f.source(), Name: "proj", Branch: "patience", AgentName: "argv", SubmittedAt: time.Now()}); !res.OK {
		t.Fatal(res.Error)
	}
	listed := f.awaitRecord(t, "p1", 30*time.Second, func(p pendingFile) bool { return p.Listed })
	if listed.retired() {
		t.Fatal("handed over before the patience")
	}
	got := f.awaitRecord(t, "p1", 30*time.Second, func(p pendingFile) bool { return p.retired() })
	if got.Gone || got.ReplacedBy != got.WorktreeID() {
		t.Fatalf("record %+v", got)
	}
}

// A dismiss of a record whose host left the config returns promptly
// with every goroutine ended, even while a settle waits on the host in
// its backoff and an attempt is open; and a host that is back between
// the dismiss's checks keeps its record with its goroutines restarted.
func TestRelayDismissEndsStuckGoroutines(t *testing.T) {
	shortWait(t, time.Second)
	f := newRelayFixture(t, []string{"loading"})
	// Done and OK, prompt not delivered, and the listing still owed
	// with the host down: settle waits in retire's backoff.
	if res := f.request(t, protocol.Message{Type: protocol.TypeAdd, ID: "s1", Relay: "vm", Repo: f.source(), Name: "proj", Branch: "stuck", AgentName: "claude", Prompt: "p", SubmittedAt: time.Now()}); !res.OK {
		t.Fatal(res.Error)
	}
	f.awaitRecord(t, "s1", 30*time.Second, func(p pendingFile) bool { return p.Done && p.Listed })
	f.remote.mu.Lock()
	f.remote.down = errors.New("down")
	f.remote.mu.Unlock()
	f.local.setPending("s1", true, func(p *pendingFile) { p.Listed = false })
	f.local.relay.mu.Lock()
	f.local.startRunnerLocked(f.ctx, "s1", f.local.settle)
	f.local.relay.mu.Unlock()
	f.awaitRecord(t, "s1", 5*time.Second, func(p pendingFile) bool { return p.Unreachable != "" })
	if res := f.request(t, protocol.Message{Type: protocol.TypePrompt, ID: "s1"}); res.OK || !strings.Contains(res.Error, "attempt 1 is open") {
		t.Fatalf("p while down %+v", res)
	}
	f.hosts.set()
	done := make(chan protocol.Message, 1)
	go func() { done <- f.request(t, protocol.Message{Type: protocol.TypeDismiss, ID: "s1"}) }()
	select {
	case res := <-done:
		if !res.OK {
			t.Fatalf("dismiss %+v", res)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("dismiss hung")
	}
	f.local.relay.mu.Lock()
	n := len(f.local.relay.runners["s1"])
	f.local.relay.mu.Unlock()
	if n != 0 {
		t.Fatalf("%d runners after dismiss", n)
	}
	// The host back between the two checks: the goroutines stopped by
	// the dismiss are started again and the record stays.
	f.remote.mu.Lock()
	f.remote.down = nil
	f.remote.mu.Unlock()
	f.hosts.set(client.Host{Name: "vm", SSH: "vm"})
	if res := f.request(t, protocol.Message{Type: protocol.TypeAdd, ID: "s2", Relay: "vm", Repo: f.source(), Name: "proj", Branch: "back", AgentName: "argv", SubmittedAt: time.Now()}); !res.OK {
		t.Fatal(res.Error)
	}
	f.awaitRecord(t, "s2", 30*time.Second, func(p pendingFile) bool { return p.Taken })
	f.hosts.setFlip(1) // gone for the first read, back for the next
	res := f.request(t, protocol.Message{Type: protocol.TypeDismiss, ID: "s2"})
	if res.OK || !strings.Contains(res.Error, "still running") {
		t.Fatalf("dismiss with the host back %+v", res)
	}
	if got := f.awaitRecord(t, "s2", 30*time.Second, func(p pendingFile) bool { return p.retired() }); !got.OK {
		t.Fatalf("record after the restart %+v", got)
	}
}

// A machine under the host's name that is not the accepted one leaves
// a sent add stuck by design, marked as a mismatch, and the record is
// then dismissable.
func TestRelayMismatchDismissable(t *testing.T) {
	f := newRelayFixture(t, nil)
	other := New(Config{EnvironmentID: "elsewhere", Host: "vm", Version: "other", Targets: []Target{{Label: "laatmux", Tmux: &fakeServer{}, Managed: true}}, Store: f.store, Commands: t.TempDir()})
	discovered(other)
	otherRemote := newFakeRemote(t, f.ctx, other)
	var mu sync.Mutex
	current := f.remote
	dial := func(ctx context.Context, h client.Host) (*client.Conn, error) {
		mu.Lock()
		r := current
		mu.Unlock()
		return r.dial(ctx, h)
	}
	local := New(Config{
		EnvironmentID: "lenv", Version: "local", Hosts: f.hosts.get, Dial: dial, Pending: f.dir,
		MergedIdle: 200 * time.Millisecond, ReconnectMin: 20 * time.Millisecond,
	})
	discovered(local)
	go local.Run(f.ctx)
	f.setLocal(local)
	// Sent to the right machine, then another answers under the name.
	p := pendingFile{Pending: protocol.Pending{ID: "mm", Host: "vm", EnvironmentID: "henv", Source: f.source(), Repo: "proj", Branch: "mm", Agent: "argv", SubmittedAt: time.Now(), UpdatedAt: time.Now(), Taken: true}, Sent: true}
	if _, err := f.local.relay.create(p); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	current = otherRemote
	mu.Unlock()
	f.local.startPending(f.ctx, "mm")
	got := f.awaitRecord(t, "mm", 5*time.Second, func(p pendingFile) bool { return p.Mismatch != "" })
	if got.Done {
		t.Fatalf("done on a mismatch %+v", got)
	}
	if res := f.request(t, protocol.Message{Type: protocol.TypeDismiss, ID: "mm"}); !res.OK {
		t.Fatalf("dismiss on a mismatch %+v", res)
	}
	if _, ok := f.local.relay.get("mm"); ok {
		t.Fatal("record kept")
	}
}
