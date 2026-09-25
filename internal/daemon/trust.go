package daemon

import (
	"context"
	"strings"
	"time"
)

// Claude Code asks, the first time it starts in a folder, whether the
// folder is trusted, and waits for the answer before it shows its
// prompt box or takes a prompt given on its command line. A worktree
// laatmux makes can be such a folder, and an add would stop there: a
// typed prompt times out, one on the command line waits unseen.
// laatmux made or took up the folder, from a repository the user
// configured, on the user's host, so it answers yes for them, and only
// then:
//
//   - in the pane the add launched, still in the session it was
//     launched as on the tmux server instance it was launched on, with
//     a verified live Claude identified in it;
//   - for the add's root under the host's worktrees directory;
//   - when the bottom of the screen is the whole question and nothing
//     after it, naming exactly that root, with exactly its two options
//     and one cursor on them;
//   - each key under the root's delivery lock, which a paste into the
//     pane also takes, on a capture made under that lock.
//
// Anything else is left alone, and the wait for the agent times out as
// before. A user who moves the selection between the capture and the
// key press is the one race a capture cannot rule out; the keys are a
// handful at most.

// The question as Claude Code 2 draws it, at the bottom of the pane:
//
//	Accessing workspace:
//	/home/me/src/worktrees/proj/branch
//	Quick safety check: Is this a project you created or one you trust? ...
//	...
//	Security guide
//	❯ No, exit
//	  Yes, I trust this folder
//	Enter to confirm · Esc to cancel
const (
	trustHead     = "Accessing workspace:"
	trustQuestion = "Quick safety check:"
	trustNo       = "No, exit"
	trustYes      = "Yes, I trust this folder"
	trustFoot     = "Enter to confirm"
	trustCursor   = "❯"
)

// trustPoll is how often the pane is looked at for the question; a
// variable for tests.
var trustPoll = 500 * time.Millisecond

// trustChoice reads a screen for the trust question about root. ok is
// that the screen ends with the whole question, naming root, the path
// joined back when the pane wrapped it; moves is the Down presses,
// negative for Up, that bring the cursor to yes, 0 when it is on it.
func trustChoice(screen []string, root string) (moves int, ok bool) {
	var lines []string
	for _, l := range screen {
		if t := strings.TrimSpace(l); t != "" {
			lines = append(lines, t)
		}
	}
	n := len(lines)
	// The footer is the last line, the two options just above it.
	if n < 6 || !strings.HasPrefix(lines[n-1], trustFoot) {
		return 0, false
	}
	cursor, yes := -1, -1
	for _, i := range []int{n - 3, n - 2} {
		label, selected := trustOption(lines[i])
		if selected {
			if cursor >= 0 {
				return 0, false
			}
			cursor = i
		}
		switch label {
		case trustYes:
			yes = i
		case trustNo:
		default:
			return 0, false
		}
	}
	if cursor < 0 || yes < 0 || lines[n-3] == lines[n-2] {
		return 0, false
	}
	// Above them the question, and above it the path under the heading:
	// the last heading, and between it and the question the path alone.
	head := -1
	for i := n - 4; i >= 0; i-- {
		if lines[i] == trustHead {
			head = i
			break
		}
		if strings.HasPrefix(lines[i], trustCursor) {
			return 0, false
		}
	}
	if head < 0 {
		return 0, false
	}
	question := -1
	for i := head + 1; i < n-3; i++ {
		if strings.HasPrefix(lines[i], trustQuestion) {
			question = i
			break
		}
	}
	if question < 0 || question == head+1 {
		return 0, false
	}
	if strings.Join(lines[head+1:question], "") != root {
		return 0, false
	}
	return yes - cursor, true
}

// trustOption is an option line's label, without the cursor and a
// "1." numbering, and whether the cursor is on it.
func trustOption(line string) (label string, selected bool) {
	if strings.HasPrefix(line, trustCursor) {
		selected = true
		line = strings.TrimSpace(strings.TrimPrefix(line, trustCursor))
	}
	if i := strings.Index(line, ". "); i > 0 && i <= 2 && strings.Trim(line[:i], "0123456789") == "" {
		line = strings.TrimSpace(line[i+2:])
	}
	return line, selected
}

// trustTarget is the launch a watcher answers for: the pane, the session
// and the tmux server instance it was made in, and the root.
type trustTarget struct {
	pane, session, root string
	serverPID           int
}

// trustState is what the latest observation says of the target: gone
// for good when the pane is in another session or on another server
// instance, claude when a verified live Claude is identified in it, and
// ready when its prompt box is up, past the question.
func (d *Daemon) trustState(t trustTarget) (gone, claude, ready bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	st, ok := d.panes[paneKey(d.managed.Label, t.pane)]
	if !ok {
		return false, false, false
	}
	o := st.obs
	if o.session != "" && (o.session != t.session || o.serverPID != t.serverPID) {
		return true, false, false
	}
	claude = o.verified && o.identity.Agent == "claude"
	return false, claude, claude && o.idle
}

// startTrust starts the watcher for a launch, unless the daemon is
// stopping; StopRuns cancels the watchers and waits for them.
func (d *Daemon) startTrust(t trustTarget, wait, poll time.Duration) {
	if d.cfg.Store == nil || !d.cfg.Store.Owns(t.root) {
		return
	}
	d.mu.Lock()
	if d.stopping {
		d.mu.Unlock()
		return
	}
	if d.trustCancel == nil {
		d.trustCtx, d.trustCancel = context.WithCancel(context.Background())
	}
	ctx := d.trustCtx
	d.trusting++
	d.mu.Unlock()
	go func() {
		defer func() {
			d.mu.Lock()
			d.trusting--
			d.mu.Unlock()
		}()
		run := d.runCtx()
		ctx, cancel := context.WithTimeout(ctx, wait)
		defer cancel()
		stop := context.AfterFunc(run, cancel)
		defer stop()
		d.answerTrust(ctx, t, poll)
	}()
}

// answerTrust watches the launch for the trust question and answers
// yes once: the cursor moved onto yes, then Enter with it there, each
// press under the root's delivery lock on a capture made under it. It
// stops once it has answered, the prompt box is up, the pane is taken
// by another session or server, the capture fails, or ctx ends.
func (d *Daemon) answerTrust(ctx context.Context, t trustTarget, poll time.Duration) {
	presses := 0
	for ctx.Err() == nil {
		gone, claude, ready := d.trustState(t)
		if gone || ready {
			return
		}
		if claude {
			done, stop := d.trustStep(ctx, t, &presses)
			if done || stop {
				return
			}
		}
		select {
		case <-ctx.Done():
		case <-time.After(poll):
		}
	}
}

// trustStep is one look and, when the question is there, one press,
// under the root's delivery lock. done is that Enter was pressed; stop
// that the watcher should give up.
func (d *Daemon) trustStep(ctx context.Context, t trustTarget, presses *int) (done, stop bool) {
	unlock := d.lockDeliveries(t.root)
	defer unlock()
	if gone, claude, _ := d.trustState(t); gone || !claude {
		return false, gone
	}
	screen, err := d.managed.Tmux.Capture(ctx, t.pane, d.cfg.CaptureLines)
	if err != nil {
		return false, true
	}
	moves, ok := trustChoice(screen, t.root)
	if !ok {
		return false, false
	}
	key := "Enter"
	switch {
	case moves > 0:
		key = "Down"
	case moves < 0:
		key = "Up"
	}
	if *presses >= 4 {
		return false, true
	}
	*presses++
	if err := d.managed.Tmux.SendKeys(ctx, t.pane, key); err != nil {
		return false, true
	}
	if key != "Enter" {
		return false, false
	}
	d.cfg.Logger.Printf("agent: answered the folder trust question for %s in pane %s", t.root, t.pane)
	return true, false
}
