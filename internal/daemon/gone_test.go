package daemon

import (
	"context"
	"testing"
	"time"

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
// anywhere does.
func rmOnHost(t *testing.T, f *relayFixture, id, branch, root string) {
	t.Helper()
	pc := conn(t, f.host)
	pc.Write(protocol.Message{Type: protocol.TypeRm, ID: id, Repo: f.source(), Branch: branch, Root: root, Force: true})
	if res, _ := result(t, pc, id); !res.OK {
		t.Fatal(res.Error)
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
	f.local.mu.Lock()
	f.local.mhosts["vm"].cancel()
	f.local.mhosts["vm"].cancel = func() {}
	f.local.mu.Unlock()
	rmOnHost(t, f, "rm-n2", "missed", p.Root)
	time.Sleep(200 * time.Millisecond)
	if got, _ := f.local.relay.get("n2"); got.Gone {
		t.Fatal("gone without a listing")
	}
	f.local.mu.Lock()
	f.local.hostListedLocked("henv", map[string]bool{})
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
	f.local.mu.Lock()
	mh := f.local.mhosts["vm"]
	mh.cancel()
	mh.cancel = func() {}
	f.local.mu.Unlock()
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
// task as it was; the same listing again starts no second check.
func TestRelayNotGoneWhilePresent(t *testing.T) {
	shortWait(t, time.Second)
	f := newRelayFixture(t, []string{"loading"})
	c, _, _ := f.merged(t)
	defer c.Close()
	notDelivered(t, f, "n5", "present")
	f.local.mu.Lock()
	f.local.hostListedLocked("henv", map[string]bool{})
	f.local.mu.Unlock()
	time.Sleep(time.Second)
	for i := 0; i < 100; i++ {
		f.local.relay.mu.Lock()
		busy := f.local.relay.checking["n5"]
		f.local.relay.mu.Unlock()
		if !busy {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if got, _ := f.local.relay.get("n5"); got.Gone {
		t.Fatal("gone while the host lists the worktree")
	}
	f.local.mu.Lock()
	f.local.hostListedLocked("henv", map[string]bool{})
	f.local.mu.Unlock()
	time.Sleep(100 * time.Millisecond)
	f.local.relay.mu.Lock()
	again := f.local.relay.checking["n5"]
	f.local.relay.mu.Unlock()
	if again {
		t.Fatal("the same listing started a second check")
	}
}
