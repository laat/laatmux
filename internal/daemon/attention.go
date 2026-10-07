package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/laat/laatmux/internal/home"
	"github.com/laat/laatmux/internal/protocol"
)

// Attention: what the user has seen. The merging daemon keeps, per agent,
// the last activity it saw and when the agent went from working to idle,
// its finish, and when a tmux client of this machine last showed it after
// that, its visit. Both times are this machine's clock: a host's clock is
// never compared with it, and a host's activity_at is only a mark that
// the activity changed. An idle agent whose finish is after its visit is
// done, which the views show with ✅.
//
// The daemon's own agents are tracked from its poll, a remote host's from
// its upserts and snapshots while it is followed; the records the merged
// stream keeps for a host while it reconnects are not a source. A
// snapshot counts as an upsert does, so a finish while the laptop slept or
// the host was not followed is found when the host's snapshot comes,
// against the state kept on disk.
//
// A client shows an agent through a live attach pane whose target is the
// agent's managed session on the agent's host, or, on this machine's
// default server, as the agent's own pane. See ClientView.

// ClientView is what one tmux client of this machine's default server
// shows, as the Clients listing reads it: the pane, after a focused
// sidebar pane is replaced by the pane beside it, and the tags that say
// what that pane attaches to. Target is the attach pane's
// @laatmux_attach_target; Host, Attach and Workspace are the session's
// @laatmux_host, @laatmux_attach and @laatmux_workspace, for an attach
// pane from before the target was tagged.
type ClientView struct {
	Client     string
	Pane       string
	Dead       bool
	AttachPane bool
	Target     string
	Host       string
	Attach     string
	Workspace  string
}

// seenInterval is how often the clients are listed while an agent is
// done and not yet seen and a view is open or closed within the idle
// time. See seenWanted.
const seenInterval = time.Second

// attnEntry is one agent's attention state, as kept on disk. The agent
// id and the identity key it: a record with another identity is a new
// agent in the same pane, and starts over.
type attnEntry struct {
	Host       string            `json:"host"`            // the configured host name
	Local      bool              `json:"local,omitempty"` // the daemon's own agent
	PID        int               `json:"pid"`
	Start      int64             `json:"start"`
	Activity   protocol.Activity `json:"activity"`
	ActivityAt time.Time         `json:"activity_at"` // the host's; an opaque mark
	FinishedAt time.Time         `json:"finished_at,omitzero"`
	SeenAt     time.Time         `json:"seen_at,omitzero"`
}

func (e *attnEntry) record(id string) protocol.Attention {
	return protocol.Attention{AgentID: id, FinishedAt: e.FinishedAt, SeenAt: e.SeenAt}
}

// published is that the entry has a record in the stream: it has
// finished at least once.
func (e *attnEntry) published() bool { return !e.FinishedAt.IsZero() }

func (e *attnEntry) unseen() bool { return e.FinishedAt.After(e.SeenAt) }

// attention is the tracked agents and the file they are kept in.
type attention struct {
	path    string
	entries map[string]*attnEntry // by agent id
	poke    chan struct{}
	// dirty is that an entry changed since the file was written: the
	// file is written once per batch, a poll's upsert or a host's
	// snapshot, not once per agent.
	dirty bool
}

type attnFile struct {
	Entries map[string]*attnEntry `json:"entries"`
}

// openAttention reads the file; a missing one is empty.
func openAttention(path string) (*attention, error) {
	a := &attention{path: path, entries: map[string]*attnEntry{}, poke: make(chan struct{}, 1)}
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return a, nil
	}
	if err != nil {
		return nil, err
	}
	var f attnFile
	if err := json.Unmarshal(b, &f); err != nil {
		return nil, err
	}
	for id, e := range f.Entries {
		if e != nil {
			a.entries[id] = e
		}
	}
	return a, nil
}

// saveAttentionLocked marks the file for writing at the end of the batch.
func (d *Daemon) saveAttentionLocked() { d.attn.dirty = true }

// flushAttentionLocked writes the file whole when an entry changed,
// through a temporary file, so a crash leaves the old or the new one. It
// is a few hundred bytes an agent, written under d.mu as the relay's
// files are.
func (d *Daemon) flushAttentionLocked() {
	a := d.attn
	if a == nil || !a.dirty {
		return
	}
	b, err := json.Marshal(attnFile{Entries: a.entries})
	if err == nil {
		if err = os.MkdirAll(filepath.Dir(a.path), 0o700); err == nil {
			err = home.WriteAtomic(a.path, b)
		}
	}
	if err != nil {
		// Still dirty: the next batch, or the seen loop's next tick,
		// tries again.
		d.logOnce(&d.lastAttnErr, "attention: %v", err)
		return
	}
	a.dirty = false
	d.lastAttnErr = ""
}

// tracked reports whether an agent can be done: one in a managed session
// on any host, or on this machine's own default server. An agent on a
// remote host's default server or on another observed server cannot be
// jumped to, so it would never be seen.
func tracked(a protocol.Agent, local bool) bool {
	switch server := a.Server; {
	case server == protocol.ServerLaatmux:
		return true
	case local && server == protocol.ServerDefault:
		return true
	}
	return false
}

func identityOf(a protocol.Agent) (int, int64) {
	if a.Identity == nil {
		return 0, 0
	}
	return a.Identity.PID, a.Identity.StartUnix
}

// attendLocked takes one agent record from a source: the daemon's own
// poll, or a remote host's upsert or snapshot. A record that shows the
// agent idle with a new activity mark, where the last activity kept was
// working, is a finish, dated now.
func (d *Daemon) attendLocked(host string, local bool, a protocol.Agent) {
	if d.attn == nil {
		return
	}
	if !tracked(a, local) {
		d.forgetLocked(a.ID)
		return
	}
	pid, start := identityOf(a)
	e := d.attn.entries[a.ID]
	if e == nil || e.PID != pid || e.Start != start || e.Local != local || !local && e.Host != host {
		// A new agent, or another in the same pane: the old times go.
		// This machine's own agent keeps its entry across a rename of
		// the machine, which is only a label for the same agent.
		d.forgetLocked(a.ID)
		d.attn.entries[a.ID] = &attnEntry{Host: host, Local: local, PID: pid, Start: start, Activity: a.Activity, ActivityAt: a.ActivityAt}
		d.saveAttentionLocked()
		return
	}
	if e.Host != host {
		e.Host = host
		d.saveAttentionLocked()
	}
	// Unknown is the detector unable to tell, a startup grace after a
	// restart say: the activity kept stands, so working, unknown, then
	// idle is still a finish.
	act := a.Activity
	if act == protocol.Unknown {
		act = e.Activity
	}
	if e.Activity == act && e.ActivityAt.Equal(a.ActivityAt) {
		return
	}
	finished := e.Activity == protocol.Working && act == protocol.Idle && !e.ActivityAt.Equal(a.ActivityAt)
	e.Activity, e.ActivityAt = act, a.ActivityAt
	if finished {
		e.FinishedAt = time.Now()
		rec := e.record(a.ID)
		d.mbroadcastLocked(protocol.Message{Type: protocol.TypeUpsert, Attention: &rec})
		// The clients are listed now, not at the next tick: a client
		// that shows the agent when the finish is observed sees it at
		// once. An earlier listing is not asked, since the user may
		// have left since, an interrupt and a switch away say.
		select {
		case d.attn.poke <- struct{}{}:
		default:
		}
	}
	d.saveAttentionLocked()
}

// forgetLocked drops an agent's entry, and its record from the stream.
func (d *Daemon) forgetLocked(id string) {
	if d.attn == nil {
		return
	}
	e, ok := d.attn.entries[id]
	if !ok {
		return
	}
	delete(d.attn.entries, id)
	if e.published() {
		d.mbroadcastLocked(protocol.Message{Type: protocol.TypeRemove, AttentionID: id})
	}
	d.saveAttentionLocked()
}

// forgetHostLocked drops the entries of a remote host's agents that its
// snapshot no longer has.
func (d *Daemon) forgetHostLocked(host string, keep map[string]bool) {
	if d.attn == nil {
		return
	}
	for id, e := range d.attn.entries {
		if !e.Local && e.Host == host && !keep[id] {
			d.forgetLocked(id)
		}
	}
}

// forgetUnconfiguredLocked drops the entries of remote hosts no longer in
// the config.
func (d *Daemon) forgetUnconfiguredLocked(names []string) {
	if d.attn == nil {
		return
	}
	configured := map[string]bool{}
	for _, n := range names {
		configured[n] = true
	}
	for id, e := range d.attn.entries {
		if !e.Local && !configured[e.Host] {
			d.forgetLocked(id)
		}
	}
	d.flushAttentionLocked()
}

// forgetLocalLocked drops the entries of the daemon's own agents that its
// first complete poll did not find: they went while it was down.
func (d *Daemon) forgetLocalLocked() {
	if d.attn == nil {
		return
	}
	have := map[string]bool{}
	for key := range d.agents {
		have[d.agentID(key)] = true
	}
	for id, e := range d.attn.entries {
		if e.Local && !have[id] {
			d.forgetLocked(id)
		}
	}
	d.flushAttentionLocked()
}

// attentionsLocked is the published records, for a merged snapshot.
func (d *Daemon) attentionsLocked() []protocol.Attention {
	if d.attn == nil {
		return nil
	}
	var out []protocol.Attention
	for id, e := range d.attn.entries {
		if e.published() {
			out = append(out, e.record(id))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].AgentID < out[j].AgentID })
	return out
}

// agentLocked is the current record of a tracked agent.
func (d *Daemon) agentLocked(id string, e *attnEntry) (protocol.Agent, bool) {
	if e.Local {
		for _, a := range d.agents {
			if a.ID == id {
				return a, true
			}
		}
		return protocol.Agent{}, false
	}
	mh := d.mhosts[e.Host]
	if mh == nil {
		return protocol.Agent{}, false
	}
	a, ok := mh.agents[id]
	return a, ok
}

// homeSessionLocked is the home session of the worktree a workspace key
// names, "" when no record has it.
func (d *Daemon) homeSessionLocked(key string) string {
	env, root := protocol.SplitSessionKey(key)
	if env == d.cfg.EnvironmentID {
		if w, ok := d.worktrees[root]; ok {
			return w.Session
		}
	}
	for _, mh := range d.mhosts {
		for _, w := range mh.worktrees {
			if w.EnvironmentID == env && w.Root == root {
				return w.Session
			}
		}
	}
	return ""
}

// shownLocked reports whether any client in views shows the agent.
func (d *Daemon) shownLocked(views []ClientView, id string, e *attnEntry) bool {
	if len(views) == 0 {
		return false
	}
	a, ok := d.agentLocked(id, e)
	if !ok {
		return false
	}
	managed := a.Server == protocol.ServerLaatmux
	agentHost := e.Host
	if mh := d.localHostLocked(); e.Local && mh != nil {
		// This machine by the name the config gives it now, which
		// the sessions a jump makes are tagged with.
		agentHost = mh.status.Name
	}
	for _, v := range views {
		switch {
		case v.Dead:
			// Kept by remain-on-exit after its ssh ended: old output.
			continue
		case v.AttachPane:
			if !managed {
				continue
			}
			host, target := v.Host, v.Target
			if target == "" {
				// An attach pane from before the target was tagged: the
				// plain attachment's session, or the home session of the
				// worktree the workspace session is for.
				if h, s, ok := strings.Cut(v.Attach, "/"); ok && v.Attach != "" {
					target = s
					if host == "" {
						host = h
					}
				} else if v.Workspace != "" {
					target = d.homeSessionLocked(v.Workspace)
				}
			}
			if target != "" && target == a.Session && host == agentHost {
				return true
			}
		case e.Local && a.Server == protocol.ServerDefault && v.Pane != "" && v.Pane == a.PaneID:
			return true
		}
	}
	return false
}

// runSeen lists the clients once a second while seenWanted, and at once
// when poked, and moves the visit of every done agent a client shows to
// now.
func (d *Daemon) runSeen(ctx context.Context) {
	t := time.NewTicker(seenInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			d.mu.Lock()
			d.flushAttentionLocked() // a write that failed, again
			d.mu.Unlock()
			if !d.seenWanted() {
				continue
			}
		case <-d.attn.poke:
		}
		// A listing sees only the finishes before it began: one
		// observed while it ran may come after the user left, and the
		// listing that finish pokes decides it.
		began := time.Now()
		views, err := d.cfg.Clients(ctx)
		if ctx.Err() != nil {
			return
		}
		d.mu.Lock()
		if err != nil {
			d.logOnce(&d.lastClientsErr, "clients: %v", err)
		} else {
			d.lastClientsErr = ""
			d.markSeenLocked(views, began)
		}
		d.mu.Unlock()
	}
}

// seenWanted is whether the clients are listed on this tick: an agent is
// done and not yet seen, and a view, a merged subscriber, is open or the
// last one left less than the idle time ago, which is how long d.mctx
// outlives it. A listing with nothing unseen moves nothing. One with no
// view open only records a visit for a later view, and on a host's
// daemon, whose agents the laptop's daemon tracks for its own views, no
// later view comes, and a listing each second for as long as one of them
// stays done would be for nothing. The idle time covers a dashboard's
// jump, whose subscriber leaves as the jump lands. A finish lists at
// once, view or not, through its poke.
func (d *Daemon) seenWanted() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.mctx == nil {
		return false
	}
	for _, e := range d.attn.entries {
		if e.unseen() {
			return true
		}
	}
	return false
}

// markSeenLocked moves the visit of every done agent a client shows to
// now, of the finishes before began, when the listing began. A client
// sitting on an agent that is not done writes nothing.
func (d *Daemon) markSeenLocked(views []ClientView, began time.Time) {
	changed := false
	now := time.Now()
	for id, e := range d.attn.entries {
		if !e.unseen() || !e.FinishedAt.Before(began) || !d.shownLocked(views, id, e) {
			continue
		}
		e.SeenAt = now
		rec := e.record(id)
		d.mbroadcastLocked(protocol.Message{Type: protocol.TypeUpsert, Attention: &rec})
		changed = true
	}
	if changed {
		d.saveAttentionLocked()
		d.flushAttentionLocked()
	}
}

// Poke has the clients listed now.
func (d *Daemon) Poke() {
	if d.attn == nil {
		return
	}
	select {
	case d.attn.poke <- struct{}{}:
	default:
	}
}
