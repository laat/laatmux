package main

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/laat/laatmux/internal/client"
	"github.com/laat/laatmux/internal/protocol"
	"github.com/laat/laatmux/internal/workspace"
)

// The merged stream, read from the local daemon. The daemon dials the
// hosts in the config and republishes one stream over the local socket
// with every host's records, a host record per host, and the local
// workspace sessions, so a client per window is one local connection
// rather than an ssh channel per host. See internal/daemon/merge.go.

// dialMerged connects to the local daemon when it has the merged
// capability. False means dial each host directly: the daemon could not
// be reached, or is an older build without the capability.
func dialMerged(ctx context.Context) (*client.Conn, bool) {
	c, err := client.Dial(ctx, client.Host{Name: "local"})
	if err != nil {
		return nil, false
	}
	if !protocol.Has(c.Hello.Capabilities, protocol.CapMerged) {
		c.Close()
		return nil, false
	}
	return c, true
}

// readMerged subscribes to the merged stream and applies it until ready
// holds or the wait is over, then returns the hosts that are still
// neither listed nor failed. The snapshot comes at once with what the
// daemon knows, which on a cold daemon is the host rows alone, and the
// hosts fill in as their connections come up.
func (m *merged) readMerged(ctx context.Context, c *client.Conn, wait time.Duration, ready func(*merged) bool) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	defer c.CloseOnDone(ctx)()
	if err := c.Write(protocol.Message{Type: protocol.TypeSubscribe, Merged: true}); err != nil {
		return nil, err
	}
	snapshot := false
	for {
		msg, err := c.Read()
		if err != nil {
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				if !snapshot {
					// Nothing arrived, not even the host rows: the
					// daemon is stuck before its first poll, or the
					// sessions listing hangs. An empty listing would
					// look complete.
					return nil, fmt.Errorf("local daemon: no snapshot after %s", wait)
				}
				break
			}
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return nil, fmt.Errorf("local daemon: %w", err)
		}
		if msg.Type == protocol.TypeError {
			return nil, fmt.Errorf("local daemon: %s", msg.Error)
		}
		if msg.Type == protocol.TypeSnapshot {
			snapshot = true
		}
		m.applyMerged(msg)
		m.mu.Lock()
		done := ready(m)
		m.mu.Unlock()
		if done {
			break
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.pending(), nil
}

// followMerged keeps the merged stream applied, reconnecting to the local
// daemon with backoff when it goes away, which a restart for an upgrade
// does. The last state stays on screen with the daemon row saying so.
func (m *merged) followMerged(ctx context.Context, c *client.Conn) {
	backoff := followBackoffMin
	for ctx.Err() == nil {
		var ok bool
		if c == nil {
			c, ok = dialMerged(ctx)
		} else {
			ok = true
		}
		if ok {
			// The backoff resets on a snapshot, not on a connection: a
			// daemon that answers the hello and then drops the
			// subscription every time is retried as slowly as one that
			// does not answer at all.
			stop := c.CloseOnDone(ctx)
			if err := c.Write(protocol.Message{Type: protocol.TypeSubscribe, Merged: true}); err == nil {
				for {
					msg, err := c.Read()
					if err != nil {
						break
					}
					if msg.Type == protocol.TypeError {
						m.setDaemonErr(msg.Error)
						break
					}
					if msg.Type == protocol.TypeSnapshot {
						backoff = followBackoffMin
					}
					m.setDaemonErr("")
					m.applyMerged(msg)
				}
			}
			stop()
			c.Close()
			c = nil
			if ctx.Err() == nil && m.daemonErrIs("") {
				m.setDaemonErr("disconnected; reconnecting")
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, followBackoffMax)
	}
}

// followBackoff is the wait between attempts to reach the local daemon's
// merged stream, doubling from the min to the max. A variable so a test
// can shorten it.
var (
	followBackoffMin = time.Second
	followBackoffMax = 30 * time.Second
)

func (m *merged) setDaemonErr(s string) {
	m.mu.Lock()
	changed := m.daemonErr != s
	m.daemonErr = s
	m.mu.Unlock()
	if changed {
		m.notify()
	}
}

func (m *merged) daemonErrIs(s string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.daemonErr == s
}

// applyMerged applies one message of the merged stream. A snapshot
// replaces everything; records are attributed to hosts through the
// environment id the host records carry.
func (m *merged) applyMerged(msg protocol.Message) {
	m.mu.Lock()
	switch msg.Type {
	case protocol.TypeSnapshot:
		m.agents, m.worktrees = map[string]protocol.Agent{}, map[string]protocol.Worktree{}
		m.hosts, m.byHost = map[string]hostState{}, map[string]string{}
		m.sessions = map[string]protocol.Session{}
		m.sessionsErr = msg.SessionsError
		for _, st := range msg.Hosts {
			m.hosts[st.Name] = fromStatus(st)
		}
		for _, a := range msg.Agents {
			m.agents[a.ID] = a
			m.byHost[a.ID] = m.hostOfLocked(a.EnvironmentID)
		}
		for _, w := range msg.Worktrees {
			m.worktrees[w.ID] = w
			m.byHost[w.ID] = m.hostOfLocked(w.EnvironmentID)
		}
		for _, s := range msg.Sessions {
			m.sessions[s.Name] = s
		}
		// The handoffs merge into what is known; a snapshot's list is
		// the last day's, and a view may hold an anchor older than that.
		m.pendings = map[string]protocol.Pending{}
		for _, p := range msg.Pendings {
			m.pendings[p.ID] = p
		}
		if m.handoffs == nil {
			m.handoffs = map[string]string{}
		}
		for _, h := range msg.Handoffs {
			m.handoffs[h.ID] = h.ReplacedBy
		}
	case protocol.TypeUpsert:
		if st := msg.HostStatus; st != nil {
			m.hosts[st.Name] = fromStatus(*st)
		}
		if a := msg.Agent; a != nil {
			m.agents[a.ID] = *a
			m.byHost[a.ID] = m.hostOfLocked(a.EnvironmentID)
		}
		if w := msg.Worktree; w != nil {
			m.worktrees[w.ID] = *w
			m.byHost[w.ID] = m.hostOfLocked(w.EnvironmentID)
		}
		if s := msg.LocalSession; s != nil {
			if m.sessions == nil {
				m.sessions = map[string]protocol.Session{}
			}
			m.sessions[s.Name] = *s
		}
		if msg.SessionsError != "" {
			m.sessionsErr = msg.SessionsError
		}
		if msg.SessionsListed {
			m.sessionsErr = ""
		}
		if p := msg.Pending; p != nil {
			if m.pendings == nil {
				m.pendings = map[string]protocol.Pending{}
			}
			m.pendings[p.ID] = *p
		}
	case protocol.TypeRemove:
		if msg.PendingID != "" {
			delete(m.pendings, msg.PendingID)
			if msg.ReplacedBy != "" {
				if m.handoffs == nil {
					m.handoffs = map[string]string{}
				}
				m.handoffs[msg.PendingID] = msg.ReplacedBy
			}
		}
		if msg.HostName != "" {
			delete(m.hosts, msg.HostName)
			for id, h := range m.byHost {
				if h == msg.HostName {
					delete(m.agents, id)
					delete(m.worktrees, id)
					delete(m.byHost, id)
				}
			}
		}
		if msg.AgentID != "" {
			delete(m.agents, msg.AgentID)
			delete(m.byHost, msg.AgentID)
		}
		if msg.WorktreeID != "" {
			delete(m.worktrees, msg.WorktreeID)
			delete(m.byHost, msg.WorktreeID)
		}
		if msg.LocalSessionName != "" {
			delete(m.sessions, msg.LocalSessionName)
		}
	}
	m.mu.Unlock()
	m.notify()
}

// hostOf is hostOfLocked under the lock: the configured host with the
// environment id, "" when none has answered a hello with it.
func (m *merged) hostOf(envID string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.hostOfLocked(envID)
}

// hostOfLocked maps a record's environment id to the configured host
// with it. The daemon forwards records unchanged; the name is the
// client's to look up.
func (m *merged) hostOfLocked(envID string) string {
	names := make([]string, 0, len(m.hosts))
	for n := range m.hosts {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		if st := m.hosts[n]; st.EnvID != "" && st.EnvID == envID {
			return n
		}
	}
	return ""
}

// pending lists the hosts that are neither listed nor failed, sorted.
// Called with m.mu held.
func (m *merged) pending() []string {
	var out []string
	for n, st := range m.hosts {
		if !st.ready() {
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return out
}

// timedOut marks hosts a one-shot client gave up waiting on, so the
// listing says which hosts it is not complete for. The wait is over, so
// a reconnect the host was in is no longer something to wait on, and the
// state is terminal.
func (m *merged) timedOut(names []string, wait time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, n := range names {
		st := m.hosts[n]
		st.Connected, st.Listed, st.Reconnecting = false, false, false
		st.Error = fmt.Sprintf("no snapshot after %s", wait)
		m.hosts[n] = st
	}
}

// locals are the local sessions as the merged stream last published
// them.
func (m *merged) locals() []workspace.Local {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.localsLocked()
}

// localsLocked is locals with m.mu held.
func (m *merged) localsLocked() []workspace.Local {
	out := make([]workspace.Local, 0, len(m.sessions))
	for _, s := range m.sessions {
		out = append(out, workspace.Local(s))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// hostSnapshot is one host's part of the merged state as the direct path
// would have fetched it: a hello built from the host record and a
// snapshot of its records. Not ok when the host is not in the stream; an
// error when the host is down, as the direct dial would have failed. A
// host still reconnecting when the caller stopped waiting is down with
// its error as well; the caller names it as pending first.
func (m *merged) hostSnapshot(name string) (hello, snap protocol.Message, ok bool, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	st, ok := m.hosts[name]
	if !ok {
		return hello, snap, false, nil
	}
	if st.Error != "" {
		return hello, snap, true, fmt.Errorf("%s: %s", name, st.down())
	}
	hello = protocol.Message{Type: protocol.TypeHello, Protocol: protocol.Version, EnvironmentID: st.EnvID, Version: st.Version, Host: name, Capabilities: st.Caps}
	snap.Type = protocol.TypeSnapshot
	for _, a := range m.agents {
		if m.byHost[a.ID] == name {
			snap.Agents = append(snap.Agents, a)
		}
	}
	for _, w := range m.worktrees {
		if m.byHost[w.ID] == name {
			snap.Worktrees = append(snap.Worktrees, w)
		}
	}
	return hello, snap, true, nil
}
