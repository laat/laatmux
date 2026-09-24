package daemon

import (
	"context"
	"sort"
	"strings"
	"time"

	"github.com/laat/laatmux/internal/client"
	"github.com/laat/laatmux/internal/protocol"
)

// The merged stream. The daemon on the machine the user sits at is the one
// process there, so it is where every host's stream is merged: it dials
// the hosts in its config, subscribes to each, and republishes one stream
// with every host's records, a host record per host saying whether that
// host is reachable, and this machine's local workspace sessions. A
// sidebar pane per window is then one subscriber of the local socket, and
// each remote daemon sees one subscriber per laptop.
//
// Nothing crosses the network except state: the merged stream is the same
// records, forwarded. The daemon's own records are published into it
// directly rather than through its own socket. The merged sequence is the
// merging daemon's own; remote sequences are consumed here.

// Merged stream timing.
const (
	// DefaultMergedIdle is how long remote subscriptions are held after
	// the last merged subscriber leaves. A laptop with no sidebar and no
	// dashboard open holds no ssh channels; the first subscriber after
	// that pays the reconnect, which is what ls pays without merging.
	DefaultMergedIdle = 60 * time.Second
	// DefaultSessionInterval is how often the local sessions are listed
	// while there is a merged subscriber. Settle and unsettle reach every
	// sidebar within it.
	DefaultSessionInterval = time.Second
	// Reconnect backoff for a host that is down, doubling to the max.
	DefaultReconnectMin = time.Second
	reconnectMax        = 30 * time.Second
)

// mergedHost is one configured host in the merged stream: its record, the
// records cached from it, and the goroutine following it.
type mergedHost struct {
	host      client.Host
	status    protocol.HostStatus
	agents    map[string]protocol.Agent    // by id; empty for the local host, whose records are the daemon's own
	worktrees map[string]protocol.Worktree // by id
	cancel    context.CancelFunc           // stops the follow goroutine; nil while not following
}

// mergedSubscribe registers a merged subscriber and returns its snapshot.
// The hosts are read from the config on every subscription, so a host
// added to the file shows up on the next ls without a restart, and one
// removed gets a remove for its host record. The local sessions are
// listed once, synchronously, so the snapshot's sessions are as fresh as
// the connection whether the poll had been idle or never started.
func (d *Daemon) mergedSubscribe(ctx context.Context, drop func()) (*subscriber, protocol.Message) {
	// The config read and the listing are serialized with their
	// application, against the poll and against another subscription,
	// so neither is ever applied after a newer one.
	d.subMu.Lock()
	hosts, err := d.cfg.Hosts()
	if err != nil {
		// Keep the last host set: a config that does not parse is
		// reported by the client, which reads the same file.
		d.logOnce(&d.lastHostsErr, "hosts: %v", err)
	} else {
		d.lastHostsErr = ""
	}
	sessions, serr := d.listSessions(ctx)
	d.mu.Lock()
	d.stopIdleLocked()
	if d.mctx == nil {
		d.mctx, d.mcancel = context.WithCancel(ctx)
		go d.runSessions(d.mctx)
	}
	if err == nil {
		d.reconcileHostsLocked(hosts)
	}
	for _, mh := range d.mhosts {
		if !mh.host.Local() && mh.cancel == nil {
			d.startFollowLocked(mh)
		}
	}
	d.applySessionsLocked(sessions, serr)
	d.subMu.Unlock()
	s := &subscriber{ch: make(chan protocol.Message, subscriberBuffer), drop: drop, merged: true}
	d.msubs[s] = struct{}{}
	snap := d.mergedSnapshotLocked()
	d.mu.Unlock()
	return s, snap
}

// mergedUnsubscribe removes a subscriber whose connection ended.
func (d *Daemon) mergedUnsubscribe(s *subscriber) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, ok := d.msubs[s]; ok {
		d.mergedGoneLocked(s, false)
	}
}

// mergedGoneLocked removes a merged subscriber, closing its transport
// when it is being dropped for falling behind, and starts the idle timer
// when it was the last. Both ways out go through here, so a subscriber
// dropped by overflow starts the timer as one that hung up does.
func (d *Daemon) mergedGoneLocked(s *subscriber, dropped bool) {
	delete(d.msubs, s)
	close(s.ch)
	if dropped && s.drop != nil {
		go s.drop()
	}
	if len(d.msubs) == 0 && d.mctx != nil && d.midle == nil {
		d.midleGen++
		gen := d.midleGen
		d.midle = time.AfterFunc(d.cfg.MergedIdle, func() { d.mergedIdle(gen) })
	}
}

// stopIdleLocked cancels a pending idle timer. A callback that has
// already fired and is waiting for the mutex sees the generation moved
// on and does nothing, so a subscriber that arrives and leaves in that
// window gets its own full idle time from the timer its leaving sets.
func (d *Daemon) stopIdleLocked() {
	if d.midle != nil {
		d.midle.Stop()
		d.midle = nil
		d.midleGen++
	}
}

// mergedIdle drops the remote subscriptions and the sessions poll once no
// merged subscriber has been around for the idle time. The cached records
// stay for the next snapshot, with the host records saying they are
// neither connected nor listed, which is the state of a host not yet
// reached and what a one-shot client waits on.
func (d *Daemon) mergedIdle(gen uint64) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if gen != d.midleGen {
		return // stopped, or superseded by a later timer
	}
	d.midle = nil
	if len(d.msubs) > 0 || d.mctx == nil {
		return
	}
	d.mcancel()
	d.mctx, d.mcancel = nil, nil
	now := time.Now()
	for _, mh := range d.mhosts {
		if mh.cancel != nil {
			mh.cancel()
			mh.cancel = nil
		}
		if !mh.host.Local() {
			mh.status.Connected, mh.status.Listed, mh.status.Error, mh.status.Reconnecting, mh.status.Since = false, false, "", false, now
		}
	}
}

// reconcileHostsLocked brings the host set in line with the config. A
// host whose entry changed is dropped and added again, since its ssh
// alias is how it is reached.
func (d *Daemon) reconcileHostsLocked(hosts []client.Host) {
	want := map[string]client.Host{}
	for _, h := range hosts {
		want[h.Name] = h
	}
	for name, mh := range d.mhosts {
		if h, ok := want[name]; ok && h == mh.host {
			continue
		}
		d.dropHostLocked(mh)
		delete(d.mhosts, name)
	}
	// Names are unique: the config rejects a host listed twice.
	d.mnames = d.mnames[:0]
	now := time.Now()
	for _, h := range hosts {
		d.mnames = append(d.mnames, h.Name)
		if _, ok := d.mhosts[h.Name]; ok {
			continue
		}
		mh := &mergedHost{host: h, status: protocol.HostStatus{Name: h.Name, SSH: h.SSH, Since: now},
			agents: map[string]protocol.Agent{}, worktrees: map[string]protocol.Worktree{}}
		if h.Local() {
			// This machine is itself: connected, and listed once its
			// first poll of every server and of git is complete, which
			// markDiscovered publishes.
			mh.status.Connected, mh.status.Listed = true, d.panesDiscovered && d.worktreesDiscovered
			mh.status.EnvironmentID, mh.status.Version = d.cfg.EnvironmentID, d.cfg.Version
			mh.status.Capabilities = d.capabilities()
		}
		d.mhosts[h.Name] = mh
		st := mh.status
		d.mbroadcastLocked(protocol.Message{Type: protocol.TypeUpsert, HostStatus: &st})
		if h.Local() {
			// Subscribers from before the local host was configured
			// have never seen its records.
			for _, a := range d.agents {
				a := a
				d.mbroadcastLocked(protocol.Message{Type: protocol.TypeUpsert, Agent: &a})
			}
			for _, w := range d.worktrees {
				w := w
				d.mbroadcastLocked(protocol.Message{Type: protocol.TypeUpsert, Worktree: &w})
			}
		}
	}
}

// dropHostLocked stops following a host and removes everything of its from
// the stream: its records, then the host record.
func (d *Daemon) dropHostLocked(mh *mergedHost) {
	if mh.cancel != nil {
		mh.cancel()
		mh.cancel = nil
	}
	if mh.host.Local() {
		for key := range d.agents {
			d.mbroadcastLocked(protocol.Message{Type: protocol.TypeRemove, AgentID: d.agentID(key)})
		}
		for root := range d.worktrees {
			d.mbroadcastLocked(protocol.Message{Type: protocol.TypeRemove, WorktreeID: d.worktreeID(root)})
		}
	}
	for id := range mh.agents {
		d.mbroadcastLocked(protocol.Message{Type: protocol.TypeRemove, AgentID: id})
	}
	for id := range mh.worktrees {
		d.mbroadcastLocked(protocol.Message{Type: protocol.TypeRemove, WorktreeID: id})
	}
	d.mbroadcastLocked(protocol.Message{Type: protocol.TypeRemove, HostName: mh.status.Name})
}

// localHostLocked is the merged host that is this machine, nil when the
// config has none: then the daemon's own records are not in the merged
// stream, as ls without merging would not show them either.
func (d *Daemon) localHostLocked() *mergedHost {
	for _, mh := range d.mhosts {
		if mh.host.Local() {
			return mh
		}
	}
	return nil
}

// localListedLocked marks the local host's record listed, on discovery.
func (d *Daemon) localListedLocked() {
	mh := d.localHostLocked()
	if mh == nil || mh.status.Listed {
		return
	}
	mh.status.Listed = true
	mh.status.Since = time.Now()
	st := mh.status
	d.mbroadcastLocked(protocol.Message{Type: protocol.TypeUpsert, HostStatus: &st})
}

// forwardLocalLocked publishes one of the daemon's own upserts or removes
// into the merged stream. Called from broadcastLocked with d.mu held.
func (d *Daemon) forwardLocalLocked(m protocol.Message) {
	if len(d.msubs) == 0 || d.localHostLocked() == nil {
		return
	}
	d.mbroadcastLocked(m)
}

// mbroadcastLocked numbers a message on the merged sequence and sends it
// to every merged subscriber, dropping one that has fallen behind as
// broadcastLocked does.
func (d *Daemon) mbroadcastLocked(m protocol.Message) {
	d.mseq++
	m.Seq = d.mseq
	for s := range d.msubs {
		select {
		case s.ch <- m:
		default:
			d.mergedGoneLocked(s, true)
		}
	}
}

// mergedSnapshotLocked is the merged state: every host's record in config
// order, all cached records, and the local sessions by name.
func (d *Daemon) mergedSnapshotLocked() protocol.Message {
	m := protocol.Message{Type: protocol.TypeSnapshot, Seq: d.mseq, SessionsError: d.sessionsErr}
	for _, name := range d.mnames {
		mh := d.mhosts[name]
		m.Hosts = append(m.Hosts, mh.status)
		if mh.host.Local() {
			for _, a := range d.agents {
				m.Agents = append(m.Agents, a)
			}
			m.Worktrees = append(m.Worktrees, d.worktreesLocked()...)
			continue
		}
		for _, a := range mh.agents {
			m.Agents = append(m.Agents, a)
		}
		for _, w := range mh.worktrees {
			m.Worktrees = append(m.Worktrees, w)
		}
	}
	for _, s := range d.msessions {
		m.Sessions = append(m.Sessions, s)
	}
	sort.Slice(m.Sessions, func(i, j int) bool { return m.Sessions[i].Name < m.Sessions[j].Name })
	if d.relay != nil {
		m.Pendings, m.Handoffs = d.relay.pendings()
	}
	return m
}

// startFollowLocked starts the goroutine that keeps one remote host
// subscribed, under the merged context.
func (d *Daemon) startFollowLocked(mh *mergedHost) {
	ctx, cancel := context.WithCancel(d.mctx)
	mh.cancel = cancel
	go d.follow(ctx, mh)
}

// follow keeps one host subscribed until ctx is done, reconnecting with
// backoff. Cached records stay while the host is down; the host record
// says so. A daemon started by a client of the user's inherits its
// environment, so ssh finds the agent and the ControlMaster socket the
// user's shell has; where it does not, the host record carries ssh's own
// error.
func (d *Daemon) follow(ctx context.Context, mh *mergedHost) {
	backoff := d.cfg.ReconnectMin
	for ctx.Err() == nil {
		c, err := d.cfg.Dial(ctx, mh.host)
		switch {
		case err != nil:
			// The client prefixes its errors with the host name, which
			// the host record already carries.
			msg := strings.TrimPrefix(err.Error(), mh.host.Name+": ")
			d.setHostStatus(ctx, mh, func(st *protocol.HostStatus) {
				st.Connected, st.Listed, st.Error, st.Reconnecting = false, false, msg, false
			})
		case !protocol.Has(c.Hello.Capabilities, protocol.CapStatus):
			c.Close()
			d.setHostStatus(ctx, mh, func(st *protocol.HostStatus) {
				st.Connected, st.Listed, st.Error, st.Reconnecting = false, false, "daemon "+c.Hello.Version+" has no status capability", false
			})
		default:
			hello := c.Hello
			d.setHostStatus(ctx, mh, func(st *protocol.HostStatus) {
				st.Connected, st.Listed, st.Error, st.Reconnecting = true, false, "", false
				st.EnvironmentID, st.Version, st.Capabilities = hello.EnvironmentID, hello.Version, hello.Capabilities
			})
			backoff = d.cfg.ReconnectMin
			stop := c.CloseOnDone(ctx)
			if err := c.Write(protocol.Message{Type: protocol.TypeSubscribe}); err == nil {
				for {
					msg, err := c.Read()
					if err != nil {
						break
					}
					d.applyRemote(ctx, mh, msg)
				}
			}
			stop()
			c.Close()
			// A connection that was up has dropped: the record says so
			// and that the next dial is coming, so a client that needs
			// the host waits for it rather than failing on a daemon
			// restarting. The dial's outcome replaces both.
			msg := "disconnected"
			if diag := c.Diag(); diag != "" {
				msg += ": " + diag
			}
			d.setHostStatus(ctx, mh, func(st *protocol.HostStatus) {
				st.Connected, st.Listed, st.Error, st.Reconnecting = false, false, msg, true
			})
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, reconnectMax)
	}
}

// setHostStatus changes a host's record and publishes it. Nothing is
// changed once the follow's context is cancelled, which happens under
// d.mu: the state after a drop or an idle is the canceller's to set.
func (d *Daemon) setHostStatus(ctx context.Context, mh *mergedHost, change func(*protocol.HostStatus)) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if ctx.Err() != nil || d.mhosts[mh.status.Name] != mh {
		return
	}
	change(&mh.status)
	mh.status.Since = time.Now()
	st := mh.status
	d.mbroadcastLocked(protocol.Message{Type: protocol.TypeUpsert, HostStatus: &st})
}

// applyRemote forwards one message from a host into the merged stream. A
// snapshot replaces every record of the host's in one step, then marks it
// listed; an upsert or remove is forwarded with the merged sequence.
func (d *Daemon) applyRemote(ctx context.Context, mh *mergedHost, msg protocol.Message) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if ctx.Err() != nil || d.mhosts[mh.status.Name] != mh {
		return
	}
	switch msg.Type {
	case protocol.TypeSnapshot:
		seen := map[string]bool{}
		for i := range msg.Agents {
			a := msg.Agents[i]
			seen[a.ID] = true
			mh.agents[a.ID] = a
			d.mbroadcastLocked(protocol.Message{Type: protocol.TypeUpsert, Agent: &a})
		}
		for i := range msg.Worktrees {
			w := msg.Worktrees[i]
			seen[w.ID] = true
			mh.worktrees[w.ID] = w
			d.mbroadcastLocked(protocol.Message{Type: protocol.TypeUpsert, Worktree: &w})
		}
		for id := range mh.agents {
			if !seen[id] {
				delete(mh.agents, id)
				d.mbroadcastLocked(protocol.Message{Type: protocol.TypeRemove, AgentID: id})
			}
		}
		for id := range mh.worktrees {
			if !seen[id] {
				delete(mh.worktrees, id)
				d.mbroadcastLocked(protocol.Message{Type: protocol.TypeRemove, WorktreeID: id})
			}
		}
		mh.status.Listed = true
		mh.status.Since = time.Now()
		st := mh.status
		d.mbroadcastLocked(protocol.Message{Type: protocol.TypeUpsert, HostStatus: &st})
	case protocol.TypeUpsert:
		if msg.Agent != nil {
			mh.agents[msg.Agent.ID] = *msg.Agent
		}
		if msg.Worktree != nil {
			mh.worktrees[msg.Worktree.ID] = *msg.Worktree
		}
		if msg.Agent != nil || msg.Worktree != nil {
			d.mbroadcastLocked(protocol.Message{Type: protocol.TypeUpsert, Agent: msg.Agent, Worktree: msg.Worktree})
		}
	case protocol.TypeRemove:
		if msg.AgentID != "" {
			delete(mh.agents, msg.AgentID)
		}
		if msg.WorktreeID != "" {
			delete(mh.worktrees, msg.WorktreeID)
		}
		if msg.AgentID != "" || msg.WorktreeID != "" {
			d.mbroadcastLocked(protocol.Message{Type: protocol.TypeRemove, AgentID: msg.AgentID, WorktreeID: msg.WorktreeID})
		}
	}
}

// listSessions runs the configured listing; a daemon without one has no
// sessions. Called with d.subMu held.
func (d *Daemon) listSessions(ctx context.Context) ([]protocol.Session, error) {
	if d.cfg.Sessions == nil {
		return nil, nil
	}
	return d.cfg.Sessions(ctx)
}

// runSessions lists the local sessions once an interval while there is a
// merged subscriber. This is its own poll, not a rider on the status
// poll: the daemon may not observe the default server at all, and the
// workspace sessions are there regardless.
func (d *Daemon) runSessions(ctx context.Context) {
	t := time.NewTicker(d.cfg.SessionInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		d.mu.Lock()
		n := len(d.msubs)
		d.mu.Unlock()
		if n == 0 {
			continue
		}
		d.subMu.Lock()
		recs, err := d.listSessions(ctx)
		if ctx.Err() == nil {
			d.mu.Lock()
			d.applySessionsLocked(recs, err)
			d.mu.Unlock()
		}
		d.subMu.Unlock()
	}
}

// applySessionsLocked publishes the difference between the last listing
// and this one. A listing that fails is an unavailable observation: the
// records are kept, and the failure is published once, as its recovery
// is, so a subscriber knows its sessions are the last listed rather than
// the current ones.
func (d *Daemon) applySessionsLocked(recs []protocol.Session, err error) {
	if err != nil {
		if msg := err.Error(); msg != d.sessionsErr {
			d.cfg.Logger.Printf("sessions: %v", err)
			d.sessionsErr = msg
			d.mbroadcastLocked(protocol.Message{Type: protocol.TypeUpsert, SessionsError: msg})
		}
		return
	}
	if d.sessionsErr != "" {
		d.cfg.Logger.Printf("sessions: listed again")
		d.sessionsErr = ""
		d.mbroadcastLocked(protocol.Message{Type: protocol.TypeUpsert, SessionsListed: true})
	}
	seen := map[string]bool{}
	for _, s := range recs {
		seen[s.Name] = true
		if prev, had := d.msessions[s.Name]; had && prev == s {
			continue
		}
		d.msessions[s.Name] = s
		s := s
		d.mbroadcastLocked(protocol.Message{Type: protocol.TypeUpsert, LocalSession: &s})
	}
	for name := range d.msessions {
		if !seen[name] {
			delete(d.msessions, name)
			d.mbroadcastLocked(protocol.Message{Type: protocol.TypeRemove, LocalSessionName: name})
		}
	}
}

// logOnce logs a message once per change of its text, keeping the text in
// last so a repeating failure is one line.
func (d *Daemon) logOnce(last *string, format string, err error) {
	if msg := err.Error(); msg != *last {
		d.cfg.Logger.Printf(format, err)
		*last = msg
	}
}
