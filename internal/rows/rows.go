// Package rows builds the rows the listing, the sidebar and the dashboard
// show: each host's worktrees joined with its agents by the managed
// session the worktree record names, then agents with no worktree, then
// observed agents on other servers, then the local sessions whose
// worktree is gone. The three views draw the same rows; this is the one
// place the join is made.
package rows

import (
	"fmt"
	"sort"
	"time"

	"github.com/laat/laatmux/internal/protocol"
	"github.com/laat/laatmux/internal/tmux"
	"github.com/laat/laatmux/internal/workspace"
)

// Host is one configured host as the rows need it: the connectivity axis
// and the environment id that attributes records to it.
type Host struct {
	Name          string
	Local         bool
	EnvironmentID string
	// Connected is a live connection with a completed hello; Listed is
	// that the records come from a snapshot of that connection;
	// Worktrees is that the daemon publishes worktree records. Absence
	// of a worktree is authoritative only with all three.
	Connected bool
	Listed    bool
	Worktrees bool
	Error     string
}

// Input is everything the rows are built from.
type Input struct {
	Hosts     []Host
	Agents    []protocol.Agent
	Worktrees []protocol.Worktree
	Locals    []workspace.Local
	// Current is the local session the viewer is in, "" when none: the
	// session the sidebar pane sits in or the popup was opened from.
	Current string
}

// Row is one entry: a worktree with or without its agent, an agent with
// no worktree, or a local session whose worktree is gone.
type Row struct {
	Host     string // configured host name; "" when no host record claims the record
	Name     string // <repo>/<branch>, the agent's session, or the stale session's name
	Worktree *protocol.Worktree
	Agent    *protocol.Agent
	// Local is the local session for the row, when there is one: the
	// workspace session by key, the plain attachment by tag, or the
	// observed session itself on the local default server.
	Local    *workspace.Local
	Settled  bool
	Stale    bool // a local workspace session with no worktree on a listed host
	Current  bool // the viewer's own session
	HostDown bool // the host is not connected
	// Dim is decided from measured axes only: no identified agent, an
	// agent that is gone, a host that is down, a stale session, a settled
	// workspace. Age is never a reason.
	Dim bool
}

// Rows are the three groups in display order.
type Rows struct {
	Main    []Row
	Settled []Row
	Stale   []Row
}

// All is every row in display order: main, settled, stale.
func (r Rows) All() []Row {
	out := make([]Row, 0, len(r.Main)+len(r.Settled)+len(r.Stale))
	out = append(out, r.Main...)
	out = append(out, r.Settled...)
	return append(out, r.Stale...)
}

// ID identifies the row across rebuilds: the worktree's id, else the
// agent's, else the local session's name. A view keeps its selection on
// it while rows come and go.
func (r Row) ID() string {
	switch {
	case r.Worktree != nil:
		return r.Worktree.ID
	case r.Agent != nil:
		return r.Agent.ID
	case r.Local != nil:
		return "session/" + r.Local.Name
	}
	return r.Name
}

// Rank is the row's sort group: blocked, working, idle, unknown, then
// rows without a live agent.
func (r Row) Rank() int {
	if r.Agent == nil {
		return 4
	}
	switch r.Agent.Activity {
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

// Mark is the activity mark ls prints: "!" blocked, "*" working, "-"
// idle, " " otherwise.
func (r Row) Mark() string {
	if r.Agent == nil {
		return " "
	}
	switch r.Agent.Activity {
	case protocol.Blocked:
		return "!"
	case protocol.Working:
		return "*"
	case protocol.Idle:
		return "-"
	}
	return " "
}

// State is the second-line text for a row without an agent: what the
// row is instead. "" for a row with one.
func (r Row) State() string {
	switch {
	case r.Stale:
		return "no worktree"
	case r.Agent != nil:
		return ""
	case r.Worktree != nil && r.Worktree.Session != "":
		return "no agent"
	default:
		return "no session"
	}
}

// AgentName is the agent's name, "shell" for an identified pane with no
// named agent, "" without an agent.
func (r Row) AgentName() string {
	if r.Agent == nil {
		return ""
	}
	if r.Agent.Agent == "" {
		return "shell"
	}
	return r.Agent.Agent
}

// Server is the agent's tmux server label. Daemons from before servers
// were carried in records only ever watched the managed server.
func Server(a protocol.Agent) string {
	if a.Server == "" {
		return tmux.LaatmuxServer.Label()
	}
	return a.Server
}

// Build makes the rows. Records are attributed to hosts through the
// environment id in the host records; the first host by name wins when
// two share one.
func Build(in Input) Rows {
	hosts := map[string]Host{}
	byEnv := map[string]string{}
	names := make([]string, 0, len(in.Hosts))
	for _, h := range in.Hosts {
		hosts[h.Name] = h
		names = append(names, h.Name)
	}
	sort.Strings(names)
	for i := len(names) - 1; i >= 0; i-- {
		if h := hosts[names[i]]; h.EnvironmentID != "" {
			byEnv[h.EnvironmentID] = h.Name
		}
	}
	byKey := map[string]*workspace.Local{}
	byAttach := map[string]*workspace.Local{}
	byName := map[string]*workspace.Local{}
	for i := range in.Locals {
		l := &in.Locals[i]
		byName[l.Name] = l
		if l.Workspace() {
			byKey[l.Key] = l
		} else if l.Attach != "" {
			byAttach[l.Attach] = l
		}
	}
	// The join is by environment id and managed session, never by host
	// name: two hosts that are down, or that no host record claims,
	// would otherwise share the empty name and pair the wrong agent.
	bySession := map[string]*protocol.Agent{} // environment id + managed session -> agent
	for i := range in.Agents {
		a := &in.Agents[i]
		if Server(*a) == tmux.LaatmuxServer.Label() {
			bySession[a.EnvironmentID+"\x00"+a.Session] = a
		}
	}
	used := map[*protocol.Agent]bool{}
	var rows []Row
	seenKey := map[string]bool{}
	for i := range in.Worktrees {
		w := &in.Worktrees[i]
		host := byEnv[w.EnvironmentID]
		r := Row{Host: host, Worktree: w}
		if w.Branch == "" {
			r.Name = w.Repo + " (detached) " + w.Root
		} else {
			r.Name = w.Repo + "/" + w.Branch
		}
		if w.Session != "" {
			if a := bySession[w.EnvironmentID+"\x00"+w.Session]; a != nil {
				r.Agent, used[a] = a, true
			}
		}
		key := workspace.Key(w.EnvironmentID, w.Root)
		seenKey[key] = true
		if l := byKey[key]; l != nil {
			r.Local, r.Settled = l, l.Settled
		}
		rows = append(rows, r)
	}
	for _, a := range bySession {
		if used[a] {
			continue
		}
		host := byEnv[a.EnvironmentID]
		r := Row{Host: host, Name: a.Session, Agent: a}
		if l := byAttach[host+"/"+a.Session]; l != nil {
			r.Local = l
		}
		rows = append(rows, r)
	}
	for i := range in.Agents {
		a := &in.Agents[i]
		if Server(*a) == tmux.LaatmuxServer.Label() {
			continue
		}
		host := byEnv[a.EnvironmentID]
		r := Row{Host: host, Name: a.Session, Agent: a}
		if h := hosts[host]; h.Local && Server(*a) == tmux.DefaultServer.Label() {
			// An observed session on this machine's own default server
			// is a local session, whatever tags it carries.
			if l := byName[a.Session]; l != nil {
				r.Local = l
			} else {
				r.Local = &workspace.Local{Name: a.Session}
			}
		}
		rows = append(rows, r)
	}
	// Stale: a local workspace session whose worktree is gone from a
	// host that can say so. A host that is down, whose snapshot has not
	// arrived, or whose daemon does not publish worktrees cannot, so its
	// sessions are not stale.
	up := map[string]bool{} // environment id
	for _, h := range in.Hosts {
		if h.Connected && h.Listed && h.Worktrees && h.EnvironmentID != "" {
			up[h.EnvironmentID] = true
		}
	}
	for i := range in.Locals {
		l := &in.Locals[i]
		if !l.Workspace() || seenKey[l.Key] {
			continue
		}
		if env, _ := workspace.SplitKey(l.Key); up[env] {
			// The host is the one that answers for the environment id
			// now, not the name the session was tagged with, which a
			// renamed host leaves behind.
			rows = append(rows, Row{Host: byEnv[env], Name: l.Name, Local: l, Stale: true, Settled: l.Settled})
		}
	}
	for i := range rows {
		r := &rows[i]
		h, known := hosts[r.Host]
		r.HostDown = !known || !h.Connected
		r.Current = in.Current != "" && r.Local != nil && r.Local.Name == in.Current
		r.Dim = r.Agent == nil || r.Agent.Liveness == protocol.Gone || r.HostDown || r.Stale || r.Settled
	}
	sort.SliceStable(rows, func(i, j int) bool { return less(rows[i], rows[j]) })
	var out Rows
	for _, r := range rows {
		switch {
		case r.Stale:
			out.Stale = append(out.Stale, r)
		case r.Settled:
			out.Settled = append(out.Settled, r)
		default:
			out.Main = append(out.Main, r)
		}
	}
	return out
}

// less is the sort order: rank, then most recent activity first, then
// host, then name. Stale rows sort by name alone, as ls lists them.
func less(a, b Row) bool {
	if a.Stale || b.Stale {
		if a.Stale != b.Stale {
			return !a.Stale
		}
		return a.Name < b.Name
	}
	ra, rb := a.Rank(), b.Rank()
	if ra != rb {
		return ra < rb
	}
	if a.Agent != nil && b.Agent != nil && !a.Agent.ActivityAt.Equal(b.Agent.ActivityAt) {
		return a.Agent.ActivityAt.After(b.Agent.ActivityAt)
	}
	if a.Host != b.Host {
		return a.Host < b.Host
	}
	return a.Name < b.Name
}

// Ago formats how long ago something happened, in the unit that fits,
// two digits wide.
func Ago(d time.Duration) string {
	switch {
	case d < 0:
		return " 0s"
	case d < time.Minute:
		return fmt.Sprintf("%2ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%2dm", int(d.Minutes()))
	default:
		return fmt.Sprintf("%2dh", int(d.Hours()))
	}
}
