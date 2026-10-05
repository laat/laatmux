package view

import (
	"context"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/laat/laatmux/internal/term"
)

// Host is what a view's host supplies: a signal that the rows changed,
// how to refresh the model from them, and what to do on an action.
type Host struct {
	// Changed is signalled when the rows may have changed.
	Changed <-chan struct{}
	// Refresh fills the model's rows and header from the host's state.
	Refresh func(m *Model)
	// Act handles a jump or another key; true ends the view.
	Act func(m *Model, a Action) (done bool)
	// Commands carries what reaches the view from outside the
	// terminal: a sidebar subcommand over the pane's socket, folds
	// another pane wrote. Each runs on the view's goroutine with the
	// model, and its action goes to Act as a key's would.
	Commands <-chan func(m *Model) Action
}

// tick is how often the ages are redrawn when no time in seconds is on
// screen. The spinner on a working row is redrawn every spinTick, only
// while a visible row spins, and a time in seconds, `m:ss`, every
// second, only while one is drawn, so an idle pane costs nothing
// between the ticks.
const (
	tick       = 5 * time.Second
	secondTick = time.Second
)

// answerLate is how long an answer to the background query is expected
// after the query gave up on it.
const answerLate = 3 * time.Second

// escapeWait is how long a bare escape, or the start of a sequence, is
// held for the rest before it is read as the escape key. tmux writes a
// sequence in one go, so the wait is only ever paid for the escape key.
const escapeWait = 50 * time.Millisecond

// Run draws the model and handles keys until the host is done, q is
// pressed, or ctx ends. The keys come from the terminal's Input, a
// batch per read or flush, and the screen is redrawn after each. The rows are refreshed on every change signal
// and the ages every five seconds, every second while a time in seconds
// is drawn, the spinner four times a second while a working row is on
// the list; a resize redraws. An overlay that
// finishes on its own is noticed on the change signal, so a host that
// ends one from another goroutine signals it.
func Run(ctx context.Context, t *term.Term, m *Model, h Host) error {
	keys := t.Input(ctx)
	winch := make(chan os.Signal, 1)
	signal.Notify(winch, syscall.SIGWINCH)
	defer signal.Stop(winch)
	tk := time.NewTicker(tick)
	defer tk.Stop()
	draw := func() {
		m.Now = time.Now()
		m.Width, m.Height = t.Size()
		t.Draw(encode(m.Render(), t.Theme))
		// The new screen is on the terminal from here: a click read
		// before now was on the one before it.
		m.hitAt = time.Now()
	}
	handle := func(ks []term.Key) (done bool) {
		for _, k := range ks {
			a := m.Handle(k)
			if a.Kind == ActionQuit {
				return true
			}
			if a.Kind != ActionNone && h.Act(m, a) {
				return true
			}
			if m.SettingsChanged() && h.Act(m, Action{Kind: ActionSettings}) {
				return true
			}
		}
		return false
	}
	var spin <-chan time.Time
	h.Refresh(m)
	if m.SettingsChanged() && h.Act(m, Action{Kind: ActionSettings}) {
		return nil
	}
	draw()
	for {
		spin = nil
		switch {
		case m.Spinning():
			spin = time.After(spinTick)
		case m.Ticking():
			spin = time.After(secondTick)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-h.Changed:
			h.Refresh(m)
			if a := m.Poll(); a.Kind != ActionNone && h.Act(m, a) {
				return nil
			}
			// A fold carried across a handoff is a setting changed.
			if m.SettingsChanged() && h.Act(m, Action{Kind: ActionSettings}) {
				return nil
			}
		case <-tk.C:
			// The rows again, not only the ages: an agent idle long
			// enough turns stale with no record changing.
			h.Refresh(m)
			if m.SettingsChanged() && h.Act(m, Action{Kind: ActionSettings}) {
				return nil
			}
		case f := <-h.Commands:
			if a := f(m); a.Kind != ActionNone && a.Kind != ActionQuit && h.Act(m, a) {
				return nil
			} else if a.Kind == ActionQuit {
				return nil
			}
			if m.SettingsChanged() && h.Act(m, Action{Kind: ActionSettings}) {
				return nil
			}
		case <-spin:
		case <-winch:
		case ks, ok := <-keys:
			if !ok {
				return nil
			}
			if handle(ks) {
				return nil
			}
		}
		draw()
	}
}
