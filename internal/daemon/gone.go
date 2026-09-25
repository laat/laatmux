package daemon

import (
	"context"
	"time"

	"github.com/laat/laatmux/internal/protocol"
)

// A task that needs the user and whose worktree was listed after its
// add is kept until dismissed. Its worktree can go meanwhile, removed
// by rm here or on another machine, or by hand, and the record would go
// on saying what it said, standing for a worktree row that is not
// there. So the daemon watches the listings it already has: a worktree
// the host reports removed, and a host's full listing that lacks a
// task's worktree, send the task's host to be asked again, as the
// handoff asks it, and a worktree the listing does not have makes the
// task gone. A host that disconnects says nothing about its worktrees
// and is not a removal.

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

// hostListedLocked is a host's full listing of worktrees: a task on the
// environment whose worktree it lacks is checked.
func (d *Daemon) hostListedLocked(environmentID string, listed map[string]bool) {
	if environmentID == "" {
		return
	}
	d.tasksAtLocked(func(p protocol.Pending) bool {
		return p.EnvironmentID == environmentID && !listed[p.WorktreeID()]
	})
}

// recheckTasks starts a runner for each task that match selects and
// that is listed, not gone, not retired, and not followed by a runner
// already: one running settles or retires on its own.
func (d *Daemon) recheckTasks(match func(protocol.Pending) bool) {
	ctx := d.runCtx()
	if ctx.Err() != nil {
		return
	}
	d.relay.mu.Lock()
	defer d.relay.mu.Unlock()
	for id, p := range d.relay.recs {
		if !p.Listed || p.Gone || p.retired() || p.WorktreeID() == "" || len(d.relay.runners[id]) > 0 || !match(p.Pending) {
			continue
		}
		d.startRunnerLocked(ctx, id, d.checkGone)
	}
}

// checkGone asks the task's host whether its worktree is still listed,
// and marks the task gone when it is not. A host that cannot be asked is
// asked again with backoff, for as long as a handoff waits; past that
// the next listing that lacks the worktree asks again.
func (d *Daemon) checkGone(ctx context.Context, id string) {
	wait := d.cfg.ReconnectMin
	deadline := time.Now().Add(handoffPatience)
	for ctx.Err() == nil && time.Now().Before(deadline) {
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

// dismissAt drops the tasks at a worktree that rm removed: the add of
// each has an outcome, and with the worktree gone there is nothing left
// to deliver to or jump into. Each goes through dismiss, whose rules
// stand: one whose add is still running, or whose attempt is open,
// stays and is marked gone once the listing lacks its worktree. The
// answer is ok whatever was dropped.
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
		d.dismiss(id)
	}
	return res
}
