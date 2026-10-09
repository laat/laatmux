package daemon

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/laat/laatmux/internal/protocol"
	"github.com/laat/laatmux/internal/tmux"
	"github.com/laat/laatmux/internal/worktree"
)

// factWorkers bounds the roots the facts messages read at once, those
// of every connection together: each is a handful of git calls, and a
// status in a large repository takes a while.
const factWorkers = 4

// facts answers prune's facts message with one record per root, read
// beside the connection's loop as relayPrompt is, so the connection
// stays read while git runs. The reads end with the connection: a
// prune interrupted leaves nothing running here. Only a root that git
// lists as a worktree of a checkout here, under the worktrees
// directory, is read; any other has an error of its own, as does one
// whose read failed.
func (c *clientConn) facts(m protocol.Message) error {
	d := c.d
	res := protocol.Message{Type: protocol.TypeResult, ID: m.ID}
	if d.cfg.Store == nil {
		res.Error = "this host has no repos and worktrees directories configured"
		return c.pc.Write(res)
	}
	ctx, cancel := c.context()
	go func() {
		defer cancel()
		res.Facts, res.OK = d.tasks.readFacts(ctx, d.factSlots, m.Roots), true
		if ctx.Err() != nil {
			return
		}
		if err := c.pc.Write(res); err != nil {
			c.drop()
		}
	}()
	return nil
}

// context is the daemon's context, ended with the connection too, for
// work done for this connection alone; cancel releases it sooner.
func (c *clientConn) context() (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(c.ctx)
	go func() {
		select {
		case <-c.quit:
			cancel()
		case <-ctx.Done():
		}
	}()
	return ctx, cancel
}

// readFacts is the facts for each root, in order, the listing and the
// watched servers read once for all of them; slots bounds the roots
// read at once. What runs in a root is what inUseAt finds there, said
// in the plan, which rm's unused checks again under its locks.
func (rn *taskRunner) readFacts(ctx context.Context, slots chan struct{}, roots []string) []protocol.RootFacts {
	store := rn.cfg.Store
	recs, listErr := store.List(ctx)
	panes, panesErr := rn.listActive(ctx)
	listed := map[string]bool{}
	for _, r := range recs {
		listed[r.Root] = true
	}
	out := make([]protocol.RootFacts, len(roots))
	var wg sync.WaitGroup
	for i, root := range roots {
		out[i].Root = root
		clean := filepath.Clean(root)
		switch {
		case listed[clean]:
		case listErr != nil:
			out[i].Error = listErr.Error()
			continue
		default:
			out[i].Error = tmux.Printable(root) + " is not a worktree under the worktrees directory " + tmux.Printable(store.Dirs.Worktrees)
			continue
		}
		select {
		case slots <- struct{}{}:
		case <-ctx.Done():
			out[i].Error = ctx.Err().Error()
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-slots }()
			f, err := worktree.ReadFacts(ctx, clean)
			f.Root = root
			switch {
			case err != nil:
				f.Error = err.Error()
			case panesErr != nil:
				f.InUse = "what the host's tmux servers hold, not known: " + panesErr.Error()
			default:
				f.InUse = rn.inUseAt(clean, panes)
			}
			out[i] = f
		}()
	}
	wg.Wait()
	return out
}

// activePane is a pane of a watched server as inUseAt places it: what
// to call it, and the root laatmux made it at, when it made it, and its
// current path, each as tmux reports it and resolved.
type activePane struct {
	desc       string
	made, path []string
}

// listTimeout bounds each watched server's listing for prune: rm lists
// them holding every repository, and a server whose hook hangs must
// not hold every add with it. A variable for tests.
var listTimeout = 10 * time.Second

// listActive lists every server the daemon watches now, not as the
// last poll saw them, since a poll that fails keeps the records it had:
// the panes something of the user's may run in, laatmux's own left
// out, as the pane records leave them. A server that is not running
// has none; any other failure is an error, as a pane it hides may be
// in a worktree. Paths are resolved by the daemon's resolver, which
// never waits on the file system: a pane on a hung mount holds up
// nothing, and its path counts as tmux reports it until the answer is
// there.
func (rn *taskRunner) listActive(ctx context.Context) ([]activePane, error) {
	var out []activePane
	for _, t := range rn.targets {
		lctx, cancel := context.WithTimeout(ctx, listTimeout)
		panes, err := t.Tmux.ListPanes(lctx)
		cancel()
		if err != nil && !tmux.HookOnly(err) {
			if tmux.NoServer(err) {
				continue
			}
			return nil, fmt.Errorf("the %s server's panes: %w", t.Label, err)
		}
		for _, p := range panes {
			if p.Own {
				continue
			}
			a := activePane{desc: fmt.Sprintf("pane %s of session %s on the %s server", p.ID, tmux.Printable(p.Session), t.Label)}
			if p.Managed && p.Cwd != "" {
				a.made = []string{p.Cwd, rn.paths.resolve(p.Cwd)}
			}
			if p.CurrentPath != "" {
				a.path = []string{p.CurrentPath, rn.paths.resolve(p.CurrentPath)}
			}
			out = append(out, a)
		}
	}
	return out, nil
}

// inUseAt is what runs in a worktree root, for prune, which removes
// only a worktree nothing uses: an add at it with no outcome yet; a
// run; or a pane of panes, listed by listActive, made at the root, as
// rm kills it, or with its path under it. "" when nothing does.
func (rn *taskRunner) inUseAt(root string, panes []activePane) string {
	if rn.journal != nil {
		if id := rn.journal.liveAt(root); id != "" {
			return "add " + id + ", which has no outcome yet"
		}
	}
	rn.mu.Lock()
	runs := len(rn.runs[root])
	rn.mu.Unlock()
	if runs > 0 {
		return "a run"
	}
	for _, p := range panes {
		if slices.Contains(p.made, root) || slices.ContainsFunc(p.path, func(path string) bool { return under(path, root) }) {
			return p.desc
		}
	}
	return ""
}

// under reports whether path is root or inside it.
func under(path, root string) bool { return path == root || strings.HasPrefix(path, root+"/") }

// closeRoot fences a worktree root while rm's unused looks at it and
// removes it, until the returned func reopens it: a run that resolved
// before cannot register, by the removal generation bumped here, and
// none registers while it is closed; new refuses a session in it, and
// kills one it made meanwhile. rm looks after closing it, so what
// started before is seen, and what starts after is refused.
func (rn *taskRunner) closeRoot(root string) func() {
	rn.mu.Lock()
	defer rn.mu.Unlock()
	rn.rootGen[root]++
	rn.closing[root] = true
	return func() {
		rn.mu.Lock()
		defer rn.mu.Unlock()
		delete(rn.closing, root)
	}
}

// closedRootLocked is the closed root dir is in or at, "" when there is
// none. Called with rn.mu held.
func (rn *taskRunner) closedRootLocked(dir string) string {
	for root := range rn.closing {
		if under(dir, root) {
			return root
		}
	}
	return ""
}

// genUnderLocked is the removal generations of the roots dir is in or
// at, summed: they only grow, so a change between two reads is a root
// of dir closed by prune's rm or removed by any rm meanwhile, even one
// reopened since. Called with rn.mu held.
func (rn *taskRunner) genUnderLocked(dir string) uint64 {
	var n uint64
	for root, gen := range rn.rootGen {
		if under(dir, root) {
			n += gen
		}
	}
	return n
}

// newInWorktree makes a managed session named name for new when its
// directory, resolved, is in a worktree: refused while prune's rm has
// the worktree closed, and, made, killed again and refused when a root
// of it was closed or removed meanwhile, also one reopened and made
// again since, or the directory is gone, which tmux would have started
// the session outside of, in the home directory. create makes the
// session.
func (rn *taskRunner) newInWorktree(ctx context.Context, dir, name string, create func() (tmux.Session, error)) (tmux.Session, error) {
	rn.mu.Lock()
	closed, gen := rn.closedRootLocked(dir), rn.genUnderLocked(dir)
	rn.mu.Unlock()
	if closed != "" {
		return tmux.Session{}, fmt.Errorf("the worktree at %s is being removed; not made", tmux.Printable(closed))
	}
	if _, err := os.Stat(dir); err != nil {
		return tmux.Session{}, tmux.PrintablePath(err)
	}
	made, err := create()
	if err != nil {
		return made, err
	}
	rn.mu.Lock()
	moved := rn.genUnderLocked(dir) != gen
	rn.mu.Unlock()
	_, statErr := os.Stat(dir)
	if !moved && statErr == nil {
		return made, nil
	}
	why := "was closed for removal while its session was made; not made, retry"
	if statErr != nil {
		why = "was removed while its session was made; not made"
	}
	if err := rn.killMade(ctx, made, name); err != nil {
		return tmux.Session{}, fmt.Errorf("the worktree at %s %s, and the session made is left: %w", tmux.Printable(dir), why, err)
	}
	return tmux.Session{}, fmt.Errorf("the worktree at %s %s", tmux.Printable(dir), why)
}

// killMade kills the session new just made, found by its pane on the
// server it was made on and only while that pane is still in the
// session of the name made: a pane moved into another session since
// is left, with that session. One gone already is no error.
func (rn *taskRunner) killMade(ctx context.Context, made tmux.Session, name string) error {
	panes, err := rn.listManaged(ctx)
	if err != nil {
		return err
	}
	for _, p := range panes {
		if p.ID != made.PaneID || p.ServerPID != made.ServerPID {
			continue
		}
		if p.Session != name {
			return fmt.Errorf("its pane %s is in session %s now", p.ID, tmux.Printable(p.Session))
		}
		return rn.managed.Tmux.KillSessionID(ctx, p.SessionID, p.ServerPID)
	}
	return nil
}

// deleteBranch is rm's delete_branch, once git has removed the
// worktree from checkout: the branch goes when it is still at the
// commit the client decided on, and stays, said with why, when it has
// moved or another worktree has it checked out. Either is a progress
// message of its own: the worktree is gone, and what rm does after,
// the sessions, is still to be done. checkout is "" when this run
// removed no worktree, one that ran before the daemon restarted say;
// which clone the branch was in is not known then, and it stays.
func (rn *taskRunner) deleteBranch(ctx context.Context, c *command, m protocol.Message, checkout string) {
	say := func(state, detail string) {
		c.emit(protocol.Message{Type: protocol.TypeProgress, ID: m.ID, Stage: protocol.StageBranch, State: state, Detail: detail})
	}
	b := tmux.Printable(m.Branch)
	if checkout == "" {
		say(protocol.StateSkip, "branch "+b+" kept: no worktree of it was removed here")
		return
	}
	deleted, err := worktree.DeleteBranch(ctx, checkout, m.Branch, m.Head)
	switch {
	case err != nil:
		say(protocol.StateSkip, "branch "+b+" kept: "+err.Error())
	case deleted:
		say(protocol.StateDone, "deleted branch "+b)
	default:
		say(protocol.StateSkip, "branch "+b+" is gone already")
	}
}
