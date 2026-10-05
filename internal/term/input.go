package term

import (
	"context"
	"time"
)

// Read is one read of the terminal, stamped as it arrived: a click is
// on the screen that was drawn then, whatever is drawn before it is
// handled.
type Read struct {
	B  []byte
	At time.Time
}

// Input is the terminal's keys as a loop drives them: Reads carries
// each read as it arrives, from a goroutine that reads until the read
// fails, when Reads is closed, or ctx ends, when the goroutine stops at
// its next send and Reads stays open; Flush fires when held bytes are
// due, the escape wait or a paste's grace later. The loop decodes each
// read with Decode and the due bytes with Flushed, handles the keys,
// then Rearms the timer, so a sequence split by a slow handler is still
// waited for after it. Start, first, decodes what Background read.
type Input struct {
	Reads <-chan Read
	Flush <-chan time.Time
	dec   Decoder
	t     *Term
}

// Input starts reading the terminal.
func (t *Term) Input(ctx context.Context) *Input {
	reads := make(chan Read)
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
			case reads <- Read{b, time.Now()}:
			case <-ctx.Done():
				return
			}
		}
	}()
	return &Input{Reads: reads, t: t}
}

// Start is the keys that came while Background asked the terminal, to
// handle before anything is drawn; an answer the query did not get is
// expected for a while, so a late one is swallowed rather than read as
// keys.
func (in *Input) Start() []Key {
	if in.t.unanswered {
		in.dec.ExpectAnswer(time.Now().Add(answerLate))
	}
	if len(in.t.pending) == 0 {
		return nil
	}
	ks := in.dec.FeedAt(in.t.pending, time.Now())
	in.t.pending = nil
	return ks
}

// Decode is the keys a read gives, complete ones; what it holds back
// is Flushed when due. The timer is stopped, for Rearm after the keys
// are handled.
func (in *Input) Decode(r Read) []Key {
	in.Flush = nil
	return in.dec.FeedAt(r.B, r.At)
}

// Flushed is the keys the held bytes give once their wait is up.
func (in *Input) Flushed() []Key {
	in.Flush = nil
	return in.dec.Flush()
}

// Rearm sets the timer for the held bytes, if any are held: after a
// read, the escape wait; after a flush, a paste under way is looked at
// again, so one whose end never comes is taken once its bytes have
// stopped.
func (in *Input) Rearm() {
	if w := in.dec.Wait(); w > 0 {
		in.Flush = time.After(w)
	}
}
