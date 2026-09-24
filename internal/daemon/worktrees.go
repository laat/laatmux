package daemon

import (
	"context"
	"time"

	"github.com/laat/laatmux/internal/protocol"
	"github.com/laat/laatmux/internal/tmux"
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
	recs, err := d.cfg.Store.List(ctx)
	if ctx.Err() != nil {
		return
	}
	if err != nil {
		if msg := err.Error(); msg != d.lastListErr {
			d.cfg.Logger.Printf("worktrees: %v", err)
			d.lastListErr = msg
			d.mu.Lock()
			d.listErr = msg
			d.publishListingLocked()
			d.mu.Unlock()
		}
	} else {
		d.lastListErr = ""
		d.mu.Lock()
		d.lastList = recs
		d.listed = true
		d.listing, d.listErr = stamp, ""
		d.publishWorktreesLocked(time.Now())
		d.publishListingLocked()
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
	d.seq++
	d.broadcastLocked(protocol.Message{Type: protocol.TypeUpsert, Seq: d.seq, Listing: &l, ListingError: d.listErr})
}

// pokeWorktrees asks for a poll now; a poll already pending is enough.
func (d *Daemon) pokeWorktrees() {
	select {
	case d.poke <- struct{}{}:
	default:
	}
}

// setManagedRoots records which roots have a managed session, from the
// managed server's panes. A change re-derives the worktree records from
// the last git listing, so a session appearing or exiting updates the
// record without a git call.
func (d *Daemon) setManagedRoots(panes []tmux.Pane, now time.Time) {
	roots := managedRoots(panes)
	d.mu.Lock()
	defer d.mu.Unlock()
	if sameRoots(roots, d.managedRoots) {
		return
	}
	d.managedRoots = roots
	if d.listed {
		d.publishWorktreesLocked(now)
	}
}

// managedRoots maps a root to the managed session whose single pane
// records it. Two sessions on one root is not a state add creates; the
// lexically first name wins so the record is stable.
func managedRoots(panes []tmux.Pane) map[string]string {
	count := map[string]int{}
	for _, p := range panes {
		count[p.Session]++
	}
	roots := map[string]string{}
	for _, p := range panes {
		if !p.Managed || p.Cwd == "" || count[p.Session] != 1 {
			continue
		}
		if cur, ok := roots[p.Cwd]; !ok || p.Session < cur {
			roots[p.Cwd] = p.Session
		}
	}
	return roots
}

func sameRoots(a, b map[string]string) bool {
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
// sessions and broadcasts what changed. Called with d.mu held.
func (d *Daemon) publishWorktreesLocked(now time.Time) {
	seen := map[string]bool{}
	for _, r := range d.lastList {
		seen[r.Root] = true
		w := protocol.Worktree{
			ID:            d.worktreeID(r.Root),
			EnvironmentID: d.cfg.EnvironmentID,
			Repo:          r.Repo,
			Source:        r.Source,
			Branch:        r.Branch,
			Root:          r.Root,
			Session:       d.managedRoots[r.Root],
			UpdatedAt:     now,
		}
		if prev, had := d.worktrees[r.Root]; had && prev.Repo == w.Repo && prev.Source == w.Source && prev.Branch == w.Branch && prev.Session == w.Session {
			continue
		}
		d.worktrees[r.Root] = w
		d.seq++
		d.broadcastLocked(protocol.Message{Type: protocol.TypeUpsert, Seq: d.seq, Worktree: &w})
	}
	for root := range d.worktrees {
		if seen[root] {
			continue
		}
		delete(d.worktrees, root)
		d.seq++
		d.broadcastLocked(protocol.Message{Type: protocol.TypeRemove, Seq: d.seq, WorktreeID: d.worktreeID(root)})
	}
}

// worktreeID is <environment_id>/worktree/<root>: the root is absolute
// and unique on its host, and the environment id is hex, so the id parses
// from the left.
func (d *Daemon) worktreeID(root string) string { return d.cfg.EnvironmentID + "/worktree/" + root }

// Worktrees returns the current worktree records.
func (d *Daemon) Worktrees() []protocol.Worktree {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.worktreesLocked()
}

func (d *Daemon) worktreesLocked() []protocol.Worktree {
	out := make([]protocol.Worktree, 0, len(d.worktrees))
	for _, w := range d.worktrees {
		out = append(out, w)
	}
	return out
}
