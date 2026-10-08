package daemon

import (
	"context"
	"path/filepath"
	"sync"

	"github.com/laat/laatmux/internal/protocol"
	"github.com/laat/laatmux/internal/tmux"
	"github.com/laat/laatmux/internal/worktree"
)

// factWorkers bounds the roots a facts request reads at once: each is
// a handful of git calls, and a status in a large repository takes a
// while.
const factWorkers = 4

// facts answers prune's facts message with one record per root, read
// beside the connection's loop as relayPrompt is, so the connection
// stays read while git runs. Only a root that git lists as a worktree
// of a checkout here, under the worktrees directory, is read; any
// other has an error of its own, as does one whose read failed.
func (c *clientConn) facts(m protocol.Message) error {
	d := c.d
	res := protocol.Message{Type: protocol.TypeResult, ID: m.ID}
	if d.cfg.Store == nil {
		res.Error = "this host has no repos and worktrees directories configured"
		return c.pc.Write(res)
	}
	go func() {
		res.Facts, res.OK = readFacts(c.ctx, d.cfg.Store, m.Roots), true
		if err := c.pc.Write(res); err != nil {
			c.drop()
		}
	}()
	return nil
}

// readFacts is the facts for each root, in order, the listing read
// once for all of them.
func readFacts(ctx context.Context, store *worktree.Store, roots []string) []protocol.RootFacts {
	recs, listErr := store.List(ctx)
	listed := map[string]bool{}
	for _, r := range recs {
		listed[r.Root] = true
	}
	out := make([]protocol.RootFacts, len(roots))
	slots := make(chan struct{}, factWorkers)
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
		wg.Add(1)
		slots <- struct{}{}
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

// deleteBranch is rm's delete_branch, once git has removed the
// worktree from checkout: the branch goes when it is still at the
// commit the client decided on, and stays, said with why, when it has
// moved or another worktree has it checked out. Either is a progress
// message of its own: the worktree is gone, and what rm does after,
// the sessions, is still to be done. checkout is "" when this run
// found no worktree to remove, one that ran before the daemon
// restarted say; which clone the branch was in is not known then, and
// it stays.
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
