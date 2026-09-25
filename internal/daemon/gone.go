package daemon

import (
	"context"

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
// selects. Called with d.mu held; the relay's mutex comes before d.mu,
// so the records are read on a goroutine of their own.
func (d *Daemon) tasksAtLocked(match func(protocol.Pending) bool) {
	if d.relay == nil {
		return
	}
	go d.recheckTasks(match)
}

// worktreeRemovedLocked is a worktree the host reported gone.
func (d *Daemon) worktreeRemovedLocked(worktreeID string) {
	d.tasksAtLocked(func(p protocol.Pending) bool { return p.WorktreeID() == worktreeID })
}

// hostListedLocked is a host's successful listing of worktrees: a task
// on the environment whose worktree it lacks is checked. Polls repeat a
// listing many times over; one that is the same as the last one checked
// against changes nothing, since a task is listed only with its
// worktree present and a check runs until the host answers.
func (d *Daemon) hostListedLocked(environmentID string, listed map[string]bool) {
	if environmentID == "" || d.relay == nil {
		return
	}
	if last, ok := d.listedSets[environmentID]; ok && sameSet(last, listed) {
		return
	}
	if d.listedSets == nil {
		d.listedSets = map[string]map[string]bool{}
	}
	d.listedSets[environmentID] = listed
	d.tasksAtLocked(func(p protocol.Pending) bool {
		return p.EnvironmentID == environmentID && !listed[p.WorktreeID()]
	})
}

// recheckTasks starts a check for each task that match selects and
// that is listed, not gone and not retired, unless one runs for it
// already. Another runner, a delivery attempt say, is no reason to skip:
// a removal it overlaps would be lost.
func (d *Daemon) recheckTasks(match func(protocol.Pending) bool) {
	ctx := d.runCtx()
	if ctx.Err() != nil {
		return
	}
	d.relay.mu.Lock()
	defer d.relay.mu.Unlock()
	for id, p := range d.relay.recs {
		if !p.Listed || p.Gone || p.retired() || p.WorktreeID() == "" || d.relay.checking[id] || !match(p.Pending) {
			continue
		}
		d.relay.checking[id] = true
		d.startRunnerLocked(ctx, id, func(ctx context.Context, id string) {
			defer func() {
				d.relay.mu.Lock()
				delete(d.relay.checking, id)
				d.relay.mu.Unlock()
			}()
			d.checkGone(ctx, id)
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

func sameSet(a, b map[string]bool) bool {
	if len(a) != len(b) {
		return false
	}
	for k := range a {
		if !b[k] {
			return false
		}
	}
	return true
}
