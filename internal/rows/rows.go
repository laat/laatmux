// Package rows builds the rows the listing, the sidebar and the dashboard
// show: the relay's pending tasks first, then each host's worktrees
// joined with its agents by the managed session the worktree record
// names, then agents with no worktree, then observed agents on other
// servers, then the local sessions whose worktree is gone. The three
// views draw the same rows; this is the one place the join is made.
package rows

import (
	"fmt"
	"sort"
	"strings"
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
	// Pendings are the relay's background adds that have not handed
	// over to their worktree rows.
	Pendings []protocol.Pending
	// Current is the local session the viewer is in, "" when none: the
	// session the sidebar pane sits in or the popup was opened from.
	Current string
}

// Row is one entry: a pending task, a worktree with or without its
// agent, an agent with no worktree, or a local session whose worktree
// is gone.
type Row struct {
	Host string // configured host name; "" when no host record claims the record
	Name string // <repo>/<branch>, the agent's session, or the stale session's name
	// Pending is the relay's record of a background add. Its row stands
	// for the worktree row with the same environment and root until the
	// record hands over, so it carries that row's worktree, agent and
	// local session, when there are any, for the jump.
	Pending *protocol.Pending
	// Removed is that a pending record's host is gone from the config;
	// Replaced that its host answers as another machine than the one
	// the task was accepted for, which the view sees in the host's
	// environment id before the relay, having stopped contacting the
	// host, records it.
	Removed  bool
	Replaced bool
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

// ID identifies the row across rebuilds: a pending task's command id,
// else the worktree's id, else the agent's, else the local session's
// name. A view keeps its selection on it while rows come and go.
func (r Row) ID() string {
	switch {
	case r.Pending != nil:
		return r.Pending.ID
	case r.Worktree != nil:
		return r.Worktree.ID
	case r.Agent != nil:
		return r.Agent.ID
	case r.Local != nil:
		return "session/" + r.Local.Name
	}
	return r.Name
}

// Alias is the id of the row a pending task becomes, the worktree row
// at its root, once the host has reported the root; "" otherwise. A
// view's selection on the task follows it there.
func (r Row) Alias() string {
	if r.Pending == nil {
		return ""
	}
	return r.Pending.WorktreeID()
}

// NeedsUser is a pending task that has stopped where only the user can
// move it: its host gone from the config or answering as another
// machine, the add failed, the prompt not delivered or its delivery
// unknown, or the worktree gone after the add. A running task, one
// delivering its prompt, and one done and awaiting the listing do not.
func (r Row) NeedsUser() bool {
	p := r.Pending
	switch {
	case p == nil:
		return false
	case r.Removed || r.Replaced || p.Mismatch != "":
		return true
	case !p.Done || p.AttemptOpen:
		return false
	}
	return !p.Complete() || p.Gone
}

// Rank is the row's sort group: pending tasks, then blocked, working,
// idle, unknown, then rows without a live agent.
func (r Row) Rank() int {
	if r.Pending != nil {
		return -1
	}
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
// idle, " " otherwise; a pending task is "!" when it needs the user and
// "*" while it runs.
func (r Row) Mark() string {
	if r.Pending != nil {
		if r.NeedsUser() {
			return "!"
		}
		return "*"
	}
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
// row is instead. "" for a row with one. A pending task says where the
// add is, whatever agent its worktree has.
func (r Row) State() string {
	switch {
	case r.Pending != nil:
		s, _ := r.pendingState()
		return s
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

// Detail is the line under a row's state: a pending task's progress
// detail or the reason it stopped; "" for other rows, whose line under
// is the pane title.
func (r Row) Detail() string {
	if r.Pending == nil {
		return ""
	}
	_, d := r.pendingState()
	return d
}

// pendingState is PendingState with what the rows know besides the
// record: the host's absence from the config, and a host that answers
// as another machine before the relay has said so.
func (r Row) pendingState() (string, string) {
	p := *r.Pending
	if r.Replaced && !r.Removed && p.Mismatch == "" {
		return "host replaced", r.Host + " answers as another machine than " + p.EnvironmentID
	}
	return PendingState(p, r.Removed)
}

// PendingState is where a pending task is, in a few words, and the
// detail or reason to go with it. removed is that the host is gone
// from the config, which is said first, since the task is dismissable
// then whatever else it says.
func PendingState(p protocol.Pending, removed bool) (state, detail string) {
	switch {
	case removed:
		return "host removed", p.Error
	case p.Mismatch != "":
		return "host replaced", p.Mismatch
	case p.Done && !p.OK && strings.HasPrefix(p.Error, outcomeUnknown):
		return outcomeUnknown, strings.TrimPrefix(strings.TrimPrefix(p.Error, outcomeUnknown), ": ")
	case p.Done && !p.OK && p.Stage != "":
		// The relay's error says the stage already.
		return "failed at " + p.Stage, strings.TrimPrefix(p.Error, "failed at "+p.Stage+": ")
	case p.Done && !p.OK:
		return "failed", p.Error
	case p.Done && p.Gone:
		return "done, worktree gone", ""
	case p.Done && p.AttemptOpen:
		return "delivering the prompt", fmt.Sprintf("attempt %d", p.Attempt)
	case p.Done && p.Prompt == protocol.DeliveryNotDelivered:
		return "prompt not delivered", firstOf(p.AttemptError, p.Error)
	case p.Done && p.Prompt == protocol.DeliveryUnknown:
		return "prompt delivery unknown", firstOf(p.AttemptError, p.Error)
	case p.Done && !p.Listed:
		return "done, awaiting the listing", p.ListingError
	case p.Done:
		return "done", ""
	case !p.Reachable && p.Unreachable != "":
		return "host unreachable, retrying", p.Unreachable
	case !p.Taken:
		return "submitted", ""
	case p.Stage == "":
		return "adding", p.Detail
	}
	return "adding: " + p.Stage, p.Detail
}

// stands is a task that may still become the worktree row at its root,
// and so stands for it: not one that failed, nor one whose worktree was
// gone after the add.
func stands(p protocol.Pending) bool {
	return !p.Gone && !(p.Done && !p.OK)
}

// outcomeUnknown is how the relay's error begins for an add whose
// outcome no daemon can say.
const outcomeUnknown = "outcome unknown"

func firstOf(s ...string) string {
	for _, v := range s {
		if v != "" {
			return v
		}
	}
	return ""
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
	// A pending task stands for the worktree row at its root until it
	// hands over: the two are joined by environment and root, never by
	// name, and the worktree row is not drawn while any task for it
	// stands, two tasks for one explicit branch included. A task that
	// can no longer become that row does not stand for it: one whose
	// worktree was gone after the add, whose row would hide a worktree
	// made again at the root, and one that failed.
	var pendings []Row
	byAlias := map[string][]int{}
	for i := range in.Pendings {
		p := &in.Pendings[i]
		h, configured := hosts[p.Host]
		r := Row{Host: p.Host, Name: p.Repo + "/" + p.Branch, Pending: p, Removed: !configured,
			Replaced: configured && h.EnvironmentID != "" && p.EnvironmentID != "" && h.EnvironmentID != p.EnvironmentID}
		if alias := r.Alias(); alias != "" && stands(*p) {
			byAlias[alias] = append(byAlias[alias], len(pendings))
		}
		pendings = append(pendings, r)
	}
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
		if idx := byAlias[w.ID]; len(idx) > 0 {
			for _, j := range idx {
				pendings[j].Worktree, pendings[j].Agent, pendings[j].Local = r.Worktree, r.Agent, r.Local
			}
			continue
		}
		rows = append(rows, r)
	}
	// The host lists the agent in the task's session before the
	// worktree that has it, and a jump makes the local session before
	// the listing too; the task takes both by what the host reported,
	// so the agent is not a row of its own meanwhile, the session not a
	// stale one, and the viewer's own row is followed.
	for i := range pendings {
		p := pendings[i].Pending
		if !stands(*p) {
			continue
		}
		if p.EnvironmentID != "" && p.Root != "" {
			key := workspace.Key(p.EnvironmentID, p.Root)
			seenKey[key] = true
			if l := byKey[key]; l != nil && pendings[i].Local == nil {
				pendings[i].Local = l
			}
		}
		if pendings[i].Agent != nil || p.EnvironmentID == "" || p.Session == "" {
			continue
		}
		// The session name alone could be a later session's that took
		// the name: the agent's pane must start at the task's root, as
		// the host's own join of a pane to a worktree has it.
		if a := bySession[p.EnvironmentID+"\x00"+p.Session]; a != nil && !used[a] && p.Root != "" && a.Cwd == p.Root {
			pendings[i].Agent, used[a] = a, true
			for j := range pendings {
				if q := pendings[j].Pending; j != i && pendings[j].Agent == nil && stands(*q) && q.EnvironmentID == p.EnvironmentID && q.Session == p.Session && q.Root == p.Root {
					pendings[j].Agent = a
				}
			}
		}
	}
	rows = append(rows, pendings...)
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
		if r.Pending != nil {
			// Not dim while it runs, dim once it needs the user, as a
			// stale row is; never settled away from the main group.
			r.Dim, r.Settled = r.NeedsUser(), false
		}
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
// host, then name. Pending tasks come first, the newest first, since a
// task is what the user just asked for. Stale rows sort by name alone,
// as ls lists them.
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
	if a.Pending != nil && b.Pending != nil {
		if !a.Pending.SubmittedAt.Equal(b.Pending.SubmittedAt) {
			return a.Pending.SubmittedAt.After(b.Pending.SubmittedAt)
		}
		return a.Pending.ID < b.Pending.ID
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
