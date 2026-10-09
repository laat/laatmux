package daemon

import (
	"context"
	"sync"
	"time"

	"github.com/laat/laatmux/internal/protocol"
	"github.com/laat/laatmux/internal/tmux"
)

// taskRunner is the daemon's commands: add, rm, run and the prompt
// deliveries, with the runs and trust watchers they start. It shares
// the daemon's lock, mu, its config, journal, managed target and its
// pane and git tables. Its own state below is used under mu, except
// where a field says otherwise. It calls back into the core for the
// rest, through taskCore. See commands.go, task.go, runs.go and
// trust.go.
type taskRunner struct {
	mu      *sync.Mutex // the daemon's
	cfg     *Config     // the daemon's
	journal *journal    // the daemon's, nil without the task capability
	managed *target     // the daemon's, nil when the laatmux server is not watched
	targets []*target   // the daemon's, every watched server, the managed one among them
	// panes and gits are the daemon's tables, used under mu (a git
	// entry's due is set here).
	panes map[string]*paneState
	gits  map[string]*gitEntry
	core  taskCore

	cmds  *commandTable // recent add, rm, run and prompt by key, with a lock of its own
	locks *keyedLocks   // the repository, name, delivery and attempt locks, with a lock of its own
	// repos is held shared by every add for its repository's lock and
	// alone by rm, which must see every add in flight complete.
	repos sync.RWMutex
	// Runs by root, and the removal generation per root that rm bumps
	// once git has removed the worktree; see runs.go.
	runs     map[string]map[*runJob]struct{}
	rootGen  map[string]uint64
	stopping bool // stopRuns has begun; no run registers and no paste starts after it
	// closing is the roots prune's rm has closed, under mu: no run
	// registers and no session is made in one; see closeRoot.
	closing map[string]bool
	// paths is the daemon's resolver, which never waits on the file
	// system.
	paths *resolver
	// The trust watchers: their shared context, cancelled by stopRuns,
	// and how many run, which stopRuns waits for; see trust.go.
	trustCtx    context.Context
	trustCancel context.CancelFunc
	trusting    int
	pasting     int // pastes in flight, which stopRuns waits for
	// pasted is when a pane was last pasted into, by pane key: a
	// delivery needs an observation made after it. waits counts the
	// deliveries that have begun waiting for a pane, for tests.
	pasted map[string]time.Time
	waits  int
}

// taskCore is what the runner asks of the daemon beside the state it
// shares: the listing's stamp and pokes, the managed roots after a
// launch, a git refresh, the run records, the context a task outlives
// its connection under, and the agents' commands as the config last
// read said.
type taskCore interface {
	stepRevision() protocol.Listing
	pokeWorktrees()
	setManagedRoots(panes []tmux.Pane, now time.Time)
	gitDue(root string)
	markRemoving(root string, on bool)
	runStarted(r *runJob, at time.Time)
	runEndedLocked(r *runJob)
	runCtx() context.Context
	agentCmds() map[string][]string
}

// newRunner is the daemon's runner, sharing what it shares; called by
// New once the journal and the managed target are known.
func newRunner(d *Daemon) *taskRunner {
	return &taskRunner{mu: &d.mu, cfg: &d.cfg, journal: d.journal, managed: d.managed, targets: d.targets, panes: d.panes, gits: d.gits, core: d,
		cmds: newCommandTable(d.cfg.Timings.CommandTTL), locks: newKeyedLocks(),
		runs: map[string]map[*runJob]struct{}{}, rootGen: map[string]uint64{}, closing: map[string]bool{}, paths: d.paths, pasted: map[string]time.Time{}}
}

// listManaged lists the managed server's panes for one command. A
// listing a user's after-list-panes hook failed after, on a managed
// server started by hand with their config, is every pane: they are
// taken, and the hook's error is not logged here, since the poll lists
// the same server the same way and logs it once (hookOnce).
func (rn *taskRunner) listManaged(ctx context.Context) ([]tmux.Pane, error) {
	panes, err := rn.managed.Tmux.ListPanes(ctx)
	if tmux.HookOnly(err) {
		err = nil
	}
	return panes, err
}

// StopRuns closes the registry, cancels every run and waits for them,
// and waits for every paste in flight, all bounded by ctx: what a
// clean shutdown does, so a restart for an upgrade leaves no orphan
// and no prompt in a buffer on the server.
func (d *Daemon) StopRuns(ctx context.Context) { d.tasks.stopRuns(ctx) }
