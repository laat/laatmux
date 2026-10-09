// Package protocol defines the JSON-lines wire format between a laatmux
// client and a laatmux daemon. One JSON object per line, in both directions.
//
// The contract is the boundary between independently released clients and
// daemons. A client branches on the capability set in the daemon's hello
// reply, never on the version string.
package protocol

import (
	"bufio"
	"encoding/json"
	"io"
	"slices"
	"strings"
	"sync"
	"time"
)

// Version is the protocol version this build speaks.
const Version = 1

// Message types.
const (
	TypeHello     = "hello"     // both directions; first message on a connection
	TypeSubscribe = "subscribe" // client -> daemon
	TypeSnapshot  = "snapshot"  // daemon -> client, full state after subscribe
	TypeUpsert    = "upsert"    // daemon -> client, one record changed or appeared
	TypeRemove    = "remove"    // daemon -> client, one record disappeared
	TypeNew       = "new"       // client -> daemon, create a managed session
	TypeAdd       = "add"       // client -> daemon, create a worktree and start an agent in it
	TypeRm        = "rm"        // client -> daemon, remove a worktree and its managed session
	TypeRun       = "run"       // client -> daemon, run a command in a worktree and stream its output
	TypeFollow    = "follow"    // client -> daemon, attach to a command sent earlier under the same id
	TypeCancel    = "cancel"    // client -> daemon, stop a run
	TypeShutdown  = "shutdown"  // client -> daemon, exit cleanly; answered with a result before it does
	TypePoke      = "poke"      // client -> merging daemon with attention, list this machine's tmux clients now; not answered
	TypeSelect    = "select"    // client -> daemon with select, make a pane and its window the managed server's current; answered with a result
	TypePrompt    = "prompt"    // client -> daemon, deliver a prompt to the agent an add started, as one numbered attempt; to a relay, without a number, deliver a pending record's prompt now
	TypeFacts     = "facts"     // client -> daemon with prune, what prune decides on for each of roots; answered with a result carrying facts
	TypeDismiss   = "dismiss"   // client -> relay, drop a pending record that needs the user, or one that handed over; with environment_id and root, the finished ones at that worktree, the id then the request's own, and with listing, rm's stamp, the handed-over ones whose add it is after
	TypeProgress  = "progress"  // daemon -> client, one step of a running add
	TypeResult    = "result"    // daemon -> client, reply to a command
	TypePing      = "ping"
	TypePong      = "pong"
	TypeError     = "error"
)

// Capabilities a daemon may advertise.
const (
	CapStatus    = "status"    // subscribe / snapshot / upsert / remove
	CapNew       = "new"       // the new command
	CapWorktrees = "worktrees" // worktree records in the subscription stream
	CapAdd       = "add"       // the add command
	CapRm        = "rm"        // the rm command
	// CapFollow is numbered progress and the follow message: a client
	// that lost its connection follows a command by id from the last n
	// it saw, rather than resending the command. Every command with
	// progress needs it.
	CapFollow = "follow"
	// CapRun is the run command, with cancel. A daemon with run has
	// follow.
	CapRun = "run"
	// CapShutdown is the shutdown message: the daemon exits as on
	// SIGTERM, cancelling its runs and waiting for them. stop uses it
	// so the process it ends is the daemon that answered, not a pid a
	// file remembers.
	CapShutdown = "shutdown"
	// CapTask is the task capability of a host: the command journal that
	// remembers an add's branch allocation and prompt delivery across
	// daemon restarts, generated branch names allocated in an allocate
	// stage, the prompt field of add and the prompt message with its
	// attempts, follow answered from the journal, and worktree listings
	// stamped with the observation revision so a result's listing
	// barrier can be waited for. A daemon with task has add and follow.
	CapTask = "task"
	// CapRelay is the background add on the machine the user sits at: an
	// add with Relay naming a host is written to a pending file, answered
	// accepted, and run against the host by the daemon, which follows it
	// to its result and to the listing after it; the pending records
	// travel in the merged stream, with the handoffs of retired ones;
	// dismiss drops a record that needs the user and prompt without an
	// attempt number delivers its prompt now. A daemon with relay has
	// merged.
	CapRelay = "relay"
	// CapDismissRoot is dismiss with environment_id and root: the relay
	// drops the finished records at that worktree, which rm sends after
	// removing it, and with listing, the stamp of the removal from rm's
	// result, the records that handed over there from an add before it;
	// without the stamp the host's listing decides those. A daemon
	// without it reads the message as a dismiss of the request's own id.
	CapDismissRoot = "dismiss-root"
	// CapMerged is subscribe with merged: one stream with every configured
	// host's records, a host record per host, and this machine's local
	// workspace sessions. Every daemon serve starts has it; a host's
	// config lists only the host itself, as a rule, and then its merged
	// stream is its own records.
	CapMerged = "merged"
	// CapRepoEntry is the repository coming from the machine the user
	// sits at: an add with repo_entry for a repository this host's
	// config does not list is resolved against that entry, and the
	// worktree listing covers every checkout under the repos directory.
	// A repository the config lists is resolved against the config's
	// entry, whatever entry the add brought for it; an entry for another
	// source is refused. A daemon without it ignores the entry and
	// resolves against its own config.
	CapRepoEntry = "repo-entry"
	// CapRemember is the relay listing a new repository in this
	// machine's config: a relayed add with remember has its repo_entry
	// appended to the config's repos, under the entry's name, once the
	// host's add has succeeded, and a refused add leaves the config as
	// it was. A source the config lists in any form is not added again.
	// A daemon without it reads such an add without the field, and adds
	// nothing to the config; a client sends remember only to a daemon
	// with it.
	CapRemember = "remember"
	// CapAttribution is the host attributing what runs to its worktrees:
	// every agent record carries the worktree_id of the worktree whose
	// root contains its pane's path, so a worktree has any number of
	// agents; panes without an agent inside a worktree root are
	// published as pane records, and run jobs as run records while they
	// run; and a worktree's session is its home session, the managed
	// one whose panes are all inside the root. A client pairs agents
	// with worktrees by worktree_id against a daemon with it, and by
	// the worktree's session against one without. A merging daemon
	// with it forwards the field and the records whatever it lists
	// itself; one without drops them.
	CapAttribution = "attribution"
	// CapAttention is the merging daemon's record of what the user has
	// seen: an attention record per agent that went from working to idle,
	// with when the finish and the last visit after it were, both on this
	// machine's clock, in the merged stream; and the poke message, which
	// has the daemon look at what this machine's tmux clients show at
	// once. A view shows an idle agent as done while its finish is after
	// its visit. Without it no agent is done.
	CapAttention = "attention"
	// CapGitStatus is the git object on worktree records: the branch's
	// diff against its base, the uncommitted diff, ahead and behind, and
	// the dirty, conflict and rebase marks, read by the host's daemon. A
	// merging daemon with it forwards the object; one without drops it,
	// as it decodes the record without the field.
	CapGitStatus = "git-status"
	// CapBranches is the merging daemon's PR and check state per branch,
	// read from GitHub through gh on this machine: branch_status records
	// in the merged stream, keyed by source and branch, and github_error
	// when it cannot read them. Without it no PR state is shown.
	CapBranches = "branches"
	// CapSelect is the select command: the daemon runs select-window
	// and select-pane for a pane on its managed server, so a jump from
	// a view lands on the pane, not only the session. A pane gone
	// answers an error.
	CapSelect = "select"
	// CapCheckouts is the main checkouts' records: a host publishes a
	// worktree record with main set for each main checkout under its
	// repos directory in use, to a subscriber that asks with checkouts
	// on subscribe, and attributes to it the agents in plain sessions
	// on its default server whose pane is in the checkout. A subscriber
	// that does not ask, an older client or merging daemon, never gets
	// one, so none takes a main checkout for a worktree it may remove.
	// A merging daemon with it asks its hosts and forwards the records
	// to a merged subscriber that asks.
	CapCheckouts = "checkouts"
	// CapPrune is what prune needs of a host: the facts message, which
	// reads for each root it names whether the worktree is clean, how
	// many commits it has beyond the repository's default branch and
	// whether its branch is on origin; and on rm, head, which refuses
	// the removal of a worktree whose HEAD is at another commit, unused,
	// which refuses one that a pane or a run is in, and delete_branch,
	// which deletes the branch once the worktree is gone when it is
	// still at head. A daemon without it reads an rm without the three
	// fields and removes the worktree whatever its HEAD is and whatever
	// runs in it; a client sends them only to a daemon with it. A daemon
	// with prune has rm.
	CapPrune = "prune"
)

// Progress states, in Message.State of a progress message. A stage may
// report several steps; each step ends in done or skip, and a step that
// runs a command may stream its output first.
const (
	StateStart  = "start"  // a mutating step began; Detail names it
	StateDone   = "done"   // the step completed
	StateSkip   = "skip"   // the step was already done; Detail says how that was seen
	StateOutput = "output" // one line of a setup command's output, in Detail
	// StateGap is one message in place of output a follow cannot have:
	// a run keeps a bounded tail for replay, and a follower whose after
	// is before the tail gets a gap saying how many lines it missed, at
	// the position they had.
	StateGap = "gap"
)

// Stages of add, in order. A result carries the stage that failed.
const (
	StageResolve = "resolve"
	StageClone   = "clone"
	StageFetch   = "fetch"
	// StageAllocate, from a daemon with task, is the branch decided: a
	// generated name made unique against the branches, the worktrees
	// and the journal, or the given one as is. Its done or skip line
	// carries Branch and Root, the ones the add uses from then on.
	StageAllocate = "allocate"
	StageWorktree = "worktree"
	StageCopy     = "copy"
	StageSetup    = "setup"
	StageAgent    = "agent"
	// StageRun is every progress message of a run: start with the root
	// as detail, output with FD, gap.
	StageRun = "run"
	// StageBranch is rm's deletion of the branch, asked for with
	// delete_branch: done when it went, skip with why when it stays.
	StageBranch = "branch"
)

// ErrUnknownCommand is the result error a follow gets for an id the
// daemon does not know: never sent, finished more than the retention
// ago, or sent to a daemon since restarted. The add and rm clients then
// resend their command; the run client reports the outcome unknown.
const ErrUnknownCommand = "unknown command"

// ErrCancelled is the result error of a run stopped by cancel, or by the
// daemon shutting down.
const ErrCancelled = "cancelled"

// Result errors of a daemon with task, on a follow or an add.
const (
	// ErrInterrupted answers a follow for an add the journal knows and
	// the daemon died in: Stage is the stage reached. The sender resends
	// the add under the same id and the daemon resumes it.
	ErrInterrupted = "interrupted"
	// ErrSubmissionExpired refuses an add whose SubmittedAt is more than
	// a day in the daemon's future or more than the journal's retention
	// in its past, and a follow for an id the journal has swept.
	ErrSubmissionExpired = "submission expired"
	// ErrRemoved answers a follow, or a resend, for an add whose worktree
	// rm has removed since.
	ErrRemoved = "removed"
	// ErrUnknownAttempt answers a follow with an attempt number the
	// journal has no record of; the sender resends the prompt message.
	ErrUnknownAttempt = "unknown attempt"
	// ErrRecoveryExpired answers a prompt message for an id the journal
	// no longer holds: the prompt cannot be delivered by the daemon any
	// more and is the user's to paste by hand.
	ErrRecoveryExpired = "recovery expired"
	// ErrAttemptNotRecorded prefixes the answer to a prompt message
	// whose attempt the journal could not write: nothing was done, the
	// number is not taken, and the sender retries it as it is.
	ErrAttemptNotRecorded = "attempt not recorded"
)

// Delivery states of a prompt, in the Prompt field of an add's result
// and of a prompt message's result. Every state but none may come with
// the reason in Error, on a result that is otherwise ok.
const (
	DeliveryNone         = "none"          // the add carried no prompt
	DeliveryDelivered    = "delivered"     // the agent was started with the prompt as its argument, or an attempt reached Enter
	DeliveryNotDelivered = "not delivered" // provably nothing transferred; Error says why
	DeliveryUnknown      = "unknown"       // a side effect may have taken; Error says where
)

// Pending is one background add as the relay holds it, without the
// prompt: what was submitted, where the add is, and what became of it.
// Ids are the client's, so a relay resent after a lost laptop daemon is
// the same id on the host and attaches rather than starts again.
type Pending struct {
	ID            string    `json:"id"`
	Host          string    `json:"host"`
	EnvironmentID string    `json:"environment_id,omitempty"` // bound at accept from the host row, or at the first hello
	Source        string    `json:"source"`
	Repo          string    `json:"repo"`
	Branch        string    `json:"branch"` // the proposal until the host allocates, then the branch used
	Generated     bool      `json:"generated,omitempty"`
	Agent         string    `json:"agent,omitempty"`
	Cmd           []string  `json:"cmd,omitempty"`
	SubmittedAt   time.Time `json:"submitted_at"`
	// Taken is that the host has the add; Reachable is the relay's
	// connection to the host, the connectivity axis, kept apart from
	// the outcome. Stage, State and Detail are the last progress
	// message's; Root and Session come from the host as it reports
	// them.
	Taken bool `json:"taken,omitempty"`
	// Sent is that the add may have reached the host: the relay follows
	// rather than sends it. Never sent nor taken, the host has no trace
	// of it, and it can be dismissed whatever it is waiting on.
	Sent        bool   `json:"sent,omitempty"`
	Reachable   bool   `json:"reachable,omitempty"`
	Unreachable string `json:"unreachable,omitempty"` // why the host cannot be reached, while Reachable is false
	// Mismatch is that the machine answering for the host is not the
	// one the task was accepted for, with the two environment ids: the
	// host will never answer for the task, and the record is dismissable
	// as one whose host left the config.
	Mismatch string `json:"mismatch,omitempty"`
	Stage    string `json:"stage,omitempty"`
	State    string `json:"state,omitempty"`
	Detail   string `json:"detail,omitempty"`
	Root     string `json:"root,omitempty"`
	Session  string `json:"session,omitempty"`
	// Done is that the add has an outcome: OK with the delivery state in
	// Prompt and its reason in Error, or failed with Error saying why
	// and Stage where. Attempt is the number of the last delivery
	// attempt, AttemptOpen that it is unresolved. Listed is that the
	// host's listing after the result has been seen, Retired that the
	// record handed over to its worktree row.
	Done        bool   `json:"done,omitempty"`
	OK          bool   `json:"ok,omitempty"`
	Error       string `json:"error,omitempty"`
	Prompt      string `json:"prompt,omitempty"`
	Attempt     int    `json:"attempt,omitempty"`
	AttemptOpen bool   `json:"attempt_open,omitempty"`
	// AttemptError is the host's refusal of the last attempt, recovery
	// expired say, kept apart from Error, the add's own outcome.
	AttemptError string `json:"attempt_error,omitempty"`
	Listed       bool   `json:"listed,omitempty"`
	ListingError string `json:"listing_error,omitempty"` // why the host's listing after the result fails, while it does
	Gone         bool   `json:"gone,omitempty"`          // the listing after the result had no worktree at the root
	// RememberError is why the add's repository, new to this machine's
	// config, could not be appended to it after the add succeeded, while
	// it cannot: the relay holds the handoff and tries again at start
	// and whenever the config file changes.
	RememberError string    `json:"remember_error,omitempty"`
	UpdatedAt     time.Time `json:"updated_at"`
}

// Complete reports whether nothing about the add needs the user: it
// succeeded and its prompt is delivered, or there was none. The relay
// retires a complete record once the host's listing shows its
// worktree; the views hide the worktree row behind a record that is
// not complete and offer p and x on it.
func (p Pending) Complete() bool { return p.OK && p.Delivered() }

// Delivered reports whether the prompt is with the agent, or there was
// none: the relay scrubs the prompt from its file on it, whatever the
// add did, and delivers no more. Only the two states say so; an add
// that failed before the agent stage has no delivery state, and its
// prompt is kept.
func (p Pending) Delivered() bool {
	return p.Done && (p.Prompt == DeliveryDelivered || p.Prompt == DeliveryNone)
}

// WorktreeID is the id of the worktree row the record becomes, "" until
// the host has reported the root.
func (p Pending) WorktreeID() string {
	if p.EnvironmentID == "" || p.Root == "" {
		return ""
	}
	return p.EnvironmentID + "/worktree/" + p.Root
}

// Handoff is a pending record retired into its worktree row, carried in
// merged snapshots for as long as the worktree is there: a view that
// missed the remove re-anchors a selection on the worktree row with it,
// and it says which task the worktree was made for, whose prompt the
// relay keeps on this machine.
type Handoff struct {
	ID         string `json:"id"`
	ReplacedBy string `json:"replaced_by"`
}

// Listing stamps a worktree listing with when it was read: the daemon
// generation, its start, and the observation revision the daemon had
// stepped to before git was asked. An add's result carries the revision
// its mutation made as a barrier; a listing at that revision or later in
// the same generation reflects the add, and so does any successful
// listing of a later generation, since a daemon that started after the
// mutation lists after it.
type Listing struct {
	Generation int64  `json:"generation"`
	Revision   uint64 `json:"revision"`
}

// Satisfies reports whether the listing l reflects the mutation barrier b.
func (l Listing) Satisfies(b Listing) bool {
	if l.Generation == 0 {
		return false
	}
	return l.Generation > b.Generation || (l.Generation == b.Generation && l.Revision >= b.Revision)
}

// Activity is what the agent on screen appears to be doing.
type Activity string

const (
	Working Activity = "working"
	Blocked Activity = "blocked"
	Idle    Activity = "idle"
	Unknown Activity = "unknown"
)

// Liveness is whether the identified agent process is still there.
type Liveness string

const (
	Alive Liveness = "alive"
	Gone  Liveness = "gone" // pane exists, identified process does not
	None  Liveness = "none" // no agent process was ever identified in this pane
)

// Identity pins an agent instance: the process, not the pane.
type Identity struct {
	PID       int    `json:"pid"`
	StartUnix int64  `json:"start_unix"`
	Comm      string `json:"comm"`
	LeaderPID int    `json:"leader_pid,omitempty"` // foreground process group leader of the tty
}

// The tmux server labels records carry, as the tmux package labels its
// servers: the managed server, where laatmux makes sessions, and this
// machine's default server, which a daemon may watch and a local client
// switch within. Any other label names another server, by its -L name
// or its -S socket path, watched read-only.
const (
	ServerLaatmux = "laatmux"
	ServerDefault = "default"
)

// Agent is one pane on one host as the sidebar sees it. Only panes with an
// identified agent instance, alive or gone, are published.
type Agent struct {
	ID            string    `json:"id"` // "<environment_id>/<server>/<pane_id>"; opaque to clients
	EnvironmentID string    `json:"environment_id"`
	Server        string    `json:"server,omitempty"` // tmux server label: ServerLaatmux, ServerDefault, or another server's -L name or -S path
	Session       string    `json:"session"`
	Window        int       `json:"window"`
	PaneID        string    `json:"pane_id"`
	TTY           string    `json:"tty"`
	Cwd           string    `json:"cwd"`
	Title         string    `json:"title"`
	Agent         string    `json:"agent"` // "claude", "codex", "" when none identified
	Activity      Activity  `json:"activity"`
	Liveness      Liveness  `json:"liveness"`
	Identity      *Identity `json:"identity,omitempty"`
	Rule          string    `json:"rule,omitempty"`   // detection rule that produced Activity
	Reason        string    `json:"reason,omitempty"` // detection explanation
	Managed       bool      `json:"managed"`          // created by laatmux new
	// WorktreeID, from a daemon with attribution, is the id of the
	// worktree whose root contains the pane's path: the recorded one of
	// a pane laatmux made, the current one otherwise. "" when the pane
	// is in no listed worktree, or the worktree has not been listed yet.
	// From a daemon with checkouts, to a subscriber that asked for them,
	// it may be a main checkout's record's instead (Worktree.Main).
	WorktreeID string    `json:"worktree_id,omitempty"`
	ActivityAt time.Time `json:"activity_at"` // when Activity last changed
	UpdatedAt  time.Time `json:"updated_at"`
}

// Pane is a pane with no identified agent inside a worktree root, from a
// daemon with attribution: a shell, a test watcher, a dev server. It
// carries no screen detection, and is upserted when what it shows here
// changes, not on every poll. A pane outside every worktree root is not
// published, and one whose agent is identified becomes an agent record.
type Pane struct {
	ID            string `json:"id"` // "<environment_id>/pane/<server>/<pane_id>"; opaque to clients
	EnvironmentID string `json:"environment_id"`
	Server        string `json:"server"`
	Session       string `json:"session"`
	Window        int    `json:"window"`
	PaneID        string `json:"pane_id"`
	Command       string `json:"command"` // the foreground command, as tmux names it
	PID           int    `json:"pid"`     // the pane's first process
	Cwd           string `json:"cwd"`
	WorktreeID    string `json:"worktree_id"`
	// Managed is a pane laatmux made, by new or add, whose cwd is the
	// directory it was made at rather than where it is now; false from
	// an older daemon.
	Managed   bool      `json:"managed"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Run is one run job while it runs, from a daemon with attribution:
// published once its process has started, removed when it ends, by
// itself, by cancel or by rm.
type Run struct {
	ID            string    `json:"id"` // "<environment_id>/run/<command id>"; opaque to clients
	EnvironmentID string    `json:"environment_id"`
	Root          string    `json:"root"`
	WorktreeID    string    `json:"worktree_id"`
	Cmd           []string  `json:"cmd"`
	StartedAt     time.Time `json:"started_at"`
}

// Attention is what the merging daemon knows of an agent's finish and
// the user's visit after it: FinishedAt is when it was seen to go from
// working to idle, SeenAt when a tmux client of this machine last showed
// it after that. Both are this machine's clock. The agent is done while
// it is idle and FinishedAt is after SeenAt.
type Attention struct {
	AgentID    string    `json:"agent_id"`
	FinishedAt time.Time `json:"finished_at"`
	SeenAt     time.Time `json:"seen_at,omitzero"`
}

// Done reports whether the finish is after the last visit.
func (a Attention) Done() bool { return a.FinishedAt.After(a.SeenAt) }

// BranchKey is what a branch_status record is keyed by: the source key,
// normalised as source.Key does, and the branch.
type BranchKey struct {
	Source string `json:"source"`
	Branch string `json:"branch"`
}

// BranchStatus is a pushed branch's PR and checks as GitHub has them,
// fetched at FetchedAt, this machine's clock; Stale is that the answer is
// older than the daemon trusts, or the last query for it failed.
// HeadOID is the commit the checks are of: the PR's last commit, or the
// branch's own. ChecksURL is the PR's checks page, or the commit's.
type BranchStatus struct {
	BranchKey
	FetchedAt time.Time    `json:"fetched_at"`
	Stale     bool         `json:"stale,omitempty"`
	HeadOID   string       `json:"head_oid,omitempty"`
	ChecksURL string       `json:"checks_url,omitempty"`
	PR        *PullRequest `json:"pr,omitempty"`
	Checks    *Checks      `json:"checks,omitempty"`
}

// PullRequest is a branch's PR: an open one, else the newest merged or
// closed, from the source's own repository. State is open, merged or
// closed; Base is the branch it was opened against, "" from a daemon
// that did not ask.
type PullRequest struct {
	Number int    `json:"number"`
	State  string `json:"state"`
	Draft  bool   `json:"draft,omitempty"`
	URL    string `json:"url"`
	Base   string `json:"base,omitempty"`
}

// Checks is the head commit's check rollup, counted from its aggregates:
// State is success, failure or pending; Passed and Total leave out
// neutral, skipped and stale contexts; Failing is the first failing
// check's name; PendingSince is when this machine first saw the head's
// checks pending.
type Checks struct {
	State        string    `json:"state"`
	Passed       int       `json:"passed"`
	Total        int       `json:"total"`
	Failing      string    `json:"failing,omitempty"`
	PendingSince time.Time `json:"pending_since,omitzero"`
}

// Check states.
const (
	ChecksSuccess = "success"
	ChecksFailure = "failure"
	ChecksPending = "pending"
)

// RootFacts is what a host reads for prune at one worktree root, from
// a daemon with prune: the branch git has checked out there, "" when
// detached, and HEAD's commit; Changed, the files git status lists as
// the status refresh reads it, untracked ones included, 0 when clean;
// Ignored and IgnoredDirs, the ignored files and directories git
// status lists with --ignored=matching, which a removal deletes with
// the worktree; Locked, a worktree git keeps from removal, with the
// reason given to git worktree lock, and Submodules, one with a
// submodule in it, which git removes only by force; Base, the
// repository's default branch, origin/HEAD's, else origin/main,
// origin/master, main or master, the first that is a commit, "" when
// none is, and Ahead, the commits HEAD has that Base does not;
// OnOrigin, that origin's branch of the name is there as the last
// fetch left it, and Pushed, that HEAD is in it; InUse, what the host
// finds running in the worktree, a pane, a run or an add with no
// outcome yet, "" when nothing, which rm's unused checks again. Error
// is why the facts could not be read, a root that is no worktree of
// the host's under the worktrees directory say; the rest is then
// empty.
type RootFacts struct {
	Root        string `json:"root"`
	Branch      string `json:"branch,omitempty"`
	Head        string `json:"head,omitempty"`
	Changed     int    `json:"changed,omitempty"`
	Ignored     int    `json:"ignored,omitempty"`
	IgnoredDirs int    `json:"ignored_dirs,omitempty"`
	Locked      bool   `json:"locked,omitempty"`
	LockReason  string `json:"lock_reason,omitempty"`
	Submodules  bool   `json:"submodules,omitempty"`
	Base        string `json:"base,omitempty"`
	Ahead       int    `json:"ahead,omitempty"`
	OnOrigin    bool   `json:"on_origin,omitempty"`
	Pushed      bool   `json:"pushed,omitempty"`
	InUse       string `json:"in_use,omitempty"`
	Error       string `json:"error,omitempty"`
}

// Worktree is one git worktree on one host, under the host's configured
// worktree directory, in a checkout of a known repository. Git is the
// source of truth: a worktree made by hand is listed, one removed by hand
// disappears, and one whose directory is gone (prunable) is not published.
// With Main it is a main checkout under the repos directory instead.
type Worktree struct {
	ID            string `json:"id"` // "<environment_id>/worktree/<root>", "<environment_id>/checkout/<root>" with Main; opaque to clients
	EnvironmentID string `json:"environment_id"`
	Repo          string `json:"repo"`             // repository label from the host's config
	Source        string `json:"source,omitempty"` // repository source, the identity; "" from older daemons
	Branch        string `json:"branch"`           // "" for a detached worktree
	Root          string `json:"root"`             // absolute path as git registered it
	// BranchDisplayOnly is that Branch is the branch as tmux.Printable
	// shows it, not its name: the name, checked out by hand, is one
	// laatmux cannot carry between client and daemon (worktree.CheckWire),
	// with a byte that is not UTF-8 or with U+FFFD. No command names the
	// worktree by it; one that names the worktree's root may send it as
	// the branch there, which the daemon takes for that root alone.
	BranchDisplayOnly bool `json:"branch_display_only,omitempty"`
	// Session is the worktree's home session: the managed session with
	// a pane laatmux made at Root, all of whose panes are inside Root;
	// jump attaches to it. "" when there is none. A daemon without
	// attribution names it only while it has that single pane.
	Session string `json:"session,omitempty"`
	// Main, from a daemon with checkouts, is that the record is a
	// repository's main checkout under the repos directory, not a
	// worktree: Root is the checkout's directory and Branch the branch
	// it has checked out. It has no home session and no workspace
	// session; its agents are those in plain sessions on the host's
	// default server, and rm refuses it.
	Main bool `json:"main,omitempty"`
	// Git, from a daemon with git-status, is the worktree's git state;
	// nil until its first refresh.
	Git       *GitStatus `json:"git,omitempty"`
	UpdatedAt time.Time  `json:"updated_at"`
}

// GitStatus is a worktree's git state as its host reads it. Committed is
// the branch against its merge base with Base, lines added and deleted;
// Uncommitted the working tree and index against HEAD with the untracked
// files that are not ignored, a lower bound when UncommittedPartial.
// Ahead and Behind count commits against Base. Conflict is nil when the
// host's git cannot tell (older than 2.38) or the branch is the base.
// Stale is that the last refresh timed out and the rest is from the one
// before. ChangedAt is when a value last changed.
type GitStatus struct {
	Base               string    `json:"base,omitempty"`
	Committed          [2]int    `json:"committed"`
	Uncommitted        [2]int    `json:"uncommitted"`
	UncommittedPartial bool      `json:"uncommitted_partial,omitempty"`
	Ahead              int       `json:"ahead"`
	Behind             int       `json:"behind"`
	Dirty              bool      `json:"dirty,omitempty"`
	Conflict           *bool     `json:"conflict,omitempty"`
	Rebasing           bool      `json:"rebasing,omitempty"`
	Stale              bool      `json:"stale,omitempty"`
	ChangedAt          time.Time `json:"changed_at"`
}

// Same reports whether two states carry the same values, whatever their
// ChangedAt.
func (g GitStatus) Same(o GitStatus) bool {
	g.ChangedAt, o.ChangedAt = time.Time{}, time.Time{}
	if (g.Conflict == nil) != (o.Conflict == nil) || g.Conflict != nil && *g.Conflict != *o.Conflict {
		return false
	}
	g.Conflict, o.Conflict = nil, nil
	return g == o
}

// HostStatus is one configured host as the merging daemon sees it: a
// separate axis from agent state, never folded into the records. Connected
// is a live connection with a completed hello. Listed is that the host's
// records in the merged stream come from a snapshot of the current
// connection; while it is false the records are cached from before a drop,
// or absent on a host never reached. Absence of a record is authoritative
// only when both are set. EnvironmentID is empty until the host has
// answered a hello once; agent and worktree records carry theirs, and a
// client maps it to the host name through these records.
type HostStatus struct {
	Name          string `json:"name"`
	SSH           string `json:"ssh,omitempty"` // "" is the merging daemon's own machine
	EnvironmentID string `json:"environment_id,omitempty"`
	Connected     bool   `json:"connected"`
	Listed        bool   `json:"listed"`
	Error         string `json:"error,omitempty"` // why it is not connected; "" while connecting
	// Reconnecting is that a connection that was up has dropped and the
	// daemon is dialling again; Error says what ended it. It holds until
	// the next dial's outcome: a host that answers again clears it with
	// the error, one that refuses keeps the error alone. A one-shot
	// client waits on a reconnecting host as on one still connecting,
	// since a daemon restarted for an upgrade is back within seconds,
	// where ssh's own errors mean the host is not reachable now.
	Reconnecting bool      `json:"reconnecting,omitempty"`
	Version      string    `json:"version,omitempty"`
	Capabilities []string  `json:"capabilities,omitempty"`
	Since        time.Time `json:"since"` // when the record last changed
}

// Local reports whether the host is the merging daemon's own machine.
func (h HostStatus) Local() bool { return h.SSH == "" }

// Session is a session on the merging daemon's default tmux server with
// the laatmux tags it carries: a workspace session has Key, a plain
// attachment Attach, and one with neither is not laatmux's. The
// workspace package lists them all; only laatmux's are published.
type Session struct {
	Name    string `json:"name"`
	Key     string `json:"key,omitempty"`     // the key @laatmux_workspace holds: <environment_id>/<root>
	Host    string `json:"host,omitempty"`    // @laatmux_host
	Source  string `json:"source,omitempty"`  // @laatmux_repo; "" when unknown
	Branch  string `json:"branch,omitempty"`  // @laatmux_branch
	Attach  string `json:"attach,omitempty"`  // @laatmux_attach
	Settled bool   `json:"settled,omitempty"` // @laatmux_settled
}

// Workspace reports whether the session is a workspace session.
func (s Session) Workspace() bool { return s.Key != "" }

// Laatmux reports whether the session is laatmux's at all: a workspace
// or a plain attachment.
func (s Session) Laatmux() bool { return s.Key != "" || s.Attach != "" }

// SessionKey is the workspace key a session carries: <environment_id>/
// <root>. The environment id is hex, so the key parses from the left.
func SessionKey(environmentID, root string) string { return environmentID + "/" + root }

// SplitSessionKey returns the environment id and root of a key.
func SplitSessionKey(key string) (environmentID, root string) {
	environmentID, root, _ = strings.Cut(key, "/")
	return environmentID, root
}

// Message is the single envelope. Fields are used per Type; unused ones are
// omitted on the wire.
type Message struct {
	Type string `json:"type"`

	// hello. PID is the daemon's, so a client that read a runtime record
	// can see that the daemon it reached is the one the record names;
	// a socket path is reused by the next daemon.
	Protocol      int      `json:"protocol,omitempty"`
	Client        string   `json:"client,omitempty"`
	EnvironmentID string   `json:"environment_id,omitempty"`
	Version       string   `json:"version,omitempty"`
	Host          string   `json:"host,omitempty"`
	Capabilities  []string `json:"capabilities,omitempty"`
	PID           int      `json:"pid,omitempty"`

	// subscribe. Merged asks for every configured host's records in one
	// stream; without it the daemon sends its own host's records only.
	// Checkouts asks for the main checkouts' records too, from a daemon
	// with checkouts; without it none is sent.
	Merged    bool `json:"merged,omitempty"`
	Checkouts bool `json:"checkouts,omitempty"`

	// snapshot / upsert / remove
	Seq        uint64     `json:"seq,omitempty"`
	Agents     []Agent    `json:"agents,omitempty"`
	Agent      *Agent     `json:"agent,omitempty"`
	AgentID    string     `json:"agent_id,omitempty"`
	Worktrees  []Worktree `json:"worktrees,omitempty"`
	Worktree   *Worktree  `json:"worktree,omitempty"`
	WorktreeID string     `json:"worktree_id,omitempty"`
	// From a daemon with attribution: the pane records and the run
	// records, and on a remove the id of the one gone. A client that
	// does not know them passes over the message.
	Panes        []Pane `json:"panes,omitempty"`
	Pane         *Pane  `json:"pane,omitempty"`
	PaneRecordID string `json:"pane_record_id,omitempty"`
	Runs         []Run  `json:"runs,omitempty"`
	Run          *Run   `json:"run,omitempty"`
	RunID        string `json:"run_id,omitempty"`

	// merged snapshot / upsert / remove, from a daemon with attention: the
	// attention records, and on a remove the agent id of the one gone,
	// under a key of its own, since agent_id on a remove means an agent
	// is gone.
	Attentions  []Attention `json:"attentions,omitempty"`
	Attention   *Attention  `json:"attention,omitempty"`
	AttentionID string      `json:"attention_id,omitempty"`

	// merged snapshot / upsert / remove, from a daemon with branches:
	// the branch status records, and on a remove the key of the one
	// gone; GitHubError, in a snapshot or an upsert, is why the daemon
	// cannot read GitHub, and GitHubOK in an upsert clears it.
	BranchStatuses  []BranchStatus `json:"branch_statuses,omitempty"`
	BranchStatus    *BranchStatus  `json:"branch_status,omitempty"`
	BranchStatusKey *BranchKey     `json:"branch_status_key,omitempty"`
	GitHubError     string         `json:"github_error,omitempty"`
	GitHubOK        bool           `json:"github_ok,omitempty"`

	// merged snapshot / upsert / remove, from a daemon with relay: the
	// pending records and the recent handoffs; a remove names the record
	// gone and, when it retired into its worktree row, that row's id in
	// ReplacedBy, a field of its own, since worktree_id on a remove means
	// a worktree is gone to every client.
	Pendings   []Pending `json:"pendings,omitempty"`
	Pending    *Pending  `json:"pending,omitempty"`
	PendingID  string    `json:"pending_id,omitempty"`
	ReplacedBy string    `json:"replaced_by,omitempty"`
	Handoffs   []Handoff `json:"handoffs,omitempty"`
	// Relay on an add names the host the local daemon is to run it
	// against, in the background.
	Relay string `json:"relay,omitempty"`

	// merged snapshot / upsert / remove. The host record and the local
	// session record are keyed by name; a remove names the one gone.
	// SessionsError, in a snapshot or an upsert, is that the local
	// sessions could not be listed for a reason other than no server, so
	// an incomplete listing says so; the records are the last listed.
	// SessionsListed in an upsert is that a listing succeeded again and
	// the error is cleared.
	Hosts            []HostStatus `json:"hosts,omitempty"`
	HostStatus       *HostStatus  `json:"host_status,omitempty"`
	HostName         string       `json:"host_name,omitempty"`
	Sessions         []Session    `json:"sessions,omitempty"`
	LocalSession     *Session     `json:"local_session,omitempty"`
	LocalSessionName string       `json:"local_session_name,omitempty"`
	SessionsError    string       `json:"sessions_error,omitempty"`
	SessionsListed   bool         `json:"sessions_listed,omitempty"`

	// commands and results
	ID      string   `json:"id,omitempty"` // client-chosen command id
	Name    string   `json:"name,omitempty"`
	Cwd     string   `json:"cwd,omitempty"`
	Cmd     []string `json:"cmd,omitempty"`
	OK      bool     `json:"ok,omitempty"`
	Error   string   `json:"error,omitempty"`
	Session string   `json:"session,omitempty"`
	PaneID  string   `json:"pane_id,omitempty"`

	// add and rm
	Repo   string `json:"repo,omitempty"`   // repository source, or a label as the daemon lists it
	Branch string `json:"branch,omitempty"` // branch and worktree name
	// RepoEntry on an add is the repository as the sender's config has
	// it, its source Repo's: a daemon with repo-entry resolves the add
	// against it when its own config does not list the repository.
	RepoEntry *RepoEntry `json:"repo_entry,omitempty"`
	// Remember on a relayed add asks a daemon with remember to append
	// the repo_entry to its config's repos once the add has succeeded:
	// the repository is new to the config, a source pasted into the
	// task form or given to add's --repo.
	Remember bool `json:"remember,omitempty"`
	// AgentName is the configured agent to start; Cmd, when set, is the
	// command instead. The key is agent_name because agent is the upsert's
	// record in this envelope.
	AgentName string `json:"agent_name,omitempty"`
	// Prompt, on add and on the prompt message, is the text the agent is
	// to be started with, or given; on a result, and on the relay's
	// pending records, it is the delivery state, one of the
	// Delivery constants. The text never travels in a result, a progress
	// message or a record. Generated asks a daemon with task to allocate
	// the branch: Branch is a proposal, made unique in the allocate
	// stage. SubmittedAt is the sender's clock at submit, unchanged on
	// every resend, so the daemon can refuse an add older than its
	// journal keeps; Attempt numbers a prompt message's delivery from 1
	// per add, on the message, on a follow of one, and on the result.
	// Listing on an add's result is the barrier the worktree listing
	// must pass to reflect it, and on an rm's result the same for the
	// removal, from a daemon with attribution, which rm passes on in its
	// dismiss at the root; on a snapshot it stamps the listing sent.
	Prompt      string    `json:"prompt,omitempty"`
	Generated   bool      `json:"generated,omitempty"`
	SubmittedAt time.Time `json:"submitted_at,omitzero"`
	Attempt     int       `json:"attempt,omitempty"`
	// NextAttempt is the number the host's journal expects next, on a
	// result refusing a prompt attempt out of order; 0 otherwise. The
	// relay takes its number from it rather than from the error text.
	NextAttempt int      `json:"next_attempt,omitempty"`
	Listing     *Listing `json:"listing,omitempty"`
	// ListingError, with Listing on a snapshot or an upsert of the
	// stamp alone, is why the last listing failed; the stamp is then
	// the last successful listing's.
	ListingError string `json:"listing_error,omitempty"`
	// RemovedIn, on the remove of a worktree from a daemon with
	// attribution, is the stamp of the listing that found it gone: a
	// remove that satisfies an add's barrier is of the worktree that add
	// made or a later one at its root, never of one from before. It is
	// kept apart from Listing, which marks a listing complete.
	RemovedIn *Listing `json:"removed_in,omitempty"`
	// Root on rm is the worktree root from the record. It is what reaches
	// a managed session whose worktree is already gone, since a branch
	// alone cannot be mapped to a root then; send it whenever it is known.
	// Alone, it removes a detached worktree. On a result: the worktree root.
	Root  string `json:"root,omitempty"`
	Force bool   `json:"force,omitempty"` // rm: remove a dirty or locked worktree
	// Head on rm, to a daemon with prune, is the commit the worktree's
	// HEAD must be at for the removal: prune decided on it, and a
	// worktree with a commit made since is refused. Unused refuses a
	// worktree that a run, or a pane of a server the daemon watches, is
	// in: prune found it unused, and one in use since is not what it
	// decided to remove. DeleteBranch asks for the branch to be deleted
	// too, once the worktree is gone, when it is still at Head; how that
	// went is a progress message of the branch stage, and the result is
	// the removal's.
	Head         string `json:"head,omitempty"`
	Unused       bool   `json:"unused,omitempty"`
	DeleteBranch bool   `json:"delete_branch,omitempty"`
	// Roots on facts names the worktrees to read; Facts on its result
	// is one record per root, in the order asked.
	Roots []string    `json:"roots,omitempty"`
	Facts []RootFacts `json:"facts,omitempty"`

	// progress, and the failed stage in a result
	Stage  string `json:"stage,omitempty"`
	State  string `json:"state,omitempty"`
	Detail string `json:"detail,omitempty"`
	// N numbers a command's progress messages from 1, from a daemon
	// with follow. After on a follow is the highest N the client has
	// seen; the daemon replays from After+1.
	N     uint64 `json:"n,omitempty"`
	After uint64 `json:"after,omitempty"`
	// FD on a run's output says which stream the line came from, 1 or
	// 2. Exit on a run's result is the process's exit status; OK says
	// whether it exited at all.
	FD   int `json:"fd,omitempty"`
	Exit int `json:"exit,omitempty"`
}

// RepoEntry is a repository as the machine the user sits at knows it:
// its source; its name, which places a new clone and new worktrees; and
// the personal copy rules and setup commands a new worktree of it gets
// after the committed ones, the sender's top-level copy rules among
// them.
type RepoEntry struct {
	Source string   `json:"source"`
	Name   string   `json:"name"`
	Copy   []string `json:"copy,omitempty"`
	Setup  []string `json:"setup,omitempty"`
}

// Conn is a line-oriented JSON connection. Writes are serialized.
type Conn struct {
	r  *bufio.Reader
	w  io.Writer
	mu sync.Mutex
}

func NewConn(rw io.ReadWriter) *Conn {
	return &Conn{r: bufio.NewReaderSize(rw, 1<<20), w: rw}
}

func NewConnRW(r io.Reader, w io.Writer) *Conn {
	return &Conn{r: bufio.NewReaderSize(r, 1<<20), w: w}
}

// Read blocks for the next message. io.EOF when the peer closed.
func (c *Conn) Read() (Message, error) {
	var m Message
	line, err := c.r.ReadBytes('\n')
	if err != nil {
		if err == io.EOF && len(line) > 0 {
			// trailing message without newline
			if uerr := json.Unmarshal(line, &m); uerr == nil {
				return m, nil
			}
		}
		return m, err
	}
	if err := json.Unmarshal(line, &m); err != nil {
		return m, err
	}
	return m, nil
}

func (c *Conn) Write(m Message) error {
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	c.mu.Lock()
	defer c.mu.Unlock()
	_, err = c.w.Write(b)
	return err
}

// Has reports whether the capability set includes cap.
func Has(caps []string, cap string) bool { return slices.Contains(caps, cap) }
