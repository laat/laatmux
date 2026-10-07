// Package merged is a client's copy of the local daemon's merged
// stream: every host's records, a host record per host, the relay's
// pending records and the local workspace sessions, kept current as the
// stream is applied. Host connectivity is a separate axis from agent
// state and lives in the host records, not in the agent and worktree
// records. Records are attributed to hosts through the environment id
// the host records carry: the daemon forwards records unchanged, and
// the host name is the client's to look up.
//
// A State is filled by Read, for a one-shot client, or Follow, for one
// that stays, and read through Status, one copy of what the reporters
// need taken under the lock. See internal/daemon/merge.go for the
// daemon's side.
package merged

import (
	"fmt"
	"maps"
	"sort"
	"sync"
	"time"

	"github.com/laat/laatmux/internal/config"
	"github.com/laat/laatmux/internal/protocol"
	"github.com/laat/laatmux/internal/rows"
	"github.com/laat/laatmux/internal/source"
	"github.com/laat/laatmux/internal/workspace"
)

// State is the merged stream applied. Every method takes the lock; a
// reporter reads a Status rather than the maps.
type State struct {
	mu        sync.Mutex
	agents    map[string]protocol.Agent    // by agent id
	worktrees map[string]protocol.Worktree // by worktree id
	hosts     map[string]Host              // by host name
	byHost    map[string]string            // agent or worktree id -> host name
	// sessions are the local workspace sessions as the merged stream
	// publishes them, nil before the first snapshot. sessionsErr is the
	// daemon's listing failure, if any.
	sessions    map[string]protocol.Session
	sessionsErr string
	// pendings are the relay's background adds, by id, and handoffs the
	// worktree ids retired records became, kept across resnapshots for
	// a day, so a view can re-anchor a selection that was on a record.
	pendings map[string]protocol.Pending
	handoffs map[string]handoffSeen
	// daemonErr says the local daemon's merged stream is down, on
	// Follow, while it reconnects; the last state stays on screen.
	daemonErr string
	change    chan struct{}
	// snapshotted is that a merged snapshot has been applied: a view
	// says it is loading until then.
	snapshotted bool
	// stripped is that the records come through a merging daemon
	// without the attribution capability, which drops the worktree of
	// every agent it forwards.
	stripped bool
	// labels is this machine's name for a repository by its source, for
	// the rows: a host labels a checkout its config does not list by
	// its directory. nil keeps the host's labels.
	labels func(src string) (string, bool)
	// attentions are the merging daemon's attention records, by agent
	// id; sidebar is the config's row order and stale settings.
	attentions map[string]protocol.Attention
	sidebar    config.Sidebar
	// branches are the merging daemon's PR and check records, by
	// source key and branch.
	branches map[protocol.BranchKey]protocol.BranchStatus
	// panes and runs are the pane and run records of hosts with
	// attribution, by id, the tree's children beside the agents.
	panes map[string]protocol.Pane
	runs  map[string]protocol.Run
}

// New is an empty state: no snapshot yet, nothing known.
func New() *State {
	return &State{agents: map[string]protocol.Agent{}, worktrees: map[string]protocol.Worktree{}, hosts: map[string]Host{}, byHost: map[string]string{},
		pendings: map[string]protocol.Pending{}, handoffs: map[string]handoffSeen{}, change: make(chan struct{}, 1)}
}

// Configure takes what the rows need from the config: this machine's
// repository names, the sort order and the stale settings.
func (m *State) Configure(cfg config.Config) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.labels = repoLabels(cfg)
	m.sidebar = cfg.Sidebar
}

// repoLabels is the config's names for its repositories, by source in
// any form source.Same takes as one.
func repoLabels(cfg config.Config) func(string) (string, bool) {
	names := map[string]string{}
	for _, r := range cfg.Repos {
		names[source.Key(r.Source)] = r.Name
	}
	return func(src string) (string, bool) {
		name, ok := names[source.Key(src)]
		return name, ok
	}
}

// Changed is signalled when the state changes: a message applied, the
// daemon's error set or cleared, or Notify called. One signal is kept
// while nobody is waiting.
func (m *State) Changed() <-chan struct{} { return m.change }

// Notify wakes whoever waits on Changed, for a change beside the
// stream, a command's progress say, that the same loop redraws on.
func (m *State) Notify() {
	select {
	case m.change <- struct{}{}:
	default:
	}
}

// Host is a host record as the client holds it.
type Host struct {
	Name      string
	Local     bool // this machine, as the config says
	Connected bool
	Error     string
	// Reconnecting is that the error is a dropped connection the daemon
	// is dialling again.
	Reconnecting bool
	Version      string
	EnvID        string
	// Worktrees is the daemon's worktrees capability; without it a
	// snapshot carries no records and says nothing about worktrees.
	// Listed is set once the host's snapshot has arrived. Until both, its
	// records are unknown, not absent, and nothing of its is orphaned.
	Worktrees bool
	Listed    bool
	Caps      []string // the daemon's capabilities, from its hello
	// Attribution is the daemon's attribution capability: its agent
	// records say which worktree they belong to.
	Attribution bool
}

// fromStatus is the host record of the merged stream as this state
// holds it.
func fromStatus(st protocol.HostStatus) Host {
	return Host{Name: st.Name, Local: st.Local(), Connected: st.Connected, Error: st.Error, Reconnecting: st.Reconnecting, Version: st.Version, EnvID: st.EnvironmentID,
		Worktrees: protocol.Has(st.Capabilities, protocol.CapWorktrees), Listed: st.Listed, Caps: st.Capabilities,
		Attribution: protocol.Has(st.Capabilities, protocol.CapAttribution)}
}

// ready reports whether a one-shot client can stop waiting on the host:
// its records are listed or it has failed. A host that is connecting,
// connected with its snapshot pending, or reconnecting after a drop, is
// neither: a daemon restarted for an upgrade is back within seconds,
// and the wait is bounded by the snapshot timeout as for a cold host.
func (h Host) ready() bool { return h.Listed || (h.Error != "" && !h.Reconnecting) }

// Down is the host's error as a header line says it: with the reconnect
// noted when one is under way.
func (h Host) Down() string {
	if h.Reconnecting {
		return h.Error + " (reconnecting)"
	}
	return h.Error
}

// localSession is a local session as the daemon published it, its key
// decoded: a daemon of an earlier build publishes the key as tmux has
// it stored, and a session this build made for a root that is stored
// encoded would not match its worktree's key. A key a daemon of this
// build publishes is decoded already, and reads as it is.
func localSession(s protocol.Session) protocol.Session {
	s.Key = workspace.DecodeKey(s.Key)
	return s
}

// Apply applies one message of the merged stream. A snapshot replaces
// everything; records are attributed to hosts through the environment
// id the host records carry.
func (m *State) Apply(msg protocol.Message) {
	m.mu.Lock()
	switch msg.Type {
	case protocol.TypeSnapshot:
		m.snapshotted = true
		m.agents, m.worktrees = map[string]protocol.Agent{}, map[string]protocol.Worktree{}
		m.hosts, m.byHost = map[string]Host{}, map[string]string{}
		m.sessions = map[string]protocol.Session{}
		m.sessionsErr = msg.SessionsError
		m.panes, m.runs = map[string]protocol.Pane{}, map[string]protocol.Run{}
		for _, p := range msg.Panes {
			m.panes[p.ID] = p
		}
		for _, r := range msg.Runs {
			m.runs[r.ID] = r
		}
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
			m.sessions[s.Name] = localSession(s)
		}
		// The handoffs merge into what is known; a view may hold an
		// anchor whose handoff the daemon has dropped since.
		m.pendings = map[string]protocol.Pending{}
		for _, p := range msg.Pendings {
			m.pendings[p.ID] = p
		}
		for _, h := range msg.Handoffs {
			m.handoffLocked(h.ID, h.ReplacedBy)
		}
		m.pruneHandoffsLocked()
		m.attentions = map[string]protocol.Attention{}
		for _, a := range msg.Attentions {
			m.attentions[a.AgentID] = a
		}
		m.branches = map[protocol.BranchKey]protocol.BranchStatus{}
		for _, b := range msg.BranchStatuses {
			m.branches[b.BranchKey] = b
		}
	case protocol.TypeUpsert:
		if b := msg.BranchStatus; b != nil {
			if m.branches == nil {
				m.branches = map[protocol.BranchKey]protocol.BranchStatus{}
			}
			m.branches[b.BranchKey] = *b
		}
		if a := msg.Attention; a != nil {
			if m.attentions == nil {
				m.attentions = map[string]protocol.Attention{}
			}
			m.attentions[a.AgentID] = *a
		}
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
		if p := msg.Pane; p != nil {
			if m.panes == nil {
				m.panes = map[string]protocol.Pane{}
			}
			m.panes[p.ID] = *p
		}
		if r := msg.Run; r != nil {
			if m.runs == nil {
				m.runs = map[string]protocol.Run{}
			}
			m.runs[r.ID] = *r
		}
		if s := msg.LocalSession; s != nil {
			if m.sessions == nil {
				m.sessions = map[string]protocol.Session{}
			}
			m.sessions[s.Name] = localSession(*s)
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
		if msg.AttentionID != "" {
			delete(m.attentions, msg.AttentionID)
		}
		if k := msg.BranchStatusKey; k != nil {
			delete(m.branches, *k)
		}
		if msg.PendingID != "" {
			delete(m.pendings, msg.PendingID)
			if msg.ReplacedBy != "" {
				m.handoffLocked(msg.PendingID, msg.ReplacedBy)
			}
		}
		if msg.HostName != "" {
			env := m.hosts[msg.HostName].EnvID
			delete(m.hosts, msg.HostName)
			for id, h := range m.byHost {
				if h == msg.HostName {
					delete(m.agents, id)
					delete(m.worktrees, id)
					delete(m.byHost, id)
				}
			}
			for id, p := range m.panes {
				if env != "" && p.EnvironmentID == env {
					delete(m.panes, id)
				}
			}
			for id, r := range m.runs {
				if env != "" && r.EnvironmentID == env {
					delete(m.runs, id)
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
		if msg.PaneRecordID != "" {
			delete(m.panes, msg.PaneRecordID)
		}
		if msg.RunID != "" {
			delete(m.runs, msg.RunID)
		}
		if msg.LocalSessionName != "" {
			delete(m.sessions, msg.LocalSessionName)
		}
	}
	m.mu.Unlock()
	m.Notify()
}

// handoffSeen is a handoff as the client keeps it: the worktree id and
// when the client first saw it. The stream carries no age, so the day
// is counted from the first sight, which is no earlier than the
// retirement.
type handoffSeen struct {
	to string
	at time.Time
}

// handoffRetention is how long a handoff is kept for re-anchoring a
// selection, a day, though the daemon keeps a retired record for its
// worktree's life. Past it an anchor on the record is not found, and the
// selection is cleared rather than moved to a worktree id that may have
// been reused. A variable for tests.
var handoffRetention = 24 * time.Hour

// handoffLocked records a handoff, keeping the time it was first seen.
func (m *State) handoffLocked(id, to string) {
	if m.handoffs == nil {
		m.handoffs = map[string]handoffSeen{}
	}
	if h, ok := m.handoffs[id]; ok && h.to == to {
		return
	}
	m.handoffs[id] = handoffSeen{to: to, at: time.Now()}
}

// pruneHandoffsLocked drops the handoffs older than the retention.
func (m *State) pruneHandoffsLocked() {
	for id, h := range m.handoffs {
		if time.Since(h.at) > handoffRetention {
			delete(m.handoffs, id)
		}
	}
}

// HostOf is the configured host with the environment id, "" when none
// has answered a hello with it.
func (m *State) HostOf(envID string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.hostOfLocked(envID)
}

// hostOfLocked maps a record's environment id to the configured host
// with it, the first by name when two share one.
func (m *State) hostOfLocked(envID string) string {
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

// HostCaps is a host's cached daemon capabilities, ok when the host has
// answered a hello.
func (m *State) HostCaps(name string) ([]string, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	st, ok := m.hosts[name]
	if !ok || st.EnvID == "" {
		return nil, false
	}
	return st.Caps, true
}

// Waiting lists the hosts a one-shot client is still waiting on, those
// neither listed nor failed, sorted.
func (m *State) Waiting() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []string
	for n, st := range m.hosts {
		if !st.ready() {
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return out
}

// TimedOut marks hosts a one-shot client gave up waiting on, so the
// listing says which hosts it is not complete for. The wait is over, so
// a reconnect the host was in is no longer something to wait on, and the
// state is terminal.
func (m *State) TimedOut(names []string, wait time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, n := range names {
		st := m.hosts[n]
		st.Connected, st.Listed, st.Reconnecting = false, false, false
		st.Error = fmt.Sprintf("no snapshot after %s", wait)
		m.hosts[n] = st
	}
}

// localsLocked is the local sessions as the merged stream last
// published them, by name.
func (m *State) localsLocked() []protocol.Session {
	out := make([]protocol.Session, 0, len(m.sessions))
	for _, s := range m.sessions {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// HostSnapshot is one host's part of the merged state as a direct dial
// of the host would have fetched it: a hello built from the host record
// and a snapshot of its records. Not ok when the host is not in the
// stream; an error when the host is down, as the direct dial would have
// failed. A host still reconnecting when the caller stopped waiting is
// down with its error as well; the caller names it as still waited on
// first.
func (m *State) HostSnapshot(name string) (hello, snap protocol.Message, ok bool, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	st, ok := m.hosts[name]
	if !ok {
		return hello, snap, false, nil
	}
	if st.Error != "" {
		return hello, snap, true, fmt.Errorf("%s: %s", name, st.Down())
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

// Status is the state as a reporter reads it, one copy taken under the
// lock: ls's listing, the dashboard's fill and tasks's report are
// functions of it. Its maps and slices are the reporter's own; the
// records in them share what they point to (an agent's identity, a
// branch's PR and checks, a host's capabilities) with the state, which
// replaces those whole and never writes through them, and a reporter
// reads them.
type Status struct {
	// Loaded is that a snapshot has been applied; a view says it is
	// loading until then.
	Loaded bool
	// DaemonErr is the local daemon's merged stream being down, while
	// Follow reconnects; SessionsErr the daemon's session listing
	// failure.
	DaemonErr   string
	SessionsErr string
	Hosts       []Host // by name
	// Handoffs are the worktree ids retired records became, by record
	// id, the day's only.
	Handoffs map[string]string
	// ByHost is the host each agent or worktree record was attributed
	// to when it arrived, by record id.
	ByHost map[string]string
	// Input is the rows package's view of the records, the pending
	// records among them, with the local sessions and the viewer's
	// session, and the worktrees labelled by this machine's names where
	// Configure gave them.
	Input rows.Input
}

// Host is the host record by name.
func (s Status) Host(name string) (Host, bool) {
	for _, h := range s.Hosts {
		if h.Name == name {
			return h, true
		}
	}
	return Host{}, false
}

// Status takes the state for a reporter, with current the viewer's
// session. The handoffs older than the retention are dropped on the
// way.
func (m *State) Status(current string) Status {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.pruneHandoffsLocked()
	s := Status{Loaded: m.snapshotted, DaemonErr: m.daemonErr, SessionsErr: m.sessionsErr,
		Handoffs: make(map[string]string, len(m.handoffs)), ByHost: make(map[string]string, len(m.byHost))}
	for id, h := range m.handoffs {
		s.Handoffs[id] = h.to
	}
	for id, h := range m.byHost {
		s.ByHost[id] = h
	}
	names := make([]string, 0, len(m.hosts))
	for n := range m.hosts {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		s.Hosts = append(s.Hosts, m.hosts[n])
	}
	s.Input = m.inputLocked(current)
	return s
}

// inputLocked is the rows package's view of the state, with the local
// sessions and the viewer's session. Its maps and slices are copies:
// the rows are built after the lock is released, while the stream goes
// on being applied.
func (m *State) inputLocked(current string) rows.Input {
	after, dim, collapse := m.sidebar.Stale()
	in := rows.Input{Locals: m.localsLocked(), Current: current, Attention: maps.Clone(m.attentions), Branches: maps.Clone(m.branches), Now: time.Now(),
		StaleAfter: after, DimStale: dim, CollapseStale: collapse, Sort: m.sidebar.Sort}
	for name, st := range m.hosts {
		// A merging daemon older than attribution forwards agent records
		// without the field, whatever the host sends.
		in.Hosts = append(in.Hosts, rows.Host{Name: name, Local: st.Local, EnvironmentID: st.EnvID,
			Connected: st.Connected, Listed: st.Listed, Worktrees: st.Worktrees,
			Attribution: st.Attribution && !m.stripped})
	}
	// The rows package attributes records to hosts by environment id,
	// which every host that has answered a hello has, connected or not.
	for _, a := range m.agents {
		in.Agents = append(in.Agents, a)
	}
	for _, w := range m.worktrees {
		if m.labels != nil && w.Source != "" {
			if name, ok := m.labels(w.Source); ok {
				w.Repo = name
			}
		}
		in.Worktrees = append(in.Worktrees, w)
	}
	for _, p := range m.pendings {
		in.Pendings = append(in.Pendings, p)
	}
	for _, p := range m.panes {
		in.Panes = append(in.Panes, p)
	}
	for _, r := range m.runs {
		in.Runs = append(in.Runs, r)
	}
	return in
}
