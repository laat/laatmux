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
	"github.com/laat/laatmux/internal/tmux"
	"github.com/laat/laatmux/internal/workspace"
)

// merged is the client-side merge of every host's stream. Host connectivity
// is a separate axis from agent state and lives here, not in the records.
type merged struct {
	mu        sync.Mutex
	agents    map[string]protocol.Agent    // by agent id
	worktrees map[string]protocol.Worktree // by worktree id
	hosts     map[string]hostState         // by host name
	byHost    map[string]string            // agent or worktree id -> host name
	change    chan struct{}
}

type hostState struct {
	Connected bool
	Error     string
	Version   string
	EnvID     string
	Since     time.Time
	// Listed is set once the host's snapshot has arrived. Until then its
	// records are unknown, not absent, and nothing of its is stale.
	Listed bool
}

func newMerged() *merged {
	return &merged{agents: map[string]protocol.Agent{}, worktrees: map[string]protocol.Worktree{}, hosts: map[string]hostState{}, byHost: map[string]string{}, change: make(chan struct{}, 1)}
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
			m.setHost(h.Name, hostState{Error: err.Error()})
		case !protocol.Has(c.Hello.Capabilities, protocol.CapStatus):
			c.Close()
			m.setHost(h.Name, hostState{Error: "daemon " + c.Hello.Version + " has no status capability"})
		default:
			m.setHost(h.Name, hostState{Connected: true, Version: c.Hello.Version, EnvID: c.Hello.EnvironmentID})
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
			m.setHost(h.Name, hostState{Error: "disconnected"})
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		if backoff < 30*time.Second {
			backoff *= 2
		}
	}
}

func activityRank(a protocol.Activity) int {
	switch a {
	case protocol.Blocked:
		return 0
	case protocol.Working:
		return 1
	case protocol.Idle:
		return 2
	default:
		return 3
	}
}

// row is one line of the listing: a worktree with or without its agent,
// or an agent with no worktree.
type row struct {
	host     string
	name     string // <repo>/<branch>, or the agent's session
	worktree *protocol.Worktree
	agent    *protocol.Agent
	settled  bool
}

func (r row) rank() int {
	if r.agent == nil {
		return 4
	}
	return activityRank(r.agent.Activity)
}

// rows joins each host's worktrees with its agents: a worktree pairs with
// the agent on the managed server in the session the record names. What
// is left over is listed on its own: a worktree without an agent, which is
// the state after the agent exits or when the worktree was made by hand,
// and an agent without a worktree. Settled rows come from the local
// sessions. Called with m.mu held.
func (m *merged) rows(locals []workspace.Local) (main, settled []row) {
	settledKeys := map[string]bool{}
	for _, l := range locals {
		if l.Workspace() && l.Settled {
			settledKeys[l.Key] = true
		}
	}
	bySession := map[string]*protocol.Agent{} // host + managed session -> agent
	for id := range m.agents {
		a := m.agents[id]
		if serverOf(a) == tmux.LaatmuxServer.Label() {
			bySession[m.byHost[id]+"\x00"+a.Session] = &a
		}
	}
	used := map[*protocol.Agent]bool{}
	var rows []row
	for id := range m.worktrees {
		w := m.worktrees[id]
		host := m.byHost[id]
		r := row{host: host, worktree: &w, settled: settledKeys[workspace.Key(w.EnvironmentID, w.Root)]}
		if w.Branch == "" {
			r.name = w.Repo + " (detached) " + w.Root
		} else {
			r.name = w.Repo + "/" + w.Branch
		}
		if w.Session != "" {
			if a := bySession[host+"\x00"+w.Session]; a != nil {
				r.agent, used[a] = a, true
			}
		}
		rows = append(rows, r)
	}
	for _, a := range bySession {
		if !used[a] {
			rows = append(rows, row{host: m.byHost[a.ID], name: a.Session, agent: a})
		}
	}
	for id := range m.agents {
		a := m.agents[id]
		if serverOf(a) != tmux.LaatmuxServer.Label() {
			rows = append(rows, row{host: m.byHost[id], name: a.Session, agent: &a})
		}
	}
	sort.Slice(rows, func(i, j int) bool {
		ri, rj := rows[i].rank(), rows[j].rank()
		if ri != rj {
			return ri < rj
		}
		if rows[i].agent != nil && rows[j].agent != nil && !rows[i].agent.ActivityAt.Equal(rows[j].agent.ActivityAt) {
			return rows[i].agent.ActivityAt.After(rows[j].agent.ActivityAt)
		}
		if rows[i].host != rows[j].host {
			return rows[i].host < rows[j].host
		}
		return rows[i].name < rows[j].name
	})
	for _, r := range rows {
		if r.settled {
			settled = append(settled, r)
		} else {
			main = append(main, r)
		}
	}
	return main, settled
}

// stale lists local workspace sessions whose workspace no longer exists on
// its host: the worktree was removed by hand or from another machine. A
// host that is down, or whose snapshot has not arrived, cannot say, so
// its sessions are not stale. Called with m.mu held.
func (m *merged) stale(locals []workspace.Local) []workspace.Local {
	roots := map[string]bool{} // key
	for _, w := range m.worktrees {
		roots[workspace.Key(w.EnvironmentID, w.Root)] = true
	}
	up := map[string]bool{} // environment id
	for _, st := range m.hosts {
		if st.Connected && st.Listed && st.EnvID != "" {
			up[st.EnvID] = true
		}
	}
	var out []workspace.Local
	for _, l := range locals {
		if !l.Workspace() || roots[l.Key] {
			continue
		}
		if env, _ := workspace.SplitKey(l.Key); up[env] {
			out = append(out, l)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
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
	for _, n := range hostNames {
		st := m.hosts[n]
		if st.Connected {
			fmt.Fprintf(&b, "%s  connected  %s\n", n, st.Version)
		} else {
			fmt.Fprintf(&b, "%s  DOWN  %s\n", n, st.Error)
		}
	}
	main, settled := m.rows(locals)
	now := time.Now()
	if len(main) > 0 {
		b.WriteString("\n")
	}
	for _, r := range main {
		m.renderRow(&b, r, now)
	}
	if len(settled) > 0 {
		b.WriteString("\nsettled\n")
		for _, r := range settled {
			m.renderRow(&b, r, now)
		}
	}
	if stale := m.stale(locals); len(stale) > 0 {
		b.WriteString("\nstale\n")
		for _, l := range stale {
			_, root := workspace.SplitKey(l.Key)
			fmt.Fprintf(&b, "  %-40s no worktree %s on %s\n", l.Name, root, l.Host)
		}
	}
	return b.String()
}

// renderRow prints one line: activity mark and state, the agent, the
// name, where it is, and the agent's last change and title. A worktree
// without an agent, or without a session, says so; so does a managed
// agent with no worktree.
func (m *merged) renderRow(b *strings.Builder, r row, now time.Time) {
	hs := m.hosts[r.host]
	where := r.host
	note := ""
	if !hs.Connected {
		note += " (host down)"
	}
	if r.agent == nil {
		// A managed session with no identified agent, or no session at
		// all: the worktree was made by hand, or its session was killed.
		state := "no session"
		if r.worktree.Session != "" {
			state = "no agent"
		}
		fmt.Fprintf(b, "  %-15s %-32s @%s%s\n", state, r.name, where, note)
		return
	}
	a := r.agent
	mark := " "
	switch a.Activity {
	case protocol.Blocked:
		mark = "!"
	case protocol.Working:
		mark = "*"
	case protocol.Idle:
		mark = "-"
	}
	if a.Liveness == protocol.Gone {
		note = " (gone)" + note
	}
	if r.worktree == nil && a.Managed {
		note = " (no worktree)" + note
	}
	agent := a.Agent
	if agent == "" {
		agent = "shell"
	}
	title := strings.TrimSpace(a.Title)
	if len(title) > 48 {
		title = title[:48]
	}
	// Agents on the managed server are the common case and show the
	// host alone; anything else names its server, which is also what
	// jump --server takes.
	if srv := serverOf(*a); srv != tmux.LaatmuxServer.Label() {
		where += "/" + srv
	}
	fmt.Fprintf(b, "%s %-8s %-6s %-32s @%s%s  %s  %s\n", mark, a.Activity, agent, r.name, where, note, ago(now.Sub(a.ActivityAt)), title)
}

// serverOf is the agent's tmux server label. Daemons from before servers
// were carried in records only ever watched the managed server.
func serverOf(a protocol.Agent) string {
	if a.Server == "" {
		return tmux.LaatmuxServer.Label()
	}
	return a.Server
}

func ago(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%2ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%2dm", int(d.Minutes()))
	default:
		return fmt.Sprintf("%2dh", int(d.Hours()))
	}
}

func cmdLs(ctx context.Context, args []string) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	m := newMerged()
	var wg sync.WaitGroup
	for _, h := range cfg.Hosts {
		wg.Add(1)
		go func(h config.Host) {
			defer wg.Done()
			c, err := client.Dial(ctx, h.Host)
			if err != nil {
				m.setHost(h.Name, hostState{Error: err.Error()})
				return
			}
			defer c.Close()
			if !protocol.Has(c.Hello.Capabilities, protocol.CapStatus) {
				m.setHost(h.Name, hostState{Error: "daemon " + c.Hello.Version + " has no status capability"})
				return
			}
			m.setHost(h.Name, hostState{Connected: true, Version: c.Hello.Version, EnvID: c.Hello.EnvironmentID})
			sctx, cancel := context.WithTimeout(ctx, 20*time.Second)
			defer cancel()
			snap, err := c.Snapshot(sctx)
			if err != nil {
				m.setHost(h.Name, hostState{Error: err.Error()})
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
	for _, h := range cfg.Hosts {
		go m.follow(ctx, h.Host)
	}
	t := time.NewTicker(5 * time.Second) // refresh relative times
	defer t.Stop()
	for {
		// Settled and stale come from the local sessions, read on each
		// redraw so a settle from another pane shows on the next change.
		locals, _ := workspace.List(ctx)
		fmt.Print("\033[H\033[2J" + m.render(locals))
		select {
		case <-ctx.Done():
			return nil
		case <-m.change:
		case <-t.C:
		}
	}
}
