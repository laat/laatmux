// Package rows builds the rows the listing, the sidebar and the dashboard
// show, from one join of the hosts' records: each host's worktrees with
// its agents, by the worktree the host attributed each agent to or, from
// a host without attribution, by the managed session the worktree record
// names; the relay's pending tasks where their worktrees will be; agents
// with no worktree and observed agents on other servers under other
// sessions; and the local sessions whose worktree is gone. Tree is the
// join, rooted at repositories; Agents is the agent view's tiles drawn
// from it. This file holds the row type, what a row says, the choice
// of a line's agent and the order of agents the join uses, and the
// tiles' sort order.
package rows

import (
	"fmt"
	"math"
	"path"
	"strings"
	"time"

	"github.com/laat/laatmux/internal/protocol"
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
}

// Input is everything the rows are built from.
type Input struct {
	Hosts     []Host
	Agents    []protocol.Agent
	Worktrees []protocol.Worktree
	Locals    []protocol.Session
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
	// dim, CollapseStale folds it into the Stale group. Sort is the
	// tiles' order: priority, the default, recency or window.
	Now           time.Time
	StaleAfter    time.Duration
	DimStale      bool
	CollapseStale bool
	Sort          string
}

// Sort orders of the tiles.
const (
	SortPriority = "priority"
	SortRecency  = "recency"
	SortWindow   = "window"
)

// Row is one tile of the agent view or one node of the tree: see Kind.
type Row struct {
	Host string // configured host name; "" when no host record claims the record
	Name string // <repo>/<branch>, the agent's session, a task's or the orphaned session's name, or the node's label
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
	Local    *protocol.Session
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
	// Worst is the most pressing agent's row under a worktree or task
	// line, for the folded line's icon; the line's own Agent is the one
	// its jump goes through.
	Worst    *Row
	Current  bool // the viewer's own session
	HostDown bool // the host is not connected
	// Dim is no identified agent, an agent that is gone, a host that is
	// down, an orphaned session, a settled workspace whose agent does not
	// want the user, or a stale agent when stale rows are dimmed.
	Dim bool
}

// Rows are the agent view's tiles in display order: the main group and
// the stale fold, which holds the stale agents and those of settled
// workspaces.
type Rows struct {
	Main  []Row
	Stale []Row
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
// without an agent or with a gone one, whose last activity, which the
// daemon keeps, says nothing now.
func (r Row) Rank() int {
	switch {
	case r.Pending != nil:
		return -1
	case r.Agent == nil || r.Agent.Liveness == protocol.Gone:
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
// add is, whatever agent its worktree has. An orphaned row is not
// State's: ls and the status_label token say `worktree gone` first.
func (r Row) State() string {
	switch {
	case r.Pending != nil:
		s, _ := r.pendingState()
		return s
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
		managed := a.Server == protocol.ServerLaatmux
		switch {
		case w.Session != "" && (!managed || a.Session != w.Session):
			continue
		case w.Session == "" && managed && !(a.Managed && a.Cwd == w.Root):
			continue
		case w.Session == "" && !managed && a.Server != protocol.ServerDefault:
			// Another observed server's sessions are not jumped to.
			continue
		}
		if best == nil || before(a, best) {
			best = a
		}
	}
	return best
}

// newer orders tasks newest first: by submission, then by id for two
// submitted in the same instant.
func newer(a, b *protocol.Pending) bool {
	if !a.SubmittedAt.Equal(b.SubmittedAt) {
		return a.SubmittedAt.After(b.SubmittedAt)
	}
	return a.ID < b.ID
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

// less is the tiles' sort order. Pending tasks come first in every
// order, the newest first, since a task is what the user just asked
// for. The rest, every one an agent's tile: by priority, rank, then
// most recent activity first; by recency, most recent activity first;
// by window, the session, then the window. Ties go by host, then name.
func less(a, b Row, order string) bool {
	if (a.Pending != nil) != (b.Pending != nil) {
		return a.Pending != nil
	}
	if a.Pending != nil {
		return newer(a.Pending, b.Pending)
	}
	switch order {
	case SortRecency:
		// Activity alone, below.
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

// session is what a tile sorts by first in window order: its host and
// its agent's session.
func (r Row) session() string {
	return r.Host + "\x00" + r.Agent.Session
}

// window is the agent's window index.
func (r Row) window() int {
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
