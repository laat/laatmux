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

// commandTTL is how long a finished command's outcome is kept, so a client
// that lost its bridge can repeat the id and get the result back.
const commandTTL = 5 * time.Minute

// command is one add or rm in flight or recently finished. Its events,
// progress then the result, are appended as they happen; a connection
// that sends the same id, while it runs or after, replays them and follows.
type command struct {
	mu     sync.Mutex
	cond   *sync.Cond
	events []protocol.Message
	done   bool
	doneAt time.Time
}

func newCommand() *command {
	c := &command{}
	c.cond = sync.NewCond(&c.mu)
	return c
}

func (c *command) emit(m protocol.Message) {
	c.mu.Lock()
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
	now := time.Now()
	for k, c := range d.cmds {
		c.mu.Lock()
		expired := c.done && now.Sub(c.doneAt) > commandTTL
		c.mu.Unlock()
		if expired {
			delete(d.cmds, k)
		}
	}
	if c, ok := d.cmds[id]; ok {
		return c, false
	}
	c := newCommand()
	d.cmds[id] = c
	return c, true
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
func (d *Daemon) runRm(ctx context.Context, m protocol.Message, c *command) {
	res := protocol.Message{Type: protocol.TypeResult, ID: m.ID}
	err := func() error {
		root, checkout, source := m.Root, "", ""
		if root == "" {
			repo, ok := d.cfg.Store.Repo(m.Repo)
			if !ok {
				return fmt.Errorf("unknown repository %q: not in this host's config", m.Repo)
			}
			if m.Branch == "" {
				return errors.New("rm needs a branch or a root")
			}
			source = repo.Source
			co, found, err := d.cfg.Store.Checkout(ctx, repo)
			if err != nil {
				return err
			}
			if found {
				entries, err := worktree.ListWorktrees(ctx, co)
				if err != nil {
					return err
				}
				for _, e := range entries {
					if e.Branch == m.Branch && e.Root != co && !e.Prunable {
						root, checkout = e.Root, co
					}
				}
			}
		} else if rec, co, found, err := d.cfg.Store.Find(ctx, root); err != nil {
			return err
		} else if found {
			checkout, source = co, rec.Source
		}
		if source != "" {
			l := d.repoLock(source)
			l.Lock()
			defer l.Unlock()
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
}

func stageErr(stage string, err error) error { return &worktree.StageError{Stage: stage, Err: err} }
