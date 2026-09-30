// Package rows builds the rows the listing, the sidebar and the dashboard
// show: the relay's pending tasks first, then each host's worktrees
// joined with its agents, by the worktree the host attributed each agent
// to or, from a host without attribution, by the managed session the
// worktree record names, then agents with no worktree, then observed agents on other
// servers, then the local sessions whose worktree is gone. The three
// views draw the same rows; this is the one place the join is made.
package rows

import (
	"fmt"
	"math"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/laat/laatmux/internal/config"
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
	// Attribution is that the host's agent records carry the worktree
	// they belong to, and reach the view with it: a worktree row takes
	// its agent by that, not by session name.
	Attribution bool
	Error       string
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
	// Panes and Runs are the pane and run records of hosts with
	// attribution, the tree's children beside the agents.
	Panes []protocol.Pane
	Runs  []protocol.Run
	// Attention is the merging daemon's attention records by agent id:
	// an idle agent whose finish is after the user's last visit is done.
	Attention map[string]protocol.Attention
	// Branches are the merging daemon's PR and check records, by source
	// key and branch: a worktree row shows its branch's.
	Branches map[protocol.BranchKey]protocol.BranchStatus
	// Now is the time stale is measured at; StaleAfter how long an agent
	// is idle before it is stale, 0 for never. DimStale draws a stale row
	// dim, CollapseStale folds it into the Stale group. Sort is the order
	// of the main group: priority, the default, recency or window.
	Now           time.Time
	StaleAfter    time.Duration
	DimStale      bool
	CollapseStale bool
	Sort          string
}

// Sort orders of the main group.
const (
	SortPriority = "priority"
	SortRecency  = "recency"
	SortWindow   = "window"
)

// Row is one entry: a pending task, a worktree with or without its
// agent, an agent with no worktree, or a local session whose worktree
// is gone.
type Row struct {
	Host string // configured host name; "" when no host record claims the record
	Name string // <repo>/<branch>, the agent's session, or the orphaned session's name
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
	Orphaned bool // a local workspace session with no worktree on a listed host
	// Done is an idle agent that went from working to idle since the
	// user last saw it; Stale an idle agent idle for longer than the
	// stale time. A done agent is never stale.
	Done  bool
	Stale bool
	// Branch is the PR and checks of the row's branch, nil when the
	// daemon has none.
	Branch *protocol.BranchStatus
	// The tree's fields: what the node is, its level, its id when it
	// has no record's, the pane or run record of a child, how many nodes
	// are under a foldable one, and the agent view's suffix for tiles
	// that share a label.
	Kind     Kind
	Depth    int
	Node     string
	Pane     *protocol.Pane
	Run      *protocol.Run
	Children int
	Suffix   string
	Current  bool // the viewer's own session
	HostDown bool // the host is not connected
	// Dim is no identified agent, an agent that is gone, a host that is
	// down, an orphaned session, a settled workspace whose agent does not
	// want the user, or a stale agent when stale rows are dimmed.
	Dim bool
}

// Rows are the groups in display order: the stale rows fold with the
// settled ones, in the view's collapsed group.
type Rows struct {
	Main     []Row
	Stale    []Row
	Settled  []Row
	Orphaned []Row
}

// All is every row in display order: main, stale, settled, orphaned.
func (r Rows) All() []Row {
	out := make([]Row, 0, len(r.Main)+len(r.Stale)+len(r.Settled)+len(r.Orphaned))
	out = append(out, r.Main...)
	out = append(out, r.Stale...)
	out = append(out, r.Settled...)
	return append(out, r.Orphaned...)
}

// Pressing is an agent that wants the user: blocked, or done. It is never
// stale, and stays in place in a settled workspace.
func (r Row) Pressing() bool {
	return r.Done || r.Agent != nil && r.Agent.Activity == protocol.Blocked && r.Agent.Liveness != protocol.Gone
}

// ID identifies the row across rebuilds: a pending task's command id,
// else the worktree's id, else the agent's, else the local session's
// name. A view keeps its selection on it while rows come and go.
func (r Row) ID() string {
	switch {
	case r.Node != "":
		return r.Node
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
// at its root, once the host has reported the root and while the task
// stands for it; "" otherwise. A view's selection on the task follows
// it there.
func (r Row) Alias() string {
	if !r.stands() {
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

// Rank is the row's sort group in priority order: pending tasks, then
// blocked, done, working, idle and unknown, stale or settled, then rows
// without an agent.
func (r Row) Rank() int {
	switch {
	case r.Pending != nil:
		return -1
	case r.Agent == nil:
		return 5
	case r.Agent.Activity == protocol.Blocked:
		return 0
	case r.Done:
		return 1
	case r.Stale || r.Settled:
		return 4
	case r.Agent.Activity == protocol.Working:
		return 2
	}
	return 3
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
	case r.Orphaned:
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
// gone after the add, nor one the relay cannot follow, its host gone
// from the config or answering as another machine; a host renamed in
// the config lists the worktree under its new name, and the row is
// drawn.
func (r Row) stands() bool {
	p := r.Pending
	return p != nil && !r.Removed && !r.Replaced && p.Mismatch == "" && !p.Gone && !(p.Done && !p.OK)
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

// Labels are the row's names as the views draw them: the primary, the
// branch, and the secondary, the repository. `main` and `master` are
// never primary when there is a better name: on them the repository is
// primary and the branch secondary. A detached worktree is named by its
// root's last element, with the repository and "detached" after it. A
// row that is no worktree's, an agent in a session `new` made or one
// observed, and an orphaned session, is its session's name alone.
func (r Row) Labels() (primary, secondary string) {
	switch {
	case r.Pending != nil:
		return mainless(r.Pending.Branch, r.Pending.Repo)
	case r.Worktree != nil && r.Worktree.Branch == "":
		return path.Base(r.Worktree.Root), r.Worktree.Repo + " detached"
	case r.Worktree != nil:
		return mainless(r.Worktree.Branch, r.Worktree.Repo)
	}
	return r.Name, ""
}

// mainless is the labels of a branch of a repository.
func mainless(branch, repo string) (string, string) {
	if branch == "main" || branch == "master" {
		return repo, branch
	}
	return branch, repo
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
	// From a host with attribution the agents come to a worktree by
	// the worktree id the host gave them, from any session and server.
	attributes := func(env string) bool {
		h, ok := hosts[byEnv[env]]
		return ok && h.Attribution
	}
	byWorktree := map[string][]*protocol.Agent{}
	for i := range in.Agents {
		if a := &in.Agents[i]; a.WorktreeID != "" && attributes(a.EnvironmentID) {
			byWorktree[a.WorktreeID] = append(byWorktree[a.WorktreeID], a)
		}
	}
	// agentLocal is the local session an agent's row stands for: the
	// plain attachment to its managed session, or the observed session
	// itself on this machine's own default server, whatever tags it
	// carries.
	agentLocal := func(host string, a *protocol.Agent) *workspace.Local {
		switch {
		case Server(*a) == tmux.LaatmuxServer.Label():
			return byAttach[host+"/"+a.Session]
		case hosts[host].Local && Server(*a) == tmux.DefaultServer.Label():
			if l := byName[a.Session]; l != nil {
				return l
			}
			return &workspace.Local{Name: a.Session}
		}
		return nil
	}
	used := map[*protocol.Agent]bool{}
	var rows []Row
	seenKey := map[string]bool{}
	// A pending task stands for the worktree row at its root until it
	// hands over: the two are joined by environment and root, never by
	// name, and the worktree row is not drawn while any task for it
	// stands, two tasks for one explicit branch included. A task that
	// can no longer become that row does not stand for it: see stands.
	var pendings []Row
	byAlias := map[string][]int{}
	for i := range in.Pendings {
		p := &in.Pendings[i]
		h, configured := hosts[p.Host]
		r := Row{Host: p.Host, Name: p.Repo + "/" + p.Branch, Pending: p, Removed: !configured,
			Replaced: configured && h.EnvironmentID != "" && p.EnvironmentID != "" && h.EnvironmentID != p.EnvironmentID}
		if alias := r.Alias(); alias != "" {
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
		switch {
		case attributes(w.EnvironmentID):
			if a := rowAgent(byWorktree[w.ID], w); a != nil {
				r.Agent, used[a] = a, true
			}
		case w.Session != "":
			if a := bySession[w.EnvironmentID+"\x00"+w.Session]; a != nil {
				r.Agent, used[a] = a, true
			}
		}
		key := workspace.Key(w.EnvironmentID, w.Root)
		seenKey[key] = true
		if w.Session == "" && r.Agent != nil && Server(*r.Agent) == tmux.DefaultServer.Label() {
			// With no home session and its agent on this machine's
			// default server the row is jumped to by switching to the
			// agent's session, and stands for it, whatever workspace
			// session is left. One whose agent is in a managed session
			// is attached to through the worktree's own workspace
			// session, and one on a remote host's default server cannot
			// be jumped to, and keeps the workspace session.
			r.Local = agentLocal(host, r.Agent)
		}
		if r.Local == nil {
			r.Local = byKey[key]
		}
		// Settled as the session the row stands for is: settling is that
		// session's.
		r.Settled = r.Local != nil && r.Local.Settled
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
	// orphaned one, and the viewer's own row is followed.
	for i := range pendings {
		p := pendings[i].Pending
		if p.EnvironmentID != "" && p.Root != "" && !p.Gone && !(p.Done && !p.OK) {
			// A session at the root of an add that may still make the
			// worktree is not orphaned, whether or not the task stands for
			// it: a host renamed mid-add has not listed it yet.
			seenKey[workspace.Key(p.EnvironmentID, p.Root)] = true
		}
		if !pendings[i].stands() {
			continue
		}
		if p.EnvironmentID != "" && p.Root != "" {
			if l := byKey[workspace.Key(p.EnvironmentID, p.Root)]; l != nil && pendings[i].Local == nil {
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
				if q := pendings[j].Pending; j != i && pendings[j].Agent == nil && pendings[j].stands() && q.EnvironmentID == p.EnvironmentID && q.Session == p.Session && q.Root == p.Root {
					pendings[j].Agent = a
				}
			}
		}
	}
	rows = append(rows, pendings...)
	// Every managed agent not shown on a worktree or task row has a row
	// of its own, a second one in a session included.
	for i := range in.Agents {
		a := &in.Agents[i]
		if Server(*a) != tmux.LaatmuxServer.Label() || used[a] {
			continue
		}
		host := byEnv[a.EnvironmentID]
		rows = append(rows, Row{Host: host, Name: a.Session, Agent: a, Local: agentLocal(host, a)})
	}
	for i := range in.Agents {
		a := &in.Agents[i]
		if Server(*a) == tmux.LaatmuxServer.Label() || used[a] {
			continue
		}
		host := byEnv[a.EnvironmentID]
		rows = append(rows, Row{Host: host, Name: a.Session, Agent: a, Local: agentLocal(host, a)})
	}
	// Orphaned: a local workspace session whose worktree is gone from a
	// host that can say so. A host that is down, whose snapshot has not
	// arrived, or whose daemon does not publish worktrees cannot, so its
	// sessions are not orphaned.
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
			rows = append(rows, Row{Host: byEnv[env], Name: l.Name, Local: l, Orphaned: true, Settled: l.Settled})
		}
	}
	for i := range rows {
		r := &rows[i]
		h, known := hosts[r.Host]
		r.HostDown = !known || !h.Connected
		r.Current = in.Current != "" && r.Local != nil && r.Local.Name == in.Current
		if w := r.Worktree; w != nil && w.Branch != "" && w.Source != "" {
			if b, ok := in.Branches[protocol.BranchKey{Source: config.SourceKey(w.Source), Branch: w.Branch}]; ok {
				r.Branch = &b
			}
		}
		if a := r.Agent; a != nil && r.Pending == nil && a.Liveness != protocol.Gone && a.Activity == protocol.Idle {
			r.Done = in.Attention[a.ID].Done()
			r.Stale = !r.Done && in.StaleAfter > 0 && in.Now.Sub(a.ActivityAt) > in.StaleAfter
		}
		r.Dim = r.Agent == nil || r.Agent.Liveness == protocol.Gone || r.HostDown || r.Orphaned ||
			r.Settled && !r.Pressing() || r.Stale && in.DimStale
		if r.Pending != nil {
			// Never dim: a task that runs is under way, and one that
			// needs the user wants them, which its waiting icon says;
			// never settled away from the main group.
			r.Dim, r.Settled = false, false
		}
	}
	sort.SliceStable(rows, func(i, j int) bool { return less(rows[i], rows[j], in.Sort) })
	var out Rows
	for _, r := range rows {
		switch {
		case r.Orphaned:
			out.Orphaned = append(out.Orphaned, r)
		case r.Current:
			// The viewer's own row stays in sight, settled or stale, so
			// the pane always shows the session it sits in, and z there
			// can unsettle it.
			out.Main = append(out.Main, r)
		case r.Settled && !r.Pressing():
			// A settled workspace's agent that wants the user stays in
			// place with its own icon.
			out.Settled = append(out.Settled, r)
		case r.Stale && in.CollapseStale:
			out.Stale = append(out.Stale, r)
		default:
			out.Main = append(out.Main, r)
		}
	}
	return out
}

// rowAgent is the agent a worktree row shows of the agents attributed
// to it, the row being jumped to through it. With a home session it is
// an agent there or none. Without one it is the agent laatmux made at
// the root, in the worktree's own session that a pane gone elsewhere
// took the home from, or one on a default server, never one on another
// observed server, which is not jumped to: an agent in another
// managed session is that session's, and keeps its row, since the
// worktree's workspace session attaches to its own managed session
// only. Among several the choice never turns on activity, which would
// swap the row's agent and the others' rows as they work: a live agent
// before a gone one, then the one that started first, then the id. The
// rest keep rows of their own until the views show several agents per
// worktree.
func rowAgent(agents []*protocol.Agent, w *protocol.Worktree) *protocol.Agent {
	var best *protocol.Agent
	for _, a := range agents {
		managed := Server(*a) == tmux.LaatmuxServer.Label()
		switch {
		case w.Session != "" && (!managed || a.Session != w.Session):
			continue
		case w.Session == "" && managed && !(a.Managed && a.Cwd == w.Root):
			continue
		case w.Session == "" && !managed && Server(*a) != tmux.DefaultServer.Label():
			// Another observed server's sessions are not jumped to.
			continue
		}
		if best == nil || before(a, best) {
			best = a
		}
	}
	return best
}

// before is a ahead of b in rowAgent's choice.
func before(a, b *protocol.Agent) bool {
	if (a.Liveness == protocol.Gone) != (b.Liveness == protocol.Gone) {
		return b.Liveness == protocol.Gone
	}
	sa, sb := started(a), started(b)
	if sa != sb {
		return sa < sb
	}
	return a.ID < b.ID
}

// started is when the agent's process started, the latest possible for
// one without an identity.
func started(a *protocol.Agent) int64 {
	if a.Identity == nil {
		return math.MaxInt64
	}
	return a.Identity.StartUnix
}

// less is the sort order. Pending tasks come first in every order, the
// newest first, since a task is what the user just asked for; orphaned
// rows sort by name alone, as ls lists them. The rest, by priority: rank,
// then most recent activity first; by recency: most recent activity
// first, rows without an agent last; by window: the session, then the
// window. Ties go by host, then name.
func less(a, b Row, order string) bool {
	if a.Orphaned || b.Orphaned {
		if a.Orphaned != b.Orphaned {
			return !a.Orphaned
		}
		return a.Name < b.Name
	}
	if (a.Pending != nil) != (b.Pending != nil) {
		return a.Pending != nil
	}
	if a.Pending != nil {
		if !a.Pending.SubmittedAt.Equal(b.Pending.SubmittedAt) {
			return a.Pending.SubmittedAt.After(b.Pending.SubmittedAt)
		}
		return a.Pending.ID < b.Pending.ID
	}
	switch order {
	case SortRecency:
		if (a.Agent != nil) != (b.Agent != nil) {
			return a.Agent != nil
		}
	case SortWindow:
		if sa, sb := a.session(), b.session(); sa != sb {
			return sa < sb
		}
		if wa, wb := a.window(), b.window(); wa != wb {
			return wa < wb
		}
	default:
		if ra, rb := a.Rank(), b.Rank(); ra != rb {
			return ra < rb
		}
	}
	if a.Agent != nil && b.Agent != nil && !a.Agent.ActivityAt.Equal(b.Agent.ActivityAt) {
		return a.Agent.ActivityAt.After(b.Agent.ActivityAt)
	}
	if a.Host != b.Host {
		return a.Host < b.Host
	}
	return a.Name < b.Name
}

// session is what a row sorts by first in window order: its host and its
// agent's session, else its host and its name.
func (r Row) session() string {
	if r.Agent != nil {
		return r.Host + "\x00" + r.Agent.Session
	}
	return r.Host + "\x00" + r.Name
}

// window is the agent's window index, -1 without an agent.
func (r Row) window() int {
	if r.Agent == nil {
		return -1
	}
	return r.Agent.Window
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
