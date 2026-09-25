package daemon

import (
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
	if res := f.request(t, protocol.Message{Type: protocol.TypeDismiss, ID: "req-2", EnvironmentID: "other", Root: a.Root}); !res.OK {
		t.Fatalf("dismiss at another environment: %+v", res)
	}
}
