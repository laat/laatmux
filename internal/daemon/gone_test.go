package daemon

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"testing"
	"time"

	"github.com/laat/laatmux/internal/peer"
	"github.com/laat/laatmux/internal/protocol"
)

// notDelivered makes a task that ends needing the user: the add done,
// its worktree listed, the prompt not delivered to an agent that never
// becomes ready.
func notDelivered(t *testing.T, f *relayFixture, id, branch string) pendingFile {
	t.Helper()
	if res := f.request(t, protocol.Message{Type: protocol.TypeAdd, ID: id, Relay: "vm", Repo: f.source(), Name: "proj", Branch: branch, AgentName: "claude", Prompt: "p", SubmittedAt: time.Now()}); !res.OK {
		t.Fatal(res.Error)
	}
	p := f.awaitRecord(t, id, 30*time.Second, func(p pendingFile) bool { return p.Done && p.Listed })
	if p.Prompt != protocol.DeliveryNotDelivered || p.retired() || p.Gone {
		t.Fatalf("record %+v", p)
	}
	return p
}

// rmOnHost removes a worktree through the host's daemon, as an rm from
// anywhere does, and waits for the host's own listing to lack it, as a
// listing that triggers a check does.
func rmOnHost(t *testing.T, f *relayFixture, id, branch, root string) {
	t.Helper()
	pc := conn(t, f.host)
	pc.Write(protocol.Message{Type: protocol.TypeRm, ID: id, Repo: f.source(), Branch: branch, Root: root, Force: true})
	if res, _ := result(t, pc, id); !res.OK {
		t.Fatal(res.Error)
	}
	for i := 0; ; i++ {
		f.host.mu.Lock()
		_, listed := f.host.worktrees[root]
		f.host.mu.Unlock()
		if !listed {
			return
		}
		if i > 500 {
			t.Fatal("the host's listing kept the removed worktree")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// stopFollow cancels the laptop daemon's follow of the host, so only the
// listings the test makes reach the task, and waits for what the follow
// had started to end. It cancels a connected follow, under the lock the
// follow says so under: its read then fails and it returns without
// dialing again, so a test's checks are the host's only dials after. The
// task is left with no memo, as the follow's latest listing, which shows
// its worktree, leaves it: one from before the worktree was listed, run
// late, could have left the memo of an empty listing, which the test's
// own would then pass over.
func stopFollow(t *testing.T, f *relayFixture, id string) *mergedHost {
	t.Helper()
	var mh *mergedHost
	for i := 0; ; i++ {
		f.local.mu.Lock()
		mh = f.local.mhosts["vm"]
		connected := mh.status.Connected
		if connected {
			mh.cancel()
			mh.cancel = func() {}
		}
		f.local.mu.Unlock()
		if connected {
			break
		}
		if i > 500 {
			t.Fatal("the follow never connected")
		}
		time.Sleep(10 * time.Millisecond)
	}
	awaitRechecks(t, f, id)
	f.local.relay.mu.Lock()
	delete(f.local.relay.checked, id)
	f.local.relay.mu.Unlock()
	return mh
}

// rechecksCreated is the line a goroutine dump has for each goroutine
// tasksAtLocked started, named from the method so a rename cannot leave
// awaitRechecks waiting for nothing; TestRelayRechecksInDump pins it.
var rechecksCreated = []byte("created by " + runtime.FuncForPC(reflect.ValueOf((*Daemon).tasksAtLocked).Pointer()).Name() + " ")

// goroutines is a dump of every goroutine's stack, whole.
func goroutines() []byte {
	buf := make([]byte, 1<<16)
	for {
		n := runtime.Stack(buf, true)
		if n < len(buf) {
			return buf[:n]
		}
		buf = make([]byte, 2*len(buf))
	}
}

// awaitRechecks waits for every goroutine tasksAtLocked started to have
// run, and then for the task's check to end. A listing is applied under
// d.mu and reads the records on a goroutine of its own, with no handle
// to join: one applied before the follow's cancel still clears the
// task's memo, or starts its check, whenever that goroutine runs, after
// a listing the test makes included. A goroutine started before the
// call is in every dump until it has run, so the first dump without one
// is the join, whatever starts after.
func awaitRechecks(t *testing.T, f *relayFixture, id string) {
	t.Helper()
	for i := 0; ; i++ {
		if !bytes.Contains(goroutines(), rechecksCreated) {
			break
		}
		if i > 500 {
			t.Fatal("a listing's rechecks did not run")
		}
		time.Sleep(10 * time.Millisecond)
	}
	for i := 0; ; i++ {
		f.local.relay.mu.Lock()
		busy := f.local.relay.checking[id]
		f.local.relay.mu.Unlock()
		if !busy {
			return
		}
		if i > 500 {
			t.Fatalf("%s's check did not end", id)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// awaitRechecks finds what tasksAtLocked starts: a goroutine held back
// by d.mu, which recheckTasks takes first, is in the dump by that line.
// A refactor that moves the go statement fails here, not as a memo the
// tests below appear to lose.
func TestRelayRechecksInDump(t *testing.T) {
	f := newRelayFixture(t, nil)
	f.local.mu.Lock()
	f.local.tasksAtLocked("", func(protocol.Pending) bool { return false }, nil)
	dump := goroutines()
	f.local.mu.Unlock()
	if !bytes.Contains(dump, rechecksCreated) {
		t.Fatalf("no %q in the goroutine dump", rechecksCreated)
	}
}

// A task that needs the user, its worktree removed afterwards: the
// host's report of the removal, which the laptop daemon follows while a
// view is subscribed, has the host asked again, and the task is gone
// rather than standing for a worktree that is not there.
func TestRelayListedTaskGoneAfterRemoval(t *testing.T) {
	shortWait(t, time.Second)
	f := newRelayFixture(t, []string{"loading"})
	c, _, _ := f.merged(t)
	defer c.Close()
	p := notDelivered(t, f, "n1", "later")
	rmOnHost(t, f, "rm-n1", "later", p.Root)
	got := f.awaitRecord(t, "n1", 30*time.Second, func(p pendingFile) bool { return p.Gone })
	if got.retired() {
		t.Fatalf("handed over a removed worktree: %+v", got)
	}
}

// The removal missed, the laptop daemon's follow of the host being
// down: a full listing that lacks the worktree, the next snapshot say,
// has the task checked all the same.
func TestRelayListedTaskGoneOnListing(t *testing.T) {
	shortWait(t, time.Second)
	f := newRelayFixture(t, []string{"loading"})
	c, _, _ := f.merged(t)
	defer c.Close()
	p := notDelivered(t, f, "n2", "missed")
	stopFollow(t, f, "n2")
	rmOnHost(t, f, "rm-n2", "missed", p.Root)
	time.Sleep(200 * time.Millisecond)
	if got, _ := f.local.relay.get("n2"); got.Gone {
		t.Fatal("gone without a listing")
	}
	f.local.mu.Lock()
	f.local.hostListedLocked("henv", map[string]bool{}, false)
	f.local.mu.Unlock()
	f.awaitRecord(t, "n2", 30*time.Second, func(p pendingFile) bool { return p.Gone })
}

// rm drops the tasks at the worktree it removed: dismiss with the
// environment and root, the tasks elsewhere untouched.
func TestRelayDismissAt(t *testing.T) {
	shortWait(t, time.Second)
	f := newRelayFixture(t, []string{"loading"})
	a := notDelivered(t, f, "d1", "one")
	notDelivered(t, f, "d2", "two")
	// Another environment's worktree at the same root is not this one.
	if res := f.request(t, protocol.Message{Type: protocol.TypeDismiss, ID: "req-0", EnvironmentID: "other", Root: a.Root}); !res.OK {
		t.Fatalf("dismiss at another environment: %+v", res)
	}
	if _, ok := f.local.relay.get("d1"); !ok {
		t.Fatal("a task on another environment was dropped")
	}
	// The request carries an id of its own, as every client's does; the
	// root is what names the tasks.
	res := f.request(t, protocol.Message{Type: protocol.TypeDismiss, ID: "req-1", EnvironmentID: "henv", Root: a.Root})
	if !res.OK {
		t.Fatalf("dismiss at: %+v", res)
	}
	if _, ok := f.local.relay.get("d1"); ok {
		t.Fatal("the task at the root stayed")
	}
	if _, ok := f.local.relay.get("d2"); !ok {
		t.Fatal("a task elsewhere was dropped")
	}
}

// A removal that lands while another runner follows the task, a
// delivery attempt say, still has it checked.
func TestRelayGoneWhileAnotherRunner(t *testing.T) {
	shortWait(t, time.Second)
	f := newRelayFixture(t, []string{"loading"})
	c, _, _ := f.merged(t)
	defer c.Close()
	p := notDelivered(t, f, "n3", "busy")
	f.local.relay.mu.Lock()
	f.local.startRunnerLocked(f.ctx, "n3", func(ctx context.Context, id string) { <-ctx.Done() })
	f.local.relay.mu.Unlock()
	rmOnHost(t, f, "rm-n3", "busy", p.Root)
	f.awaitRecord(t, "n3", 30*time.Second, func(p pendingFile) bool { return p.Gone })
	f.local.stopRunners("n3")
}

// A listing that comes back after failing: the host's stamp, upserted
// by a poll that succeeded, has the tasks it lacks checked, with no
// reconnect and no removal seen.
func TestRelayGoneOnListingStamp(t *testing.T) {
	shortWait(t, time.Second)
	f := newRelayFixture(t, []string{"loading"})
	c, _, _ := f.merged(t)
	defer c.Close()
	p := notDelivered(t, f, "n4", "stamp")
	// The follow has the worktree cached, as it has since the worktree
	// was listed, and a stamp read against that cache shows it: it asks
	// nothing.
	for i := 0; ; i++ {
		f.local.mu.Lock()
		_, cached := f.local.mhosts["vm"].worktrees[p.WorktreeID()]
		f.local.mu.Unlock()
		if cached {
			break
		}
		if i > 500 {
			t.Fatal("the follow never had the worktree")
		}
		time.Sleep(10 * time.Millisecond)
	}
	mh := stopFollow(t, f, "n4")
	dials := f.remote.count()
	f.local.applyRemote(f.ctx, mh, protocol.Message{Type: protocol.TypeUpsert, Listing: &protocol.Listing{Generation: 1, Revision: 1}})
	awaitRechecks(t, f, "n4")
	if f.remote.count() != dials {
		t.Fatal("a stamp whose listing shows the worktree asked the host")
	}
	rmOnHost(t, f, "rm-n4", "stamp", p.Root)
	f.local.mu.Lock()
	delete(mh.worktrees, p.WorktreeID())
	f.local.mu.Unlock()
	// A failed poll's stamp says nothing.
	f.local.applyRemote(f.ctx, mh, protocol.Message{Type: protocol.TypeUpsert, Listing: &protocol.Listing{Generation: 1, Revision: 1}, ListingError: "git: broken"})
	time.Sleep(300 * time.Millisecond)
	if got, _ := f.local.relay.get("n4"); got.Gone {
		t.Fatal("gone on a failed listing")
	}
	f.local.applyRemote(f.ctx, mh, protocol.Message{Type: protocol.TypeUpsert, Listing: &protocol.Listing{Generation: 1, Revision: 2}})
	f.awaitRecord(t, "n4", 30*time.Second, func(p pendingFile) bool { return p.Gone })
}

// Dismiss by root drops only finished tasks: a running add at the root
// stays, even one whose host is removed from the config, whose own
// dismiss would drop it.
func TestRelayDismissAtKeepsRunning(t *testing.T) {
	f := newRelayFixture(t, nil)
	running := &pendingFile{Pending: protocol.Pending{ID: "r1", Host: "gone", EnvironmentID: "henv", Repo: "proj", Branch: "b", Root: "/w/b", Sent: true, Taken: true, SubmittedAt: time.Now()}}
	open := &pendingFile{Pending: protocol.Pending{ID: "r2", Host: "gone", EnvironmentID: "henv", Repo: "proj", Branch: "b", Root: "/w/b", Sent: true, Taken: true, Done: true, OK: true, Prompt: protocol.DeliveryUnknown, AttemptOpen: true, Attempt: 1, SubmittedAt: time.Now()}}
	f.local.relay.mu.Lock()
	f.local.relay.recs["r1"], f.local.relay.recs["r2"] = running, open
	f.local.relay.mu.Unlock()
	if res := f.request(t, protocol.Message{Type: protocol.TypeDismiss, ID: "req-3", EnvironmentID: "henv", Root: "/w/b"}); !res.OK {
		t.Fatalf("dismiss at: %+v", res)
	}
	for _, id := range []string{"r1", "r2"} {
		if _, ok := f.local.relay.get(id); !ok {
			t.Fatalf("%s dropped by dismiss at", id)
		}
	}
}

// A listing that lacks the worktree while the host still has it, a
// merged record that fell behind say, is asked about and leaves the
// task as it was; the same listing again starts no second check, until
// a listing that shows the worktree has the task forget it.
func TestRelayNotGoneWhilePresent(t *testing.T) {
	shortWait(t, time.Second)
	f := newRelayFixture(t, []string{"loading"})
	c, _, _ := f.merged(t)
	defer c.Close()
	p := notDelivered(t, f, "n5", "present")
	memo := func() string {
		f.local.relay.mu.Lock()
		defer f.local.relay.mu.Unlock()
		return f.local.relay.checked["n5"]
	}
	// listing has the laptop daemon see a listing of the host's, waits
	// out what it started, and says whether the host was asked: every
	// check dials it.
	listing := func(listed map[string]bool) bool {
		dials := f.remote.count()
		f.local.mu.Lock()
		f.local.hostListedLocked("henv", listed, false)
		f.local.mu.Unlock()
		awaitRechecks(t, f, "n5")
		return f.remote.count() > dials
	}
	// Only the listings below reach the task: the host's own polls,
	// which show the worktree, would clear its memo.
	mh := stopFollow(t, f, "n5")
	if !listing(map[string]bool{}) {
		t.Fatal("a listing that lacks the worktree did not ask the host")
	}
	if got, _ := f.local.relay.get("n5"); got.Gone {
		t.Fatal("gone while the host lists the worktree")
	}
	// The same listing again starts nothing: recheckTasks sets checking
	// under the relay's mutex before any runner starts, so reading it as
	// soon as it returns is exact.
	absent := func(p protocol.Pending) bool { return p.EnvironmentID == "henv" }
	f.local.recheckTasks("henv\x00", absent, nil)
	f.local.relay.mu.Lock()
	again, sig := f.local.relay.checking["n5"], f.local.relay.checked["n5"]
	f.local.relay.mu.Unlock()
	if again || sig != "henv\x00" {
		t.Fatalf("the same listing started a second check, or none was recorded: %v %q", again, sig)
	}
	// Another listing that lacks it is checked again.
	elsewhere := map[string]bool{"henv/worktree//elsewhere": true}
	asked := listing(elsewhere)
	if sig = memo(); !asked || sig != "henv\x00henv/worktree//elsewhere" {
		t.Fatalf("a changed listing was not checked: asked %v, memo %q", asked, sig)
	}
	// One that shows it has the task forget that, and the same listing
	// that lacks it is checked again.
	if listing(map[string]bool{p.WorktreeID(): true, "henv/worktree//elsewhere": true}) {
		t.Fatal("a listing that shows the worktree asked the host")
	}
	if sig = memo(); sig != "" {
		t.Fatalf("a listing that shows the worktree kept the memo: %q", sig)
	}
	asked = listing(elsewhere)
	if sig = memo(); !asked || sig != "henv\x00henv/worktree//elsewhere" {
		t.Fatalf("the listing was not checked again: asked %v, memo %q", asked, sig)
	}
	// Then the worktree goes while the connection is down, and the
	// reconnect's snapshot reads as the listing already checked against:
	// a fresh listing is checked all the same.
	rmOnHost(t, f, "rm-n5", "present", p.Root)
	f.local.applyRemote(f.ctx, mh, protocol.Message{Type: protocol.TypeSnapshot, Listing: &protocol.Listing{Generation: 1, Revision: 9}, Worktrees: []protocol.Worktree{{ID: "henv/worktree//elsewhere", EnvironmentID: "henv", Root: "/elsewhere"}}})
	f.awaitRecord(t, "n5", 30*time.Second, func(p pendingFile) bool { return p.Gone })
}

// Only a listing starts a check: a snapshot without the stamp, from a
// host whose listing has not succeeded, is not one; with the stamp it
// is, driven through applyRemote as the follow drives it.
func TestRelaySnapshotNeedsListing(t *testing.T) {
	shortWait(t, time.Second)
	f := newRelayFixture(t, []string{"loading"})
	c, _, _ := f.merged(t)
	defer c.Close()
	p := notDelivered(t, f, "n7", "snap")
	mh := stopFollow(t, f, "n7")
	rmOnHost(t, f, "rm-n7", "snap", p.Root)
	f.local.applyRemote(f.ctx, mh, protocol.Message{Type: protocol.TypeSnapshot})
	// Nor one with the last good stamp and the current poll's error.
	f.local.applyRemote(f.ctx, mh, protocol.Message{Type: protocol.TypeSnapshot, Listing: &protocol.Listing{Generation: 1, Revision: 1}, ListingError: "git: broken"})
	time.Sleep(500 * time.Millisecond)
	if got, _ := f.local.relay.get("n7"); got.Gone {
		t.Fatal("a snapshot without a successful listing started a check")
	}
	f.local.applyRemote(f.ctx, mh, protocol.Message{Type: protocol.TypeSnapshot, Listing: &protocol.Listing{Generation: 1, Revision: 1}})
	f.awaitRecord(t, "n7", 30*time.Second, func(p pendingFile) bool { return p.Gone })
}

// A task that becomes eligible after its host's listing was already
// seen is checked against that same listing: the memo is the task's,
// not the listing's.
func TestRelayGoneAgainstSeenListing(t *testing.T) {
	shortWait(t, time.Second)
	f := newRelayFixture(t, []string{"loading"})
	c, _, _ := f.merged(t)
	defer c.Close()
	// Only the listings below reach the task. The follow is stopped once
	// connected, so the host has said who it is and the add is accepted
	// pinned, as its siblings' are; it is followed on the relay's own
	// connection.
	stopFollow(t, f, "n6")
	f.local.mu.Lock()
	f.local.hostListedLocked("henv", map[string]bool{}, false)
	f.local.mu.Unlock()
	// Seen before the task is made: the listing's goroutine has run.
	awaitRechecks(t, f, "n6")
	p := notDelivered(t, f, "n6", "seen")
	rmOnHost(t, f, "rm-n6", "seen", p.Root)
	f.local.mu.Lock()
	f.local.hostListedLocked("henv", map[string]bool{}, false)
	f.local.mu.Unlock()
	f.awaitRecord(t, "n6", 30*time.Second, func(p pendingFile) bool { return p.Gone })
}

// A trigger while a task's check runs marks it to run once more; a
// retired record is neither changed nor published by a runner still
// finishing, and persist does not spin on it.
func TestRelayRecheckAndRetired(t *testing.T) {
	f := newRelayFixture(t, nil)
	f.local.relay.mu.Lock()
	f.local.relay.recs["k1"] = &pendingFile{Pending: protocol.Pending{ID: "k1", Host: "vm", EnvironmentID: "henv", Root: "/w/k", Listed: true, Done: true, OK: true, Prompt: protocol.DeliveryNotDelivered}}
	f.local.relay.checking["k1"] = true
	f.local.relay.mu.Unlock()
	f.local.recheckTasks("", func(protocol.Pending) bool { return true }, nil)
	f.local.relay.mu.Lock()
	marked := f.local.relay.recheck["k1"]
	delete(f.local.relay.checking, "k1")
	delete(f.local.relay.recheck, "k1")
	f.local.relay.recs["k2"] = &pendingFile{Pending: protocol.Pending{ID: "k2", Host: "vm", EnvironmentID: "henv", Root: "/w/r"}, ReplacedBy: "henv/worktree//w/r"}
	f.local.relay.mu.Unlock()
	if !marked {
		t.Fatal("a trigger during a check was lost")
	}
	if _, ok := f.local.setPending("k2", false, func(p *pendingFile) { p.ListingError = "x" }); ok {
		t.Fatal("a retired record was changed")
	}
	if got, _ := f.local.relay.get("k2"); got.ListingError != "" {
		t.Fatalf("retired record changed: %+v", got)
	}
	done := make(chan struct{})
	go func() {
		f.local.persist(f.ctx, "k2", func(p *pendingFile) { p.Gone = true })
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("persist spun on a retired record")
	}
}

// A host restarted with its listing failing: the reconnect's snapshot
// has no stamp, and the first successful listing comes as a stamp
// upsert. It is fresh, and checked though it reads as the listing the
// task was checked against before the disconnect; a later one that
// reads the same is not.
func TestRelayFreshStampAfterUnstampedSnapshot(t *testing.T) {
	shortWait(t, time.Second)
	f := newRelayFixture(t, []string{"loading"})
	c, _, _ := f.merged(t)
	defer c.Close()
	p := notDelivered(t, f, "n8", "fresh")
	// The task was checked against a listing without its worktree, and
	// found present. The follow's listings, which show the worktree,
	// would clear the memo; stopFollow waits for the last of them, and
	// the memo is confirmed set just before the snapshot, or the test
	// would not be about it.
	mh := stopFollow(t, f, "n8")
	f.local.relay.mu.Lock()
	f.local.relay.checked["n8"] = "henv\x00"
	f.local.relay.mu.Unlock()
	rmOnHost(t, f, "rm-n8", "fresh", p.Root)
	f.local.relay.mu.Lock()
	seeded := f.local.relay.checked["n8"]
	f.local.relay.mu.Unlock()
	if seeded != "henv\x00" {
		t.Fatalf("the seeded memo was cleared: %q", seeded)
	}
	f.local.applyRemote(f.ctx, mh, protocol.Message{Type: protocol.TypeSnapshot})
	f.local.applyRemote(f.ctx, mh, protocol.Message{Type: protocol.TypeUpsert, Listing: &protocol.Listing{Generation: 2, Revision: 1}})
	f.awaitRecord(t, "n8", 30*time.Second, func(p pendingFile) bool { return p.Gone })
	f.local.mu.Lock()
	listed := mh.listed
	f.local.mu.Unlock()
	if !listed {
		t.Fatal("the connection's listing was not recorded")
	}
}

// A check that ends without an answer, stopped while its host cannot
// answer, leaves no memo, so the same listing is checked again.
func TestRelayCancelledCheckLeavesNoMemo(t *testing.T) {
	f := newRelayFixture(t, nil)
	f.hosts.set() // the host gone from the config: relayConn refuses, the check retries
	f.local.relay.mu.Lock()
	f.local.relay.recs["c1"] = &pendingFile{Pending: protocol.Pending{ID: "c1", Host: "vm", EnvironmentID: "henv", Root: "/w/c", Listed: true, Done: true, OK: true, Prompt: protocol.DeliveryNotDelivered}, Barrier: &protocol.Listing{Generation: 1, Revision: 1}}
	f.local.relay.mu.Unlock()
	absent := func(p protocol.Pending) bool { return p.ID == "c1" }
	f.local.recheckTasks("sig", absent, nil)
	f.local.relay.mu.Lock()
	started, memo := f.local.relay.checking["c1"], f.local.relay.checked["c1"]
	f.local.relay.mu.Unlock()
	if !started || memo != "sig" {
		t.Fatalf("no check: %v %q", started, memo)
	}
	f.local.stopRunners("c1")
	f.local.relay.mu.Lock()
	running, memo := f.local.relay.checking["c1"], f.local.relay.checked["c1"]
	f.local.relay.mu.Unlock()
	if running || memo != "" {
		t.Fatalf("after the stop: checking %v memo %q", running, memo)
	}
	f.local.recheckTasks("sig", absent, nil)
	f.local.relay.mu.Lock()
	again := f.local.relay.checking["c1"]
	f.local.relay.mu.Unlock()
	if !again {
		t.Fatal("the listing was not checked again")
	}
	f.local.stopRunners("c1")
}

// delivered makes a task that hands over: the add done and its prompt
// delivered on the argv.
func delivered(t *testing.T, f *relayFixture, id, branch string) pendingFile {
	t.Helper()
	if res := f.request(t, protocol.Message{Type: protocol.TypeAdd, ID: id, Relay: "vm", Repo: f.source(), Name: "proj", Branch: branch, AgentName: "argv", Prompt: "made for " + branch, SubmittedAt: time.Now()}); !res.OK {
		t.Fatal(res.Error)
	}
	return f.awaitRecord(t, id, 30*time.Second, func(p pendingFile) bool { return p.retired() })
}

// A task that handed over is kept, prompt and all, while its worktree
// is there, and goes when the host says the worktree is gone.
func TestRelayRetiredGoesWithWorktree(t *testing.T) {
	shortWait(t, time.Second)
	f := newRelayFixture(t, nil)
	c, _, _ := f.merged(t)
	defer c.Close()
	p := delivered(t, f, "h1", "kept")
	if p.PromptText != "made for kept" {
		t.Fatalf("record %+v", p)
	}
	// Listings that have the worktree keep it.
	f.local.mu.Lock()
	f.local.hostListedLocked("henv", map[string]bool{p.ReplacedBy: true}, false)
	f.local.mu.Unlock()
	time.Sleep(200 * time.Millisecond)
	if _, ok := f.local.relay.get("h1"); !ok {
		t.Fatal("dropped while the worktree is listed")
	}
	rmOnHost(t, f, "rm-h1", "kept", p.Root)
	deadline := time.Now().Add(30 * time.Second)
	for {
		if _, ok := f.local.relay.get("h1"); !ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("retired record kept after its worktree went")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if _, err := os.Stat(filepath.Join(f.dir, FileName("h1"))); err == nil {
		t.Fatal("file kept")
	}
}

// rm's dismiss at the worktree drops a task that handed over there.
// Only by the removal's stamp, and only when the stamp is from after the
// task's add: an rm answered late must not take the task of a worktree
// made at the root since.
func TestRelayDismissAtRetired(t *testing.T) {
	shortWait(t, time.Second)
	f := newRelayFixture(t, nil)
	p := delivered(t, f, "h2", "gone")
	b := *p.Barrier
	for i, stamp := range []*protocol.Listing{nil, {Generation: b.Generation, Revision: b.Revision - 1}, {Generation: b.Generation, Revision: b.Revision + 1}} {
		if res := f.request(t, protocol.Message{Type: protocol.TypeDismiss, ID: "req-h2", EnvironmentID: "henv", Root: p.Root, Listing: stamp}); !res.OK {
			t.Fatalf("dismiss at: %+v", res)
		}
		if _, ok := f.local.relay.get("h2"); ok != (i < 2) {
			t.Fatalf("stamp %+v: kept %v", stamp, ok)
		}
	}
}

// A check that began on a task that then handed over drops the retired
// record rather than leaving it, prompt and all, for good.
func TestRelayGoneAfterHandoffDrops(t *testing.T) {
	f := newRelayFixture(t, nil)
	f.local.relay.mu.Lock()
	f.local.relay.recs["k3"] = &pendingFile{Pending: protocol.Pending{ID: "k3", Host: "vm", EnvironmentID: "henv", Root: "/w/k3", Listed: true, Done: true, OK: true, Prompt: protocol.DeliveryDelivered}, PromptText: "p", ReplacedBy: "henv/worktree//w/k3"}
	f.local.relay.recs["k4"] = &pendingFile{Pending: protocol.Pending{ID: "k4", Host: "vm", EnvironmentID: "henv", Root: "/w/k4", Listed: true, Done: true, OK: true, Prompt: protocol.DeliveryNotDelivered}, PromptText: "p"}
	f.local.relay.mu.Unlock()
	f.local.worktreeGone(f.ctx, "k3")
	if _, ok := f.local.relay.get("k3"); ok {
		t.Fatal("retired record kept")
	}
	f.local.worktreeGone(f.ctx, "k4")
	if p, ok := f.local.relay.get("k4"); !ok || !p.Gone {
		t.Fatalf("task needing the user: %+v %v", p, ok)
	}
}

// A retired record whose file cannot be removed is tried again on the
// next listing, the same listing included.
func TestRelayDropRetiredRetries(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	f := newRelayFixture(t, nil)
	rec := pendingFile{Pending: protocol.Pending{ID: "k5", Host: "vm", EnvironmentID: "henv", Root: "/w/k5", Listed: true, Done: true, OK: true, Prompt: protocol.DeliveryDelivered}, PromptText: "p", ReplacedBy: "henv/worktree//w/k5"}
	if _, err := f.local.relay.create(rec); err != nil {
		t.Fatal(err)
	}
	f.local.relay.mu.Lock()
	f.local.relay.checked["k5"] = "sig"
	f.local.relay.mu.Unlock()
	if err := os.Chmod(f.dir, 0o500); err != nil {
		t.Fatal(err)
	}
	f.local.dropRetired("k5")
	os.Chmod(f.dir, 0o700)
	f.local.relay.mu.Lock()
	_, kept := f.local.relay.recs["k5"]
	_, checked := f.local.relay.checked["k5"]
	f.local.relay.mu.Unlock()
	if !kept || checked {
		t.Fatalf("after a failed removal: kept %v, checked %v", kept, checked)
	}
	f.local.dropRetired("k5")
	if _, ok := f.local.relay.get("k5"); ok {
		t.Fatal("kept after the directory was writable again")
	}
}

// A host's report that a worktree is gone drops the task that handed
// over to it at once when the listing that found it gone reflects the
// task's add; a report from before the add, delayed, is of a worktree
// the root had before, and leaves the task to the listing's check.
func TestRelayRetiredDroppedOnRemoval(t *testing.T) {
	f := newRelayFixture(t, nil)
	rec := pendingFile{Pending: protocol.Pending{ID: "k6", Host: "vm", EnvironmentID: "henv", Root: "/w/k6", Listed: true, Done: true, OK: true, Prompt: protocol.DeliveryDelivered},
		PromptText: "p", ReplacedBy: "henv/worktree//w/k6", Barrier: &protocol.Listing{Generation: 5, Revision: 3}}
	if _, err := f.local.relay.create(rec); err != nil {
		t.Fatal(err)
	}
	f.local.dropRetiredAt("henv/worktree//w/other", protocol.Listing{Generation: 5, Revision: 9})
	f.local.dropRetiredAt("henv/worktree//w/k6", protocol.Listing{Generation: 5, Revision: 2})
	if _, ok := f.local.relay.get("k6"); !ok {
		t.Fatal("dropped for another worktree, or by a removal from before the add")
	}
	f.local.mu.Lock()
	f.local.worktreeRemovedLocked("henv/worktree//w/k6", &protocol.Listing{Generation: 5, Revision: 3})
	f.local.mu.Unlock()
	f.awaitGone(t, "k6")
}

// The sweep takes a host's answer only from the host as the config has
// it now: a cached answer from an entry since changed drops nothing.
func TestRelaySweepIgnoresStaleAnswer(t *testing.T) {
	f := newRelayFixture(t, nil)
	rec := pendingFile{Pending: protocol.Pending{ID: "k7", Host: "vm", EnvironmentID: "henv", Root: "/w/k7", Listed: true, Done: true, OK: true, Prompt: protocol.DeliveryDelivered},
		PromptText: "p", ReplacedBy: "henv/worktree//w/k7", RetiredAt: time.Now().Add(-2 * handoffRetention)}
	if _, err := f.local.relay.create(rec); err != nil {
		t.Fatal(err)
	}
	f.local.mu.Lock()
	f.local.mhosts["vm"] = &mergedHost{host: peer.Host{Name: "vm", SSH: "elsewhere"}, status: protocol.HostStatus{Name: "vm", EnvironmentID: "other"}}
	f.local.mu.Unlock()
	f.local.sweepRelay(time.Now())
	if _, ok := f.local.relay.get("k7"); !ok {
		t.Fatal("swept on an answer from an entry the config no longer has")
	}
	f.local.mu.Lock()
	f.local.mhosts["vm"].host = peer.Host{Name: "vm", SSH: "vm"}
	f.local.mu.Unlock()
	f.local.sweepRelay(time.Now())
	if _, ok := f.local.relay.get("k7"); ok {
		t.Fatal("kept though the host answers as another machine")
	}
}

// awaitGone waits for the relay to have dropped the record.
func (f *relayFixture) awaitGone(t *testing.T, id string) {
	t.Helper()
	for i := 0; ; i++ {
		if _, ok := f.local.relay.get(id); !ok {
			return
		}
		if i > 500 {
			t.Fatalf("%s kept", id)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
