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
)

// merged is the client-side merge of every host's stream. Host connectivity
// is a separate axis from agent state and lives here, not in the records.
type merged struct {
	mu     sync.Mutex
	agents map[string]protocol.Agent // by agent id
	hosts  map[string]hostState      // by host name
	byHost map[string]string         // agent id -> host name
	change chan struct{}
}

type hostState struct {
	Connected bool
	Error     string
	Version   string
	EnvID     string
	Since     time.Time
}

func newMerged() *merged {
	return &merged{agents: map[string]protocol.Agent{}, hosts: map[string]hostState{}, byHost: map[string]string{}, change: make(chan struct{}, 1)}
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
		for id, h := range m.byHost {
			if h == host {
				delete(m.agents, id)
				delete(m.byHost, id)
			}
		}
		for _, a := range msg.Agents {
			m.agents[a.ID] = a
			m.byHost[a.ID] = host
		}
	case protocol.TypeUpsert:
		if msg.Agent != nil {
			m.agents[msg.Agent.ID] = *msg.Agent
			m.byHost[msg.Agent.ID] = host
		}
	case protocol.TypeRemove:
		delete(m.agents, msg.AgentID)
		delete(m.byHost, msg.AgentID)
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

func (m *merged) render() string {
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
	agents := make([]protocol.Agent, 0, len(m.agents))
	for _, a := range m.agents {
		agents = append(agents, a)
	}
	sort.Slice(agents, func(i, j int) bool {
		ri, rj := activityRank(agents[i].Activity), activityRank(agents[j].Activity)
		if ri != rj {
			return ri < rj
		}
		return agents[i].ActivityAt.After(agents[j].ActivityAt)
	})
	if len(agents) > 0 {
		b.WriteString("\n")
	}
	now := time.Now()
	for _, a := range agents {
		host := m.byHost[a.ID]
		mark := " "
		switch a.Activity {
		case protocol.Blocked:
			mark = "!"
		case protocol.Working:
			mark = "*"
		case protocol.Idle:
			mark = "-"
		}
		live := ""
		if a.Liveness == protocol.Gone {
			live = " (gone)"
		}
		hs := m.hosts[host]
		if !hs.Connected {
			live += " (host down)"
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
		where := host
		if srv := serverOf(a); srv != tmux.LaatmuxServer.Label() {
			where += "/" + srv
		}
		fmt.Fprintf(&b, "%s %-8s %-6s %-24s @%s%s  %s  %s\n", mark, a.Activity, agent, a.Session, where, live, ago(now.Sub(a.ActivityAt)), title)
	}
	return b.String()
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
	fmt.Print(m.render())
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
		fmt.Print("\033[H\033[2J" + m.render())
		select {
		case <-ctx.Done():
			return nil
		case <-m.change:
		case <-t.C:
		}
	}
}
