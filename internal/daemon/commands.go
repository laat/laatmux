package daemon

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"time"

	"github.com/laat/laatmux/internal/protocol"
	"github.com/laat/laatmux/internal/source"
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
	result    protocol.Message
	followers int // goroutines serving streams, the wakers included, for tests
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
	served := func() {
		c.mu.Lock()
		c.followers--
		c.mu.Unlock()
	}
	c.mu.Lock()
	c.followers++
	c.mu.Unlock()
	defer served()
	// The waker takes the lock, so it runs either before the wait below
	// checked quit or after the wait has released the lock, never in
	// between: the wait cannot miss it. It ends with the stream, so a
	// connection that sends many commands does not collect one per
	// finished command. A nil quit, from a test, never closes.
	if quit != nil {
		ended := make(chan struct{})
		defer close(ended)
		c.mu.Lock()
		c.followers++
		c.mu.Unlock()
		go func() {
			defer served()
			select {
			case <-quit:
				c.mu.Lock()
				c.cond.Broadcast()
				c.mu.Unlock()
			case <-ended:
			}
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

// commandTable is the recent commands by key (an id, or a prompt's id
// and attempt): those in flight, and those finished within the TTL, so
// a client that lost its bridge can follow the id and get the result
// back. Its lock guards the table alone: forgetDone takes command.mu
// under it and get runs its init under it, and nothing else is taken
// under it; never the daemon's mu. rm's forgetDone holds it under repos and the
// root's delivery lock.
type commandTable struct {
	mu   sync.Mutex
	byID map[string]*command
	ttl  time.Duration
}

func newCommandTable(ttl time.Duration) *commandTable {
	return &commandTable{byID: map[string]*command{}, ttl: ttl}
}

// get returns the command for id, creating it when unknown, with init
// run on it before any other connection can see it, so a cancel that
// arrives at once finds the job. The caller runs a new one; an existing
// one is only followed.
func (t *commandTable) get(id string, init func(*command)) (*command, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if c, ok := t.byID[id]; ok {
		return c, false
	}
	c := newCommand(id)
	if init != nil {
		init(c)
	}
	t.byID[id] = c
	return c, true
}

// lookup returns the command for id when the table still has it.
func (t *commandTable) lookup(id string) (*command, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	c, ok := t.byID[id]
	return c, ok
}

// forgetDone drops the command under id when it has finished.
func (t *commandTable) forgetDone(id string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	c, ok := t.byID[id]
	if !ok {
		return
	}
	c.mu.Lock()
	done := c.done
	c.mu.Unlock()
	if done {
		delete(t.byID, id)
	}
}

// evict forgets a finished command once the TTL has passed, whether or
// not any other command arrives meanwhile. The identity check keeps a
// timer from evicting a newer command under the same id.
func (t *commandTable) evict(id string, c *command) {
	time.AfterFunc(t.ttl, func() {
		t.mu.Lock()
		if t.byID[id] == c {
			delete(t.byID, id)
		}
		t.mu.Unlock()
	})
}

// keyedLocks hands out a lock per key, made on first use: the
// repository, name, delivery and attempt locks the commands serialize
// on. Its own lock holds only the map, never a lock handed out.
type keyedLocks struct {
	mu    sync.Mutex
	byKey map[string]*sync.Mutex
}

func newKeyedLocks() *keyedLocks { return &keyedLocks{byKey: map[string]*sync.Mutex{}} }

// get is the lock for key.
func (k *keyedLocks) get(key string) *sync.Mutex {
	k.mu.Lock()
	defer k.mu.Unlock()
	l, ok := k.byKey[key]
	if !ok {
		l = &sync.Mutex{}
		k.byKey[key] = l
	}
	return l
}

// lockDeliveries takes the root's delivery lock, which deliveries to
// the root hold across their readiness check and paste, and an add
// across the publication of its result; the returned func releases it.
func (rn *taskRunner) lockDeliveries(root string) func() {
	l := rn.repoLock("deliver/" + root)
	l.Lock()
	return l.Unlock
}

// repoLock is the keyed lock for key: "repo/<source>", which
// serializes commands per repository, since fetch and worktree add
// write to the same main checkout, while different repositories
// proceed in parallel; "name/<name>", "deliver/<root>" and
// "attempt/<id>".
func (rn *taskRunner) repoLock(key string) *sync.Mutex { return rn.locks.get(key) }

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
func (rn *taskRunner) runRm(ctx context.Context, m protocol.Message, c *command) {
	res := protocol.Message{Type: protocol.TypeResult, ID: m.ID}
	err := func() error {
		if m.Repo != "" {
			if m.Branch == "" && m.Root == "" {
				return errors.New("rm needs a branch or a root")
			}
		} else if m.Root == "" {
			return errors.New("rm needs a repository and branch, or a root")
		}
		unlock := rn.lockRepos()
		defer unlock()

		// The repository is found under the lock: an add still making
		// its first checkout is done before the checkouts are scanned.
		// One with no checkout here is still reached by the root, which
		// finds the session.
		var repo worktree.Repo
		if m.Repo != "" {
			r, ok, err := rn.cfg.Store.Known(ctx, m.Repo)
			switch {
			case err != nil:
				return err
			case ok:
				repo = r
			case m.Root != "":
				repo = worktree.Repo{Source: m.Repo, Name: m.Repo}
			default:
				return fmt.Errorf("unknown repository %q: not in this host's config, and no checkout of it here", m.Repo)
			}
		}
		root, checkout := m.Root, ""
		if root != "" {
			// The client's root, cleaned so it compares with what git
			// registered and the pane recorded. Only a root under the
			// worktrees directory is a target: sessions elsewhere, made by
			// new with any cwd, are not rm's to kill.
			root = filepath.Clean(root)
			if !rn.cfg.Store.Owns(root) {
				return fmt.Errorf("%s is not under the worktrees directory %s", tmux.Printable(root), tmux.Printable(rn.cfg.Store.Dirs.Worktrees))
			}
		}
		switch {
		case root != "":
			// The root decides the checkout, since two clones of one
			// repository each list their own worktrees; repository and
			// branch, when given, must be what git registers there.
			rec, co, found, err := rn.cfg.Store.Find(ctx, root)
			if err != nil {
				return err
			}
			if found {
				if repo.Source != "" && !source.Same(rec.Source, repo.Source) {
					return fmt.Errorf("%s is a worktree of %s, not %s", tmux.Printable(root), rec.Repo, repo.Name)
				}
				if m.Branch != "" && rec.Branch != m.Branch {
					return fmt.Errorf("%s is the worktree for %s of %s, not %s", tmux.Printable(root), branchOrDetached(rec.Branch), rec.Repo, m.Branch)
				}
				checkout = co
				break
			}
			if m.Branch != "" {
				// The registration at the root is gone; the root still
				// finds the session, unless the branch has moved to a
				// worktree elsewhere since.
				rec, _, moved, err := rn.cfg.Store.ByBranch(ctx, repo, m.Branch)
				if err != nil {
					return err
				}
				if moved {
					return fmt.Errorf("branch %s of %s is checked out at %s, not %s", m.Branch, repo.Name, tmux.Printable(rec.Root), tmux.Printable(root))
				}
			}
		default:
			rec, co, found, err := rn.cfg.Store.ByBranch(ctx, repo, m.Branch)
			if err != nil {
				return err
			}
			if found {
				root, checkout = rec.Root, co
			}
		}
		if root == "" {
			// Nothing registered for the branch and no root to match
			// sessions on: both steps skip.
			return nil
		}
		res.Root = root
		// From the removal on, everything is under the root's delivery
		// lock: git's removal, the sessions going, the journal's entries
		// marked removed and the finished commands the memory holds for
		// them dropped. A delivery holds the same lock across its
		// readiness check and paste, so it never pastes into a root git
		// has removed and, once it gets the lock, finds the tombstone
		// rather than a replacement session; an add records and
		// publishes its result under it, so no success is published
		// between the mark and the drop.
		unlockDeliveries := rn.lockDeliveries(root)
		defer unlockDeliveries()
		if checkout != "" {
			removed, err := worktree.Remove(ctx, checkout, root, m.Force)
			if err != nil {
				return err
			}
			if removed {
				// A listing from here on has one worktree fewer. The
				// result carries the step, which dates the removal
				// against the adds at the root: rm's dismiss drops only
				// the tasks from before it.
				l := rn.core.stepRevision()
				res.Listing = &l
			}
		}
		// Git has agreed to the removal: what runs in the root is
		// laatmux's own, like the session, and goes before it. The wait
		// makes the ok mean nothing of laatmux's is left there.
		rn.cancelRunsIn(root)
		// The journal's entries at the root are removed with it: a
		// follow or a resend for one is answered removed from now on,
		// which means the finished commands the memory still holds for
		// them go, so the journal answers.
		if rn.journal != nil {
			ids, err := rn.journal.markRemoved(root, time.Now())
			for _, id := range ids {
				rn.cmds.forgetDone(id)
			}
			if err != nil {
				return err
			}
		}
		panes, err := rn.managed.Tmux.ListPanes(ctx)
		if err != nil {
			if tmux.NoServer(err) {
				return nil
			}
			return err
		}
		// Each session is killed by its id, which reaches it whatever its
		// name: a session made by hand can have a name no target reaches
		// (tmux.CheckTarget), and a name no session has is looked up as a
		// client's. It is killed on the server instance it was listed on:
		// one started since, by hand or by a new that found none, numbers
		// its sessions from $0 again. A session that ended since the
		// listing, its agent having exited, is no error, and the rest are
		// still killed.
		killed := map[string]bool{}
		for _, p := range panes {
			if !p.Managed || p.Cwd != root || killed[p.SessionID] {
				continue
			}
			if err := rn.managed.Tmux.KillSessionID(ctx, p.SessionID, p.ServerPID); err != nil {
				return err
			}
			killed[p.SessionID] = true
		}
		return nil
	}()
	rn.finish(c, res, err)
}

func branchOrDetached(branch string) string {
	if branch == "" {
		return "a detached HEAD"
	}
	return "branch " + branch
}

// holdRepos is the shared hold on every repository that an add takes
// before it resolves anything and keeps until its agent is launched,
// and that rm takes alone. The returned func releases it.
func (rn *taskRunner) holdRepos() func() {
	rn.repos.RLock()
	return rn.repos.RUnlock
}

// lockRepo takes a repository's lock, by its identity, so two forms of
// one source share it, then the lock on the name that places its
// checkout, so two sources sent under one name never clone into one
// directory at once. The caller has the shared hold. The order is
// always hold, source, name, and nothing waits on a source holding a
// name. The returned func releases both locks.
func (rn *taskRunner) lockRepo(src, name string) func() {
	repo := rn.repoLock("repo/" + source.Key(src))
	repo.Lock()
	dir := rn.repoLock("name/" + name)
	dir.Lock()
	return func() {
		dir.Unlock()
		repo.Unlock()
	}
}

// lockRepos holds every repository, known or not: no add is in flight
// until the returned func releases them.
func (rn *taskRunner) lockRepos() func() {
	rn.repos.Lock()
	return rn.repos.Unlock
}

// finish records the result and asks for a worktree poll, so the record
// follows the command.
func (rn *taskRunner) finish(c *command, res protocol.Message, err error) {
	res = resultOf(res, err)
	rn.core.pokeWorktrees()
	c.emit(res)
	rn.cmds.evict(res.ID, c)
}

// resultOf is the result message for an outcome: ok, or the error with
// the stage it failed at.
func resultOf(res protocol.Message, err error) protocol.Message {
	if err == nil {
		res.OK = true
		return res
	}
	var se *worktree.StageError
	if errors.As(err, &se) {
		res.Stage = se.Stage
		res.Error = se.Err.Error()
	} else {
		res.Error = err.Error()
	}
	return res
}

func stageErr(stage string, err error) error { return &worktree.StageError{Stage: stage, Err: err} }
