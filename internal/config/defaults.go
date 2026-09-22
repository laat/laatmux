package config

import (
	"fmt"
	"strings"
)

// DefaultHost picks the host add uses. Deterministic or absent, never the
// first entry: the flag, else the last-used host for the repository, else
// the local host if it has repos and worktrees, else the only host that
// has them, else an error naming the candidates. A last-used host that no
// longer exists or can no longer add is passed over.
func (c Config) DefaultHost(flag, last string) (Host, error) {
	if flag != "" {
		h, ok := c.Find(flag)
		if !ok {
			return Host{}, fmt.Errorf("unknown host %q", flag)
		}
		return h, nil
	}
	if last != "" {
		if h, ok := c.Find(last); ok && h.CanAdd() {
			return h, nil
		}
	}
	if h, ok := c.Local(); ok && h.CanAdd() {
		return h, nil
	}
	var able []Host
	for _, h := range c.Hosts {
		if h.CanAdd() {
			able = append(able, h)
		}
	}
	switch len(able) {
	case 0:
		return Host{}, fmt.Errorf("no host has repos and worktrees configured")
	case 1:
		return able[0], nil
	}
	names := make([]string, len(able))
	for i, h := range able {
		names[i] = h.Name
	}
	return Host{}, fmt.Errorf("choose a host with --host: %s", strings.Join(names, ", "))
}

// DefaultAgent picks the agent add uses: the flag, else the last-used
// agent for the repository, else the only configured agent, else an error
// naming them. A last-used agent no longer configured is passed over.
func (c Config) DefaultAgent(flag, last string) (string, Agent, error) {
	if flag != "" {
		a, ok := c.Agents[flag]
		if !ok {
			return "", Agent{}, fmt.Errorf("unknown agent %q; configured: %s", flag, c.agentList())
		}
		return flag, a, nil
	}
	if last != "" {
		if a, ok := c.Agents[last]; ok {
			return last, a, nil
		}
	}
	switch len(c.Agents) {
	case 0:
		return "", Agent{}, fmt.Errorf("no agents configured")
	case 1:
		n := c.AgentNames()[0]
		return n, c.Agents[n], nil
	}
	return "", Agent{}, fmt.Errorf("choose an agent with --agent: %s", c.agentList())
}

func (c Config) agentList() string {
	if len(c.Agents) == 0 {
		return "none"
	}
	return strings.Join(c.AgentNames(), ", ")
}
