package daemon

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/laat/laatmux/internal/procs"
	"github.com/laat/laatmux/internal/protocol"
	"github.com/laat/laatmux/internal/tmux"
	"github.com/laat/laatmux/internal/worktree"
)

// PromptPlaceholder is the argument of an agent's cmd that the prompt
// replaces, as one argument however many lines it has. Without a prompt
// the argument is removed. A cmd without it gets the prompt typed into
// the pane once the agent is ready.
const PromptPlaceholder = "{prompt}"

// readyWait bounds how long a delivery waits for the pane to be ready;
// a variable so tests can shorten it.
var readyWait = time.Minute

// readyPoll is how often the wait looks at the detector's observation.
const readyPoll = 50 * time.Millisecond

// attemptBufferPrefix names the paste buffers of deliveries; a daemon
// that starts deletes every buffer with it, since one killed between
// loading and deleting leaves its buffer behind.
const attemptBufferPrefix = "laatmux-attempt-"

// errRecorded says an add was answered from the journal: the result is
// the recorded one, to be emitted as it is.
var errRecorded = errors.New("recorded")

// withPrompt substitutes the prompt for every placeholder argument, or
// removes the argument when there is no prompt, and reports whether the
// cmd had one.
func withPrompt(cmd []string, prompt string) (argv []string, placeholder bool) {
	for _, a := range cmd {
		if a != PromptPlaceholder {
			argv = append(argv, a)
			continue
		}
		placeholder = true
		if prompt != "" {
			argv = append(argv, prompt)
		}
	}
	return argv, placeholder
}

// runAdd runs an add under the journal and records the result on c. ctx
// is the daemon's: a command runs to completion whatever happens to the
// connection that sent it, so a client that lost its bridge can repeat
// the id and pick the stream up, and a relay can resend it after a
// daemon restart and have it resumed.
func (d *Daemon) runAdd(ctx context.Context, m protocol.Message, c *command) {
	r := &addRun{d: d, m: m, c: c, res: protocol.Message{Type: protocol.TypeResult, ID: m.ID}}
	err := r.run(ctx)
	r.unlock()
	if errors.Is(err, errRecorded) {
		c.emit(r.res)
		d.evict(m.ID, c)
		return
	}
	res := resultOf(r.res, err)
	if res.OK {
		// The mutation is done: a listing read from here on reflects
		// it, and the result says which one that is.
		l := d.stepRevision()
		res.Listing = &l
	}
	if r.created {
		now := time.Now()
		r.set(func(e *entry) {
			rec := res
			e.Result = &rec
			e.TerminalAt = now
		})
	}
	d.pokeWorktrees()
	c.emit(res)
	d.evict(m.ID, c)
}

// addRun is one add in flight: the request, its journal entry as the
// daemon last wrote it, and what the stages have decided so far.
type addRun struct {
	d       *Daemon
	m       protocol.Message
	c       *command
	e       entry
	created bool // the journal has the entry; set keeps e in step with it
	repo    worktree.Repo
	branch  string
	root    string
	cmd     []string // the configured command, placeholder and all
	res     protocol.Message
	lock    *sync.Mutex
	locked  bool
}

// set applies a change to the journal entry and keeps the local copy.
func (r *addRun) set(change func(*entry)) {
	if !r.created {
		return
	}
	e, err := r.d.journal.update(r.m.ID, change)
	if err != nil {
		r.d.cfg.Logger.Printf("journal: %s: %v", r.m.ID, err)
		return
	}
	r.e = e
}

// emit numbers and publishes one progress message, recording the stage
// begun in the journal so a follow after a daemon death can say where.
func (r *addRun) emit(p protocol.Message) {
	p.Type, p.ID = protocol.TypeProgress, r.m.ID
	if r.created && p.Stage != r.e.Stage {
		stage := p.Stage
		r.set(func(e *entry) { e.Stage = stage })
	}
	r.c.emit(p)
}

func (r *addRun) report(stage, state, detail string) {
	r.emit(protocol.Message{Stage: stage, State: state, Detail: detail})
}

func (r *addRun) unlock() {
	if r.locked {
		r.lock.Unlock()
		r.locked = false
	}
}

// run is the stages in order. The journal is read before anything is
// done for an id it has seen: a terminal entry is its recorded result
// and nothing runs; one that is not terminal is resumed, the steps by
// inspection and the allocation and the launch by what was recorded.
func (r *addRun) run(ctx context.Context) error {
	d, m := r.d, r.m
	j := d.journal
	now := time.Now()
	known := false
	if j != nil {
		if cur, ok := j.get(m.ID); ok {
			if cur.terminal() {
				r.res = cur.recorded()
				return errRecorded
			}
			r.e, known, r.created = cur, true, true
		} else if expired(m.SubmittedAt, now) {
			return errors.New(protocol.ErrSubmissionExpired)
		}
	} else if m.Prompt != "" || m.Generated {
		return stageErr(protocol.StageResolve, errors.New("this host's daemon has no task capability; a prompt or a generated branch needs one"))
	}

	// resolve
	stage := protocol.StageResolve
	repo, ok := d.cfg.Store.Repo(m.Repo)
	if !ok {
		return stageErr(stage, fmt.Errorf("unknown repository %q: not in this host's config", m.Repo))
	}
	r.repo = repo
	r.cmd = m.Cmd
	if len(r.cmd) == 0 {
		if r.cmd, ok = d.cfg.Agents[m.AgentName]; !ok {
			return stageErr(stage, fmt.Errorf("unknown agent %q: not in this host's config", m.AgentName))
		}
	}
	branch, generated := m.Branch, m.Generated
	if known {
		branch, generated = r.e.Branch, r.e.Generated
		if r.e.HasPrompt && m.Prompt == "" {
			return stageErr(stage, errors.New("the add was submitted with a prompt; a resend must carry it"))
		}
	}
	if branch == "" {
		if generated {
			return stageErr(stage, errors.New("branch proposal required"))
		}
		return stageErr(stage, errors.New("branch required"))
	}
	if err := worktree.CheckBranch(ctx, branch); err != nil {
		return stageErr(stage, err)
	}
	if j != nil && !known {
		r.e = entry{
			ID: m.ID, SubmittedAt: m.SubmittedAt, FirstSeen: now,
			Source: repo.Source, Repo: repo.Name, Agent: m.AgentName, HasPrompt: m.Prompt != "",
			Branch: branch, Generated: generated, Allocated: !generated, Stage: stage,
		}
		if err := j.create(r.e); err != nil {
			return stageErr(stage, err)
		}
		r.created = true
	}
	r.branch = branch

	r.lock = d.repoLock(repo.Source)
	r.lock.Lock()
	r.locked = true
	p, err := d.cfg.Store.Prepare(ctx, repo, r.report)
	if err != nil {
		return err
	}

	// allocate: the branch decided, against branches that are current
	// after the fetch and under the repository lock, and written before
	// it is made, so a resend takes the allocated name.
	stage = protocol.StageAllocate
	detail := "branch " + branch + " given"
	state := protocol.StateSkip
	switch {
	case generated && r.e.Allocated:
		detail = "branch " + branch + " allocated before"
	case generated:
		local, remote, err := worktree.Branches(ctx, p.Checkout)
		if err != nil {
			return stageErr(stage, err)
		}
		entries, err := worktree.ListWorktrees(ctx, p.Checkout)
		if err != nil {
			return stageErr(stage, err)
		}
		taken := map[string]bool{}
		for _, n := range local {
			taken[n] = true
		}
		for _, n := range remote {
			taken[n] = true
		}
		for _, e := range entries {
			if e.Branch != "" {
				taken[e.Branch] = true
			}
		}
		for _, n := range j.reserved(repo.Source, m.ID) {
			taken[n] = true
		}
		name := worktree.Allocate(branch, func(n string) bool { return taken[n] })
		r.set(func(e *entry) { e.Branch, e.Allocated = name, true })
		if !r.e.Allocated {
			return stageErr(stage, errors.New("journal: the allocation could not be written"))
		}
		branch, r.branch = name, name
		detail = "branch " + name
		if name != m.Branch {
			detail += " for proposal " + m.Branch
		}
		state = protocol.StateDone
	}
	root, err := d.cfg.Store.Place(ctx, p, repo, branch)
	if err != nil {
		return stageErr(stage, err)
	}
	r.root = root
	r.set(func(e *entry) { e.Root = root })
	r.res.Branch = branch
	r.emit(protocol.Message{Stage: stage, State: state, Detail: detail, Branch: branch, Root: root})

	added, err := d.cfg.Store.Materialize(ctx, p.Checkout, repo, branch, root, r.report)
	if err != nil {
		return err
	}
	r.res.Root = added.Root
	if added.Root != root {
		r.root = added.Root
		r.set(func(e *entry) { e.Root = added.Root })
	}

	delivery, reason, err := r.agent(ctx)
	r.res.Prompt = delivery
	if reason != "" && err == nil {
		r.res.Error = reason
	}
	if err != nil {
		return stageErr(protocol.StageAgent, err)
	}
	return nil
}

// agent starts the agent in a managed session, or finds the one that
// already runs in the root, and gets the prompt to it. It returns the
// delivery state with its reason. The launch is journaled as two
// transitions, launching before new-session and launched after, on
// which a resume decides: launched is resumed with its recorded session
// and delivery, and a launch the daemon died in is unknown when a prompt
// was involved, since the agent may have it or may have run and exited,
// and never launched again. Nothing is inferred from a session's
// existence: a session found in the root by an add that did not launch
// it is session existed for the prompt.
func (r *addRun) agent(ctx context.Context) (delivery, reason string, err error) {
	d, m := r.d, r.m
	stage := protocol.StageAgent
	prompt := m.Prompt
	argv, placeholder := withPrompt(r.cmd, prompt)
	switch {
	case r.created && r.e.Launch == launchLaunched:
		r.report(stage, protocol.StateSkip, "session "+r.e.Session+" launched before")
		r.res.Session, r.res.PaneID = r.e.Session, r.e.PaneID
		if r.e.Delivery != "" {
			return r.e.Delivery, r.e.DeliveryError, nil
		}
		return r.typed(ctx)
	case r.created && r.e.Launch == launchLaunching && r.e.HasPrompt:
		reason = "daemon restarted during the launch of session " + r.e.Session
		r.report(stage, protocol.StateSkip, reason+"; whether the agent has the prompt is unknown")
		r.set(func(e *entry) { e.Delivery, e.DeliveryError = protocol.DeliveryUnknown, reason })
		return protocol.DeliveryUnknown, reason, nil
	}

	// The check is by root, not by name: a worktree made before a label
	// change keeps its session under the old name. A session with the
	// intended name whose pane records another root, or that is not a
	// single managed pane, is a name in use; nothing is adopted.
	name := tmux.SessionName(r.repo.Name, r.branch)
	panes, err := d.managed.Tmux.ListPanes(ctx)
	if err != nil && !tmux.NoServer(err) {
		return r.failed(prompt, "listing panes failed", err)
	}
	bySession := map[string][]tmux.Pane{}
	for _, p := range panes {
		bySession[p.Session] = append(bySession[p.Session], p)
	}
	names := make([]string, 0, len(bySession))
	for s := range bySession {
		names = append(names, s)
	}
	sort.Strings(names)
	for _, s := range names {
		ps := bySession[s]
		if len(ps) == 1 && ps[0].Managed && ps[0].Cwd == r.root {
			r.report(stage, protocol.StateSkip, "session "+s+" runs in "+r.root)
			r.res.Session, r.res.PaneID = s, ps[0].ID
			if prompt == "" {
				r.set(func(e *entry) { e.Delivery = protocol.DeliveryNone })
				return protocol.DeliveryNone, "", nil
			}
			reason = "session existed"
			r.set(func(e *entry) { e.Delivery, e.DeliveryError = protocol.DeliveryNotDelivered, reason })
			return protocol.DeliveryNotDelivered, reason, nil
		}
	}
	if ps, ok := bySession[name]; ok {
		switch {
		case len(ps) != 1:
			err = fmt.Errorf("session %s exists with %d panes; name in use", name, len(ps))
		case !ps[0].Managed:
			err = fmt.Errorf("session %s exists and is not managed by laatmux; name in use", name)
		default:
			err = fmt.Errorf("session %s runs in %s, not %s; name in use", name, ps[0].Cwd, r.root)
		}
		return r.failed(prompt, "launch refused", err)
	}
	r.report(stage, protocol.StateStart, "tmux new-session "+name+" "+tmux.ShellJoin(r.cmd))
	r.set(func(e *entry) {
		e.Launch, e.Session, e.ArgvPrompt = launchLaunching, name, placeholder && prompt != ""
	})
	paneID, err := d.managed.Tmux.NewSession(ctx, tmux.NewSessionOpts{Name: name, Cwd: r.root, Cmd: argv, Host: d.cfg.Host})
	if err != nil {
		submitted := tmux.Submitted(err)
		err = tmux.Redact(err, prompt, PromptPlaceholder)
		if placeholder && prompt != "" && submitted {
			reason = "new-session failed after the session may have been made: " + err.Error()
			r.set(func(e *entry) { e.Delivery, e.DeliveryError = protocol.DeliveryUnknown, reason })
			return protocol.DeliveryUnknown, reason, err
		}
		return r.failed(prompt, "launch failed", err)
	}
	serverPID := 0
	if panes, err := d.managed.Tmux.ListPanes(ctx); err == nil {
		for _, p := range panes {
			if p.ID == paneID {
				serverPID = p.ServerPID
			}
		}
		// Refresh the session join now, so the record the poke publishes
		// names the session rather than waiting for the next pane poll.
		d.setManagedRoots(panes, time.Now())
	}
	r.set(func(e *entry) {
		e.Launch, e.PaneID, e.ServerPID = launchLaunched, paneID, serverPID
		switch {
		case prompt == "":
			e.Delivery = protocol.DeliveryNone
		case placeholder:
			e.Delivery = protocol.DeliveryDelivered
		}
	})
	r.report(stage, protocol.StateDone, "session "+name+" pane "+paneID)
	r.res.Session, r.res.PaneID = name, paneID
	switch {
	case prompt == "":
		return protocol.DeliveryNone, "", nil
	case placeholder:
		return protocol.DeliveryDelivered, "", nil
	}
	return r.typed(ctx)
}

// failed is the agent stage failing before anything was started: the
// delivery is not delivered with the reason when there was a prompt.
func (r *addRun) failed(prompt, what string, err error) (string, string, error) {
	if prompt == "" {
		r.set(func(e *entry) { e.Delivery = protocol.DeliveryNone })
		return protocol.DeliveryNone, "", err
	}
	reason := what + ": " + err.Error()
	r.set(func(e *entry) { e.Delivery, e.DeliveryError = protocol.DeliveryNotDelivered, reason })
	return protocol.DeliveryNotDelivered, reason, err
}

// typed delivers the prompt into the launched pane, the add's own
// delivery, after releasing the repository lock: the wait for the agent
// to be ready is up to a minute, and git is not involved.
func (r *addRun) typed(ctx context.Context) (string, string, error) {
	r.unlock()
	stage := protocol.StageAgent
	r.report(stage, protocol.StateStart, "typing the prompt into pane "+r.e.PaneID+" once the agent is ready")
	state, reason := r.d.deliver(ctx, r.m.ID, 0, r.m.Prompt)
	switch state {
	case protocol.DeliveryDelivered:
		r.report(stage, protocol.StateDone, "prompt delivered")
	default:
		r.report(stage, protocol.StateDone, "prompt "+state+": "+reason)
	}
	return state, reason, nil
}

// deliver types the prompt into the pane the journal names for id, as
// the add's own delivery when n is 0, else as attempt n of a prompt
// message, and returns the delivery state with its reason. The pane
// must be ready: a fresh observation by the detector, after the startup
// grace, with the prompt box on screen, the agent identified and
// verified, in the pane and on the server instance recorded; that
// identity is bound before the first paste and required by every later
// one. A pane not ready within the wait gets nothing. An entry without
// a target adopts the managed session in the root when there is exactly
// one and its single pane has a verified live agent.
func (d *Daemon) deliver(ctx context.Context, id string, n int, prompt string) (state, reason string) {
	j := d.journal
	e, ok := j.get(id)
	if !ok {
		return protocol.DeliveryNotDelivered, "no journal entry"
	}
	set := func(change func(*entry)) {
		if ne, err := j.update(id, change); err == nil {
			e = ne
		} else {
			d.cfg.Logger.Printf("journal: %s: %v", id, err)
		}
	}
	record := func(state, reason string) (string, string) {
		set(func(e *entry) {
			e.Delivery, e.DeliveryError, e.Typing = state, reason, false
			if n > 0 {
				if a := e.lastAttempt(); a != nil && a.N == n {
					a.State, a.Error = state, reason
				}
			}
		})
		return state, reason
	}
	if n > 0 {
		set(func(e *entry) {
			e.Attempts = append(e.Attempts, attempt{N: n, State: attemptAttempting, At: time.Now()})
		})
	}
	if e.PaneID == "" {
		target, why := d.adopt(ctx, e.Root)
		if why != "" {
			return record(protocol.DeliveryNotDelivered, why)
		}
		set(func(e *entry) {
			e.Launch, e.Session, e.PaneID, e.ServerPID = launchLaunched, target.Session, target.ID, target.ServerPID
		})
	}
	since := time.Now()
	identity, why, replaced := d.awaitReady(ctx, &e, since)
	if why != "" {
		if replaced {
			return record(protocol.DeliveryNotDelivered, "session replaced: "+why)
		}
		return record(protocol.DeliveryNotDelivered, "agent not ready within "+readyWait.String()+": "+why)
	}
	if e.Identity == nil {
		bound := protocol.Identity{PID: identity.PID, StartUnix: identity.Start.Unix(), Comm: identity.Comm, LeaderPID: identity.LeaderPID}
		set(func(e *entry) { e.Identity = &bound })
	}
	// The attempt is on disk before the paste, so a daemon that dies in
	// it leaves unknown, never a second paste.
	set(func(e *entry) { e.Typing = true })
	buffer := attemptBufferPrefix + fileName(id) + "-" + strconv.Itoa(n)
	err := d.managed.Tmux.Paste(ctx, buffer, e.PaneID, prompt)
	if err == nil {
		return record(protocol.DeliveryDelivered, "")
	}
	var pe *tmux.PasteError
	enter := errors.As(err, &pe) && pe.Step == "enter"
	err = tmux.Redact(err, prompt, PromptPlaceholder)
	if enter {
		return record(protocol.DeliveryUnknown, "paste done, Enter refused: "+err.Error())
	}
	return record(protocol.DeliveryNotDelivered, "paste refused: "+err.Error())
}

// adopt finds the target for an entry without one: the managed session
// in root, when there is exactly one and its single pane has a verified
// live agent. The reason it cannot is returned otherwise.
func (d *Daemon) adopt(ctx context.Context, root string) (tmux.Pane, string) {
	panes, err := d.managed.Tmux.ListPanes(ctx)
	if err != nil {
		if tmux.NoServer(err) {
			return tmux.Pane{}, "no agent to deliver to: no managed session in " + root
		}
		return tmux.Pane{}, "listing panes failed: " + err.Error()
	}
	count := map[string]int{}
	for _, p := range panes {
		count[p.Session]++
	}
	var found []tmux.Pane
	for _, p := range panes {
		if p.Managed && p.Cwd == root && count[p.Session] == 1 {
			found = append(found, p)
		}
	}
	switch len(found) {
	case 0:
		return tmux.Pane{}, "no agent to deliver to: no managed session in " + root
	case 1:
	default:
		return tmux.Pane{}, fmt.Sprintf("no agent to deliver to: %d managed sessions in %s", len(found), root)
	}
	p := found[0]
	d.mu.Lock()
	st, ok := d.panes[paneKey(d.managed.Label, p.ID)]
	verified := ok && st.obs.verified
	d.mu.Unlock()
	if !verified {
		return tmux.Pane{}, "no agent to deliver to: no verified agent in session " + p.Session
	}
	return p, ""
}

// awaitReady waits for the entry's pane to be ready, as deliver
// requires, and returns the verified identity; else why it was not,
// and whether the target is gone for good: the session or the server
// instance is not the recorded one, or the agent is not the bound one.
func (d *Daemon) awaitReady(ctx context.Context, e *entry, since time.Time) (procs.Identity, string, bool) {
	deadline := since.Add(readyWait)
	key := paneKey(d.managed.Label, e.PaneID)
	for {
		now := time.Now()
		var why string
		replaced := false
		d.mu.Lock()
		st, ok := d.panes[key]
		switch {
		case !ok:
			why = "pane " + e.PaneID + " not seen"
		case !st.obs.at.After(since):
			why = "no observation since the wait began"
		case st.obs.session != e.Session || st.obs.serverPID != e.ServerPID:
			why, replaced = fmt.Sprintf("pane %s is in session %s on server %d, not %s on %d", e.PaneID, st.obs.session, st.obs.serverPID, e.Session, e.ServerPID), true
		case d.sessionPanesLocked(e.Session) != 1:
			why, replaced = fmt.Sprintf("session %s has %d panes", e.Session, d.sessionPanesLocked(e.Session)), true
		case !st.obs.verified:
			why = "no verified agent in the pane"
		case e.Identity != nil && (st.obs.identity.PID != e.Identity.PID || st.obs.identity.Start.Unix() != e.Identity.StartUnix):
			why, replaced = fmt.Sprintf("agent pid %d is not the bound pid %d", st.obs.identity.PID, e.Identity.PID), true
		case now.Sub(st.obs.identity.Start) < startupGrace:
			why = "agent within its startup grace"
		case !st.obs.idle:
			why = "prompt box not on screen"
		}
		identity := procs.Identity{}
		if ok {
			identity = st.obs.identity
		}
		d.mu.Unlock()
		if why == "" {
			return identity, "", false
		}
		if replaced || now.After(deadline) {
			return procs.Identity{}, why, replaced
		}
		select {
		case <-ctx.Done():
			return procs.Identity{}, "daemon shutting down", false
		case <-time.After(readyPoll):
		}
	}
}

// sessionPanesLocked counts the managed server's panes observed in the
// session. Called with d.mu held.
func (d *Daemon) sessionPanesLocked(session string) int {
	n := 0
	for _, st := range d.panes {
		if st.target == d.managed && st.obs.session == session {
			n++
		}
	}
	return n
}

// runPrompt delivers a pending prompt to the agent an add started, as
// attempt m.Attempt, and records the result on c. The journal
// serializes attempts per add and answers a repeat of a number with its
// recorded outcome rather than pasting again; the numbers run from 1 in
// order. The result is ok with the delivery state and its reason, so a
// prompt that could not be delivered is an outcome, not a failure of
// the message; the failures are an id the journal no longer holds,
// which is recovery expired, and an add that is not finished.
func (d *Daemon) runPrompt(ctx context.Context, m protocol.Message, c *command) {
	res := protocol.Message{Type: protocol.TypeResult, ID: m.ID, Attempt: m.Attempt}
	j := d.journal
	err := func() error {
		e, ok := j.get(m.ID)
		if !ok {
			return errors.New(protocol.ErrRecoveryExpired)
		}
		switch {
		case e.Removed:
			return errors.New(protocol.ErrRemoved)
		case e.Result == nil:
			return errors.New("the add is not finished; follow it first")
		case !e.HasPrompt:
			return errors.New("the add carried no prompt")
		case m.Attempt < 1:
			return errors.New("attempt number required")
		}
		l := d.attemptLock(m.ID)
		l.Lock()
		defer l.Unlock()
		e, _ = j.get(m.ID)
		for _, a := range e.Attempts {
			if a.N == m.Attempt {
				res.Prompt, res.Error = a.State, a.Error
				return nil
			}
		}
		if next := len(e.Attempts) + 1; m.Attempt != next {
			return fmt.Errorf("attempt %d is not the next; the journal has %d", m.Attempt, len(e.Attempts))
		}
		if _, _, found, err := d.cfg.Store.Find(ctx, e.Root); err != nil {
			return err
		} else if !found {
			res.Prompt, res.Error = protocol.DeliveryNotDelivered, "worktree "+e.Root+" is gone"
			j.update(m.ID, func(e *entry) {
				e.Attempts = append(e.Attempts, attempt{N: m.Attempt, State: res.Prompt, Error: res.Error, At: time.Now()})
				e.Delivery, e.DeliveryError = res.Prompt, res.Error
			})
			return nil
		}
		res.Prompt, res.Error = d.deliver(ctx, m.ID, m.Attempt, m.Prompt)
		return nil
	}()
	if err != nil {
		res.Error = err.Error()
	} else {
		res.OK = true
	}
	c.emit(res)
	d.evict(promptKey(m.ID, m.Attempt), c)
}

// promptKey is the command key of one attempt, so a follow with the
// attempt number reattaches to it.
func promptKey(id string, attempt int) string { return id + "#" + strconv.Itoa(attempt) }

// attemptLock serializes deliveries per add.
func (d *Daemon) attemptLock(id string) *sync.Mutex { return d.repoLock("attempt/" + id) }

// answerFollow is what a follow gets for an id the daemon has no command
// for, from the journal: the recorded result of a terminal entry,
// interrupted with the stage reached for one the daemon died in, the
// recorded outcome of an attempt, or the errors that say the journal
// has nothing. nil when there is no journal, which is unknown command
// as before.
func (d *Daemon) answerFollow(m protocol.Message) *protocol.Message {
	if d.journal == nil {
		return nil
	}
	e, ok := d.journal.get(m.ID)
	if m.Attempt > 0 {
		res := protocol.Message{Type: protocol.TypeResult, ID: m.ID, Attempt: m.Attempt}
		switch {
		case !ok:
			res.Error = protocol.ErrRecoveryExpired
		case m.Attempt > len(e.Attempts):
			res.Error = protocol.ErrUnknownAttempt
		default:
			a := e.Attempts[m.Attempt-1]
			res.OK, res.Prompt, res.Error = true, a.State, a.Error
		}
		return &res
	}
	if !ok {
		return nil
	}
	var res protocol.Message
	if e.terminal() {
		res = e.recorded()
	} else {
		res = e.interrupted()
	}
	return &res
}

// sweepBuffers deletes the paste buffers a delivery interrupted between
// loading and deleting left on the managed server.
func (d *Daemon) sweepBuffers(ctx context.Context) {
	if d.managed == nil {
		return
	}
	if err := d.managed.Tmux.DeleteBuffers(ctx, attemptBufferPrefix); err != nil {
		d.cfg.Logger.Printf("sweep buffers: %v", err)
	}
}

// runJournal sweeps the journal at start and hourly until ctx is done.
func (d *Daemon) runJournal(ctx context.Context) {
	t := time.NewTicker(journalSweep)
	defer t.Stop()
	for {
		d.journal.sweep(time.Now())
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
