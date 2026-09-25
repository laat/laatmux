package view

import (
	"context"
	"os"
	"os/signal"
	"syscall"
	"time"
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
}

// tick is how often the ages are redrawn. The spinner on a working row
// is redrawn every spinTick, and only while a visible row spins, so an
// idle pane costs nothing between the ticks.
const tick = 5 * time.Second

// escapeWait is how long a bare escape, or the start of a sequence, is
// held for the rest before it is read as the escape key. tmux writes a
// sequence in one go, so the wait is only ever paid for the escape key.
const escapeWait = 50 * time.Millisecond

// Run draws the model and handles keys until the host is done, q is
// pressed, or ctx ends. The rows are refreshed on every change signal
// and the ages every five seconds, the spinner ten times a second while
// a working row is on the list; a resize redraws. An overlay that
// finishes on its own is noticed on the change signal, so a host that
// ends one from another goroutine signals it.
func Run(ctx context.Context, t *Term, m *Model, h Host) error {
	// Each read is stamped as it arrives: a click is on the screen that
	// was drawn then, whatever is drawn before it is handled.
	type input struct {
		b  []byte
		at time.Time
	}
	keys := make(chan input)
	go func() {
		buf := make([]byte, 256)
		for {
			n, err := t.in.Read(buf)
			if err != nil {
				close(keys)
				return
			}
			b := make([]byte, n)
			copy(b, buf[:n])
			select {
			case keys <- input{b, time.Now()}:
			case <-ctx.Done():
				return
			}
		}
	}()
	winch := make(chan os.Signal, 1)
	signal.Notify(winch, syscall.SIGWINCH)
	defer signal.Stop(winch)
	tk := time.NewTicker(tick)
	defer tk.Stop()
	draw := func() {
		m.Now = time.Now()
		m.Width, m.Height = t.Size()
		t.Draw(m.Render())
	}
	handle := func(ks []Key) (done bool) {
		for _, k := range ks {
			a := m.Handle(k)
			if a.Kind == ActionQuit {
				return true
			}
			if a.Kind != ActionNone && h.Act(m, a) {
				return true
			}
		}
		return false
	}
	var dec Decoder
	var flush, spin <-chan time.Time
	h.Refresh(m)
	draw()
	for {
		spin = nil
		if m.Spinning() {
			spin = time.After(spinTick)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-h.Changed:
			h.Refresh(m)
			if a := m.Poll(); a.Kind != ActionNone && h.Act(m, a) {
				return nil
			}
		case <-tk.C:
		case <-spin:
		case <-winch:
		case <-flush:
			flush = nil
			if handle(dec.Flush()) {
				return nil
			}
			if w := dec.Wait(); w > 0 {
				// A paste under way: looked at again, so one whose end
				// never comes is taken once its bytes have stopped.
				flush = time.After(w)
			}
		case in, ok := <-keys:
			if !ok {
				return nil
			}
			ks := dec.Feed(in.b)
			for i := range ks {
				if ks[i].Kind == KeyMouse {
					ks[i].At = in.at
				}
			}
			if handle(ks) {
				return nil
			}
			flush = nil
			if w := dec.Wait(); w > 0 {
				flush = time.After(w)
			}
		}
		draw()
	}
}
