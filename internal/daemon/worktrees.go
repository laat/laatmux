package daemon

import (
	"context"
	"maps"
	"slices"
	"time"

	"github.com/laat/laatmux/internal/protocol"
	"github.com/laat/laatmux/internal/tmux"
	"github.com/laat/laatmux/internal/worktree"
)

// DefaultWorktreeInterval is how often the daemon asks git for worktrees.
// Git is polled like tmux, but a worktree changes on the order of minutes,
// and one process per known repository per poll is the cost.
const DefaultWorktreeInterval = 2 * time.Second

// runWorktrees polls git until ctx is done. A poke, sent after add and rm,
// runs a poll at once so the record follows the command without waiting
// for the ticker.
func (d *Daemon) runWorktrees(ctx context.Context) {
	t := time.NewTicker(d.cfg.WorktreeInterval)
	defer t.Stop()
	for {
		d.pollWorktrees(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-d.poke:
		}
	}
}

// pollWorktrees lists worktrees and publishes the difference. A listing
// that fails is an unavailable observation: the last records are kept,
// the stamp stays where it was, and the error is logged once per change
// of message and published with the stamp. It still counts as the first
// poll, so a checkout git cannot read does not hold every snapshot.
//
// The listing is stamped with the observation revision read before git
// was asked, so a listing that overlaps a mutation carries the revision
// before it and does not stand for its outcome; the poll and its
// publication run under one lock, so an older observation never
// overwrites a newer one.
func (d *Daemon) pollWorktrees(ctx context.Context) {
	d.pollMu.Lock()
	defer d.pollMu.Unlock()
	d.mu.Lock()
	stamp := protocol.Listing{Generation: d.generation, Revision: d.revision}
	d.mu.Unlock()
	recs, mains, err := d.cfg.Store.ListAll(ctx)
	if ctx.Err() != nil {
		return
	}
	var roots []root
	if err == nil {
		paths := make([]string, 0, len(recs))
		for _, r := range recs {
			paths = append(paths, r.Root)
		}
		checkouts := make([]string, 0, len(mains))
		for _, r := range mains {
			checkouts = append(checkouts, r.Root)
		}
		roots = resolveRoots(paths, checkouts)
	}
	if err != nil {
		if d.logOnce(&d.lastListErr, "worktrees: %v", err) {
			d.mu.Lock()
			d.listErr = d.lastListErr
			d.publishListingLocked()
			d.mu.Unlock()
		}
	} else {
		d.lastListErr = ""
		d.mu.Lock()
		d.lastList, d.lastMains = recs, mains
		d.mainIDs = make(map[string]bool, len(mains))
		for _, r := range mains {
			d.mainIDs[d.worktreeID(r.Root)] = true
		}
		d.listed = true
		d.listing, d.listErr = stamp, ""
		now := time.Now()
		d.publishWorktreesLocked(now)
		d.setRootsLocked(roots, now)
		d.publishListingLocked()
		listed := listedIDs(d.worktrees)
		d.hostListedLocked(d.cfg.EnvironmentID, listed, false)
		d.mu.Unlock()
	}
	d.markDiscovered(&d.worktreesDiscovered)
}

// stepRevision counts one observation owed: an add that succeeded, or
// an rm that removed a worktree, so a listing read before it cannot
// stand for its outcome. The listing returned is the barrier such a
// listing must pass.
func (d *Daemon) stepRevision() protocol.Listing {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.revision++
	return protocol.Listing{Generation: d.generation, Revision: d.revision}
}

// publishListingLocked tells subscribers the listing's stamp and error,
// in an upsert with nothing else, which a client that does not know the
// field passes over. Called with d.mu held.
func (d *Daemon) publishListingLocked() {
	l := d.listing
	d.broadcastLocked(protocol.Message{Type: protocol.TypeUpsert, Listing: &l, ListingError: d.listErr})
}

// pokeWorktrees asks for a poll now; a poll already pending is enough.
func (d *Daemon) pokeWorktrees() {
	select {
	case d.poke <- struct{}{}:
	default:
	}
}

// setManagedRoots records which roots have a home session, from the
// managed server's panes. A change re-derives the worktree records from
// the last git listing, so a session appearing or exiting updates the
// record without a git call.
func (d *Daemon) setManagedRoots(panes []tmux.Pane, now time.Time) {
	roots := homeSessions(panes, d.paths.resolve)
	d.mu.Lock()
	defer d.mu.Unlock()
	if sameSessions(roots, d.managedRoots) {
		return
	}
	d.managedRoots = roots
	if d.listed {
		d.publishWorktreesLocked(now)
	}
}

func sameSessions(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

// publishWorktreesLocked joins the last git listing with the managed
// sessions and broadcasts what changed: the worktrees, and the main
// checkouts in use (inUseLocked). Called with d.mu held.
func (d *Daemon) publishWorktreesLocked(now time.Time) {
	seen := map[string]bool{}
	for _, r := range d.lastList {
		d.publishRecordLocked(r, now, seen)
	}
	for _, r := range d.lastMains {
		if d.inUseLocked(r) {
			d.publishRecordLocked(r, now, seen)
		}
	}
	for root, w := range d.worktrees {
		if seen[root] {
			continue
		}
		delete(d.worktrees, root)
		delete(d.gits, root)
		if w.Main {
			// Out of use, or gone: no task's worktree, and nothing the
			// listing removed.
			d.broadcastLocked(protocol.Message{Type: protocol.TypeRemove, WorktreeID: d.worktreeID(root)})
			continue
		}
		l := d.listing
		d.broadcastLocked(protocol.Message{Type: protocol.TypeRemove, WorktreeID: d.worktreeID(root), RemovedIn: &l})
		d.worktreeRemovedLocked(d.worktreeID(root), &l)
	}
}

// publishRecordLocked publishes one record of the listing when it
// changed, and marks its root seen. A main checkout's has no home
// session: a session new made in it is no workspace's. Called with d.mu
// held.
func (d *Daemon) publishRecordLocked(r worktree.Record, now time.Time, seen map[string]bool) {
	seen[r.Root] = true
	w := protocol.Worktree{
		ID:            d.worktreeID(r.Root),
		EnvironmentID: d.cfg.EnvironmentID,
		Repo:          r.Repo,
		Source:        r.Source,
		Branch:        r.Branch,
		Root:          r.Root,
		Main:          r.Main,
		UpdatedAt:     now,
	}
	if !r.Main {
		w.Session = d.managedRoots[r.Root]
	}
	// A branch checked out by hand that the connection cannot carry
	// is sent as it is shown, and marked: no command names the
	// worktree by it, and it is taken back with this root alone
	// (worktree.BranchIs).
	if worktree.CheckWire(r.Branch) != nil {
		w.Branch, w.BranchDisplayOnly = tmux.Printable(r.Branch), true
	}
	prev, had := d.worktrees[r.Root]
	if had && prev.Repo == w.Repo && prev.Source == w.Source && prev.Branch == w.Branch && prev.BranchDisplayOnly == w.BranchDisplayOnly && prev.Session == w.Session && prev.Main == w.Main {
		return
	}
	if had && prev.Branch == w.Branch {
		// The git object is the refresh's, carried across the
		// rebuild; a new branch at the root waits for its own.
		w.Git = prev.Git
	} else {
		delete(d.gits, r.Root)
	}
	d.worktrees[r.Root] = w
	d.broadcastLocked(protocol.Message{Type: protocol.TypeUpsert, Worktree: &w})
}

// inUseLocked reports whether a main checkout's record is published: its
// repository is in this host's config, a worktree of it is listed, or
// an agent is attributed to it. The rest of the checkouts under the
// repos directory, many on a machine that clones there by hand, have no
// record, no line and no git status refresh. Called with d.mu held.
func (d *Daemon) inUseLocked(r worktree.Record) bool {
	return r.Configured || r.Linked || d.mainAgents[d.worktreeID(r.Root)]
}

// syncMainsLocked publishes the main checkouts again when the set with
// an agent attributed to them changed: also is a main checkout's id an
// agent is about to be published with, whose record goes first, so no
// record names one a subscriber has not had; called again once the
// agents are published, it takes back the records no agent names any
// more. Called with d.mu held.
func (d *Daemon) syncMainsLocked(now time.Time, also string) {
	in := map[string]bool{}
	if d.mainIDs[also] {
		in[also] = true
	}
	for _, a := range d.agents {
		if d.mainIDs[a.WorktreeID] {
			in[a.WorktreeID] = true
		}
	}
	if maps.Equal(in, d.mainAgents) {
		return
	}
	d.mainAgents = in
	if d.listed {
		d.publishWorktreesLocked(now)
	}
}

// branchNameLocked is the branch of a published record as git has it:
// its Branch, or for one only shown, the last listing's for its root.
// Called with d.mu held.
func (d *Daemon) branchNameLocked(w protocol.Worktree) string {
	if !w.BranchDisplayOnly {
		return w.Branch
	}
	for _, r := range append(slices.Clip(d.lastList), d.lastMains...) {
		if r.Root == w.Root {
			return r.Branch
		}
	}
	return w.Branch
}

// worktreeID is <environment_id>/worktree/<root>: the root is absolute
// and unique on its host, and the environment id is hex, so the id parses
// from the left.
func (d *Daemon) worktreeID(root string) string { return d.cfg.EnvironmentID + "/worktree/" + root }

// worktreesLocked is the published records for a snapshot, the main
// checkouts' only for a subscriber that asked for them. Called with d.mu
// held.
func (d *Daemon) worktreesLocked(checkouts bool) []protocol.Worktree {
	out := make([]protocol.Worktree, 0, len(d.worktrees))
	for _, w := range d.worktrees {
		if !w.Main || checkouts {
			out = append(out, w)
		}
	}
	return out
}
