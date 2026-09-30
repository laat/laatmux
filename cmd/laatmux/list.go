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
	// worktree ids retired records became, kept across resnapshots for
	// a day, so a view can re-anchor a selection
	// that was on a record.
	pendings map[string]protocol.Pending
	handoffs map[string]handoffSeen
	// daemonErr says the local daemon's merged stream is down, on watch,
	// while it reconnects; the last state stays on screen.
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
	labels func(source string) (string, bool)
	// attentions are the merging daemon's attention records, by agent
	// id; sidebar is the config's row order and stale settings.
	attentions map[string]protocol.Attention
	sidebar    config.Sidebar
	// branches are the merging daemon's PR and check records, by
	// source key and branch; githubErr why it cannot read GitHub.
	branches  map[protocol.BranchKey]protocol.BranchStatus
	githubErr string
	// panes and runs are the pane and run records of hosts with
	// attribution, by id, the tree's children beside the agents.
	panes map[string]protocol.Pane
	runs  map[string]protocol.Run
}

// configure takes what the rows need from the config: this machine's
// repository names, the sort order and the stale settings.
func (m *merged) configure(cfg config.Config) {
	m.labels = repoLabels(cfg)
	m.sidebar = cfg.Sidebar
}

// repoLabels is the config's names for its repositories, by source in
// any form config.SameSource takes as one.
func repoLabels(cfg config.Config) func(string) (string, bool) {
	names := map[string]string{}
	for _, r := range cfg.Repos {
		names[config.SourceKey(r.Source)] = r.Name
	}
	return func(source string) (string, bool) {
		name, ok := names[config.SourceKey(source)]
		return name, ok
	}
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
	// records are unknown, not absent, and nothing of its is orphaned.
	Worktrees bool
	Listed    bool
	Caps      []string // the daemon's capabilities, from its hello
	// Attribution is the daemon's attribution capability: its agent
	// records say which worktree they belong to.
	Attribution bool
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
		Since: st.Since, Worktrees: protocol.Has(st.Capabilities, protocol.CapWorktrees), Listed: st.Listed, Caps: st.Capabilities,
		Attribution: protocol.Has(st.Capabilities, protocol.CapAttribution)}
}

func newMerged() *merged {
	return &merged{agents: map[string]protocol.Agent{}, worktrees: map[string]protocol.Worktree{}, hosts: map[string]hostState{}, byHost: map[string]string{},
		pendings: map[string]protocol.Pending{}, handoffs: map[string]handoffSeen{}, change: make(chan struct{}, 1)}
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
				delete(m.panes, id)
				delete(m.runs, id)
				delete(m.byHost, id)
			}
		}
		if m.panes == nil {
			m.panes, m.runs = map[string]protocol.Pane{}, map[string]protocol.Run{}
		}
		for _, a := range msg.Agents {
			m.agents[a.ID] = a
			m.byHost[a.ID] = host
		}
		for _, w := range msg.Worktrees {
			m.worktrees[w.ID] = w
			m.byHost[w.ID] = host
		}
		for _, p := range msg.Panes {
			m.panes[p.ID] = p
			m.byHost[p.ID] = host
		}
		for _, r := range msg.Runs {
			m.runs[r.ID] = r
			m.byHost[r.ID] = host
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
		if msg.Pane != nil {
			if m.panes == nil {
				m.panes = map[string]protocol.Pane{}
			}
			m.panes[msg.Pane.ID] = *msg.Pane
			m.byHost[msg.Pane.ID] = host
		}
		if msg.Run != nil {
			if m.runs == nil {
				m.runs = map[string]protocol.Run{}
			}
			m.runs[msg.Run.ID] = *msg.Run
			m.byHost[msg.Run.ID] = host
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
		if msg.PaneRecordID != "" {
			delete(m.panes, msg.PaneRecordID)
			delete(m.byHost, msg.PaneRecordID)
		}
		if msg.RunID != "" {
			delete(m.runs, msg.RunID)
			delete(m.byHost, msg.RunID)
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
			m.setHost(h.Name, hostState{Local: h.Local(), Connected: true, Version: c.Hello.Version, EnvID: c.Hello.EnvironmentID, Worktrees: protocol.Has(c.Hello.Capabilities, protocol.CapWorktrees),
				Attribution: protocol.Has(c.Hello.Capabilities, protocol.CapAttribution)})
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
	after, dim, collapse := m.sidebar.Stale()
	in := rows.Input{Locals: locals, Current: current, Attention: m.attentions, Branches: m.branches, Now: time.Now(),
		StaleAfter: after, DimStale: dim, CollapseStale: collapse, Sort: m.sidebar.Sort}
	for name, st := range m.hosts {
		// A merging daemon older than attribution forwards agent records
		// without the field, whatever the host sends.
		in.Hosts = append(in.Hosts, rows.Host{Name: name, Local: st.Local, EnvironmentID: st.EnvID,
			Connected: st.Connected, Listed: st.Listed, Worktrees: st.Worktrees, Error: st.Error,
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

// orphaned lists local workspace sessions whose workspace no longer exists on
// its host: the worktree was removed by hand or from another machine. A
// host that is down, whose snapshot has not arrived, or whose daemon does
// not publish worktrees cannot say, so its sessions are not orphaned. Called
// with m.mu held.
func (m *merged) orphaned(locals []workspace.Local) []workspace.Local {
	var out []workspace.Local
	for _, r := range rows.Build(m.input(locals, "")).Orphaned {
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
	renderTree(&b, rows.Tree(m.input(locals, "")), time.Now())
	if m.sessionsErr != "" {
		// An incomplete listing says so where the settled and orphaned
		// groups would be, rather than looking complete.
		fmt.Fprintf(&b, "\nlocal sessions not listed: %s\n", m.sessionsErr)
	}
	return b.String()
}

// renderTree prints the tree: a repository per line, its worktrees with
// their host under it, and under each its agents, panes and runs; a
// task where its worktree will be; other sessions last.
func renderTree(b *strings.Builder, nodes []rows.Row, now time.Time) {
	for _, n := range nodes {
		switch n.Kind {
		case rows.KindRepo, rows.KindGroup:
			fmt.Fprintf(b, "\n%s\n", n.Name)
		case rows.KindWorktree, rows.KindTask:
			renderLine(b, n, now)
		case rows.KindAgent:
			if n.Depth == 1 {
				// A session in other sessions.
				fmt.Fprintf(b, "  %-36s %s\n", n.Name+" ("+where(n)+")", agentText(n, now))
				continue
			}
			fmt.Fprintf(b, "    %s\n", agentText(n, now))
		case rows.KindPane:
			cmd := n.Name
			if n.Pane != nil && n.Pane.Command != "" {
				cmd = "$ " + n.Pane.Command
			}
			fmt.Fprintf(b, "    %s\n", cmd)
		case rows.KindRun:
			fmt.Fprintf(b, "    ▶ %s  %s\n", n.Name, rows.Ago(now.Sub(n.Run.StartedAt)))
		}
	}
}

// where is a node's host, with the server for an agent observed off the
// managed server, as jump --server takes it, and a note when the host
// is down.
func where(n rows.Row) string {
	s := n.Host
	if a := n.Agent; a != nil && n.Kind == rows.KindAgent {
		if srv := rows.Server(*a); srv != tmux.LaatmuxServer.Label() {
			s += "/" + srv
		}
	}
	if n.HostDown {
		s += ", host down"
	}
	return s
}

// renderLine is a worktree or task line: its label, host, and what it
// is instead of stats, a task's state say.
func renderLine(b *strings.Builder, n rows.Row, now time.Time) {
	label, _ := n.Labels()
	if n.Orphaned {
		label = n.Name
	}
	note := ""
	switch {
	case n.Pending != nil:
		note = n.Mark() + " " + n.State()
		if d := n.Detail(); d != "" {
			note += "  " + d
		}
	case n.Orphaned:
		_, root := workspace.SplitKey(n.Local.Key)
		note = "worktree gone " + root
	case n.Worktree != nil && n.Children == 0:
		note = n.State()
	}
	if n.Settled {
		note = strings.TrimSpace(note + "  settled")
	}
	fmt.Fprintf(b, "%s\n", strings.TrimRight(fmt.Sprintf("  %-36s %s", label+" ("+where(n)+")", note), " "))
}

// agentText is an agent's mark, state, name, age and title.
func agentText(n rows.Row, now time.Time) string {
	a := n.Agent
	state := string(a.Activity)
	switch {
	case a.Liveness == protocol.Gone:
		state = "gone"
	case n.Done:
		state = "done"
	case n.Stale:
		state = "stale"
	}
	title := strings.TrimSpace(a.Title)
	if r := []rune(title); len(r) > 48 {
		// By rune: a cut in the middle of one prints as garbage.
		title = string(r[:48])
	}
	return fmt.Sprintf("%s %-8s %-6s %s  %s", n.Mark(), state, n.AgentName(), rows.Ago(now.Sub(a.ActivityAt)), title)
}

func cmdLs(ctx context.Context, args []string) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	m := newMerged()
	m.configure(cfg)
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
			m.setHost(h.Name, hostState{Local: h.Local(), Connected: true, Version: c.Hello.Version, EnvID: c.Hello.EnvironmentID, Worktrees: protocol.Has(c.Hello.Capabilities, protocol.CapWorktrees),
				Attribution: protocol.Has(c.Hello.Capabilities, protocol.CapAttribution)})
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
	m.configure(cfg)
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
		// Settled and orphaned come from the local sessions: from the merged
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
