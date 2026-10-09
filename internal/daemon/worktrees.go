package daemon

import (
	"context"
	"maps"
	"slices"
	"strings"
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
		checkouts := make([]root, 0, len(mains))
		for _, r := range mains {
			checkouts = append(checkouts, root{root: r.Root, unread: r.Unread})
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
		d.listed = true
		d.listing, d.listErr = stamp, ""
		now := time.Now()
		d.publishWorktreesLocked(now)
		d.setRootsLocked(roots, now)
		d.publishReposLocked(mains)
		d.publishListingLocked()
		listed := listedIDs(d.worktrees)
		d.hostListedLocked(d.cfg.EnvironmentID, listed, false)
		d.mu.Unlock()
	}
	d.markDiscovered(&d.worktreesDiscovered)
}

// runConfig looks at the config file every worktree interval until ctx
// is done (readConfig), on a daemon with a store or without one: the
// relay of a laptop whose own entry has no directories lets go of a
// host paused too. The first look is Run's, before the relay resumes
// its records.
func (d *Daemon) runConfig(ctx context.Context) {
	t := time.NewTicker(d.cfg.WorktreeInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			d.readConfig(ctx)
		}
	}
}

// readConfig acts on a config file that has changed: the store takes
// its repositories with their steps and the copy rules for every
// worktree, and the adds the agents' commands, so the next add uses
// them, and a poll at once labels the checkouts by the repositories;
// the merged subscribers get the hosts it lists (rereadHosts); the
// relay lets go of a host paused (pauseRelays). Those two not on the
// first read, Run's before any subscription and before the relay
// resumes its records, which then read the file as it found it or
// later. A file that does not read is
// logged once per change of message, and the daemon keeps what it had.
// A read of the hosts that failed last, a file being written as a
// subscription read it say, is made again at every look, the file
// changed or not, until it succeeds: a file that settles empty is no
// change to the watch. lastReposErr and configRead are runConfig's
// alone.
func (d *Daemon) readConfig(ctx context.Context) {
	read, changed, err := d.cfg.Reread()
	first := !d.configRead
	d.configRead = true
	switch {
	case err != nil:
		d.logOnce(&d.lastReposErr, "config: %v; the daemon keeps the config it had", err)
	case changed:
		d.lastReposErr = ""
		d.commands.Store(&read.Agents)
		if d.cfg.Store != nil {
			d.cfg.Store.SetListed(read.Listed)
			d.pokeWorktrees()
		}
		if !first {
			d.rereadHosts()
			d.pauseRelays()
		}
	case !first && d.hostsFailed():
		d.rereadHosts()
		d.pauseRelays()
	}
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

// publishReposLocked publishes the host's repositories, the listing's
// main checkouts, when the set is not the one published last: to its
// subscribers in an upsert of its own, and into the merged stream on
// the local host's record, which carries a host's set there. Called
// with d.mu held.
func (d *Daemon) publishReposLocked(mains []worktree.Record) {
	cos := make([]protocol.Checkout, 0, len(mains))
	for _, r := range mains {
		cos = append(cos, protocol.Checkout{Repo: r.Repo, Source: r.Source, Root: r.Root})
	}
	if d.repos != nil && slices.Equal(d.repos.Checkouts, cos) {
		return
	}
	d.repos = &protocol.RepoSet{Checkouts: cos}
	d.broadcastLocked(protocol.Message{Type: protocol.TypeUpsert, Repos: d.repos})
	if mh := d.localHostLocked(); mh != nil {
		mh.status.Repos = d.repos
		mh.status.Since = time.Now()
		st := mh.status
		d.mbroadcastLocked(protocol.Message{Type: protocol.TypeUpsert, HostStatus: &st})
	}
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
		// A main checkout where a worktree was, one an agent or a
		// pane record still names, is in use before the agents are
		// attributed again: its record goes out first, so the old
		// id's remove can wait for them (retireLocked).
		prev, had := d.worktrees[r.Root]
		if d.inUseLocked(r) || had && !prev.Main && d.namedLocked(prev.ID) {
			d.publishRecordLocked(r, now, seen)
		}
	}
	for root, w := range d.worktrees {
		if seen[root] || w.Main && d.mainAgents[w.ID] {
			// A main checkout gone from the listing while an agent
			// still names it stays until the agents are attributed
			// again, which a listing that changes the roots does next,
			// so no agent names a record gone.
			continue
		}
		delete(d.worktrees, root)
		delete(d.gits, root)
		d.removeRecordLocked(w)
	}
}

// publishRecordLocked publishes one record of the listing when it
// changed, and marks its root seen. A main checkout's home session is
// one as a worktree's is, the one a jump makes with a shell at its root
// say: add makes none. Called with d.mu held.
func (d *Daemon) publishRecordLocked(r worktree.Record, now time.Time, seen map[string]bool) {
	seen[r.Root] = true
	w := protocol.Worktree{
		ID:            d.recordID(root{root: r.Root, main: r.Main}),
		EnvironmentID: d.cfg.EnvironmentID,
		Repo:          r.Repo,
		Source:        r.Source,
		Branch:        r.Branch,
		Root:          r.Root,
		Main:          r.Main,
		Session:       d.managedRoots[r.Root],
		UpdatedAt:     now,
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
	if had && prev.Branch == w.Branch && prev.ID == w.ID {
		// The git object is the refresh's, carried across the
		// rebuild; a new branch at the root waits for its own.
		w.Git = prev.Git
	} else {
		delete(d.gits, r.Root)
	}
	d.worktrees[r.Root] = w
	d.broadcastLocked(protocol.Message{Type: protocol.TypeUpsert, Worktree: &w})
	if had && prev.ID != w.ID {
		// A worktree where a main checkout was, or the other way
		// round, with the repos directory under the worktrees one: the
		// record the root had goes, after the one taking its place,
		// and after the agents and panes naming it are attributed
		// again, which the roots changing does next (retireLocked).
		d.retiring[prev.ID] = prev
		d.retireLocked()
	}
}

// retireLocked removes the records replaced at their root that nothing
// names any more, an agent or a pane record. Called with d.mu held.
func (d *Daemon) retireLocked() {
	for id, w := range d.retiring {
		if d.namedLocked(id) {
			continue
		}
		delete(d.retiring, id)
		d.removeRecordLocked(w)
	}
}

// namedLocked reports whether an agent or a pane record names a record
// id. Called with d.mu held.
func (d *Daemon) namedLocked(id string) bool {
	for _, a := range d.agents {
		if a.WorktreeID == id {
			return true
		}
	}
	for _, p := range d.paneRecs {
		if p.WorktreeID == id {
			return true
		}
	}
	return false
}

// removeRecordLocked tells subscribers a record is gone: a main
// checkout's, out of use or gone, with no more, as it is no task's
// worktree and nothing the listing removed; a worktree's
// with the stamp of the listing that found it gone, and its tasks
// checked. Called with d.mu held.
func (d *Daemon) removeRecordLocked(w protocol.Worktree) {
	if w.Main {
		d.broadcastLocked(protocol.Message{Type: protocol.TypeRemove, WorktreeID: w.ID})
		return
	}
	l := d.listing
	d.broadcastLocked(protocol.Message{Type: protocol.TypeRemove, WorktreeID: w.ID, RemovedIn: &l})
	d.worktreeRemovedLocked(w.ID, &l)
}

// inUseLocked reports whether a main checkout's record is published: a
// worktree of it is listed, an agent is attributed to it, or it has a
// home session, which its line shows and goes to. Being in the host's
// config is not use: a config that lists every repository, as one made
// from a directory of clones does, would publish a line for each. The
// rest of the checkouts under the repos directory, many on a machine
// that clones there by hand, have no record, no line and no git status
// refresh, and so does one whose HEAD could not be read. Called with
// d.mu held.
func (d *Daemon) inUseLocked(r worktree.Record) bool {
	return !r.Unread && (r.Linked || d.mainAgents[d.checkoutID(r.Root)] || d.managedRoots[r.Root] != "")
}

// syncMainsLocked publishes the main checkouts again when the set with
// an agent attributed to them changed: also is a main checkout's id an
// agent is about to be published with, whose record goes first, so no
// record names one a subscriber has not had; called again once the
// agents are published, it takes back the records no agent names any
// more. Called with d.mu held.
func (d *Daemon) syncMainsLocked(now time.Time, also string) {
	// A record replaced at its root goes once the agents are past it.
	defer d.retireLocked()
	in := map[string]bool{}
	if isCheckoutID(also) {
		in[also] = true
	}
	for _, a := range d.agents {
		if isCheckoutID(a.WorktreeID) {
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

// checkoutID is <environment_id>/checkout/<root>, a main checkout's id:
// a worktree's has worktree in its place, so an agent's attribution to a
// main checkout is told by the id alone, on this host's agents and on a
// host's a merging daemon forwards, and taken out for a subscriber that
// did not ask for the main checkouts (subscriber.sees).
func (d *Daemon) checkoutID(root string) string { return d.cfg.EnvironmentID + "/checkout/" + root }

// isCheckoutID reports whether a record id, of any host, is a main
// checkout's.
func isCheckoutID(id string) bool {
	_, rest, _ := strings.Cut(id, "/")
	return strings.HasPrefix(rest, "checkout/")
}

// recordID is the id of a listed root's record, a worktree's or a main
// checkout's.
func (d *Daemon) recordID(r root) string {
	if r.main {
		return d.checkoutID(r.root)
	}
	return d.worktreeID(r.root)
}

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
