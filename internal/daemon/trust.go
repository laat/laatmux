package daemon

import (
	"context"
	"path/filepath"
	"strings"
	"time"

	"github.com/laat/laatmux/internal/procs"
	"github.com/laat/laatmux/internal/tmux"
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
//     launched as on the tmux server instance it was launched on, as
//     tmux says under the lock before each key, its working directory
//     the root, with a verified live Claude identified in it, the same
//     process throughout;
//   - for the add's root under the host's worktrees directory;
//   - when the bottom of the screen is the whole question and nothing
//     after it, naming exactly that root, with exactly its two options
//     and one cursor on them;
//   - each key under the root's delivery lock, which a paste into the
//     pane also takes, on a capture made under that lock.
//
// Anything else is left alone, and the wait for the agent times out as
// before. A user who moves the selection between the capture and the
// key press is the one race a capture cannot rule out; the keys are two
// at most.

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
	cursor, yes, no := -1, -1, -1
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
			if yes >= 0 {
				return 0, false
			}
			yes = i
		case trustNo:
			if no >= 0 {
				return 0, false
			}
			no = i
		default:
			return 0, false
		}
	}
	if cursor < 0 || yes < 0 || no < 0 {
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
	if !pathLines(lines[head+1:question], root) {
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
// and the tmux server instance it was made in, and the root, with real
// its spelling with symlinks resolved, which is how tmux reports a
// pane's directory and may be how Claude names it.
type trustTarget struct {
	pane, session, root, real string
	serverPID                 int
}

// trustState is what the latest observation says of the target: gone
// for good when the pane is in another session or on another server
// instance, claude when a verified live Claude is identified in it,
// with its process identity, and ready when its prompt box is up, past
// the question. It is the detector's view, as old as its last poll;
// trustStep checks the pane itself before any key.
func (d *Daemon) trustState(t trustTarget) (gone, claude, ready bool, id procs.Identity) {
	d.mu.Lock()
	defer d.mu.Unlock()
	st, ok := d.panes[paneKey(d.managed.Label, t.pane)]
	if !ok {
		return false, false, false, procs.Identity{}
	}
	o := st.obs
	if o.session != "" && (o.session != t.session || o.serverPID != t.serverPID) {
		return true, false, false, procs.Identity{}
	}
	claude = o.verified && o.identity.Agent == "claude"
	return false, claude, claude && o.idle, o.identity
}

// startTrust starts the watcher for a launch, unless the daemon is
// stopping; StopRuns cancels the watchers and waits for them.
func (d *Daemon) startTrust(t trustTarget, wait, poll time.Duration) {
	if d.cfg.Store == nil || !d.cfg.Store.Owns(t.root) {
		return
	}
	t.real = t.root
	if r, err := filepath.EvalSymlinks(t.root); err == nil {
		t.real = r
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
//
// The first verified Claude seen in the pane is the one answered for:
// another process in its place, a wrapper's second run say, ends the
// watcher.
func (d *Daemon) answerTrust(ctx context.Context, t trustTarget, poll time.Duration) {
	moved := false
	var bound *procs.Identity
	for ctx.Err() == nil {
		gone, claude, ready, id := d.trustState(t)
		if gone || ready {
			return
		}
		// Any other process identified in the pane once one is bound,
		// Claude or not, ends the watcher.
		if bound != nil && id.PID != 0 && (id.PID != bound.PID || !id.Start.Equal(bound.Start)) {
			return
		}
		if claude {
			if bound == nil {
				bound = &id
			}
			done, stop := d.trustStep(ctx, t, *bound, &moved)
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

// trustStep is one look and, when the question is there, one action,
// under the root's delivery lock: the moves that bring the cursor to
// yes, pressed once and never again, or Enter with the cursor on yes.
// The cursor starts off yes, so a capture that shows it on yes after the
// moves was made after them; one that still shows it elsewhere is a
// screen that lags or keys that went astray, and the watcher gives up
// rather than press more. done is that Enter was pressed; stop that the
// watcher should give up.
//
// The observation is the detector's and can be a poll old, so under the
// lock the pane itself is looked at first: still there and alive, in
// the launched session, on the launched server instance, its working
// directory the root, which is also the folder Claude asks about.
func (d *Daemon) trustStep(ctx context.Context, t trustTarget, bound procs.Identity, moved *bool) (done, stop bool) {
	unlock := d.lockDeliveries(t.root)
	defer unlock()
	gone, claude, _, id := d.trustState(t)
	if gone || !claude || id.PID != bound.PID || !id.Start.Equal(bound.Start) {
		return false, gone || claude
	}
	panes, err := d.managed.Tmux.ListPanes(ctx)
	if err != nil {
		return false, true
	}
	var here *tmux.Pane
	for i := range panes {
		if panes[i].ID == t.pane {
			here = &panes[i]
		}
	}
	switch {
	case here == nil || here.Dead || here.Session != t.session || here.ServerPID != t.serverPID:
		return false, true
	case here.CurrentPath != t.root && here.CurrentPath != t.real, here.InMode:
		// Not yet: a key to a pane in copy-mode or a chooser goes to the
		// mode, whatever the capture shows.
		return false, false
	}
	// The bound Claude itself, now, not as the last poll saw it: a
	// replacement started in the pane since gets nothing.
	alive, err := d.cfg.Procs.Exists(here.TTY, bound)
	if err != nil {
		return false, false
	}
	if !alive {
		return false, true
	}
	screen, err := d.managed.Tmux.Capture(ctx, t.pane, d.cfg.CaptureLines)
	if err != nil {
		return false, true
	}
	moves, ok := trustChoice(screen, t.root)
	if !ok && t.real != t.root {
		moves, ok = trustChoice(screen, t.real)
	}
	if !ok {
		return false, false
	}
	if moves != 0 {
		if *moved {
			return false, true
		}
		key, n := "Down", moves
		if moves < 0 {
			key, n = "Up", -moves
		}
		keys := make([]string, n)
		for i := range keys {
			keys[i] = key
		}
		*moved = true
		if err := d.managed.Tmux.SendKeys(ctx, t.pane, keys...); err != nil {
			return false, true
		}
		return false, false
	}
	if err := d.managed.Tmux.SendKeys(ctx, t.pane, "Enter"); err != nil {
		return false, true
	}
	d.cfg.Logger.Printf("agent: answered the folder trust question for %s in pane %s", t.root, t.pane)
	return true, false
}

// pathLines is that the lines, trimmed, spell root as the pane wrapped
// it: in order, with nothing left over, one space of the root allowed to
// be missing at each line break and nowhere else, since tmux drops a
// line's trailing spaces and the trim a continuation's leading ones.
func pathLines(lines []string, root string) bool {
	rest := root
	for i, l := range lines {
		if i > 0 && strings.HasPrefix(rest, " ") && !strings.HasPrefix(l, " ") {
			rest = rest[1:]
		}
		if !strings.HasPrefix(rest, l) {
			return false
		}
		rest = rest[len(l):]
	}
	return rest == ""
}
