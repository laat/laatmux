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

// tick is how often the ages are redrawn.
const tick = 5 * time.Second

// escapeWait is how long a bare escape, or the start of a sequence, is
// held for the rest before it is read as the escape key. tmux writes a
// sequence in one go, so the wait is only ever paid for the escape key.
const escapeWait = 50 * time.Millisecond

// Run draws the model and handles keys until the host is done, q is
// pressed, or ctx ends. The rows are refreshed on every change signal
// and the ages every five seconds; a resize redraws.
func Run(ctx context.Context, t *Term, m *Model, h Host) error {
	keys := make(chan []byte)
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
			case keys <- b:
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
	var flush <-chan time.Time
	h.Refresh(m)
	draw()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-h.Changed:
			h.Refresh(m)
		case <-tk.C:
		case <-winch:
		case <-flush:
			flush = nil
			if handle(dec.Flush()) {
				return nil
			}
		case b, ok := <-keys:
			if !ok {
				return nil
			}
			if handle(dec.Feed(b)) {
				return nil
			}
			flush = nil
			if dec.Pending() {
				flush = time.After(escapeWait)
			}
		}
		draw()
	}
}
