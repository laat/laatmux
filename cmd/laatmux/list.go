package main

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/laat/laatmux/internal/client"
	"github.com/laat/laatmux/internal/config"
	"github.com/laat/laatmux/internal/protocol"
	"github.com/laat/laatmux/internal/rows"
	"github.com/laat/laatmux/internal/tmux"
	"github.com/laat/laatmux/internal/workspace"
)

// merged is the client's view of every host's stream: fed from the local
// daemon's merged stream when it has one, else merged here from a
// connection per host. Host connectivity is a separate axis from agent
// state and lives here, not in the records.
type merged struct {
	mu        sync.Mutex
	agents    map[string]protocol.Agent    // by agent id
	worktrees map[string]protocol.Worktree // by worktree id
	hosts     map[string]hostState         // by host name
	byHost    map[string]string            // agent or worktree id -> host name
	// sessions are the local workspace sessions as the merged stream
	// publishes them; nil on the direct path, where the client lists
	// them itself. sessionsErr is the daemon's listing failure, if any.
	sessions    map[string]protocol.Session
	sessionsErr string
	// pendings are the relay's background adds, by id, and handoffs the
	// worktree ids retired records became, kept across resnapshots, so
	// a view can re-anchor a selection that was on a record.
	pendings map[string]protocol.Pending
	handoffs map[string]string
	// daemonErr says the local daemon's merged stream is down, on watch,
	// while it reconnects; the last state stays on screen.
	daemonErr string
	change    chan struct{}
}

type hostState struct {
	Local     bool // this machine, as the config says
	Connected bool
	Error     string
	// Reconnecting is that the error is a dropped connection the daemon
	// is dialling again, from the merged stream; the direct path has no
	// such state, its own backoff is in watch.
	Reconnecting bool
	Version      string
	EnvID        string
	Since        time.Time
	// Worktrees is the daemon's worktrees capability; without it a
	// snapshot carries no records and says nothing about worktrees.
	// Listed is set once the host's snapshot has arrived. Until both, its
	// records are unknown, not absent, and nothing of its is stale.
	Worktrees bool
	Listed    bool
	Caps      []string // the daemon's capabilities, from its hello
}

// ready reports whether a one-shot client can stop waiting on the host:
// its records are listed or it has failed. A host that is connecting,
// connected with its snapshot pending, or reconnecting after a drop, is
// neither: a daemon restarted for an upgrade is back within seconds,
// and the wait is bounded by the snapshot timeout as for a cold host.
func (h hostState) ready() bool { return h.Listed || (h.Error != "" && !h.Reconnecting) }

// down is the host's error as a header line says it: with the reconnect
// noted when one is under way.
func (h hostState) down() string {
	if h.Reconnecting {
		return h.Error + " (reconnecting)"
	}
	return h.Error
}

// fromStatus is the host record of the merged stream as this view holds
// it.
func fromStatus(st protocol.HostStatus) hostState {
	return hostState{Local: st.Local(), Connected: st.Connected, Error: st.Error, Reconnecting: st.Reconnecting, Version: st.Version, EnvID: st.EnvironmentID,
		Since: st.Since, Worktrees: protocol.Has(st.Capabilities, protocol.CapWorktrees), Listed: st.Listed, Caps: st.Capabilities}
}

func newMerged() *merged {
	return &merged{agents: map[string]protocol.Agent{}, worktrees: map[string]protocol.Worktree{}, hosts: map[string]hostState{}, byHost: map[string]string{},
		pendings: map[string]protocol.Pending{}, handoffs: map[string]string{}, change: make(chan struct{}, 1)}
}

func (m *merged) notify() {
	select {
	case m.change <- struct{}{}:
	default:
	}
}

func (m *merged) setHost(name string, st hostState) {
	m.mu.Lock()
	st.Since = time.Now()
	m.hosts[name] = st
	m.mu.Unlock()
	m.notify()
}

// setHostErr marks the host down with the error, keeping what is known
// of its identity: the environment id, version and capabilities from the
// last hello, so its cached records stay attributed to it while it is
// down, as the host record in the merged stream does.
func (m *merged) setHostErr(name string, local bool, msg string) {
	m.mu.Lock()
	st := m.hosts[name]
	st.Local, st.Connected, st.Listed, st.Error, st.Reconnecting, st.Since = local, false, false, msg, false, time.Now()
	m.hosts[name] = st
	m.mu.Unlock()
	m.notify()
}

func (m *merged) apply(host string, msg protocol.Message) {
	m.mu.Lock()
	switch msg.Type {
	case protocol.TypeSnapshot:
		if st, ok := m.hosts[host]; ok {
			st.Listed = true
			m.hosts[host] = st
		}
		for id, h := range m.byHost {
			if h == host {
				delete(m.agents, id)
				delete(m.worktrees, id)
				delete(m.byHost, id)
			}
		}
		for _, a := range msg.Agents {
			m.agents[a.ID] = a
			m.byHost[a.ID] = host
		}
		for _, w := range msg.Worktrees {
			m.worktrees[w.ID] = w
			m.byHost[w.ID] = host
		}
	case protocol.TypeUpsert:
		if msg.Agent != nil {
			m.agents[msg.Agent.ID] = *msg.Agent
			m.byHost[msg.Agent.ID] = host
		}
		if msg.Worktree != nil {
			m.worktrees[msg.Worktree.ID] = *msg.Worktree
			m.byHost[msg.Worktree.ID] = host
		}
	case protocol.TypeRemove:
		if msg.AgentID != "" {
			delete(m.agents, msg.AgentID)
			delete(m.byHost, msg.AgentID)
		}
		if msg.WorktreeID != "" {
			delete(m.worktrees, msg.WorktreeID)
			delete(m.byHost, msg.WorktreeID)
		}
	}
	m.mu.Unlock()
	m.notify()
}

// follow keeps one host subscribed, reconnecting with backoff. Cached
// agents stay visible while disconnected; the host row says so.
func (m *merged) follow(ctx context.Context, h client.Host) {
	backoff := time.Second
	for ctx.Err() == nil {
		c, err := client.Dial(ctx, h)
		switch {
		case err != nil:
			m.setHostErr(h.Name, h.Local(), err.Error())
		case !protocol.Has(c.Hello.Capabilities, protocol.CapStatus):
			c.Close()
			m.setHostErr(h.Name, h.Local(), "daemon "+c.Hello.Version+" has no status capability")
		default:
			m.setHost(h.Name, hostState{Local: h.Local(), Connected: true, Version: c.Hello.Version, EnvID: c.Hello.EnvironmentID, Worktrees: protocol.Has(c.Hello.Capabilities, protocol.CapWorktrees)})
			backoff = time.Second
			stop := c.CloseOnDone(ctx)
			if err := c.Write(protocol.Message{Type: protocol.TypeSubscribe}); err == nil {
				for {
					msg, err := c.Read()
					if err != nil {
						break
					}
					m.apply(h.Name, msg)
				}
			}
			stop()
			c.Close()
			// Closing reaps ssh, so its stderr is complete and says
			// why, where the protocol only saw EOF.
			msg := "disconnected"
			if d := c.Diag(); d != "" {
				msg += ": " + d
			}
			m.setHostErr(h.Name, h.Local(), msg)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, followBackoffMax)
	}
}

// input is the rows package's view of the merged state, with the local
// sessions and the viewer's session. Called with m.mu held.
func (m *merged) input(locals []workspace.Local, current string) rows.Input {
	in := rows.Input{Locals: locals, Current: current}
	for name, st := range m.hosts {
		in.Hosts = append(in.Hosts, rows.Host{Name: name, Local: st.Local, EnvironmentID: st.EnvID,
			Connected: st.Connected, Listed: st.Listed, Worktrees: st.Worktrees, Error: st.Error})
	}
	// The rows package attributes records to hosts by environment id,
	// which every host that has answered a hello has, connected or not.
	for _, a := range m.agents {
		in.Agents = append(in.Agents, a)
	}
	for _, w := range m.worktrees {
		in.Worktrees = append(in.Worktrees, w)
	}
	return in
}

// stale lists local workspace sessions whose workspace no longer exists on
// its host: the worktree was removed by hand or from another machine. A
// host that is down, whose snapshot has not arrived, or whose daemon does
// not publish worktrees cannot say, so its sessions are not stale. Called
// with m.mu held.
func (m *merged) stale(locals []workspace.Local) []workspace.Local {
	var out []workspace.Local
	for _, r := range rows.Build(m.input(locals, "")).Stale {
		out = append(out, *r.Local)
	}
	return out
}

func (m *merged) render(locals []workspace.Local) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	var b strings.Builder
	hostNames := make([]string, 0, len(m.hosts))
	for n := range m.hosts {
		hostNames = append(hostNames, n)
	}
	sort.Strings(hostNames)
	if m.daemonErr != "" {
		fmt.Fprintf(&b, "local daemon  DOWN  %s\n", m.daemonErr)
	}
	for _, n := range hostNames {
		st := m.hosts[n]
		switch {
		case st.Connected && st.Listed:
			fmt.Fprintf(&b, "%s  connected  %s\n", n, st.Version)
		case st.Connected:
			fmt.Fprintf(&b, "%s  connected  %s  (snapshot pending)\n", n, st.Version)
		case st.Error != "":
			fmt.Fprintf(&b, "%s  DOWN  %s\n", n, st.down())
		default:
			fmt.Fprintf(&b, "%s  connecting\n", n)
		}
	}
	rs := rows.Build(m.input(locals, ""))
	now := time.Now()
	if len(rs.Main) > 0 {
		b.WriteString("\n")
	}
	for _, r := range rs.Main {
		renderRow(&b, r, now)
	}
	if len(rs.Settled) > 0 {
		b.WriteString("\nsettled\n")
		for _, r := range rs.Settled {
			renderRow(&b, r, now)
		}
	}
	if len(rs.Stale) > 0 {
		b.WriteString("\nstale\n")
		for _, r := range rs.Stale {
			_, root := workspace.SplitKey(r.Local.Key)
			fmt.Fprintf(&b, "  %-40s no worktree %s on %s\n", r.Name, root, r.Host)
		}
	}
	if m.sessionsErr != "" {
		// An incomplete listing says so where the settled and stale
		// groups would be, rather than looking complete.
		fmt.Fprintf(&b, "\nlocal sessions not listed: %s\n", m.sessionsErr)
	}
	return b.String()
}

// renderRow prints one line: activity mark and state, the agent, the
// name, where it is, and the agent's last change and title. A worktree
// without an agent, or without a session, says so; so does a managed
// agent with no worktree.
func renderRow(b *strings.Builder, r rows.Row, now time.Time) {
	where := r.Host
	note := ""
	if r.HostDown {
		note += " (host down)"
	}
	if r.Agent == nil {
		// A managed session with no identified agent, or no session at
		// all: the worktree was made by hand, or its session was killed.
		fmt.Fprintf(b, "  %-15s %-32s @%s%s\n", r.State(), r.Name, where, note)
		return
	}
	a := r.Agent
	if a.Liveness == protocol.Gone {
		note = " (gone)" + note
	}
	if r.Worktree == nil && a.Managed {
		note = " (no worktree)" + note
	}
	title := strings.TrimSpace(a.Title)
	if len(title) > 48 {
		title = title[:48]
	}
	// Agents on the managed server are the common case and show the
	// host alone; anything else names its server, which is also what
	// jump --server takes.
	if srv := rows.Server(*a); srv != tmux.LaatmuxServer.Label() {
		where += "/" + srv
	}
	fmt.Fprintf(b, "%s %-8s %-6s %-32s @%s%s  %s  %s\n", r.Mark(), a.Activity, r.AgentName(), r.Name, where, note, rows.Ago(now.Sub(a.ActivityAt)), title)
}

func cmdLs(ctx context.Context, args []string) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	m := newMerged()
	// The local daemon merges the hosts' streams when it can; a daemon
	// without the capability is an older build still running, and each
	// host is dialled from here as before.
	if c, ok := dialMerged(ctx); ok {
		defer c.Close()
		pending, err := m.readMerged(ctx, c, snapshotTimeout, func(m *merged) bool { return len(m.pending()) == 0 })
		if err != nil {
			return err
		}
		m.timedOut(pending, snapshotTimeout)
		fmt.Print(m.render(m.locals()))
		return nil
	}
	var wg sync.WaitGroup
	for _, h := range cfg.Hosts {
		wg.Add(1)
		go func(h config.Host) {
			defer wg.Done()
			c, err := client.Dial(ctx, h.Host)
			if err != nil {
				m.setHost(h.Name, hostState{Local: h.Local(), Error: err.Error()})
				return
			}
			defer c.Close()
			if !protocol.Has(c.Hello.Capabilities, protocol.CapStatus) {
				m.setHost(h.Name, hostState{Local: h.Local(), Error: "daemon " + c.Hello.Version + " has no status capability"})
				return
			}
			m.setHost(h.Name, hostState{Local: h.Local(), Connected: true, Version: c.Hello.Version, EnvID: c.Hello.EnvironmentID, Worktrees: protocol.Has(c.Hello.Capabilities, protocol.CapWorktrees)})
			sctx, cancel := context.WithTimeout(ctx, 20*time.Second)
			defer cancel()
			snap, err := c.Snapshot(sctx)
			if err != nil {
				m.setHostErr(h.Name, h.Local(), err.Error())
				return
			}
			m.apply(h.Name, snap)
		}(h)
	}
	wg.Wait()
	locals, err := workspace.List(ctx)
	if err != nil {
		return err
	}
	fmt.Print(m.render(locals))
	return nil
}

func cmdWatch(ctx context.Context, args []string) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	m := newMerged()
	direct := true
	if c, ok := dialMerged(ctx); ok {
		direct = false
		go m.followMerged(ctx, c)
	} else {
		for _, h := range cfg.Hosts {
			go m.follow(ctx, h.Host)
		}
	}
	t := time.NewTicker(5 * time.Second) // refresh relative times
	defer t.Stop()
	for {
		// Settled and stale come from the local sessions: from the merged
		// stream, or on the direct path read on each redraw so a settle
		// from another pane shows on the next change.
		locals := m.locals()
		if direct {
			locals, _ = workspace.List(ctx)
		}
		fmt.Print("\033[H\033[2J" + m.render(locals))
		select {
		case <-ctx.Done():
			return nil
		case <-m.change:
		case <-t.C:
		}
	}
}
