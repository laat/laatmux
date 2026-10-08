package daemon

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"sync"

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
		res.Facts, res.OK = readFacts(ctx, d.cfg.Store, d.factSlots, m.Roots), true
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

// readFacts is the facts for each root, in order, the listing read
// once for all of them; slots bounds the roots read at once.
func readFacts(ctx context.Context, store *worktree.Store, slots chan struct{}, roots []string) []protocol.RootFacts {
	recs, listErr := store.List(ctx)
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
			if err != nil {
				f.Error = err.Error()
			}
			out[i] = f
		}()
	}
	wg.Wait()
	return out
}

// inUse is what runs in a worktree root, for rm's unused, prune's,
// which removes only a worktree nothing uses: an add at it with no
// outcome yet; a run; a pane of the managed server, listed now, made
// at the root or with its current path under it; or a pane of any
// server the daemon watches whose path the last poll saw under it,
// laatmux's own panes left out, as the pane records leave them. ""
// when nothing does. rm holds every repository, so an add's session is
// there by now or comes after the removal; a session new makes takes
// no lock, and one made between this and the kill is killed.
func (rn *taskRunner) inUse(ctx context.Context, root string) (string, error) {
	under := func(p string) bool { return p == root || strings.HasPrefix(p, root+"/") }
	if rn.journal != nil {
		if id := rn.journal.liveAt(root); id != "" {
			return "add " + id + ", which has no outcome yet", nil
		}
	}
	rn.mu.Lock()
	runs := len(rn.runs[root])
	var seen string
	for _, st := range rn.panes {
		if st.observed && !st.pane.Own && under(st.path) {
			server := "a watched"
			if st.target != nil {
				server = "the " + st.target.Label
			}
			seen = fmt.Sprintf("pane %s of session %s on %s server", st.pane.ID, tmux.Printable(st.pane.Session), server)
			break
		}
	}
	rn.mu.Unlock()
	switch {
	case runs > 0:
		return "a run", nil
	case seen != "":
		return seen, nil
	case rn.managed == nil:
		return "", nil
	}
	panes, err := rn.listManaged(ctx)
	if err != nil && !tmux.NoServer(err) {
		return "", err
	}
	for _, p := range panes {
		if p.Managed && p.Cwd == root || under(p.CurrentPath) {
			return fmt.Sprintf("pane %s of session %s on the %s server", p.ID, tmux.Printable(p.Session), rn.managed.Label), nil
		}
	}
	return "", nil
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
