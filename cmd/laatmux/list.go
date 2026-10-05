package main

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/laat/laatmux/internal/config"
	"github.com/laat/laatmux/internal/protocol"
	"github.com/laat/laatmux/internal/rows"
	"github.com/laat/laatmux/internal/tmux"
	"github.com/laat/laatmux/internal/workspace"
)

// merged is the client's view of every host's stream, fed from the
// local daemon's merged stream. Host connectivity is a separate axis
// from agent state and lives here, not in the records.
type merged struct {
	mu        sync.Mutex
	agents    map[string]protocol.Agent    // by agent id
	worktrees map[string]protocol.Worktree // by worktree id
	hosts     map[string]hostState         // by host name
	byHost    map[string]string            // agent or worktree id -> host name
	// sessions are the local workspace sessions as the merged stream
	// publishes them, nil before the first snapshot. sessionsErr is the
	// daemon's listing failure, if any.
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
		Worktrees: protocol.Has(st.Capabilities, protocol.CapWorktrees), Listed: st.Listed, Caps: st.Capabilities,
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
		// An incomplete listing says so after the tree, rather than
		// looking complete.
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
		if srv := a.Server; srv != tmux.LaatmuxServer.Label() {
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
	// The local daemon merges the hosts' streams; the dial starts it
	// when it is not running, and a daemon that cannot be started, or
	// one of an older build without the stream, is the error.
	c, err := dialMergedOrExplain(ctx)
	if err != nil {
		return err
	}
	defer c.Close()
	pending, err := m.readMerged(ctx, c, snapshotTimeout, func(m *merged) bool { return len(m.pending()) == 0 })
	if err != nil {
		return err
	}
	m.timedOut(pending, snapshotTimeout)
	fmt.Print(m.render(m.locals()))
	return nil
}

func cmdWatch(ctx context.Context, args []string) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	m := newMerged()
	m.configure(cfg)
	c, err := dialMergedOrExplain(ctx)
	if err != nil {
		return err
	}
	go m.followMerged(ctx, c)
	t := time.NewTicker(5 * time.Second) // refresh relative times
	defer t.Stop()
	for {
		// The local sessions, settled and orphaned among them, come
		// with the merged stream.
		fmt.Print("\033[H\033[2J" + m.render(m.locals()))
		select {
		case <-ctx.Done():
			return nil
		case <-m.change:
		case <-t.C:
		}
	}
}
