package daemon

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/laat/laatmux/internal/protocol"
	"github.com/laat/laatmux/internal/tmux"
	"github.com/laat/laatmux/internal/worktree"
)

// DefaultCommandTTL is how long a finished command's outcome is kept, so a
// client that lost its bridge can repeat the id and get the result back.
const DefaultCommandTTL = 5 * time.Minute

// maxOutput bounds the setup and clone output one command retains for
// replay. Past it, output lines are dropped after one line saying so;
// step and result messages are always kept, and there are few of them.
const maxOutput = 1 << 20

// command is one add or rm in flight or recently finished. Its events,
// progress then the result, are appended as they happen; a connection
// that sends the same id, while it runs or after, replays them and follows.
type command struct {
	mu        sync.Mutex
	cond      *sync.Cond
	events    []protocol.Message
	outBytes  int
	truncated bool
	done      bool
	doneAt    time.Time
}

func newCommand() *command {
	c := &command{}
	c.cond = sync.NewCond(&c.mu)
	return c
}

func (c *command) emit(m protocol.Message) {
	c.mu.Lock()
	if m.State == protocol.StateOutput {
		c.outBytes += len(m.Detail)
		if c.outBytes > maxOutput {
			if c.truncated {
				c.mu.Unlock()
				return
			}
			c.truncated = true
			m.Detail = "(further output dropped: over 1 MiB)"
		}
	}
	c.events = append(c.events, m)
	if m.Type == protocol.TypeResult {
		c.done = true
		c.doneAt = time.Now()
	}
	c.mu.Unlock()
	c.cond.Broadcast()
}

// stream writes every event to pc, past and future, until the result has
// been sent or a write fails.
func (c *command) stream(pc *protocol.Conn) error {
	i := 0
	for {
		c.mu.Lock()
		for i >= len(c.events) && !c.done {
			c.cond.Wait()
		}
		batch := append([]protocol.Message(nil), c.events[i:]...)
		done := c.done
		c.mu.Unlock()
		for _, m := range batch {
			if err := pc.Write(m); err != nil {
				return err
			}
		}
		i += len(batch)
		if done && i >= len(c.events) {
			return nil
		}
	}
}

// command returns the command for id, creating it when unknown. The
// caller runs a new one; an existing one is only followed.
func (d *Daemon) command(id string) (*command, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if c, ok := d.cmds[id]; ok {
		return c, false
	}
	c := newCommand()
	d.cmds[id] = c
	return c, true
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
// The target is resolved under the repository lock, so an add in flight on
// the same repository is seen complete, not half done. A request that
// names the repository locks it; a root-only request, which is for a
// detached worktree, locks every known repository since the owner is not
// known until git has been asked. When repo, branch and root are all
// given they must agree: root is the worktree registered for the branch,
// or, once that registration is gone, the root the session step matches
// on. A root that git registers for another branch or repository is a
// mismatch, not a target.
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
			unlock := d.lockRepos(repo.Source)
			defer unlock()
		} else {
			if m.Root == "" {
				return errors.New("rm needs a repository and branch, or a root")
			}
			unlock := d.lockRepos()
			defer unlock()
		}

		root, checkout := m.Root, ""
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
				if taken {
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

// lockRepos takes the locks of the given repository sources, or of every
// known repository when none is given, in sorted order so two callers
// taking several never deadlock. The returned func releases them.
func (d *Daemon) lockRepos(sources ...string) func() {
	if len(sources) == 0 {
		for _, r := range d.cfg.Store.Repos {
			sources = append(sources, r.Source)
		}
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
