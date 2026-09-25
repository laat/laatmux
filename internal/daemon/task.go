package daemon

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/laat/laatmux/internal/config"
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
		// The recorded result is published under the root's delivery
		// lock, read again there: an rm that completed between the
		// read and this point has made it removed, and the memory must
		// not cache the success it replaced.
		if r.e.Root != "" {
			unlock := d.lockDeliveries(r.e.Root)
			if cur, ok := d.journal.get(m.ID); ok && cur.terminal() {
				r.res = cur.recorded()
			}
			c.emit(r.res)
			unlock()
		} else {
			c.emit(r.res)
		}
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
		// An rm that ran while the add waited on its delivery, with the
		// repository lock released, has removed the worktree and marked
		// the entry: removed is the outcome then, of this result and of
		// every follow, and the success is not recorded over it. The
		// record and the publication are under the root's delivery
		// lock, which rm holds from the tombstone to dropping the
		// command from memory, so the result the memory keeps is never
		// a success published after the tombstone.
		if r.root != "" {
			defer d.lockDeliveries(r.root)()
		}
		err := r.set(func(e *entry) {
			if e.Removed {
				return
			}
			rec := res
			e.Result = &rec
			e.TerminalAt = now
		})
		if err == nil && r.e.Removed {
			res = r.e.recorded()
		}
	}
	d.pokeWorktrees()
	c.emit(res)
	d.evict(m.ID, c)
}

// addRun is one add in flight: the request, its journal entry as the
// daemon last wrote it, and what the stages have decided so far.
type addRun struct {
	d          *Daemon
	m          protocol.Message
	c          *command
	e          entry
	created    bool // the journal has the entry; set keeps e in step with it
	repo       worktree.Repo
	branch     string
	root       string
	cmd        []string // the configured command, placeholder and all
	res        protocol.Message
	unlockRepo func() // set while the repository's lock is held
}

// set applies a change to the journal entry and keeps the local copy.
// A write that fails is returned: a decision that could not be written
// must not be acted on, since the next daemon would not know it.
func (r *addRun) set(change func(*entry)) error {
	if !r.created {
		return nil
	}
	e, err := r.d.journal.update(r.m.ID, change)
	if err != nil {
		r.d.cfg.Logger.Printf("journal: %s: %v", r.m.ID, err)
		return err
	}
	r.e = e
	return nil
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
	if r.unlockRepo != nil {
		r.unlockRepo()
		r.unlockRepo = nil
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
				r.e, r.res = cur, cur.recorded()
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
	repo, err := d.addRepo(m)
	if err != nil {
		return stageErr(stage, err)
	}
	if known && !config.SameSource(repo.Source, r.e.Source) {
		// A resend is the recorded add, never another repository's.
		return stageErr(stage, fmt.Errorf("the add %s was submitted for %s, not %s", m.ID, r.e.Source, repo.Source))
	}
	r.repo = repo
	r.cmd = m.Cmd
	if len(r.cmd) == 0 {
		var ok bool
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

	r.unlockRepo = d.lockRepo(repo.Source)
	// The journal is read again under the lock: an rm that held it
	// meanwhile may have removed the worktree this add was resuming,
	// and the entry with it, which no stage may then remake.
	if r.created {
		cur, ok := j.get(m.ID)
		if !ok {
			return stageErr(stage, errors.New("journal: the entry is gone"))
		}
		if cur.terminal() {
			r.e, r.res = cur, cur.recorded()
			return errRecorded
		}
		r.e = cur
	}
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
		var names []string
		names = append(names, local...)
		names = append(names, remote...)
		for _, e := range entries {
			if e.Branch != "" {
				names = append(names, e.Branch)
			}
		}
		names = append(names, j.reserved(repo.Source, m.ID)...)
		name, err := worktree.Allocate(branch, func(c string) bool {
			for _, n := range names {
				if worktree.RefConflict(n, c) {
					return true
				}
			}
			return false
		})
		if err != nil {
			return stageErr(stage, err)
		}
		if err := r.set(func(e *entry) { e.Branch, e.Allocated = name, true }); err != nil {
			return stageErr(stage, fmt.Errorf("journal: the allocation could not be written: %w", err))
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
	if err := r.set(func(e *entry) { e.Root = root }); err != nil {
		return stageErr(stage, fmt.Errorf("journal: %w", err))
	}
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
		// A daemon restarted before the typed prompt was delivered: the
		// agent may still be at its trust question.
		d.startTrust(trustTarget{pane: r.e.PaneID, session: r.e.Session, root: r.root, serverPID: r.e.ServerPID}, readyWait, trustPoll)
		return r.typed(ctx)
	case r.created && r.e.Launch == launchLaunching:
		// new-session may have been submitted: an agent may be there,
		// or may have run and exited. No second launch, with or without
		// a prompt; without one the delivery is none and the reason
		// says why no session is reported.
		reason = "daemon restarted during the launch of session " + r.e.Session
		if !r.e.HasPrompt {
			r.report(stage, protocol.StateSkip, reason+"; not launched again")
			r.set(func(e *entry) { e.Delivery = protocol.DeliveryNone })
			return protocol.DeliveryNone, reason, nil
		}
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
	// launching is on disk before new-session, or new-session does not
	// run: a launch the journal does not know cannot be told from none.
	if err := r.set(func(e *entry) {
		e.Launch, e.Session, e.ArgvPrompt = launchLaunching, name, placeholder && prompt != ""
	}); err != nil {
		return r.failed(prompt, "launch refused", fmt.Errorf("journal: %w", err))
	}
	made, err := d.managed.Tmux.NewSession(ctx, tmux.NewSessionOpts{Name: name, Cwd: r.root, Cmd: argv, Host: d.cfg.Host})
	if err != nil {
		submitted := tmux.Submitted(err)
		err = tmux.Redact(err, prompt, PromptPlaceholder)
		if placeholder && prompt != "" && submitted {
			reason = "new-session failed after the session may have been made: " + err.Error()
			r.set(func(e *entry) { e.Delivery, e.DeliveryError = protocol.DeliveryUnknown, reason })
			return protocol.DeliveryUnknown, reason, err
		}
		if submitted {
			// The session may exist; nothing was pasted into it, so the
			// prompt provably did not transfer.
			return r.failed(prompt, "launch failed after new-session was submitted, a session may exist", err)
		}
		return r.failed(prompt, "launch failed", err)
	}
	paneID := made.PaneID
	// A worktree the add made or took up under the host's worktrees
	// directory is a folder the agent may not have seen: its trust
	// question, when it asks one, is answered for it, whether the prompt
	// is typed or on the command line.
	d.startTrust(trustTarget{pane: paneID, session: name, root: r.root, serverPID: made.ServerPID}, readyWait, trustPoll)
	// Refresh the session join now, so the record the poke publishes
	// names the session rather than waiting for the next pane poll.
	if panes, err := d.managed.Tmux.ListPanes(ctx); err == nil {
		d.setManagedRoots(panes, time.Now())
	}
	err = r.set(func(e *entry) {
		e.Launch, e.PaneID, e.ServerPID = launchLaunched, paneID, made.ServerPID
		switch {
		case prompt == "":
			e.Delivery = protocol.DeliveryNone
		case placeholder:
			e.Delivery = protocol.DeliveryDelivered
		}
	})
	r.report(stage, protocol.StateDone, "session "+name+" pane "+paneID)
	r.res.Session, r.res.PaneID = name, paneID
	if err != nil {
		// The session is there and launched could not be written: the
		// next daemon reads launching, and so must this result.
		if prompt == "" {
			return protocol.DeliveryNone, "", nil
		}
		return protocol.DeliveryUnknown, "the launch could not be journaled: " + err.Error(), nil
	}
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
// must be ready: an observation by the detector made after the wait
// began and after the startup grace, with the prompt box on screen,
// the agent identified and seen alive by that poll, in the pane and on
// the server instance recorded; that identity is bound before the first
// paste and required by every later one. A pane not ready within the
// wait gets nothing. An entry without a target adopts the managed
// session in the root when there is exactly one and its single pane
// has a verified live agent.
//
// The wait takes no lock. The paste does: under the root's delivery
// lock, which rm holds for its removal, the entry is read again, the
// root checked and the readiness confirmed on the latest observation,
// so nothing is pasted into a root rm has taken or beside another
// delivery's paste, and rm is never held up by a wait.
func (d *Daemon) deliver(ctx context.Context, id string, n int, prompt string) (state, reason string) {
	j := d.journal
	e, ok := j.get(id)
	if !ok {
		return protocol.DeliveryNotDelivered, "no journal entry"
	}
	set := func(change func(*entry)) error {
		ne, err := j.update(id, change)
		if err != nil {
			d.cfg.Logger.Printf("journal: %s: %v", id, err)
			return err
		}
		e = ne
		return nil
	}
	// record writes the outcome; an outcome that cannot be written after
	// the paste is unknown to the next daemon, and so it is here.
	record := func(state, reason string) (string, string) {
		err := set(func(e *entry) {
			e.Delivery, e.DeliveryError, e.Typing = state, reason, false
			if n > 0 {
				if a := e.lastAttempt(); a != nil && a.N == n {
					a.State, a.Error = state, reason
				}
			}
		})
		if err != nil && state == protocol.DeliveryDelivered {
			return protocol.DeliveryUnknown, "delivered, but the outcome could not be journaled: " + err.Error()
		}
		return state, reason
	}
	// current reads the entry again and checks the root, for the steps
	// under the lock: rm's tombstone and a worktree replaced since are
	// refusals.
	current := func() string {
		cur, ok := j.get(id)
		if !ok {
			return "no journal entry"
		}
		e = cur
		if e.Removed {
			return "worktree removed"
		}
		return d.worktreeReplaced(ctx, e)
	}
	if e.PaneID == "" {
		unlock := d.lockDeliveries(e.Root)
		if why := current(); why != "" {
			unlock()
			return record(protocol.DeliveryNotDelivered, why)
		}
		target, why := d.adopt(ctx, e.Root)
		if why == "" {
			if err := set(func(e *entry) {
				e.Launch, e.Session, e.PaneID, e.ServerPID = launchLaunched, target.Session, target.ID, target.ServerPID
			}); err != nil {
				why = "journal: " + err.Error()
			}
		}
		unlock()
		if why != "" {
			return record(protocol.DeliveryNotDelivered, why)
		}
	}
	since := time.Now()
	deadline := since.Add(readyWait)
	d.mu.Lock()
	d.waits++
	d.mu.Unlock()
	var identity procs.Identity
	for {
		if _, why, replaced := d.awaitReady(ctx, &e, since, deadline); why != "" {
			if replaced {
				return record(protocol.DeliveryNotDelivered, "session replaced: "+why)
			}
			return record(protocol.DeliveryNotDelivered, "agent not ready within "+readyWait.String()+": "+why)
		}
		unlock := d.lockDeliveries(e.Root)
		if why := current(); why != "" {
			unlock()
			return record(protocol.DeliveryNotDelivered, why)
		}
		var why string
		var replaced bool
		identity, why, replaced = d.ready(&e, since)
		if why == "" {
			defer unlock()
			break
		}
		unlock()
		// The latest observation no longer says ready: another delivery
		// pasted on it meanwhile, or the agent moved on. Wait for the
		// next one, within the same deadline.
		if replaced {
			return record(protocol.DeliveryNotDelivered, "session replaced: "+why)
		}
		if time.Now().After(deadline) {
			return record(protocol.DeliveryNotDelivered, "agent not ready within "+readyWait.String()+": "+why)
		}
	}
	if e.Identity == nil {
		bound := protocol.Identity{PID: identity.PID, StartUnix: identity.Start.Unix(), Comm: identity.Comm, LeaderPID: identity.LeaderPID}
		if err := set(func(e *entry) { e.Identity = &bound }); err != nil {
			return record(protocol.DeliveryNotDelivered, "journal: "+err.Error())
		}
	}
	// A daemon shutting down starts no paste, and waits for one it has
	// started, so the buffer is deleted before the process ends.
	if !d.beginDelivery() {
		return record(protocol.DeliveryNotDelivered, "daemon shutting down")
	}
	defer d.endDelivery()
	// The paste is on disk before it happens, or it does not happen: a
	// daemon that dies in it leaves unknown, never a second paste.
	if err := set(func(e *entry) { e.Typing = true }); err != nil {
		return record(protocol.DeliveryNotDelivered, "journal: "+err.Error())
	}
	buffer := attemptBufferPrefix + FileName(id) + "-" + strconv.Itoa(n)
	err := d.managed.Tmux.Paste(ctx, buffer, e.PaneID, prompt)
	// Whatever the paste did, the pane's observation is spent: the next
	// delivery to it needs one made after this moment.
	d.mu.Lock()
	d.pasted[paneKey(d.managed.Label, e.PaneID)] = time.Now()
	d.mu.Unlock()
	if err == nil {
		return record(protocol.DeliveryDelivered, "")
	}
	// Only a failure to load the buffer proves nothing reached the
	// pane. A paste-buffer or send-keys that failed may have been run by
	// the server before its client was told, or killed after: unknown.
	var pe *tmux.PasteError
	load := errors.As(err, &pe) && pe.Step == "load"
	err = tmux.Redact(err, prompt, PromptPlaceholder)
	if load {
		return record(protocol.DeliveryNotDelivered, "paste refused: "+err.Error())
	}
	return record(protocol.DeliveryUnknown, "paste may have reached the pane: "+err.Error())
}

// beginDelivery counts a paste about to start, unless the daemon is
// stopping; endDelivery counts it done. StopRuns waits for the count.
func (d *Daemon) beginDelivery() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.stopping {
		return false
	}
	d.pasting++
	return true
}

func (d *Daemon) endDelivery() {
	d.mu.Lock()
	d.pasting--
	d.mu.Unlock()
}

// worktreeReplaced says why the entry's root is no longer the worktree
// the add made, or "" when it still is: the same repository, and the
// branch when git still has one there. A root taken by another
// worktree since is not a target, and its agent is not adopted.
func (d *Daemon) worktreeReplaced(ctx context.Context, e entry) string {
	rec, _, found, err := d.cfg.Store.Find(ctx, e.Root)
	switch {
	case err != nil:
		return "worktree " + e.Root + " could not be checked: " + err.Error()
	case !found:
		return "worktree replaced: " + e.Root + " is gone"
	case !config.SameSource(rec.Source, e.Source):
		return "worktree replaced: " + e.Root + " is now a worktree of " + rec.Repo
	case rec.Branch != "" && rec.Branch != e.Branch:
		return "worktree replaced: " + e.Root + " is now on branch " + rec.Branch + ", not " + e.Branch
	}
	return ""
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

// ready is one check of the entry's pane against the latest
// observation, as deliver requires, and returns the verified identity;
// else why it is not ready, and whether the target is gone for good:
// the session or the server instance is not the recorded one, or the
// agent is not the bound one. The observation must be from after since,
// from after the last paste into the pane, and from after the agent's
// startup grace, by its own time: a fresh look is what says the agent
// is ready, not time having passed since an older one, and one look
// serves one paste.
func (d *Daemon) ready(e *entry, since time.Time) (procs.Identity, string, bool) {
	key := paneKey(d.managed.Label, e.PaneID)
	d.mu.Lock()
	defer d.mu.Unlock()
	st, ok := d.panes[key]
	if !ok {
		return procs.Identity{}, "pane " + e.PaneID + " not seen", false
	}
	obs := st.obs
	switch {
	case !obs.at.After(since):
		return obs.identity, "no observation since the wait began", false
	case !obs.at.After(d.pasted[key]):
		return obs.identity, "no observation since the last paste into the pane", false
	case obs.session != e.Session || obs.serverPID != e.ServerPID:
		return obs.identity, fmt.Sprintf("pane %s is in session %s on server %d, not %s on %d", e.PaneID, obs.session, obs.serverPID, e.Session, e.ServerPID), true
	case d.sessionPanesLocked(e.Session) != 1:
		return obs.identity, fmt.Sprintf("session %s has %d panes", e.Session, d.sessionPanesLocked(e.Session)), true
	case !obs.verified:
		return obs.identity, "no verified agent in the pane", false
	case e.Identity != nil && (obs.identity.PID != e.Identity.PID || obs.identity.Start.Unix() != e.Identity.StartUnix):
		return obs.identity, fmt.Sprintf("agent pid %d is not the bound pid %d", obs.identity.PID, e.Identity.PID), true
	case obs.at.Sub(obs.identity.Start) < startupGrace:
		return obs.identity, "agent within its startup grace", false
	case !obs.idle:
		return obs.identity, "prompt box not on screen", false
	}
	return obs.identity, "", false
}

// awaitReady polls ready until it is, the target is gone for good, or
// the deadline is past.
func (d *Daemon) awaitReady(ctx context.Context, e *entry, since, deadline time.Time) (procs.Identity, string, bool) {
	for {
		identity, why, replaced := d.ready(e, since)
		if why == "" || replaced || time.Now().After(deadline) {
			return identity, why, replaced
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
		// The attempt is on disk before anything is done for it, or
		// nothing is: an attempt the journal could not take is not
		// taken, and the sender retries the same number.
		if _, err := j.update(m.ID, func(e *entry) {
			e.Attempts = append(e.Attempts, attempt{N: m.Attempt, State: attemptAttempting, At: time.Now()})
		}); err != nil {
			return fmt.Errorf("%s: %w", protocol.ErrAttemptNotRecorded, err)
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
	if strings.HasPrefix(res.Error, protocol.ErrAttemptNotRecorded) {
		// Nothing was done for the number: the sender retries it, and
		// the retry must run, not replay this answer.
		d.forgetDone(promptKey(m.ID, m.Attempt))
		return
	}
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
		case e.Removed:
			res.Error = protocol.ErrRemoved
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

// addRepo is the repository an add is for: the entry the add brought,
// which the machine the user sits at decides, else the one this host's
// config lists for its source or label, for a sender without entries.
// An entry is checked as the config checks its own: a name that places
// directories, copy rules that stay inside the worktree, no empty setup
// command.
func (d *Daemon) addRepo(m protocol.Message) (worktree.Repo, error) {
	e := m.RepoEntry
	if e == nil {
		repo, ok := d.cfg.Store.Repo(m.Repo)
		if !ok {
			return worktree.Repo{}, fmt.Errorf("unknown repository %q: not in this host's config", m.Repo)
		}
		return repo, nil
	}
	if e.Source == "" || !config.SameSource(e.Source, m.Repo) {
		return worktree.Repo{}, fmt.Errorf("the add's repository entry is for %q, not %q", e.Source, m.Repo)
	}
	if !config.ValidLabel(e.Name) {
		return worktree.Repo{}, fmt.Errorf("the add's repository name %q is not a valid label", e.Name)
	}
	for _, c := range e.Copy {
		if err := config.CheckCopy(c); err != nil {
			return worktree.Repo{}, fmt.Errorf("the add's repository entry: copy: %w", err)
		}
	}
	for i, cmd := range e.Setup {
		if strings.TrimSpace(cmd) == "" {
			return worktree.Repo{}, fmt.Errorf("the add's repository entry: setup: entry %d is empty", i+1)
		}
	}
	return worktree.Repo{Source: e.Source, Name: e.Name, Copy: e.Copy, Setup: e.Setup, Sent: true}, nil
}
