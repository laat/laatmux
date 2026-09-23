package daemon

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/laat/laatmux/internal/protocol"
	"github.com/laat/laatmux/internal/tmux"
	"github.com/laat/laatmux/internal/worktree"
)

// DefaultCommandTTL is how long a finished command's outcome is kept, so a
// client that lost its bridge can follow the id and get the result back.
const DefaultCommandTTL = 5 * time.Minute

// maxOutput bounds the output one command retains for replay. Past it,
// add and rm drop output for good after one line saying so, since setup
// output is a diagnostic; a run forgets its oldest lines instead and
// keeps streaming to its followers, since its output is the point. Step
// and result messages are always kept, and there are few of them. Each
// line is charged its bytes plus eventCost for the message around it,
// so a stream of empty lines is bounded too.
const (
	maxOutput = 1 << 20
	eventCost = 64
)

// command is one add, rm or run in flight or recently finished. Its
// progress is numbered from 1 and appended as it happens, and the result
// kept apart; a follow replays what it retains past the follower's mark
// and then keeps sending. Events are contiguous in N: a ring drops from
// the front, so events[i].N is events[0].N + i.
type command struct {
	id        string
	ring      bool // drop the oldest events past the budget rather than new output
	mu        sync.Mutex
	cond      *sync.Cond
	events    []protocol.Message
	next      uint64 // N of the next progress message
	outBytes  int
	truncated bool
	done      bool
	doneAt    time.Time
	result    protocol.Message
	followers int // streams in progress, for tests
	// job is the run this command is; nil for add and rm.
	job *runJob
}

func newCommand(id string) *command {
	c := &command{id: id, next: 1}
	c.cond = sync.NewCond(&c.mu)
	return c
}

// cost is what an output line counts against the budget.
func cost(line string) int { return len(line) + eventCost }

// emit appends a progress message, numbering it, or records the result.
func (c *command) emit(m protocol.Message) {
	c.mu.Lock()
	if m.Type == protocol.TypeResult {
		c.result = m
		c.done = true
		c.doneAt = time.Now()
		c.mu.Unlock()
		c.cond.Broadcast()
		return
	}
	if m.State == protocol.StateOutput {
		c.outBytes += cost(m.Detail)
		if c.outBytes > maxOutput && !c.ring {
			if c.truncated {
				c.mu.Unlock()
				return
			}
			c.truncated = true
			m.Detail = "(further output dropped: over 1 MiB)"
		}
	}
	m.N = c.next
	c.next++
	c.events = append(c.events, m)
	for c.ring && c.outBytes > maxOutput && len(c.events) > 1 {
		if c.events[0].State == protocol.StateOutput {
			c.outBytes -= cost(c.events[0].Detail)
		}
		c.events[0] = protocol.Message{}
		c.events = c.events[1:]
	}
	c.mu.Unlock()
	c.cond.Broadcast()
}

// stream writes every progress message past after to pc, retained and
// future, then the result, until a write fails or quit closes, which is
// the connection ending: a follower of a quiet command must not sleep
// on until the next event. A follower behind the retained tail gets one
// gap message for the lines between its mark and the tail, numbered as
// the last of them, so its mark moves past them. That holds for a
// follower that is connected but slow as well: the ring is the one
// buffer, so a reader more than the budget behind loses what it did not
// take, as a slow subscriber of the status stream is dropped, and the
// process is never stalled by a reader.
func (c *command) stream(pc *protocol.Conn, after uint64, quit <-chan struct{}) error {
	c.mu.Lock()
	c.followers++
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		c.followers--
		c.mu.Unlock()
	}()
	// The waker takes the lock, so it runs either before the wait below
	// checked quit or after the wait has released the lock, never in
	// between: the wait cannot miss it. A nil quit, from a test, never
	// closes.
	if quit != nil {
		go func() {
			<-quit
			c.mu.Lock()
			c.cond.Broadcast()
			c.mu.Unlock()
		}()
	}
	last := after
	for {
		c.mu.Lock()
		for !c.done && (len(c.events) == 0 || c.events[len(c.events)-1].N <= last) {
			select {
			case <-quit:
				c.mu.Unlock()
				return nil
			default:
			}
			c.cond.Wait()
		}
		var batch []protocol.Message
		if n := len(c.events); n > 0 && c.events[n-1].N > last {
			base := c.events[0].N
			if last+1 < base {
				batch = append(batch, protocol.Message{
					Type: protocol.TypeProgress, ID: c.id, N: base - 1,
					Stage: protocol.StageRun, State: protocol.StateGap,
					Detail: fmt.Sprintf("%d lines dropped", base-1-last),
				})
				last = base - 1
			}
			batch = append(batch, c.events[last+1-base:]...)
			last = c.events[n-1].N
		}
		done, res := c.done, c.result
		c.mu.Unlock()
		for _, m := range batch {
			if err := pc.Write(m); err != nil {
				return err
			}
		}
		if done {
			return pc.Write(res)
		}
	}
}

// command returns the command for id, creating it when unknown, with
// init run on it before any other connection can see it, so a cancel
// that arrives at once finds the job. The caller runs a new one; an
// existing one is only followed.
func (d *Daemon) command(id string, init func(*command)) (*command, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if c, ok := d.cmds[id]; ok {
		return c, false
	}
	c := newCommand(id)
	if init != nil {
		init(c)
	}
	d.cmds[id] = c
	return c, true
}

// lookup returns the command for id when the daemon still has it.
func (d *Daemon) lookup(id string) (*command, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	c, ok := d.cmds[id]
	return c, ok
}

// evict forgets a finished command once its TTL has passed, whether or
// not any other command arrives meanwhile. The identity check keeps a
// timer from evicting a newer command under the same id.
func (d *Daemon) evict(id string, c *command) {
	time.AfterFunc(d.commandTTL, func() {
		d.mu.Lock()
		if d.cmds[id] == c {
			delete(d.cmds, id)
		}
		d.mu.Unlock()
	})
}

// repoLock serializes commands per repository: fetch and worktree add
// write to the same main checkout, so that is the grain. Different
// repositories proceed in parallel.
func (d *Daemon) repoLock(source string) *sync.Mutex {
	d.mu.Lock()
	defer d.mu.Unlock()
	l, ok := d.locks[source]
	if !ok {
		l = &sync.Mutex{}
		d.locks[source] = l
	}
	return l
}

// runAdd runs the stages of add and records the result on c. ctx is the
// daemon's: a command runs to completion whatever happens to the
// connection that sent it, so a client that lost its bridge can repeat
// the id and pick the stream up.
func (d *Daemon) runAdd(ctx context.Context, m protocol.Message, c *command) {
	report := func(stage, state, detail string) {
		c.emit(protocol.Message{Type: protocol.TypeProgress, ID: m.ID, Stage: stage, State: state, Detail: detail})
	}
	res := protocol.Message{Type: protocol.TypeResult, ID: m.ID}
	err := func() error {
		repo, ok := d.cfg.Store.Repo(m.Repo)
		if !ok {
			return stageErr(protocol.StageResolve, fmt.Errorf("unknown repository %q: not in this host's config", m.Repo))
		}
		cmd := m.Cmd
		if len(cmd) == 0 {
			var ok bool
			if cmd, ok = d.cfg.Agents[m.AgentName]; !ok {
				return stageErr(protocol.StageResolve, fmt.Errorf("unknown agent %q: not in this host's config", m.AgentName))
			}
		}
		l := d.repoLock(repo.Source)
		l.Lock()
		defer l.Unlock()
		added, err := d.cfg.Store.Add(ctx, repo, m.Branch, report)
		if err != nil {
			return err
		}
		res.Root = added.Root
		session, paneID, err := d.agentStage(ctx, repo, m.Branch, added.Root, cmd, report)
		if err != nil {
			return stageErr(protocol.StageAgent, err)
		}
		res.Session, res.PaneID = session, paneID
		return nil
	}()
	d.finish(c, res, err)
}

// agentStage starts the agent in a managed session, or finds the one that
// already runs in the root. The check is by root, not by name: a worktree
// made before a label change keeps its session under the old name. A
// session with the intended name whose pane records another root, or
// that is not a single managed pane, is a name in use; nothing is adopted.
func (d *Daemon) agentStage(ctx context.Context, repo worktree.Repo, branch, root string, cmd []string, report worktree.Reporter) (session, paneID string, err error) {
	stage := protocol.StageAgent
	name := tmux.SessionName(repo.Name, branch)
	panes, err := d.managed.Tmux.ListPanes(ctx)
	if err != nil && !tmux.NoServer(err) {
		return "", "", err
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
		if len(ps) == 1 && ps[0].Managed && ps[0].Cwd == root {
			report(stage, protocol.StateSkip, "session "+s+" runs in "+root)
			return s, ps[0].ID, nil
		}
	}
	if ps, ok := bySession[name]; ok {
		switch {
		case len(ps) != 1:
			return "", "", fmt.Errorf("session %s exists with %d panes; name in use", name, len(ps))
		case !ps[0].Managed:
			return "", "", fmt.Errorf("session %s exists and is not managed by laatmux; name in use", name)
		default:
			return "", "", fmt.Errorf("session %s runs in %s, not %s; name in use", name, ps[0].Cwd, root)
		}
	}
	report(stage, protocol.StateStart, "tmux new-session "+name+" "+tmux.ShellJoin(cmd))
	paneID, err = d.managed.Tmux.NewSession(ctx, tmux.NewSessionOpts{Name: name, Cwd: root, Cmd: cmd, Host: d.cfg.Host})
	if err != nil {
		return "", "", err
	}
	// Refresh the session join now, so the record the poke publishes
	// names the session rather than waiting for the next pane poll.
	if panes, err := d.managed.Tmux.ListPanes(ctx); err == nil {
		d.setManagedRoots(panes, time.Now())
	}
	report(stage, protocol.StateDone, "session "+name+" pane "+paneID)
	return name, paneID, nil
}

// runRm removes a worktree, then every managed session whose pane records
// its root. Each step is inspected, so a retry after a crash between them
// finishes the job and a target where both skip is ok.
//
// The target is resolved with every repository locked, so an add in flight
// on any repository is seen complete, not half done: the root checks ask
// every checkout, and a lock on the named repository alone would let an
// add elsewhere register the root between the check and the session step.
// rm is rare and brief, and add keeps its per-repository grain. When repo,
// branch and root are all given they must agree: root is the worktree
// registered for the branch, or, once that registration is gone, the
// root the session step matches on. A root that git registers for another
// branch or repository is a mismatch, not a target.
func (d *Daemon) runRm(ctx context.Context, m protocol.Message, c *command) {
	res := protocol.Message{Type: protocol.TypeResult, ID: m.ID}
	err := func() error {
		var repo worktree.Repo
		if m.Repo != "" {
			var ok bool
			if repo, ok = d.cfg.Store.Repo(m.Repo); !ok {
				return fmt.Errorf("unknown repository %q: not in this host's config", m.Repo)
			}
			if m.Branch == "" && m.Root == "" {
				return errors.New("rm needs a branch or a root")
			}
		} else if m.Root == "" {
			return errors.New("rm needs a repository and branch, or a root")
		}
		unlock := d.lockRepos()
		defer unlock()

		root, checkout := m.Root, ""
		if root != "" {
			// The client's root, cleaned so it compares with what git
			// registered and the pane recorded. Only a root under the
			// worktrees directory is a target: sessions elsewhere, made by
			// new with any cwd, are not rm's to kill.
			root = filepath.Clean(root)
			if !d.cfg.Store.Owns(root) {
				return fmt.Errorf("%s is not under the worktrees directory %s", root, d.cfg.Store.Dirs.Worktrees)
			}
		}
		switch {
		case m.Branch != "":
			rec, co, found, err := d.cfg.Store.ByBranch(ctx, repo, m.Branch)
			if err != nil {
				return err
			}
			switch {
			case found && root != "" && rec.Root != root:
				return fmt.Errorf("branch %s of %s is checked out at %s, not %s", m.Branch, repo.Name, rec.Root, root)
			case found:
				root, checkout = rec.Root, co
			case root != "":
				// The registration is gone; root still finds the session.
				// It must not be some other worktree registered since.
				rec, _, taken, err := d.cfg.Store.Find(ctx, root)
				if err != nil {
					return err
				}
				if taken && (rec.Source != repo.Source || rec.Branch != m.Branch) {
					return fmt.Errorf("%s is the worktree for %s of %s, not %s", root, branchOrDetached(rec.Branch), rec.Repo, m.Branch)
				}
			}
		default:
			rec, co, found, err := d.cfg.Store.Find(ctx, root)
			if err != nil {
				return err
			}
			if found {
				if repo.Source != "" && rec.Source != repo.Source {
					return fmt.Errorf("%s is a worktree of %s, not %s", root, rec.Repo, repo.Name)
				}
				checkout = co
			}
		}
		if root == "" {
			// Nothing registered for the branch and no root to match
			// sessions on: both steps skip.
			return nil
		}
		res.Root = root
		if checkout != "" {
			if _, err := worktree.Remove(ctx, checkout, root, m.Force); err != nil {
				return err
			}
		}
		// Git has agreed to the removal: what runs in the root is
		// laatmux's own, like the session, and goes before it. The wait
		// makes the ok mean nothing of laatmux's is left there.
		d.cancelRunsIn(root)
		panes, err := d.managed.Tmux.ListPanes(ctx)
		if err != nil {
			if tmux.NoServer(err) {
				return nil
			}
			return err
		}
		killed := map[string]bool{}
		for _, p := range panes {
			if !p.Managed || p.Cwd != root || killed[p.Session] {
				continue
			}
			if err := d.managed.Tmux.KillSession(ctx, p.Session); err != nil {
				return err
			}
			killed[p.Session] = true
		}
		return nil
	}()
	d.finish(c, res, err)
}

func branchOrDetached(branch string) string {
	if branch == "" {
		return "a detached HEAD"
	}
	return "branch " + branch
}

// lockRepos takes every known repository's lock in sorted order, so two
// callers taking several never deadlock, and add's single lock nests
// inside. The returned func releases them.
func (d *Daemon) lockRepos() func() {
	var sources []string
	for _, r := range d.cfg.Store.Repos {
		sources = append(sources, r.Source)
	}
	sort.Strings(sources)
	locks := make([]*sync.Mutex, 0, len(sources))
	for i, src := range sources {
		if i > 0 && src == sources[i-1] {
			continue
		}
		l := d.repoLock(src)
		l.Lock()
		locks = append(locks, l)
	}
	return func() {
		for i := len(locks) - 1; i >= 0; i-- {
			locks[i].Unlock()
		}
	}
}

// finish records the result and asks for a worktree poll, so the record
// follows the command.
func (d *Daemon) finish(c *command, res protocol.Message, err error) {
	if err != nil {
		var se *worktree.StageError
		if errors.As(err, &se) {
			res.Stage = se.Stage
			res.Error = se.Err.Error()
		} else {
			res.Error = err.Error()
		}
	} else {
		res.OK = true
	}
	d.pokeWorktrees()
	c.emit(res)
	d.evict(res.ID, c)
}

func stageErr(stage string, err error) error { return &worktree.StageError{Stage: stage, Err: err} }
