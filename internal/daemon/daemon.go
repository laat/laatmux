// Package daemon is the per-host laatmux server. It polls one tmux server,
// derives agent state for every pane, and streams snapshots and updates to
// subscribers. Detection never crosses the network: a daemon only ever looks
// at its own host's tmux.
package daemon

import (
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"sync"
	"time"

	"github.com/laat/laatmux/internal/detect"
	"github.com/laat/laatmux/internal/procs"
	"github.com/laat/laatmux/internal/protocol"
	"github.com/laat/laatmux/internal/tmux"
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

// Panes is the tmux side of the daemon. tmux.Server implements it; tests
// supply a fake.
type Panes interface {
	ListPanes(ctx context.Context) ([]tmux.Pane, error)
	Capture(ctx context.Context, paneID string, n int) ([]string, error)
	EnsureConfigured(ctx context.Context) error
	NewSession(ctx context.Context, o tmux.NewSessionOpts) (string, error)
	Managed() bool
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
	Server        tmux.Server
	Tmux          Panes     // defaults to Server
	Procs         Processes // defaults to the OS process table
	Interval      time.Duration
	CaptureLines  int
	EnvironmentID string
	Host          string // label this host uses for itself; informational
	Version       string
	Logger        *log.Logger
}

// Daemon holds the derived state for one tmux server.
type Daemon struct {
	cfg Config

	mu     sync.Mutex
	seq    uint64
	agents map[string]protocol.Agent // by pane id
	panes  map[string]*paneState
	subs   map[*subscriber]struct{}

	// discovered closes after the first complete poll, so a snapshot is never
	// an empty or partial view of a host that has panes.
	discovered     chan struct{}
	discoveredOnce sync.Once

	// configuredServer is the tmux server pid the managed configuration was
	// last applied to. A different pid is a new server, started by hand or
	// by new-session, and gets reconciled on discovery.
	configuredServer int
}

type paneState struct {
	identity     procs.Identity
	hasIdentity  bool // an agent instance is known; it may be gone
	gone         bool // the known instance no longer exists
	instanceAt   time.Time
	activity     protocol.Activity
	activityAt   time.Time
	pendingIdle  *time.Time
	pendingCount int
	lastResult   detect.Result
}

type subscriber struct {
	ch   chan protocol.Message
	drop func() // closes the transport so the peer sees EOF and resnapshots
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
	if cfg.Tmux == nil {
		cfg.Tmux = cfg.Server
	}
	if cfg.Procs == nil {
		cfg.Procs = osProcs{}
	}
	return &Daemon{
		cfg:    cfg,
		agents: map[string]protocol.Agent{},
		panes:  map[string]*paneState{},
		subs:   map[*subscriber]struct{}{},

		discovered: make(chan struct{}),
	}
}

// Run polls until ctx is done.
func (d *Daemon) Run(ctx context.Context) error {
	t := time.NewTicker(d.cfg.Interval)
	defer t.Stop()
	for {
		err := d.poll(ctx)
		if err != nil && ctx.Err() == nil {
			d.cfg.Logger.Printf("poll: %v", err)
		}
		if err == nil {
			d.discoveredOnce.Do(func() { close(d.discovered) })
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
		}
	}
}

func (d *Daemon) poll(ctx context.Context) error {
	now := time.Now()
	panes, err := d.cfg.Tmux.ListPanes(ctx)
	if err != nil {
		if tmux.NoServer(err) {
			d.removeAll(now)
			d.configuredServer = 0
			return nil
		}
		return err
	}
	// A managed server started by hand, or restarted, has the user's config
	// and default bindings. Reconcile once per server instance.
	if len(panes) > 0 && d.cfg.Tmux.Managed() && panes[0].ServerPID != d.configuredServer {
		if err := d.cfg.Tmux.EnsureConfigured(ctx); err != nil {
			d.cfg.Logger.Printf("configure managed server: %v", err)
		} else {
			d.configuredServer = panes[0].ServerPID
		}
	}
	seen := map[string]bool{}
	for _, p := range panes {
		seen[p.ID] = true
		d.observe(ctx, p, now)
	}
	d.mu.Lock()
	var removed []string
	for id := range d.agents {
		if !seen[id] {
			removed = append(removed, id)
		}
	}
	for _, id := range removed {
		delete(d.agents, id)
		delete(d.panes, id)
		d.seq++
		d.broadcastLocked(protocol.Message{Type: protocol.TypeRemove, Seq: d.seq, AgentID: d.agentID(id)})
	}
	d.mu.Unlock()
	return nil
}

func (d *Daemon) removeAll(now time.Time) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for id := range d.agents {
		delete(d.agents, id)
		delete(d.panes, id)
		d.seq++
		d.broadcastLocked(protocol.Message{Type: protocol.TypeRemove, Seq: d.seq, AgentID: d.agentID(id)})
	}
}

func (d *Daemon) agentID(paneID string) string { return d.cfg.EnvironmentID + "/" + paneID }

// observe runs one detection cycle for one pane and publishes a change if any.
func (d *Daemon) observe(ctx context.Context, p tmux.Pane, now time.Time) {
	d.mu.Lock()
	st, ok := d.panes[p.ID]
	if !ok {
		st = &paneState{activity: protocol.Unknown, activityAt: now}
		d.panes[p.ID] = st
	}
	prev, had := d.agents[p.ID]
	d.mu.Unlock()

	// Liveness and identity. A verified instance is only checked for
	// existence, so a tool taking the foreground never replaces it. A
	// tentative instance (env hint) is re-searched every poll so stronger
	// evidence can replace it. A read error is an unavailable observation,
	// not absence: nothing changes. An instance found again after being
	// marked gone (a transient omission) is restored without a reset.
	switch {
	case st.hasIdentity && !st.gone && !st.identity.Tentative:
		ok, err := d.cfg.Procs.Exists(p.TTY, st.identity)
		if err != nil {
			d.cfg.Logger.Printf("procs %s: %v", p.ID, err)
		} else if !ok {
			st.gone = true
		}
	default:
		id, found, err := d.cfg.Procs.Find(p.TTY)
		switch {
		case err != nil:
			d.cfg.Logger.Printf("procs %s: %v", p.ID, err)
		case !found && st.hasIdentity && st.identity.Tentative:
			st.gone = true
		case found && st.hasIdentity && st.identity.Same(id):
			st.gone = false
			st.identity.Tentative = id.Tentative
		case found:
			st.identity = id
			st.hasIdentity = true
			st.gone = false
			st.instanceAt = now
			st.pendingIdle = nil
			st.pendingCount = 0
			st.activity = protocol.Unknown
			st.activityAt = now
		}
	}
	liveness := protocol.None
	switch {
	case st.hasIdentity && !st.gone:
		liveness = protocol.Alive
	case st.hasIdentity:
		liveness = protocol.Gone
	}

	// Screen and title.
	var res detect.Result
	if st.hasIdentity && !st.gone {
		screen, cerr := d.cfg.Tmux.Capture(ctx, p.ID, d.cfg.CaptureLines)
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
	st.lastResult = res
	activity := d.nextActivity(st, res, now)

	// Build the record and publish on change.
	a := protocol.Agent{
		ID:            d.agentID(p.ID),
		EnvironmentID: d.cfg.EnvironmentID,
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
	if st.hasIdentity {
		a.Agent = st.identity.Agent
		a.Identity = &protocol.Identity{PID: st.identity.PID, StartUnix: st.identity.Start.Unix(), Comm: st.identity.Comm, LeaderPID: st.identity.LeaderPID}
	}
	_ = prev

	d.mu.Lock()
	defer d.mu.Unlock()
	if had && sameRecord(prev, a) {
		return
	}
	d.agents[p.ID] = a
	d.seq++
	d.broadcastLocked(protocol.Message{Type: protocol.TypeUpsert, Seq: d.seq, Agent: &a})
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
		a.Managed != b.Managed || a.Rule != b.Rule {
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

// Snapshot returns the current state.
func (d *Daemon) Snapshot() (uint64, []protocol.Agent) {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]protocol.Agent, 0, len(d.agents))
	for _, a := range d.agents {
		out = append(out, a)
	}
	return d.seq, out
}

func (d *Daemon) broadcastLocked(m protocol.Message) {
	for s := range d.subs {
		select {
		case s.ch <- m:
		default:
			// Slow subscriber. Drop it and close its transport so the peer
			// sees EOF, reconnects, and gets an authoritative snapshot.
			delete(d.subs, s)
			close(s.ch)
			if s.drop != nil {
				go s.drop()
			}
		}
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
	return s, protocol.Message{Type: protocol.TypeSnapshot, Seq: d.seq, Agents: agents}
}

func (d *Daemon) unsubscribe(s *subscriber) {
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
	hello := protocol.Message{
		Type:          protocol.TypeHello,
		Protocol:      protocol.Version,
		EnvironmentID: d.cfg.EnvironmentID,
		Version:       d.cfg.Version,
		Host:          d.cfg.Host,
		Capabilities:  []string{protocol.CapStatus, protocol.CapNew},
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
			select {
			case <-d.discovered:
			case <-ctx.Done():
				return
			}
			s, snap := d.subscribe(drop)
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
			res := protocol.Message{Type: protocol.TypeResult, ID: m.ID}
			paneID, err := d.cfg.Tmux.NewSession(ctx, tmux.NewSessionOpts{Name: m.Name, Cwd: m.Cwd, Cmd: m.Cmd, Host: m.Host})
			if err != nil {
				res.Error = err.Error()
			} else {
				res.OK = true
				res.Session = m.Name
				res.PaneID = paneID
			}
			if err := pc.Write(res); err != nil {
				return
			}
		default:
			_ = pc.Write(protocol.Message{Type: protocol.TypeError, ID: m.ID, Error: fmt.Sprintf("unknown message type %q", m.Type)})
		}
	}
}
