package daemon

import (
	"context"
	"sort"
	"strings"

	"github.com/laat/laatmux/internal/protocol"
)

// A task that needs the user and whose worktree was listed after its
// add is kept until dismissed. Its worktree can go meanwhile, removed
// by rm here or on another machine, or by hand, and the record would go
// on saying what it said, standing for a worktree row that is not
// there. So the daemon watches the listings it already has: a worktree
// the host reports removed, and a host's successful listing that lacks
// a task's worktree, the snapshot of a connection or a later poll's,
// send the task's host to be asked again, as the handoff asks it, and a
// worktree the listing does not have makes the task gone. A host that
// disconnects, or whose listing failed, says nothing about its
// worktrees and is not a removal.
//
// A task that has handed over to its worktree row is kept for the
// worktree's life, and is checked the same way: once the host says the
// worktree is gone, its record goes, prompt and all.

// tasksAtLocked schedules the check for every listed task that match
// selects, against the listing sig names, "" for a reported removal or a
// fresh listing, which always checks. Called with d.mu held; the relay's
// mutex comes before d.mu (see the lock order on Daemon), so the records
// are read on a goroutine of their own.
func (d *Daemon) tasksAtLocked(sig string, match, shown func(protocol.Pending) bool) {
	if d.relay == nil {
		return
	}
	go d.recheckTasks(sig, match, shown)
}

// worktreeRemovedLocked is a worktree the host reported gone, in the
// listing stamped at, nil from a daemon that does not say. A task that
// handed over to it goes at once when that listing reflects its add:
// the report is then the end of the worktree the task made, and asking
// the listing could find one made again at the root since. A report
// from before the add, delayed, is of a worktree the root had before,
// and the task is checked against the listing as every other is.
func (d *Daemon) worktreeRemovedLocked(worktreeID string, at *protocol.Listing) {
	if d.relay != nil && at != nil {
		go d.dropRetiredAt(worktreeID, *at)
	}
	d.tasksAtLocked("", func(p protocol.Pending) bool { return p.WorktreeID() == worktreeID }, nil)
}

// dropRetiredAt drops the tasks that handed over to the worktree whose
// adds the listing that found it gone reflects, each under its own
// remember lock (dropRetired); what a record that handed over says of
// its worktree does not change, so the choice made under the relay's
// mutex holds after it.
func (d *Daemon) dropRetiredAt(worktreeID string, at protocol.Listing) {
	var ids []string
	d.relay.mu.Lock()
	for id, p := range d.relay.recs {
		if p.retired() && p.ReplacedBy == worktreeID && p.Barrier != nil && at.Satisfies(*p.Barrier) {
			ids = append(ids, id)
		}
	}
	d.relay.mu.Unlock()
	for _, id := range ids {
		d.dropRetired(id, "its worktree is removed")
	}
}

// listedIDs is the ids of a host's worktree records, for
// hostListedLocked: the worktrees alone, since a task's worktree is never
// a main checkout, and a main checkout whose record comes and goes with
// its agents is no change of listing for the tasks.
func listedIDs(ws map[string]protocol.Worktree) map[string]bool {
	out := make(map[string]bool, len(ws))
	for _, w := range ws {
		if !w.Main {
			out[w.ID] = true
		}
	}
	return out
}

// hostListedLocked is a host's successful listing of worktrees: a task
// on the environment whose worktree it lacks is checked, once per
// listing that differs, since polls repeat a listing many times over. A
// fresh listing, a connection's first, is checked whatever came before:
// the worktree may have gone while the connection was down, and the
// listing after it can read the same as one from before.
func (d *Daemon) hostListedLocked(environmentID string, listed map[string]bool, fresh bool) {
	if environmentID == "" {
		return
	}
	sig := ""
	if !fresh {
		ids := make([]string, 0, len(listed))
		for id := range listed {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		sig = environmentID + "\x00" + strings.Join(ids, "\x00")
	}
	d.tasksAtLocked(sig, func(p protocol.Pending) bool {
		return p.EnvironmentID == environmentID && !listed[p.WorktreeID()]
	}, func(p protocol.Pending) bool {
		return p.EnvironmentID == environmentID && listed[p.WorktreeID()]
	})
}

// recheckTasks starts a check for each task that match selects and
// that is listed and not gone, retired or not. A task already checked
// against a listing with this very content is not checked again, until
// a listing that shows its worktree, which shown selects, clears that:
// content can recur, a listing from before the worktree was made and
// one after it went being the same. One whose check runs is marked to
// be checked once more when it ends, so a removal that lands meanwhile
// is not lost. Another runner, a delivery attempt say, is no reason to
// skip.
func (d *Daemon) recheckTasks(sig string, match, shown func(protocol.Pending) bool) {
	ctx := d.runCtx()
	if ctx.Err() != nil {
		return
	}
	d.relay.mu.Lock()
	defer d.relay.mu.Unlock()
	for id, p := range d.relay.recs {
		if shown != nil && shown(p.Pending) {
			delete(d.relay.checked, id)
			continue
		}
		if !p.Listed || p.Gone || p.WorktreeID() == "" || !match(p.Pending) {
			continue
		}
		if sig != "" && d.relay.checked[id] == sig {
			continue
		}
		if sig != "" {
			d.relay.checked[id] = sig
		}
		if d.relay.checking[id] {
			d.relay.recheck[id] = true
			continue
		}
		d.relay.checking[id] = true
		d.startRunnerLocked(ctx, id, func(ctx context.Context, id string) {
			for {
				d.checkGone(ctx, id)
				d.relay.mu.Lock()
				if ctx.Err() != nil {
					// Ended without an answer, by a dismiss that kept the
					// record say: the listing it was for is not checked.
					delete(d.relay.checked, id)
				}
				again := d.relay.recheck[id] && ctx.Err() == nil
				delete(d.relay.recheck, id)
				if !again {
					delete(d.relay.checking, id)
					d.relay.mu.Unlock()
					return
				}
				d.relay.mu.Unlock()
			}
		})
	}
}

// checkGone asks the task's host whether its worktree is still listed,
// and marks the task gone when it is not, or drops it when it had handed
// over. A host that cannot be asked, down or its listings failing, is
// asked again with backoff until it answers, the task is dismissed, or
// the daemon stops.
func (d *Daemon) checkGone(ctx context.Context, id string) {
	wait := d.cfg.ReconnectMin
	for ctx.Err() == nil {
		p, ok := d.relay.get(id)
		if !ok || !p.Listed || p.Gone {
			return
		}
		present, ok := d.listingHas(ctx, id, p)
		if ok {
			if !present {
				d.worktreeGone(ctx, id)
			}
			return
		}
		if !d.pause(ctx, &wait) {
			return
		}
	}
}

// dismissAt drops the tasks at a worktree that rm removed: with the
// worktree gone there is nothing left to deliver to or jump into. Only
// a finished one goes, through the settled path under the record's own
// locks: one whose add is still running, or whose attempt is open,
// stays, whatever its host, and is marked gone once the listing lacks
// its worktree. A task that handed over goes when removed, the stamp of
// rm's removal, reflects its add: an rm answered late must not take the
// task of a worktree made at the root since, and without the stamp the
// listing's check decides. The answer is ok whatever was dropped.
func (d *Daemon) dismissAt(requestID, environmentID, root string, removed *protocol.Listing) protocol.Message {
	res := protocol.Message{Type: protocol.TypeResult, ID: requestID, OK: true}
	if d.relay == nil {
		res.OK, res.Error = false, "this daemon has no relay capability"
		return res
	}
	var ids, retired []string
	d.relay.mu.Lock()
	for id, p := range d.relay.recs {
		if p.EnvironmentID != environmentID || p.Root != root {
			continue
		}
		if p.retired() {
			if removed != nil && p.Barrier != nil && removed.Satisfies(*p.Barrier) {
				retired = append(retired, id)
			}
			continue
		}
		ids = append(ids, id)
	}
	d.relay.mu.Unlock()
	for _, id := range retired {
		d.dropRetired(id, "dismissed by rm of its worktree")
	}
	for _, id := range ids {
		d.dismissEnded(id)
	}
	return res
}

// worktreeGone marks the task gone, its worktree not listed, or drops
// it when it has handed over. The record as it is now decides, not the
// copy the check began with: one that handed over while the host was
// asked is dropped, which persist, refusing a retired record, leaves to
// dropRetired.
func (d *Daemon) worktreeGone(ctx context.Context, id string) {
	if _, ok := d.persist(ctx, id, func(p *pendingFile) { p.Gone = true }); !ok {
		d.dropRetired(id, "its worktree is gone")
	}
}

// dropRetired removes a record that had handed over, its worktree gone
// or the user dismissing it: nothing is left for the prompt to say what
// it was made for. The stream has no message for it; its handoff is
// absent from the next snapshot. It runs under the record's remember
// lock, so an append the record still asked for is not made after, and
// is logged as dropped, why saying what dropped it; dropped is what the
// log says.
func (d *Daemon) dropRetired(id, why string) (dropped string, err error) {
	rl := d.relay.rememberLock(id)
	rl.Lock()
	defer rl.Unlock()
	d.relay.mu.Lock()
	if p, ok := d.relay.recs[id]; ok && p.retired() {
		dropped = droppedAppend(p)
	}
	err = d.dropRetiredLocked(id)
	d.relay.mu.Unlock()
	if err != nil {
		return "", err
	}
	d.logDropped(id, why, dropped)
	return dropped, nil
}

// dropRetiredLocked is dropRetired with the relay's mutex held. A file
// that cannot be removed leaves the record unchecked, so the next
// listing without its worktree tries again rather than passing over a
// listing already checked.
func (d *Daemon) dropRetiredLocked(id string) error {
	if p, ok := d.relay.recs[id]; !ok || !p.retired() {
		return nil
	}
	if err := d.relay.removeLocked(id); err != nil {
		d.cfg.Logger.Printf("pending: drop %s: %v", id, err)
		delete(d.relay.checked, id)
		return err
	}
	return nil
}
