package term

import (
	"context"
	"time"
)

// Input reads the terminal until ctx ends or the read fails, decodes
// what it reads and sends the keys in the batches they decode to: a
// read's worth, or what a flush gives after the escape wait or a
// paste's grace. Each read is stamped as it arrives, so a click is on
// the screen that was drawn then, whatever is drawn before it is
// handled. Keys that came while Background asked the terminal are the
// first batch, and an answer the query did not get is expected for a
// while, so a late one is swallowed rather than read as keys. The
// channel closes when the read fails.
func (t *Term) Input(ctx context.Context) <-chan []Key {
	type input struct {
		b  []byte
		at time.Time
	}
	reads := make(chan input)
	go func() {
		buf := make([]byte, 256)
		for {
			n, err := t.in.Read(buf)
			if err != nil {
				close(reads)
				return
			}
			b := make([]byte, n)
			copy(b, buf[:n])
			select {
			case reads <- input{b, time.Now()}:
			case <-ctx.Done():
				return
			}
		}
	}()
	keys := make(chan []Key)
	go func() {
		defer close(keys)
		var dec Decoder
		var flush <-chan time.Time
		// Every batch is sent, an empty one too: the view redraws on
		// each, as it did on each read.
		send := func(ks []Key) bool {
			select {
			case keys <- ks:
				return true
			case <-ctx.Done():
				return false
			}
		}
		if t.unanswered {
			dec.ExpectAnswer(time.Now().Add(answerLate))
		}
		if len(t.pending) > 0 {
			ks := dec.FeedAt(t.pending, time.Now())
			t.pending = nil
			if !send(ks) {
				return
			}
			if w := dec.Wait(); w > 0 {
				flush = time.After(w)
			}
		}
		for {
			select {
			case <-ctx.Done():
				return
			case <-flush:
				flush = nil
				if !send(dec.Flush()) {
					return
				}
				if w := dec.Wait(); w > 0 {
					// A paste under way: looked at again, so one whose
					// end never comes is taken once its bytes have
					// stopped.
					flush = time.After(w)
				}
			case in, ok := <-reads:
				if !ok {
					return
				}
				if !send(dec.FeedAt(in.b, in.at)) {
					return
				}
				flush = nil
				if w := dec.Wait(); w > 0 {
					flush = time.After(w)
				}
			}
		}
	}()
	return keys
}
