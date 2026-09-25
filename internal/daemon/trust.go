package daemon

import (
	"context"
	"strings"
	"time"
)

// Claude Code asks, the first time it starts in a folder, whether the
// folder is trusted, and waits for the answer before it shows its
// prompt box or takes a prompt given on its command line. Every
// worktree laatmux makes is a folder Claude has not seen, so every add
// would stop there: a typed prompt times out, one on the command line
// waits unseen. laatmux made the folder, from a repository the user
// configured, on the user's host, so it answers yes for them: only in
// the pane the add launched, only for the add's root under the host's
// worktrees directory, and only when the screen reads as the question
// for that root. Text that does not match is left alone, and
// the wait for the agent times out as before.

// The question as Claude Code 2 draws it:
//
//	Accessing workspace:
//	/home/me/src/worktrees/proj/branch
//	Quick safety check: Is this a project you created or one you trust? ...
//	❯ No, exit
//	  Yes, I trust this folder
//	Enter to confirm · Esc to cancel
const (
	trustHead   = "Accessing workspace:"
	trustYes    = "Yes, I trust this folder"
	trustCursor = "❯"
)

// trustPoll is how often the pane is looked at for the question; a
// variable for tests.
var trustPoll = 500 * time.Millisecond

// trustChoice reads a screen for the trust question about root. ok is
// that the question is there and names root, the path joined back when
// the pane wrapped it; moves is the Down presses, negative for Up, that
// bring the cursor to yes, 0 when it is on it.
func trustChoice(screen []string, root string) (moves int, ok bool) {
	head := -1
	for i, l := range screen {
		if strings.TrimSpace(l) == trustHead {
			head = i
		}
	}
	if head < 0 {
		return 0, false
	}
	path, i := "", head+1
	for ; i < len(screen) && path != root; i++ {
		path += strings.TrimSpace(screen[i])
		if !strings.HasPrefix(root, path) {
			return 0, false
		}
	}
	if path != root {
		return 0, false
	}
	cursor, yes := -1, -1
	for ; i < len(screen); i++ {
		l := strings.TrimSpace(screen[i])
		if strings.HasPrefix(l, trustCursor) {
			cursor = i
		}
		if strings.Contains(l, trustYes) {
			yes = i
		}
	}
	if cursor < 0 || yes < 0 {
		return 0, false
	}
	moves = yes - cursor
	if moves < -3 || moves > 3 {
		// Options are on consecutive lines; anything further apart is
		// not the list this reads.
		return 0, false
	}
	return moves, true
}

// answerTrust watches the pane an add launched for the trust question
// about root, for as long as the wait for an agent, and answers yes
// once: the cursor is moved onto yes, the screen read again, and Enter
// pressed only with the cursor there. It stops once it has answered,
// the pane shows the agent's prompt box, the pane is gone, or the wait
// is over. At most a handful of keys are pressed in all.
//
// wait and poll are readyWait and trustPoll as the launch read them.
func (d *Daemon) answerTrust(ctx context.Context, paneID, root string, wait, poll time.Duration) {
	ctx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	presses := 0
	for ctx.Err() == nil {
		if d.promptBoxUp(paneID) {
			return
		}
		screen, err := d.managed.Tmux.Capture(ctx, paneID, d.cfg.CaptureLines)
		if err != nil {
			return
		}
		if moves, ok := trustChoice(screen, root); ok {
			key, n := "Down", moves
			if moves < 0 {
				key, n = "Up", -moves
			}
			if moves == 0 {
				key, n = "Enter", 1
			}
			if presses+n > 6 {
				return
			}
			keys := make([]string, n)
			for i := range keys {
				keys[i] = key
			}
			if err := d.managed.Tmux.SendKeys(ctx, paneID, keys...); err != nil {
				return
			}
			presses += n
			if key == "Enter" {
				d.cfg.Logger.Printf("agent: answered the folder trust question for %s in pane %s", root, paneID)
				return
			}
		}
		select {
		case <-ctx.Done():
		case <-time.After(poll):
		}
	}
}

// promptBoxUp is that the detector saw the agent's prompt box in the
// pane: past the question, whoever answered it.
func (d *Daemon) promptBoxUp(paneID string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	st, ok := d.panes[paneKey(d.managed.Label, paneID)]
	return ok && st.obs.verified && st.obs.idle
}

// underWorktrees is that root is a worktree under the host's worktrees
// directory, one laatmux lays out.
func (d *Daemon) underWorktrees(root string) bool {
	if d.cfg.Store == nil || d.cfg.Store.Dirs.Worktrees == "" {
		return false
	}
	return strings.HasPrefix(root, strings.TrimSuffix(d.cfg.Store.Dirs.Worktrees, "/")+"/")
}
