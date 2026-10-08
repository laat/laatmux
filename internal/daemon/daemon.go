// Package daemon is the per-host laatmux server. It polls the tmux servers
// the host config names, derives agent state for every pane with an
// identified agent, and streams snapshots and updates to subscribers.
// Detection never crosses the network: a daemon only ever looks at its own
// host's tmux.
//
// Only the managed laatmux server is ever configured or created on. Every
// other server, the user's default one included, is observed read-only.
package daemon

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/laat/laatmux/internal/client"
	"github.com/laat/laatmux/internal/config"
	"github.com/laat/laatmux/internal/detect"
	"github.com/laat/laatmux/internal/github"
	"github.com/laat/laatmux/internal/peer"
	"github.com/laat/laatmux/internal/procs"
	"github.com/laat/laatmux/internal/protocol"
	"github.com/laat/laatmux/internal/tmux"
	"github.com/laat/laatmux/internal/worktree"
)

// Timing, taken from herdr's tuned values.
const (
	DefaultInterval   = 300 * time.Millisecond
	DefaultCapture    = 40 // lines; the detector's regions look at the bottom of this
	startupGrace      = 3 * time.Second
	idleConfirmations = 3
	idleConfirmCap    = 700 * time.Millisecond
	subscriberBuffer  = 256
)

// Timings are the daemon's waits and intervals; see Config.Timings.
type Timings struct {
	// ReadyWait bounds how long a delivery waits for the pane to be
	// ready, and how long a trust watcher runs; TrustPoll is how often
	// the watcher looks at the pane for its question.
	ReadyWait time.Duration
	TrustPoll time.Duration
	// JournalRetention is how long a finished journal entry is kept,
	// and how old a submission may be before it is refused as expired.
	JournalRetention time.Duration
	// HandoffRecheck is how often a handoff waiting for the merged
	// stream to show the worktree looks at the host's listing again,
	// and bounds each such look; HandoffPatience is how long it waits
	// for the stream in all before it hands off on the listing alone.
	HandoffRecheck  time.Duration
	HandoffPatience time.Duration
	// CommandTTL is how long a finished command's outcome is kept;
	// KillDelay how long a cancelled run gets between SIGTERM and
	// SIGKILL.
	CommandTTL time.Duration
	KillDelay  time.Duration
}

// DefaultTimings are the timings a zero field takes.
var DefaultTimings = Timings{
	ReadyWait:        time.Minute,
	TrustPoll:        500 * time.Millisecond,
	JournalRetention: 30 * 24 * time.Hour,
	HandoffRecheck:   3 * time.Second,
	HandoffPatience:  time.Minute,
	CommandTTL:       DefaultCommandTTL,
	KillDelay:        DefaultKillDelay,
}

// withDefaults is t with every zero field at its default.
func (t Timings) withDefaults() Timings {
	def := func(v *time.Duration, d time.Duration) {
		if *v == 0 {
			*v = d
		}
	}
	def(&t.ReadyWait, DefaultTimings.ReadyWait)
	def(&t.TrustPoll, DefaultTimings.TrustPoll)
	def(&t.JournalRetention, DefaultTimings.JournalRetention)
	def(&t.HandoffRecheck, DefaultTimings.HandoffRecheck)
	def(&t.HandoffPatience, DefaultTimings.HandoffPatience)
	def(&t.CommandTTL, DefaultTimings.CommandTTL)
	def(&t.KillDelay, DefaultTimings.KillDelay)
	return t
}

// Panes is the tmux side of one watched server. tmux.Server implements it;
// tests supply a fake.
type Panes interface {
	ListPanes(ctx context.Context) ([]tmux.Pane, error)
	Capture(ctx context.Context, paneID string, n int) ([]string, error)
	EnsureConfigured(ctx context.Context) error
	NewSession(ctx context.Context, o tmux.NewSessionOpts) (tmux.Session, error)
	// KillSessionID kills the session with the id a pane is listed
	// with, on the managed server's instance with the pane's ServerPID:
	// rm's. A session gone, with its server or not, is no error.
	KillSessionID(ctx context.Context, id string, serverPID int) error
	// Paste types text into a pane as one bracketed paste and Enter
	// through the named buffer; DeleteBuffers deletes the buffers with
	// the prefix. Both act on the managed server only.
	Paste(ctx context.Context, buffer, paneID, text string) error
	DeleteBuffers(ctx context.Context, prefix string) error
	// SendKeys presses tmux key names in a pane, on the managed server:
	// the answer to an agent's question at launch.
	SendKeys(ctx context.Context, paneID string, keys ...string) error
	// SelectPane makes a pane and its window current on the managed
	// server, for the select command.
	SelectPane(ctx context.Context, paneID string) error
}

// Target is one tmux server the daemon watches.
type Target struct {
	Label string // tmux.Server.Label(); part of every agent id from this server
	Tmux  Panes
	// Managed marks the server laatmux owns: reconciled on discovery and the
	// destination of new. At most one target is managed. The rest are read
	// with list-panes and capture-pane only.
	Managed bool
}

// Targets wraps servers for Config.Targets.
func Targets(servers ...tmux.Server) []Target {
	out := make([]Target, 0, len(servers))
	for _, s := range servers {
		out = append(out, Target{Label: s.Label(), Tmux: s, Managed: s.Managed()})
	}
	return out
}

// Processes is the process-table side. The procs package implements it;
// tests supply controlled tables and errors.
type Processes interface {
	Find(tty string) (procs.Identity, bool, error)
	Exists(tty string, id procs.Identity) (bool, error)
}

type osProcs struct{}

func (osProcs) Find(tty string) (procs.Identity, bool, error)      { return procs.Find(tty) }
func (osProcs) Exists(tty string, id procs.Identity) (bool, error) { return procs.Exists(tty, id) }

// ConfigRead is what the daemon takes from a read of this machine's
// config file again: what its store lists, and the commands of the
// agents an add starts, by label. The hosts it reads through
// Config.Hosts; tmux_servers and github_hosts it reads at start alone.
type ConfigRead struct {
	Listed worktree.Listed
	Agents map[string][]string
}

type Config struct {
	Targets       []Target  // defaults to the managed laatmux server alone
	Procs         Processes // defaults to the OS process table
	Interval      time.Duration
	CaptureLines  int
	EnvironmentID string
	Host          string // this host's configured name; recorded on panes add creates
	Version       string
	Logger        *log.Logger

	// Store is the host's checkouts and worktrees; nil when the host has no
	// repos and worktrees directories, and then there are no worktree
	// records and no add or rm. Agents maps an agent label to its command
	// for add, as the config said at start. WorktreeInterval is how often
	// git is asked.
	Store            *worktree.Store
	Agents           map[string][]string
	WorktreeInterval time.Duration
	// Reread reads the config file again, every WorktreeInterval:
	// changed when the file changed since the last read, so Store
	// follows the repositories an add or the task form appended, and
	// hand edits, without a restart, their steps and the copy rules for
	// every worktree too, the adds the agents' commands, the merged
	// subscribers the hosts, and the relay retries the appends a config
	// it could not take held. A file that does not read keeps what the
	// daemon has. nil keeps what it was made with, and retries the
	// appends at start alone.
	Reread func() (read ConfigRead, changed bool, err error)
	// Commands is the directory of the command journal, one file per
	// add, which with Store and the managed server is the task
	// capability; "" means none.
	Commands string

	// The merged stream. Hosts reads the configured hosts, on every
	// merged subscription; nil means no merged capability. Dial connects
	// to a remote host, client.Dial by default. Sessions lists this
	// machine's local workspace sessions; nil means none. A listing
	// that a user's hook failed after returns its sessions with the
	// *tmux.HookError. The durations default to the constants in
	// merge.go.
	Hosts           func() ([]peer.Host, error)
	Dial            func(ctx context.Context, h peer.Host) (*client.Conn, error)
	Sessions        func(ctx context.Context) ([]protocol.Session, error)
	MergedIdle      time.Duration
	SessionInterval time.Duration
	ReconnectMin    time.Duration

	// Shutdown ends the daemon as SIGTERM does, for the shutdown
	// message; nil means no shutdown capability.
	Shutdown func()

	// Timings are the daemon's waits and intervals: delivery and trust,
	// the journal's retention, the relay's handoff, the command table
	// and run cancellation; a zero field takes its default. Tests
	// shorten them.
	Timings Timings

	// Pending is the directory of the relay's pending files, which with
	// Hosts is the relay capability; "" means none. AppendRepo appends
	// a repository to this machine's config, reporting whether it was
	// not listed yet, for a relayed add with remember; with the relay
	// it is the remember capability, and nil means none.
	Pending    string
	AppendRepo func(src, name string) (bool, error)

	// Attention is the file the attention state is kept in, which with
	// Hosts is the attention capability; "" means none. Clients lists
	// what this machine's tmux clients show; nil means no agent is ever
	// seen. A listing that a user's hook failed after returns its views
	// with the *tmux.HookError. See attention.go.
	Attention string
	Clients   func(ctx context.Context) ([]ClientView, error)

	// GitHub runs the GraphQL queries for the PR and check state, gh by
	// default in serve; Branches is the file the answers are kept in.
	// With Hosts, both are the branches capability; nil or "" means
	// none. See branches.go.
	GitHub   github.Runner
	Branches string
	// GitHubHosts are the GitHub Enterprise hosts the config trusts
	// beside github.com: a source on any other host is never asked
	// about, so gh never sends a token to it.
	GitHubHosts []string
}

// Daemon holds the derived state for every watched tmux server.
//
// Locks. mu guards the mutable fields from it down, but for those that
// say otherwise, the runner's state (tasks, which holds mu by pointer),
// and the stream: every record is published under it, so a snapshot
// and the upserts after it never interleave. The locks outside it, and
// the order they are taken in, are:
//
//   - relay.mu, the pending records, before mu: a record's mutation and
//     its publication are one step, and the merged snapshot reads the
//     records the same way. tasksAtLocked and worktreeRemovedLocked,
//     under mu, read them on a goroutine of their own for that.
//   - subMu serializes a subscription's config read and session listing
//     with the poll's, so neither is applied after a newer one; taken
//     before relay.mu and mu. lastHostsErr is under it.
//   - pollMu holds the worktree poll and its publication together, so an
//     older observation never overwrites a newer one; taken before mu.
//     lastListErr is under it.
//   - repos, and the keyed locks repoLock hands out, all held across mu
//     and never taken under it. An add holds repos shared, then its
//     repository's "repo/" lock, then its "name/" lock (holdRepos,
//     lockRepo) until its agent is launched; a typed prompt's wait runs
//     without them. rm holds repos alone (lockRepos). "deliver/<root>"
//     is held by a delivery's readiness check and paste, by rm from
//     git's removal on, by an add's result and by a trust step, each
//     on its own; when nested it is the inner one, under repos (rm) or
//     under this host's "attempt/<id>" (a prompt's attempts), never
//     the other way.
//   - the relay's keyed locks per pending record, "attempt/<id>" for a
//     delivery attempt or a dismiss and "settle/<id>" for its retiring
//     and handoff, never nested; taken before relay.mu and mu, never
//     under them.
//   - the command table's lock, under which forgetDone takes command.mu
//     and get runs its init; the keyed locks' own lock, held across the
//     map alone, never across a lock it hands out; journal.mu,
//     runJob.mu, command.mu and the resolver's. These are leaves: each
//     guards its own struct and takes nothing under it but as said.
//     Nothing enforces any more that a keyed lock is not taken under
//     mu (repoLock took mu itself, so such a call deadlocked at once);
//     the order above is the rule. The table's lock, a leaf, may be.
//
// A method with the Locked suffix is called with its receiver's lock
// held: mu for a Daemon method and for a branches or taskRunner method
// (their mu is the daemon's), relay.mu for a relay method, journal.mu
// for a journal method, the resolver's for its own. The exceptions say
// which lock: runAttemptLocked, the relay's attempt lock;
// startRunnerLocked and dropRetiredLocked, relay.mu;
// mergedSnapshotLocked, relay.mu and mu. Called with a lock held but
// without the suffix: publishPending and publishRemoved, relay.mu,
// taking mu inside; listSessions, subMu.
type Daemon struct {
	cfg     Config
	targets []*target
	managed *target // nil when the laatmux server is not watched

	mu     sync.Mutex
	seq    uint64
	agents map[string]protocol.Agent // by pane key, published records only
	panes  map[string]*paneState     // by pane key, every pane seen
	subs   map[*subscriber]struct{}

	// Worktrees: the last git listing, the managed sessions by root, and
	// the published join of the two.
	worktrees map[string]protocol.Worktree // by root, the main checkouts in use among them
	lastList  []worktree.Record
	// lastMains is every main checkout of the last listing, and
	// mainAgents the ids of those an agent is attributed to, which are
	// in use (inUseLocked), a checkout gone from the listing among them
	// until its agents are attributed again.
	lastMains  []worktree.Record
	mainAgents map[string]bool
	// retiring is the records replaced at their root, by id, until
	// nothing names them (retireLocked).
	retiring     map[string]protocol.Worktree
	listed       bool
	managedRoots map[string]string // root -> session
	lastListErr  string            // logged once per change; under pollMu
	lastReposErr string            // the same for the config file; runConfig's
	configRead   bool              // runConfig has read the file once
	// commands is the commands of the agents an add starts, by label:
	// Config.Agents, then each read of the file that changed, replaced
	// whole.
	commands atomic.Pointer[map[string][]string]
	poke     chan struct{}
	// Attribution: the listed roots, longest first; the pane records of
	// panes without an agent inside a root, by pane key; the run
	// records by id; and the resolved-path cache, with a lock of its
	// own. See attribution.go and resolve.go.
	roots    []root
	paneRecs map[string]protocol.Pane
	runRecs  map[string]protocol.Run
	paths    *resolver

	// The commands and what they start, sharing mu; see runner.go.
	tasks *taskRunner
	// The journal, nil without the task capability; the observation
	// revision and the daemon generation that stamp listings, the
	// stamp and error of the last listing, and the lock the poll and
	// its publication run under.
	journal *journal
	relay   *relay // nil without the relay capability
	// fatal is what New could not do without, the relay's directory
	// say: Err, and Run ends with it at once.
	fatal error
	attn  *attention // nil without the attention capability
	// The last errors of the attention file and the clients listing,
	// logged once per change, and the last hook error of the clients
	// listing and of the sessions listing (hookOnce).
	lastAttnErr     string
	lastClientsErr  string
	clientsHookErr  string
	sessionsHookErr string
	// The worktrees' git status refreshes, by root; see gitstatus.go.
	gits map[string]*gitEntry
	// The branch records and the state of asking GitHub about them,
	// nil without the branches capability. See branches.go.
	branches   *branches
	ctx        context.Context // Run's context, for goroutines that outlive a connection
	generation int64
	revision   uint64
	listing    protocol.Listing
	listErr    string
	pollMu     sync.Mutex

	// The merged stream: its own sequence and subscribers, the hosts by
	// name and in config order, the local sessions, and the context the
	// follows and the sessions poll run under, nil while idle.
	mseq         uint64
	msubs        map[*subscriber]struct{}
	mhosts       map[string]*mergedHost
	mnames       []string
	msessions    map[string]protocol.Session
	sessionsErr  string
	lastHostsErr string     // under subMu
	subMu        sync.Mutex // serializes a subscription's config read and session listing with the poll
	mctx         context.Context
	mcancel      context.CancelFunc
	midle        *time.Timer
	midleGen     uint64 // bumped when the timer is set or stopped; a fired callback checks it

	// discovered closes after the first complete poll of every server and
	// of git, so a snapshot is never an empty or partial view of a host
	// that has panes or worktrees.
	discovered          chan struct{}
	discoveredOnce      sync.Once
	panesDiscovered     bool
	worktreesDiscovered bool
}

type target struct {
	Target
	// configuredServer is the tmux server pid the managed configuration was
	// last applied to. A different pid is a new server, started by hand or
	// by new-session, and gets reconciled on discovery.
	configuredServer int
	// lastHookErr is the hook error last logged for the server's pane
	// listing; see hookOnce.
	lastHookErr string
}

// hookOnce reports whether err, a tmux listing's error, is a
// *tmux.HookError: the listing's records are whole, and the caller
// keeps them. The hook is the user's, from their config, and fails at
// every listing until they fix it, and the pane poll, the sessions and
// the clients list at every tick. So its error is logged once in last,
// the field of the listing and its server, after what; a listing that
// works clears last, and the hook failing again after one is logged
// again.
func (d *Daemon) hookOnce(last *string, err error, what string) bool {
	var he *tmux.HookError
	if errors.As(err, &he) {
		d.logOnce(last, "%v", fmt.Errorf("%s: %w", what, err))
		return true
	}
	if err == nil {
		*last = ""
	}
	return false
}

// paneKey identifies a pane across servers. Pane ids are per server, so
// %1 on the laatmux server and %1 on the default server are different panes.
func paneKey(label, paneID string) string { return label + "/" + paneID }

type paneState struct {
	target       *target
	identity     procs.Identity
	hasIdentity  bool // an agent instance is known; it may be gone
	gone         bool // the known instance no longer exists
	activity     protocol.Activity
	activityAt   time.Time
	pendingIdle  *time.Time
	pendingCount int
	// obs is what the last observation of the pane saw, for a delivery
	// waiting on it; written and read under d.mu, where the rest of the
	// state is the poll goroutine's own.
	obs observation
	// The last observation as attribution needs it, under d.mu too: the
	// pane, its resolved path, whether it has been observed at all, and
	// whether no agent was identified in it, so it is a pane record's.
	pane     tmux.Pane
	path     string
	observed bool
	bare     bool
}

// observation is one poll's view of a pane as a delivery needs it: when
// it was made, where the pane is, whether a verified live agent is in
// it and which, and whether the prompt box is on screen.
type observation struct {
	at        time.Time
	session   string
	serverPID int
	verified  bool // an agent identified, not tentative, and seen alive by this poll's process check
	identity  procs.Identity
	idle      bool // the detector saw the prompt box: VisibleIdle, not the fallback
}

type subscriber struct {
	ch     chan protocol.Message
	drop   func() // closes the transport so the peer sees EOF and resnapshots
	merged bool   // on the merged stream rather than this host's own
	// checkouts is that it asked for the main checkouts' records.
	checkouts bool
}

func New(cfg Config) *Daemon {
	if cfg.Interval == 0 {
		cfg.Interval = DefaultInterval
	}
	if cfg.CaptureLines == 0 {
		cfg.CaptureLines = DefaultCapture
	}
	if cfg.Logger == nil {
		cfg.Logger = log.New(io.Discard, "", 0)
	}
	if len(cfg.Targets) == 0 {
		cfg.Targets = Targets(tmux.LaatmuxServer)
	}
	if cfg.Procs == nil {
		cfg.Procs = osProcs{}
	}
	cfg.Timings = cfg.Timings.withDefaults()
	if cfg.WorktreeInterval == 0 {
		cfg.WorktreeInterval = DefaultWorktreeInterval
	}
	if cfg.Dial == nil {
		cfg.Dial = client.Dial
	}
	if cfg.MergedIdle == 0 {
		cfg.MergedIdle = DefaultMergedIdle
	}
	if cfg.SessionInterval == 0 {
		cfg.SessionInterval = DefaultSessionInterval
	}
	if cfg.ReconnectMin == 0 {
		cfg.ReconnectMin = DefaultReconnectMin
	}
	d := &Daemon{
		cfg:    cfg,
		agents: map[string]protocol.Agent{},
		panes:  map[string]*paneState{},
		subs:   map[*subscriber]struct{}{},

		worktrees:    map[string]protocol.Worktree{},
		managedRoots: map[string]string{},
		retiring:     map[string]protocol.Worktree{},
		poke:         make(chan struct{}, 1),
		paneRecs:     map[string]protocol.Pane{},
		runRecs:      map[string]protocol.Run{},
		gits:         map[string]*gitEntry{},
		paths:        newResolver(),

		msubs:     map[*subscriber]struct{}{},
		mhosts:    map[string]*mergedHost{},
		msessions: map[string]protocol.Session{},

		generation: time.Now().UnixNano(),
		discovered: make(chan struct{}),
	}
	for _, t := range cfg.Targets {
		tt := &target{Target: t}
		d.targets = append(d.targets, tt)
		if t.Managed && d.managed == nil {
			d.managed = tt
		}
	}
	if cfg.Commands != "" && cfg.Store != nil && d.managed != nil {
		j, err := openJournal(cfg.Commands, cfg.Logger, cfg.Timings.JournalRetention)
		if err != nil {
			cfg.Logger.Printf("journal: %v; task capability disabled", err)
		} else {
			d.journal = j
		}
	}
	agents := cfg.Agents
	d.commands.Store(&agents)
	d.tasks = newRunner(d)
	if cfg.Attention != "" && cfg.Hosts != nil {
		a, err := openAttention(cfg.Attention)
		if err != nil {
			cfg.Logger.Printf("attention: %v; starting over", err)
			a = &attention{path: cfg.Attention, entries: map[string]*attnEntry{}, poke: make(chan struct{}, 1)}
		}
		d.attn = a
	}
	if cfg.GitHub != nil && cfg.Branches != "" && cfg.Hosts != nil {
		d.branches = newBranches(&d.cfg, &d.mu, d.mbroadcastLocked)
	}
	if cfg.Pending != "" && cfg.Hosts != nil {
		r, err := openRelay(cfg.Pending, cfg.Logger)
		if err != nil {
			// A merging daemon without its relay would run with the
			// add path gone: Err carries the error, serve ends with it
			// before announcing the daemon, and Run ends with it too.
			d.fatal = fmt.Errorf("pending: %w", err)
		} else {
			d.relay = r
		}
	}
	return d
}

func (d *Daemon) capabilities() []string {
	caps := []string{protocol.CapStatus, protocol.CapFollow}
	if d.managed != nil {
		caps = append(caps, protocol.CapNew, protocol.CapSelect)
	}
	if d.cfg.Store != nil {
		caps = append(caps, protocol.CapWorktrees, protocol.CapRun, protocol.CapAttribution, protocol.CapGitStatus, protocol.CapCheckouts)
		if d.managed != nil {
			caps = append(caps, protocol.CapAdd, protocol.CapRm, protocol.CapRepoEntry)
		}
		if d.journal != nil {
			caps = append(caps, protocol.CapTask)
		}
	}
	if d.cfg.Hosts != nil {
		caps = append(caps, protocol.CapMerged)
		if d.cfg.Store == nil {
			// It forwards what the hosts attribute, the git objects
			// they read and their main checkouts, though it has no
			// worktrees of its own.
			caps = append(caps, protocol.CapAttribution, protocol.CapGitStatus, protocol.CapCheckouts)
		}
	}
	if d.relay != nil {
		caps = append(caps, protocol.CapRelay, protocol.CapDismissRoot)
		if d.cfg.AppendRepo != nil {
			caps = append(caps, protocol.CapRemember)
		}
	}
	if d.attn != nil {
		caps = append(caps, protocol.CapAttention)
	}
	if d.branches != nil {
		caps = append(caps, protocol.CapBranches)
	}
	if d.cfg.Shutdown != nil {
		caps = append(caps, protocol.CapShutdown)
	}
	return caps
}

// agentCmds is the commands of the agents an add starts, by label, as
// the config last read said; the map is the daemon's and is not to be
// changed.
func (d *Daemon) agentCmds() map[string][]string { return *d.commands.Load() }

// runCtx is Run's context, or the background one before Run.
func (d *Daemon) runCtx() context.Context {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.ctx == nil {
		return context.Background()
	}
	return d.ctx
}

// Err is what New could not do, the relay's directory say: serve
// checks it before announcing the daemon, and Run returns it at once.
func (d *Daemon) Err() error { return d.fatal }

// Run polls until ctx is done.
func (d *Daemon) Run(ctx context.Context) error {
	if d.fatal != nil {
		return d.fatal
	}
	d.mu.Lock()
	d.ctx = ctx
	d.mu.Unlock()
	if d.cfg.Reread != nil {
		// The config's first look, before the poll and the relay start.
		d.readConfig(ctx)
	}
	if d.cfg.Store != nil {
		go d.runWorktrees(ctx)
		go d.runGitStatus(ctx)
	} else {
		d.markDiscovered(&d.worktreesDiscovered)
	}
	if d.journal != nil {
		d.tasks.sweepBuffers(ctx)
		go d.tasks.runJournal(ctx)
	}
	if d.relay != nil {
		d.startRelays(ctx)
		go d.runRelaySweep(ctx)
	}
	if d.cfg.Reread != nil {
		go d.runConfig(ctx)
	}
	if d.attn != nil {
		// The entries of hosts gone from the config go at start, as
		// they do when a subscription reads the config.
		if hosts, err := d.cfg.Hosts(); err == nil {
			names := make([]string, len(hosts))
			for i, h := range hosts {
				names[i] = h.Name
			}
			d.mu.Lock()
			d.forgetUnconfiguredLocked(names)
			d.mu.Unlock()
		}
		if d.cfg.Clients != nil {
			go d.runSeen(ctx)
		}
	}
	if d.branches != nil {
		go d.runBranches(ctx)
	}
	t := time.NewTicker(d.cfg.Interval)
	defer t.Stop()
	pruned := false
	for {
		err := d.poll(ctx)
		if err != nil && ctx.Err() == nil {
			d.cfg.Logger.Printf("poll: %v", err)
		}
		if err == nil {
			d.markDiscovered(&d.panesDiscovered)
			if !pruned {
				// The first complete poll: the daemon's own agents that
				// went while it was down lose their entries.
				pruned = true
				d.mu.Lock()
				d.forgetLocalLocked()
				d.mu.Unlock()
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
		}
	}
}

// markDiscovered records one side's first complete poll and opens
// discovered once both are in. The local host's record in the merged
// stream becomes listed at the same moment.
func (d *Daemon) markDiscovered(flag *bool) {
	d.mu.Lock()
	*flag = true
	both := d.panesDiscovered && d.worktreesDiscovered
	if both {
		d.localListedLocked()
	}
	d.mu.Unlock()
	if both {
		d.discoveredOnce.Do(func() { close(d.discovered) })
	}
}

// poll runs one cycle over every server. A server that is down or absent is
// not an error: its panes are removed. Any other failure on one server is
// reported after the others have been polled, and keeps discovery pending.
func (d *Daemon) poll(ctx context.Context) error {
	now := time.Now()
	var first error
	for _, t := range d.targets {
		if err := d.pollTarget(ctx, t, now); err != nil && first == nil {
			first = fmt.Errorf("%s: %w", t.Label, err)
		}
	}
	return first
}

func (d *Daemon) pollTarget(ctx context.Context, t *target, now time.Time) error {
	panes, err := t.Tmux.ListPanes(ctx)
	if d.hookOnce(&t.lastHookErr, err, "poll: "+t.Label) {
		err = nil
	}
	if err != nil {
		if tmux.NoServer(err) {
			d.removeUnseen(t, nil)
			t.configuredServer = 0
			if t.Managed {
				d.setManagedRoots(nil, now)
			}
			return nil
		}
		return err
	}
	if t.Managed {
		d.setManagedRoots(panes, now)
	}
	// A managed server started by hand, or restarted, has the user's config
	// and default bindings. Reconcile once per server instance. Unmanaged
	// servers are the user's and are never touched.
	if len(panes) > 0 && t.Managed && panes[0].ServerPID != t.configuredServer {
		if err := t.Tmux.EnsureConfigured(ctx); err != nil {
			d.cfg.Logger.Printf("configure managed server: %v", err)
		} else {
			t.configuredServer = panes[0].ServerPID
		}
	}
	seen := map[string]bool{}
	for _, p := range panes {
		seen[paneKey(t.Label, p.ID)] = true
		d.observe(ctx, t, p, now)
	}
	d.removeUnseen(t, seen)
	return nil
}

// removeUnseen forgets this server's panes that are not in seen. A remove is
// broadcast only for panes that were published.
func (d *Daemon) removeUnseen(t *target, seen map[string]bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for key, st := range d.panes {
		if st.target != t || seen[key] {
			continue
		}
		delete(d.panes, key)
		d.dropPaneLocked(key)
		if _, had := d.agents[key]; !had {
			continue
		}
		delete(d.agents, key)
		d.broadcastLocked(protocol.Message{Type: protocol.TypeRemove, AgentID: d.agentID(key)})
	}
	// A main checkout whose last agent went is out of use.
	d.syncMainsLocked(time.Now(), "")
}

func (d *Daemon) agentID(key string) string { return d.cfg.EnvironmentID + "/" + key }

// observe runs one detection cycle for one pane and publishes a change if
// any. A pane is published as an agent once an agent instance has been
// identified in it; before that it is a pane record, while it is inside a
// worktree root.
func (d *Daemon) observe(ctx context.Context, t *target, p tmux.Pane, now time.Time) {
	key := paneKey(t.Label, p.ID)
	path := d.paths.resolve(panePath(p))
	d.mu.Lock()
	st, ok := d.panes[key]
	if !ok {
		st = &paneState{target: t, activity: protocol.Unknown, activityAt: now}
		d.panes[key] = st
	}
	prev, had := d.agents[key]
	d.mu.Unlock()

	// Liveness and identity. A verified instance is only checked for
	// existence, so a tool taking the foreground never replaces it. A
	// tentative instance (env hint) is re-searched every poll so stronger
	// evidence can replace it. A read error is an unavailable observation,
	// not absence: nothing changes, but the observation this poll
	// publishes for a delivery does not vouch for the agent. An instance
	// found again after being marked gone (a transient omission) is
	// restored without a reset.
	checked := true
	switch {
	case st.hasIdentity && !st.gone && !st.identity.Tentative:
		ok, err := d.cfg.Procs.Exists(p.TTY, st.identity)
		if err != nil {
			d.cfg.Logger.Printf("procs %s: %v", p.ID, err)
			checked = false
		} else if !ok {
			st.gone = true
		}
	default:
		id, found, err := d.cfg.Procs.Find(p.TTY)
		switch {
		case err != nil:
			d.cfg.Logger.Printf("procs %s: %v", p.ID, err)
			checked = false
		case !found && st.hasIdentity && st.identity.Tentative:
			st.gone = true
		case found && st.hasIdentity && st.identity.Same(id):
			st.gone = false
			st.identity.Tentative = id.Tentative
		case found:
			st.identity = id
			st.hasIdentity = true
			st.gone = false
			st.pendingIdle = nil
			st.pendingCount = 0
			st.activity = protocol.Unknown
			st.activityAt = now
		}
	}
	if !st.hasIdentity {
		// Nothing identified yet; keep watching, published as a pane
		// record while inside a worktree.
		d.mu.Lock()
		st.obs = observation{at: now, session: p.Session, serverPID: p.ServerPID}
		st.pane, st.path, st.observed, st.bare = p, path, true, true
		d.publishPaneLocked(key, st, now)
		d.mu.Unlock()
		return
	}
	liveness := protocol.Alive
	if st.gone {
		liveness = protocol.Gone
	}

	// Screen and title.
	var res detect.Result
	if !st.gone {
		screen, cerr := t.Tmux.Capture(ctx, p.ID, d.cfg.CaptureLines)
		if cerr != nil {
			// An unavailable screen is not an empty screen. Keep the last
			// activity rather than letting the idle fallback erase a prompt.
			d.cfg.Logger.Printf("capture %s: %v", p.ID, cerr)
			res = detect.Result{State: detect.Unknown, Reason: "capture_failed", Skip: true}
		} else {
			res = detect.Detect(detect.Input{Agent: st.identity.Agent, Title: p.Title, Screen: screen})
		}
	} else {
		res = detect.Result{State: detect.Unknown, Reason: "no_known_agent"}
	}
	activity := d.nextActivity(st, res, now)

	// Build the record and publish on change.
	a := protocol.Agent{
		ID:            d.agentID(key),
		EnvironmentID: d.cfg.EnvironmentID,
		Server:        t.Label,
		Session:       p.Session,
		Window:        p.WindowIndex,
		PaneID:        p.ID,
		TTY:           p.TTY,
		Cwd:           firstNonEmpty(p.Cwd, p.CurrentPath),
		Title:         p.Title,
		Activity:      activity,
		Liveness:      liveness,
		Rule:          res.Rule,
		Reason:        res.Reason,
		Managed:       p.Managed,
		ActivityAt:    st.activityAt,
		UpdatedAt:     now,
	}
	a.Agent = st.identity.Agent
	a.Identity = &protocol.Identity{PID: st.identity.PID, StartUnix: st.identity.Start.Unix(), Comm: st.identity.Comm, LeaderPID: st.identity.LeaderPID}

	d.mu.Lock()
	defer d.mu.Unlock()
	st.obs = observation{
		at: now, session: p.Session, serverPID: p.ServerPID,
		verified: checked && !st.gone && !st.identity.Tentative, identity: st.identity,
		idle: !res.Skip && res.State == detect.Idle && res.VisibleIdle,
	}
	st.pane, st.path, st.observed, st.bare = p, path, true, false
	a.WorktreeID = d.attributeLocked(st, a)
	d.dropPaneLocked(key)
	if had && sameRecord(prev, a) {
		return
	}
	// A main checkout the agent is in is published before the agent
	// names it, and one it has left is taken back after.
	d.syncMainsLocked(now, a.WorktreeID)
	d.agents[key] = a
	d.broadcastLocked(protocol.Message{Type: protocol.TypeUpsert, Agent: &a})
	d.syncMainsLocked(now, "")
}

// nextActivity applies the state machine: startup grace, skip, and the
// working-to-idle debounce.
func (d *Daemon) nextActivity(st *paneState, res detect.Result, now time.Time) protocol.Activity {
	if !st.hasIdentity || st.gone {
		// Keep the last activity of a gone agent; the liveness axis says gone.
		return st.activity
	}
	if res.Skip {
		return st.activity
	}
	// Do not publish a guess about a freshly started agent. The grace is
	// measured from the process start, so an agent that has run for minutes
	// before the daemon first saw it gets no grace.
	if now.Sub(st.identity.Start) < startupGrace && st.activity == protocol.Unknown && !res.VisibleBlocker && !res.VisibleWorking {
		return st.activity
	}
	next := protocol.Activity(res.State)
	if st.activity == protocol.Working && next == protocol.Idle && !res.VisibleIdle {
		// Plain idle after working: hold until confirmed.
		if st.pendingIdle == nil {
			t := now
			st.pendingIdle = &t
			st.pendingCount = 1
			return st.activity
		}
		st.pendingCount++
		if st.pendingCount < idleConfirmations && now.Sub(*st.pendingIdle) < idleConfirmCap {
			return st.activity
		}
	}
	st.pendingIdle = nil
	st.pendingCount = 0
	if next != st.activity {
		st.activity = next
		st.activityAt = now
	}
	return st.activity
}

func sameRecord(a, b protocol.Agent) bool {
	if a.Activity != b.Activity || a.Liveness != b.Liveness || a.Agent != b.Agent ||
		a.Title != b.Title || a.Cwd != b.Cwd || a.Session != b.Session || a.Window != b.Window ||
		a.Managed != b.Managed || a.Rule != b.Rule || a.WorktreeID != b.WorktreeID {
		return false
	}
	switch {
	case a.Identity == nil && b.Identity == nil:
		return true
	case a.Identity == nil || b.Identity == nil:
		return false
	}
	return *a.Identity == *b.Identity
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// broadcastLocked numbers one of this host's own changes on its
// sequence and sends it to its plain subscribers, and into the merged
// stream when this machine is one of the configured hosts, dropping a
// subscriber that has fallen behind.
func (d *Daemon) broadcastLocked(m protocol.Message) {
	d.seq++
	m.Seq = d.seq
	d.forwardLocalLocked(m)
	// The attention of the daemon's own agents follows their records:
	// after the upsert, so a finish never names an agent a subscriber
	// has not had.
	switch {
	case m.Type == protocol.TypeUpsert && m.Agent != nil:
		d.attendLocked(d.cfg.Host, true, *m.Agent)
	case m.Type == protocol.TypeRemove && m.AgentID != "":
		d.forgetLocked(m.AgentID)
	}
	d.flushAttentionLocked()
	fanout(d.subs, m, d.goneLocked)
}

// fanout sends m to every subscriber in subs; one whose buffer is full
// has fallen behind and is given to gone, which drops it and closes its
// transport, so the peer sees EOF, reconnects, and gets an
// authoritative snapshot rather than a stream with a hole in it.
func fanout(subs map[*subscriber]struct{}, m protocol.Message, gone func(*subscriber)) {
	for s := range subs {
		m, ok := s.sees(m)
		if !ok {
			continue
		}
		select {
		case s.ch <- m:
		default:
			gone(s)
		}
	}
}

// sees is what of m the subscriber is sent: all of it, but to one that
// did not ask for the main checkouts, a main checkout's record is taken
// out, the message not sent when nothing is left, and an agent's
// attribution to one, so it names no record the subscriber has not had
// and the agent is one of no worktree as before. The remove of such a
// record goes to every subscriber; one that never had it passes over it.
func (s *subscriber) sees(m protocol.Message) (protocol.Message, bool) {
	if s.checkouts {
		return m, true
	}
	if a := m.Agent; a != nil && isCheckoutID(a.WorktreeID) {
		c := *a
		c.WorktreeID = ""
		m.Agent = &c
	}
	if m.Worktree == nil || !m.Worktree.Main {
		return m, true
	}
	m.Worktree = nil
	return m, m.Agent != nil || m.Pane != nil || m.Run != nil
}

// agentsFor is agent records for a snapshot of a subscriber: as they are
// for one that asked for the main checkouts, else with an attribution to
// one taken out, as sees takes it out of an upsert.
func agentsFor(agents []protocol.Agent, checkouts bool) []protocol.Agent {
	if checkouts {
		return agents
	}
	for i := range agents {
		if isCheckoutID(agents[i].WorktreeID) {
			agents[i].WorktreeID = ""
		}
	}
	return agents
}

// goneLocked drops a plain subscriber that fell behind: out of the set,
// its channel closed, its transport closed off the lock.
func (d *Daemon) goneLocked(s *subscriber) {
	delete(d.subs, s)
	close(s.ch)
	if s.drop != nil {
		go s.drop()
	}
}

// subscribe registers a subscriber of this host's own records and returns
// its snapshot; checkouts is that it asked for the main checkouts'.
func (d *Daemon) subscribe(drop func(), checkouts bool) (*subscriber, protocol.Message) {
	d.mu.Lock()
	defer d.mu.Unlock()
	s := &subscriber{ch: make(chan protocol.Message, subscriberBuffer), drop: drop, checkouts: checkouts}
	d.subs[s] = struct{}{}
	agents := make([]protocol.Agent, 0, len(d.agents))
	for _, a := range d.agents {
		agents = append(agents, a)
	}
	snap := protocol.Message{Type: protocol.TypeSnapshot, Seq: d.seq, Agents: agentsFor(agents, checkouts), Worktrees: d.worktreesLocked(checkouts),
		Panes: d.paneRecsLocked(), Runs: d.runRecsLocked(), ListingError: d.listErr}
	if d.listed {
		l := d.listing
		snap.Listing = &l
	}
	return s, snap
}

func (d *Daemon) unsubscribe(s *subscriber) {
	if s.merged {
		d.mergedUnsubscribe(s)
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, ok := d.subs[s]; ok {
		delete(d.subs, s)
		close(s.ch)
	}
}

// Serve accepts connections until ctx is done.
func (d *Daemon) Serve(ctx context.Context, ln net.Listener) error {
	go func() {
		<-ctx.Done()
		ln.Close()
	}()
	for {
		c, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			// A unix listener's error names its socket, under the state
			// directory by default; serve ends with it.
			return tmux.PrintablePath(err)
		}
		go func() {
			defer c.Close()
			d.HandleConn(ctx, c, func() { c.Close() })
		}()
	}
}

// HandleConn speaks the protocol on one connection until it closes. closer
// must unblock a pending read on rw; it is called when the subscription is
// dropped or a write fails, so the peer always sees EOF rather than silence.
func (d *Daemon) HandleConn(ctx context.Context, rw io.ReadWriter, closer func()) {
	c := &clientConn{d: d, ctx: ctx, pc: protocol.NewConn(rw), quit: make(chan struct{})}
	var closeOnce sync.Once
	c.drop = func() {
		closeOnce.Do(func() {
			if closer != nil {
				closer()
			}
		})
	}
	defer c.drop()
	// quit closes when this connection is done, so a command stream
	// waiting for its next event lets go of the connection.
	defer close(c.quit)
	hello := protocol.Message{
		Type:          protocol.TypeHello,
		Protocol:      protocol.Version,
		EnvironmentID: d.cfg.EnvironmentID,
		Version:       d.cfg.Version,
		Host:          d.cfg.Host,
		Capabilities:  d.capabilities(),
		PID:           os.Getpid(),
	}
	if err := c.pc.Write(hello); err != nil {
		return
	}
	defer func() {
		if c.sub != nil {
			d.unsubscribe(c.sub)
		}
	}()
	for {
		m, err := c.pc.Read()
		if err != nil {
			return
		}
		h, ok := handlers[m.Type]
		if !ok {
			_ = c.pc.Write(protocol.Message{Type: protocol.TypeError, ID: m.ID, Error: fmt.Sprintf("unknown message type %q", m.Type)})
			continue
		}
		if err := h(c, m); err != nil {
			return
		}
	}
}

// clientConn is one client connection as the handlers see it: the
// protocol connection, drop to close it from any goroutine, quit closed
// when it is done, and its subscription if it has one.
type clientConn struct {
	d    *Daemon
	ctx  context.Context
	pc   *protocol.Conn
	drop func()
	quit chan struct{}
	sub  *subscriber
}

// handlers is the handler by message type. A handler's error ends the
// connection: a write that failed, or the daemon's context done.
var handlers = map[string]func(*clientConn, protocol.Message) error{
	protocol.TypeHello:     (*clientConn).hello,
	protocol.TypePing:      (*clientConn).ping,
	protocol.TypeSubscribe: (*clientConn).subscribe,
	protocol.TypeNew:       (*clientConn).newSession,
	protocol.TypeDismiss:   (*clientConn).dismiss,
	protocol.TypeAdd:       (*clientConn).command,
	protocol.TypeRm:        (*clientConn).command,
	protocol.TypeRun:       (*clientConn).command,
	protocol.TypePrompt:    (*clientConn).command,
	protocol.TypeFollow:    (*clientConn).follow,
	protocol.TypeCancel:    (*clientConn).cancel,
	protocol.TypeSelect:    (*clientConn).selectPane,
	protocol.TypePoke:      (*clientConn).poke,
	protocol.TypeShutdown:  (*clientConn).shutdown,
}

// errNoManaged is the answer to a request that needs the managed server
// on a daemon that does not watch it.
const errNoManaged = "this daemon does not watch the managed laatmux tmux server"

// hello is the client's hello; nothing to do, capabilities flow daemon
// -> client.
func (c *clientConn) hello(protocol.Message) error { return nil }

func (c *clientConn) ping(protocol.Message) error {
	_ = c.pc.Write(protocol.Message{Type: protocol.TypePong})
	return nil
}

func (c *clientConn) subscribe(m protocol.Message) error {
	d := c.d
	if c.sub != nil {
		return nil
	}
	if m.Merged && d.cfg.Hosts == nil {
		_ = c.pc.Write(protocol.Message{Type: protocol.TypeError, Error: "this daemon has no merged capability; it has no hosts in its config"})
		return nil
	}
	var s *subscriber
	var snap protocol.Message
	if m.Merged {
		// The merged snapshot is sent at once with what the daemon
		// knows; the local host's record says whether its own records
		// are complete, as a remote host's does, so a tmux the daemon
		// cannot poll holds up neither the remote hosts nor the host
		// rows.
		s, snap = d.mergedSubscribe(c.ctx, c.drop, m.Checkouts)
	} else {
		select {
		case <-d.discovered:
		case <-c.ctx.Done():
			return c.ctx.Err()
		}
		s, snap = d.subscribe(c.drop, m.Checkouts)
	}
	c.sub = s
	if err := c.pc.Write(snap); err != nil {
		return err
	}
	go func() {
		for msg := range s.ch {
			if err := c.pc.Write(msg); err != nil {
				c.drop()
				return
			}
		}
	}()
	return nil
}

// newSession makes a session on the managed server, the only one
// sessions are ever created on. A name tmux would not store as given is
// refused before anything runs: laatmux new refuses it first, but an
// older client sends it.
func (c *clientConn) newSession(m protocol.Message) error {
	d := c.d
	res := protocol.Message{Type: protocol.TypeResult, ID: m.ID}
	if d.managed == nil {
		res.Error = errNoManaged
		return c.pc.Write(res)
	}
	if err := tmux.CheckSessionName(m.Name); err != nil {
		res.Error = err.Error()
		return c.pc.Write(res)
	}
	// An older client does not check the command.
	if err := config.CheckCmd(m.Cmd); err != nil {
		res.Error = err.Error()
		return c.pc.Write(res)
	}
	made, err := d.managed.Tmux.NewSession(c.ctx, tmux.NewSessionOpts{Name: m.Name, Cwd: m.Cwd, Cmd: m.Cmd, Host: m.Host})
	if err != nil {
		res.Error = err.Error()
	} else {
		res.OK = true
		res.Session = m.Name
		res.PaneID = made.PaneID
	}
	return c.pc.Write(res)
}

// dismiss drops a pending record: a root names the worktree whose
// tasks go; the id is then the request's own, which a client always
// sets.
func (c *clientConn) dismiss(m protocol.Message) error {
	var res protocol.Message
	if m.Root != "" {
		res = c.d.dismissAt(m.ID, m.EnvironmentID, m.Root, m.Listing)
	} else {
		res = c.d.dismiss(m.ID)
	}
	return c.pc.Write(res)
}

// command runs add, rm, run or prompt, or the relay's form of the first
// and last: an add naming a host to run it on, and a prompt without an
// attempt number.
func (c *clientConn) command(m protocol.Message) error {
	d := c.d
	if m.Type == protocol.TypeAdd && m.Relay != "" {
		// The task runs once the answer has been written, or could
		// not be: the acceptance is the file, and a client killed
		// before it read the answer must not leave its task unrun
		// until the daemon restarts. The host is contacted after the
		// answer either way.
		res := d.acceptRelay(m)
		err := c.pc.Write(res)
		if res.OK {
			d.startPending(d.runCtx(), m.ID)
		}
		return err
	}
	if m.Type == protocol.TypePrompt && m.Attempt == 0 && d.relay != nil {
		go func() {
			if err := c.pc.Write(d.relayPrompt(c.ctx, m.ID)); err != nil {
				c.drop()
			}
		}()
		return nil
	}
	res := protocol.Message{Type: protocol.TypeResult, ID: m.ID}
	switch {
	case d.cfg.Store == nil:
		res.Error = "this host has no repos and worktrees directories configured"
	case d.managed == nil && m.Type != protocol.TypeRun:
		res.Error = errNoManaged
	case m.ID == "":
		res.Error = "command id required"
	case m.Type == protocol.TypePrompt && d.journal == nil:
		res.Error = "this daemon has no task capability"
	case m.Type == protocol.TypePrompt && m.Attempt < 1:
		res.Error = "attempt number required"
	case m.Type == protocol.TypePrompt && m.Prompt == "":
		res.Error = "prompt required"
	}
	if res.Error != "" {
		return c.pc.Write(res)
	}
	// The command runs under the daemon's context and outlives this
	// connection. The same id from any connection follows it rather
	// than starting it again, which is what an older client relies on
	// after a lost bridge; a client with follow sends that instead.
	key := m.ID
	if m.Type == protocol.TypePrompt {
		key = promptKey(m.ID, m.Attempt)
	}
	cmd, fresh := d.tasks.cmds.get(key, func(c *command) {
		if m.Type == protocol.TypeRun {
			c.ring = true
			c.job = newRunJob()
		}
	})
	if fresh {
		switch m.Type {
		case protocol.TypeAdd:
			go d.tasks.runAdd(c.ctx, m, cmd)
		case protocol.TypeRm:
			go d.tasks.runRm(c.ctx, m, cmd)
		case protocol.TypePrompt:
			go d.tasks.runPrompt(c.ctx, m, cmd)
		default:
			go d.tasks.runRun(c.ctx, m, cmd)
		}
	}
	c.streamCommand(cmd, 0)
	return nil
}

// streamCommand sends the command's events from after on this
// connection until it is done or the connection is.
func (c *clientConn) streamCommand(cmd *command, after uint64) {
	go func() {
		if err := cmd.stream(c.pc, after, c.quit); err != nil {
			c.drop()
		}
	}()
}

// follow streams a command the memory or the journal has.
func (c *clientConn) follow(m protocol.Message) error {
	d := c.d
	key := m.ID
	if m.Attempt > 0 {
		key = promptKey(m.ID, m.Attempt)
	}
	cmd, ok := d.tasks.cmds.lookup(key)
	if !ok {
		// The journal answers for what the memory has let go.
		res := d.tasks.answerFollow(m)
		if res == nil {
			res = &protocol.Message{Type: protocol.TypeResult, ID: m.ID, Error: protocol.ErrUnknownCommand}
		}
		return c.pc.Write(*res)
	}
	c.streamCommand(cmd, m.After)
	return nil
}

func (c *clientConn) cancel(m protocol.Message) error {
	c.d.tasks.cancelCommand(m.ID)
	return nil
}

// selectPane makes the pane and its window current on the managed
// server, where the attach shows them.
func (c *clientConn) selectPane(m protocol.Message) error {
	d := c.d
	res := protocol.Message{Type: protocol.TypeResult, ID: m.ID}
	switch {
	case d.managed == nil:
		res.Error = errNoManaged
	case m.PaneID == "":
		res.Error = "pane id required"
	default:
		if err := d.managed.Tmux.SelectPane(c.ctx, m.PaneID); err != nil {
			res.Error = err.Error()
		} else {
			res.OK = true
		}
	}
	return c.pc.Write(res)
}

// poke is not answered: the hook that sends it does not wait.
func (c *clientConn) poke(protocol.Message) error {
	c.d.Poke()
	return nil
}

func (c *clientConn) shutdown(m protocol.Message) error {
	d := c.d
	res := protocol.Message{Type: protocol.TypeResult, ID: m.ID}
	if d.cfg.Shutdown == nil {
		res.Error = "this daemon has no shutdown capability"
	} else {
		res.OK = true
	}
	if err := c.pc.Write(res); err != nil {
		return err
	}
	if res.OK {
		d.cfg.Shutdown()
	}
	return nil
}
