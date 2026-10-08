package daemon

import "github.com/laat/laatmux/internal/protocol"

// Accessors the tests read the daemon's state through; production
// reads it over the stream.

// agentRecords is the current agent records, with the stream's sequence.
func (d *Daemon) agentRecords() (uint64, []protocol.Agent) {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]protocol.Agent, 0, len(d.agents))
	for _, a := range d.agents {
		out = append(out, a)
	}
	return d.seq, out
}

// worktreeRecords is the current worktree records, the main checkouts'
// left out.
func (d *Daemon) worktreeRecords() []protocol.Worktree {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.worktreesLocked(false)
}

// create writes a new pending record, as acceptRelay does under its
// own checks; an id the relay has is not written again.
func (r *relay) create(p pendingFile) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.createLocked(p)
}
