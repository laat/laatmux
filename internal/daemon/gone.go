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

// tasksAtLocked schedules the check for every listed task that match
// selects, against the listing sig names, "" for a reported removal,
// which always checks. Called with d.mu held; the relay's mutex comes
// before d.mu, so the records are read on a goroutine of their own.
func (d *Daemon) tasksAtLocked(sig string, match, shown func(protocol.Pending) bool) {
	if d.relay == nil {
		return
	}
	go d.recheckTasks(sig, match, shown)
}

// worktreeRemovedLocked is a worktree the host reported gone.
func (d *Daemon) worktreeRemovedLocked(worktreeID string) {
	d.tasksAtLocked("", func(p protocol.Pending) bool { return p.WorktreeID() == worktreeID }, nil)
}

// hostListedLocked is a host's successful listing of worktrees: a task
// on the environment whose worktree it lacks is checked, once per
// listing that differs, since polls repeat a listing many times over.
func (d *Daemon) hostListedLocked(environmentID string, listed map[string]bool) {
	if environmentID == "" {
		return
	}
	ids := make([]string, 0, len(listed))
	for id := range listed {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	sig := environmentID + "\x00" + strings.Join(ids, "\x00")
	d.tasksAtLocked(sig, func(p protocol.Pending) bool {
		return p.EnvironmentID == environmentID && !listed[p.WorktreeID()]
	}, func(p protocol.Pending) bool {
		return p.EnvironmentID == environmentID && listed[p.WorktreeID()]
	})
}

// recheckTasks starts a check for each task that match selects and
// that is listed, not gone and not retired. A task already checked
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
		if !p.Listed || p.Gone || p.retired() || p.WorktreeID() == "" || !match(p.Pending) {
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
// and marks the task gone when it is not. A host that cannot be asked,
// down or its listings failing, is asked again with backoff until it
// answers, the task is dismissed, or the daemon stops.
func (d *Daemon) checkGone(ctx context.Context, id string) {
	wait := d.cfg.ReconnectMin
	for ctx.Err() == nil {
		p, ok := d.relay.get(id)
		if !ok || !p.Listed || p.Gone || p.retired() {
			return
		}
		present, ok := d.listingHas(ctx, id, p)
		if ok {
			if !present {
				d.persist(ctx, id, func(p *pendingFile) { p.Gone = true })
			}
			return
		}
		if !d.relayBackoff(ctx, &wait) {
			return
		}
	}
}

// dismissAt drops the tasks at a worktree that rm removed: with the
// worktree gone there is nothing left to deliver to or jump into. Only
// a finished one goes, through the settled path under the record's own
// locks: one whose add is still running, or whose attempt is open,
// stays, whatever its host, and is marked gone once the listing lacks
// its worktree. The answer is ok whatever was dropped.
func (d *Daemon) dismissAt(requestID, environmentID, root string) protocol.Message {
	res := protocol.Message{Type: protocol.TypeResult, ID: requestID, OK: true}
	if d.relay == nil {
		res.OK, res.Error = false, "this daemon has no relay capability"
		return res
	}
	var ids []string
	d.relay.mu.Lock()
	for id, p := range d.relay.recs {
		if p.EnvironmentID == environmentID && p.Root == root && !p.retired() {
			ids = append(ids, id)
		}
	}
	d.relay.mu.Unlock()
	for _, id := range ids {
		d.dismissEnded(id)
	}
	return res
}
