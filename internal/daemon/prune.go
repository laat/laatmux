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
				f.InUse = rn.inUseAt(clean, panes, false)
			}
			out[i] = f
		}()
	}
	wg.Wait()
	return out
}

// activePane is a pane of a watched server as inUseAt places it: what
// to call it, the root laatmux made it at when it made it, and its
// current path as tmux reports it and resolved.
type activePane struct {
	desc       string
	made       string
	path, real string
}

// listActive lists every server the daemon watches now, not as the
// last poll saw them, since a poll that fails keeps the records it had:
// the panes something of the user's may run in, laatmux's own left
// out, as the pane records leave them. A server that is not running
// has none; any other failure is an error, as a pane it hides may be
// in a worktree.
func (rn *taskRunner) listActive(ctx context.Context) ([]activePane, error) {
	var out []activePane
	for _, t := range rn.targets {
		panes, err := t.Tmux.ListPanes(ctx)
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
			a := activePane{desc: fmt.Sprintf("pane %s of session %s on the %s server", p.ID, tmux.Printable(p.Session), t.Label), path: p.CurrentPath}
			if p.Managed {
				a.made = p.Cwd
			}
			if real, err := filepath.EvalSymlinks(p.CurrentPath); err == nil {
				a.real = real
			}
			out = append(out, a)
		}
	}
	return out, nil
}

// inUseAt is what runs in a worktree root, for prune, which removes
// only a worktree nothing uses: an add at it with no outcome yet; a
// run; or a pane of panes, listed by listActive, made at the root, as
// rm kills it, or with its path under it. "" when nothing does. With
// fence, rm's, the root's removal generation is bumped where the runs
// are read, so a run that resolved before cannot register after;
// should the removal not happen, such a run is refused with a retry.
// rm holds every repository, which an add holds until its agent runs
// and new holds while it makes a session in a worktree, so neither
// starts anything between this and the kill.
func (rn *taskRunner) inUseAt(root string, panes []activePane, fence bool) string {
	under := func(p string) bool { return p == root || strings.HasPrefix(p, root+"/") }
	if rn.journal != nil {
		if id := rn.journal.liveAt(root); id != "" {
			return "add " + id + ", which has no outcome yet"
		}
	}
	rn.mu.Lock()
	if fence {
		rn.rootGen[root]++
	}
	runs := len(rn.runs[root])
	rn.mu.Unlock()
	if runs > 0 {
		return "a run"
	}
	for _, p := range panes {
		if p.made == root || under(p.path) || p.real != "" && under(p.real) {
			return p.desc
		}
	}
	return ""
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
