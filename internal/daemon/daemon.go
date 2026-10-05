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
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"sync"
	"time"

	"github.com/laat/laatmux/internal/client"
	"github.com/laat/laatmux/internal/detect"
	"github.com/laat/laatmux/internal/github"
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

// Panes is the tmux side of one watched server. tmux.Server implements it;
// tests supply a fake.
type Panes interface {
	ListPanes(ctx context.Context) ([]tmux.Pane, error)
	Capture(ctx context.Context, paneID string, n int) ([]string, error)
	EnsureConfigured(ctx context.Context) error
	NewSession(ctx context.Context, o tmux.NewSessionOpts) (tmux.Session, error)
	KillSession(ctx context.Context, name string) error
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
	// for add. WorktreeInterval is how often git is asked.
	Store            *worktree.Store
	Agents           map[string][]string
	WorktreeInterval time.Duration
	// Commands is the directory of the command journal, one file per
	// add, which with Store and the managed server is the task
	// capability; "" means none.
	Commands string

	// The merged stream. Hosts reads the configured hosts, on every
	// merged subscription; nil means no merged capability. Dial connects
	// to a remote host, client.Dial by default. Sessions lists this
	// machine's local workspace sessions; nil means none. The durations
	// default to the constants in merge.go.
	Hosts           func() ([]client.Host, error)
	Dial            func(ctx context.Context, h client.Host) (*client.Conn, error)
	Sessions        func(ctx context.Context) ([]protocol.Session, error)
	MergedIdle      time.Duration
	SessionInterval time.Duration
	ReconnectMin    time.Duration

	// Shutdown ends the daemon as SIGTERM does, for the shutdown
	// message; nil means no shutdown capability.
	Shutdown func()

	// Pending is the directory of the relay's pending files, which with
	// Hosts is the relay capability; "" means none.
	Pending string

	// Attention is the file the attention state is kept in, which with
	// Hosts is the attention capability; "" means none. Clients lists
	// what this machine's tmux clients show; nil means no agent is ever
	// seen. See attention.go.
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
	worktrees    map[string]protocol.Worktree // by root
	lastList     []worktree.Record
	listed       bool
	managedRoots map[string]string // root -> session
	lastListErr  string            // logged once per change
	poke         chan struct{}
	// Attribution: the listed roots, longest first; the pane records of
	// panes without an agent inside a root, by pane key; the run
	// records by id; and the resolved-path cache. See attribution.go.
	roots     []root
	paneRecs  map[string]protocol.Pane
	runRecs   map[string]protocol.Run
	resolveMu sync.Mutex
	resolved  map[string]resolution
	resolving map[string]bool

	cmds  map[string]*command    // recent add, rm and run by id
	locks map[string]*sync.Mutex // per repository source
	// repos is held shared by every add for its repository's lock and
	// alone by rm, which must see every add in flight complete.
	repos      sync.RWMutex
	commandTTL time.Duration
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
	// logged once per change.
	lastAttnErr    string
	lastClientsErr string
	// The worktrees' git status refreshes, by root, and the last error
	// logged; see gitstatus.go.
	gits       map[string]*gitEntry
	lastGitErr string
	// The branch records, by key, nil without the branches capability;
	// why GitHub cannot be read; the file's last error. See
	// branches.go.
	branches        map[string]*branchEntry
	githubErr       string
	lastBranchesErr string
	branchesListed  bool                 // every host has listed once since start
	branchErrs      map[string]bool      // per-branch errors of the last round, logged
	roundErrs       map[string]bool      // and of the round under way
	hostErrs        map[string]string    // gh's failure by host, what githubErr is joined from
	pagedNone       map[string]time.Time // when a branch's forks' pages held none of its own
	ctx             context.Context      // Run's context, for goroutines that outlive a connection
	generation      int64
	revision        uint64
	listing         protocol.Listing
	listErr         string
	pollMu          sync.Mutex
	// Runs by root, and the removal generation per root that rm bumps
	// once git has removed the worktree; see runs.go.
	runs     map[string]map[*runJob]struct{}
	rootGen  map[string]uint64
	stopping bool // StopRuns has begun; no run registers and no paste starts after it
	// The trust watchers: their shared context, cancelled by StopRuns,
	// and how many run, which StopRuns waits for; see trust.go.
	trustCtx    context.Context
	trustCancel context.CancelFunc
	trusting    int
	pasting     int // pastes in flight, which StopRuns waits for
	// pasted is when a pane was last pasted into, by pane key: a
	// delivery needs an observation made after it. waits counts the
	// deliveries that have begun waiting for a pane, for tests.
	pasted    map[string]time.Time
	waits     int
	killDelay time.Duration

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
		poke:         make(chan struct{}, 1),
		paneRecs:     map[string]protocol.Pane{},
		runRecs:      map[string]protocol.Run{},
		gits:         map[string]*gitEntry{},
		resolved:     map[string]resolution{},
		resolving:    map[string]bool{},
		cmds:         map[string]*command{},
		locks:        map[string]*sync.Mutex{},
		commandTTL:   DefaultCommandTTL,
		runs:         map[string]map[*runJob]struct{}{},
		rootGen:      map[string]uint64{},
		pasted:       map[string]time.Time{},
		killDelay:    DefaultKillDelay,

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
		j, err := openJournal(cfg.Commands, cfg.Logger)
		if err != nil {
			cfg.Logger.Printf("journal: %v; task capability disabled", err)
		} else {
			d.journal = j
		}
	}
	if cfg.Attention != "" && cfg.Hosts != nil {
		a, err := openAttention(cfg.Attention)
		if err != nil {
			cfg.Logger.Printf("attention: %v; starting over", err)
			a = &attention{path: cfg.Attention, entries: map[string]*attnEntry{}, poke: make(chan struct{}, 1)}
		}
		d.attn = a
	}
	if cfg.GitHub != nil && cfg.Branches != "" && cfg.Hosts != nil {
		b, err := openBranches(cfg.Branches)
		if err != nil {
			cfg.Logger.Printf("branches: %v; starting over", err)
			b = map[string]*branchEntry{}
		}
		d.branches = b
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
		caps = append(caps, protocol.CapWorktrees, protocol.CapRun, protocol.CapAttribution, protocol.CapGitStatus)
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
			// It forwards what the hosts attribute and the git objects
			// they read, though it has no worktrees of its own.
			caps = append(caps, protocol.CapAttribution, protocol.CapGitStatus)
		}
	}
	if d.relay != nil {
		caps = append(caps, protocol.CapRelay, protocol.CapDismissRoot)
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
	if d.cfg.Store != nil {
		go d.runWorktrees(ctx)
		go d.runGitStatus(ctx)
	} else {
		d.markDiscovered(&d.worktreesDiscovered)
	}
	if d.journal != nil {
		d.sweepBuffers(ctx)
		go d.runJournal(ctx)
	}
	if d.relay != nil {
		d.startRelays(ctx)
		go d.runRelaySweep(ctx)
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
}

func (d *Daemon) agentID(key string) string { return d.cfg.EnvironmentID + "/" + key }

// observe runs one detection cycle for one pane and publishes a change if
// any. A pane is published as an agent once an agent instance has been
// identified in it; before that it is a pane record, while it is inside a
// worktree root.
func (d *Daemon) observe(ctx context.Context, t *target, p tmux.Pane, now time.Time) {
	key := paneKey(t.Label, p.ID)
	path := d.resolve(panePath(p))
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
	a.WorktreeID = d.worktreeOfLocked(path)
	d.dropPaneLocked(key)
	if had && sameRecord(prev, a) {
		return
	}
	d.agents[key] = a
	d.broadcastLocked(protocol.Message{Type: protocol.TypeUpsert, Agent: &a})
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
		select {
		case s.ch <- m:
		default:
			gone(s)
		}
	}
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

func (d *Daemon) subscribe(drop func()) (*subscriber, protocol.Message) {
	d.mu.Lock()
	defer d.mu.Unlock()
	s := &subscriber{ch: make(chan protocol.Message, subscriberBuffer), drop: drop}
	d.subs[s] = struct{}{}
	agents := make([]protocol.Agent, 0, len(d.agents))
	for _, a := range d.agents {
		agents = append(agents, a)
	}
	snap := protocol.Message{Type: protocol.TypeSnapshot, Seq: d.seq, Agents: agents, Worktrees: d.worktreesLocked(),
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
			return err
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
	pc := protocol.NewConn(rw)
	var closeOnce sync.Once
	drop := func() {
		closeOnce.Do(func() {
			if closer != nil {
				closer()
			}
		})
	}
	defer drop()
	// quit closes when this connection is done, so a command stream
	// waiting for its next event lets go of the connection.
	quit := make(chan struct{})
	defer close(quit)
	hello := protocol.Message{
		Type:          protocol.TypeHello,
		Protocol:      protocol.Version,
		EnvironmentID: d.cfg.EnvironmentID,
		Version:       d.cfg.Version,
		Host:          d.cfg.Host,
		Capabilities:  d.capabilities(),
		PID:           os.Getpid(),
	}
	if err := pc.Write(hello); err != nil {
		return
	}
	var sub *subscriber
	defer func() {
		if sub != nil {
			d.unsubscribe(sub)
		}
	}()
	for {
		m, err := pc.Read()
		if err != nil {
			return
		}
		switch m.Type {
		case protocol.TypeHello:
			// Client's hello; nothing to do, capabilities flow daemon -> client.
		case protocol.TypePing:
			_ = pc.Write(protocol.Message{Type: protocol.TypePong})
		case protocol.TypeSubscribe:
			if sub != nil {
				continue
			}
			if m.Merged && d.cfg.Hosts == nil {
				_ = pc.Write(protocol.Message{Type: protocol.TypeError, Error: "this daemon has no merged capability; it has no hosts in its config"})
				continue
			}
			var s *subscriber
			var snap protocol.Message
			if m.Merged {
				// The merged snapshot is sent at once with what the
				// daemon knows; the local host's record says whether
				// its own records are complete, as a remote host's
				// does, so a tmux the daemon cannot poll holds up
				// neither the remote hosts nor the host rows.
				s, snap = d.mergedSubscribe(ctx, drop)
			} else {
				select {
				case <-d.discovered:
				case <-ctx.Done():
					return
				}
				s, snap = d.subscribe(drop)
			}
			sub = s
			if err := pc.Write(snap); err != nil {
				return
			}
			go func() {
				for msg := range s.ch {
					if err := pc.Write(msg); err != nil {
						drop()
						return
					}
				}
			}()
		case protocol.TypeNew:
			// Sessions are only ever created on the managed server.
			res := protocol.Message{Type: protocol.TypeResult, ID: m.ID}
			if d.managed == nil {
				res.Error = "this daemon does not watch the managed laatmux tmux server"
				if err := pc.Write(res); err != nil {
					return
				}
				continue
			}
			made, err := d.managed.Tmux.NewSession(ctx, tmux.NewSessionOpts{Name: m.Name, Cwd: m.Cwd, Cmd: m.Cmd, Host: m.Host})
			if err != nil {
				res.Error = err.Error()
			} else {
				res.OK = true
				res.Session = m.Name
				res.PaneID = made.PaneID
			}
			if err := pc.Write(res); err != nil {
				return
			}
		case protocol.TypeDismiss:
			// A root names the worktree whose tasks go; the id is then
			// the request's own, which a client always sets.
			var res protocol.Message
			if m.Root != "" {
				res = d.dismissAt(m.ID, m.EnvironmentID, m.Root, m.Listing)
			} else {
				res = d.dismiss(m.ID)
			}
			if err := pc.Write(res); err != nil {
				return
			}
		case protocol.TypeAdd, protocol.TypeRm, protocol.TypeRun, protocol.TypePrompt:
			// The relay's messages: an add naming a host to run it on,
			// and a prompt without an attempt number.
			if m.Type == protocol.TypeAdd && m.Relay != "" {
				// The task runs once the answer has been written, or
				// could not be: the acceptance is the file, and a
				// client killed before it read the answer must not
				// leave its task unrun until the daemon restarts. The
				// host is contacted after the answer either way.
				res := d.acceptRelay(m)
				err := pc.Write(res)
				if res.OK {
					d.startPending(d.runCtx(), m.ID)
				}
				if err != nil {
					return
				}
				continue
			}
			if m.Type == protocol.TypePrompt && m.Attempt == 0 && d.relay != nil {
				go func() {
					if err := pc.Write(d.relayPrompt(ctx, m.ID)); err != nil {
						drop()
					}
				}()
				continue
			}
			res := protocol.Message{Type: protocol.TypeResult, ID: m.ID}
			switch {
			case d.cfg.Store == nil:
				res.Error = "this host has no repos and worktrees directories configured"
			case d.managed == nil && m.Type != protocol.TypeRun:
				res.Error = "this daemon does not watch the managed laatmux tmux server"
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
				if err := pc.Write(res); err != nil {
					return
				}
				continue
			}
			// The command runs under the daemon's context and outlives
			// this connection. The same id from any connection follows it
			// rather than starting it again, which is what an older client
			// relies on after a lost bridge; a client with follow sends
			// that instead.
			key := m.ID
			if m.Type == protocol.TypePrompt {
				key = promptKey(m.ID, m.Attempt)
			}
			c, fresh := d.command(key, func(c *command) {
				if m.Type == protocol.TypeRun {
					c.ring = true
					c.job = newRunJob()
				}
			})
			if fresh {
				switch m.Type {
				case protocol.TypeAdd:
					go d.runAdd(ctx, m, c)
				case protocol.TypeRm:
					go d.runRm(ctx, m, c)
				case protocol.TypePrompt:
					go d.runPrompt(ctx, m, c)
				default:
					go d.runRun(ctx, m, c)
				}
			}
			go func() {
				if err := c.stream(pc, 0, quit); err != nil {
					drop()
				}
			}()
		case protocol.TypeFollow:
			key := m.ID
			if m.Attempt > 0 {
				key = promptKey(m.ID, m.Attempt)
			}
			c, ok := d.lookup(key)
			if !ok {
				// The journal answers for what the memory has let go.
				res := d.answerFollow(m)
				if res == nil {
					res = &protocol.Message{Type: protocol.TypeResult, ID: m.ID, Error: protocol.ErrUnknownCommand}
				}
				if err := pc.Write(*res); err != nil {
					return
				}
				continue
			}
			go func() {
				if err := c.stream(pc, m.After, quit); err != nil {
					drop()
				}
			}()
		case protocol.TypeCancel:
			d.cancelCommand(m.ID)
		case protocol.TypeSelect:
			// The pane and its window made current on the managed
			// server, where the attach shows them.
			res := protocol.Message{Type: protocol.TypeResult, ID: m.ID}
			switch {
			case d.managed == nil:
				res.Error = "this daemon does not watch the managed laatmux tmux server"
			case m.PaneID == "":
				res.Error = "pane id required"
			default:
				if err := d.managed.Tmux.SelectPane(ctx, m.PaneID); err != nil {
					res.Error = err.Error()
				} else {
					res.OK = true
				}
			}
			if err := pc.Write(res); err != nil {
				return
			}
		case protocol.TypePoke:
			// Not answered: the hook that sends it does not wait.
			d.Poke()
		case protocol.TypeShutdown:
			res := protocol.Message{Type: protocol.TypeResult, ID: m.ID}
			if d.cfg.Shutdown == nil {
				res.Error = "this daemon has no shutdown capability"
			} else {
				res.OK = true
			}
			if err := pc.Write(res); err != nil {
				return
			}
			if res.OK {
				d.cfg.Shutdown()
			}
		default:
			_ = pc.Write(protocol.Message{Type: protocol.TypeError, ID: m.ID, Error: fmt.Sprintf("unknown message type %q", m.Type)})
		}
	}
}
