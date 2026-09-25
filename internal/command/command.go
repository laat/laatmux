// Package command is the client side of add, rm, run and shell: one
// implementation each, called by the CLI and by the dashboard. Each does
// the work and reports through a Reporter; what is printed, and where,
// is the caller's. Resolving the inputs, a repository from a directory
// or a host from a flag, is the caller's too: it is where the caller's
// own vocabulary, flags or a selected row, becomes a request.
package command

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/laat/laatmux/internal/client"
	"github.com/laat/laatmux/internal/protocol"
)

// Reporter receives what a command has to say while it runs: one call
// per progress message from the daemon, and a note per transport event,
// a reconnect say. The CLI prints them; the dashboard draws them.
type Reporter interface {
	Progress(m protocol.Message)
	Note(s string)
}

// Discard is a Reporter that drops everything.
type Discard struct{}

func (Discard) Progress(protocol.Message) {}
func (Discard) Note(string)               {}

// ProgressLine is one progress message as the CLI prints it: the stage,
// its state and the detail; a setup command's output is indented under
// its step.
func ProgressLine(m protocol.Message) string {
	switch m.State {
	case protocol.StateOutput:
		return fmt.Sprintf("%-9s   | %s", "", m.Detail)
	case protocol.StateStart:
		return fmt.Sprintf("%-9s %-5s %s", m.Stage, "", m.Detail)
	default:
		return fmt.Sprintf("%-9s %-5s %s", m.Stage, m.State, m.Detail)
	}
}

// StageError is a command the daemon refused or failed at a stage: add
// failed at worktree, say. The dashboard keeps the stage on screen; the
// CLI prints it.
type StageError struct {
	Command string
	Stage   string
	Msg     string
}

func (e *StageError) Error() string {
	if e.Stage == "" {
		return e.Msg
	}
	return fmt.Sprintf("%s failed at %s: %s", e.Command, e.Stage, e.Msg)
}

// ID is a client-chosen id for one command invocation: unique across
// processes and time, and reused for every reconnect within it, so a
// dropped bridge resumes the same command rather than starting another.
func ID(kind string) string {
	return fmt.Sprintf("%s-%d-%d", kind, os.Getpid(), time.Now().UnixNano())
}

// SenderLifetime is how long after its submission an add may be sent or
// resent, by the sender's clock: the sender's side of the clock contract
// with the host's journal, which keeps an entry for thirty days. Past
// it the outcome is unknown rather than a fresh add on a host that has
// forgotten the id.
var SenderLifetime = 7 * 24 * time.Hour

// ErrSubmissionExpired is an add whose submission is older than the
// sender lifetime: it is not sent again, and what became of it is not
// known from here.
var ErrSubmissionExpired = errors.New("outcome unknown: the submission is older than seven days and is not sent again")

// stream sends a command with progress to the host and returns its
// result. When the transport fails mid-way it dials again and, against a
// daemon with follow, follows the id from the last numbered progress it
// saw; the daemon replays from there and the filter drops anything at or
// below the mark. Against an older daemon the command is resent and the
// replay filtered by position. A follow the daemon does not know the id
// of is answered as the kind of command decides: add and rm resend the
// command as a new execution, since every step of theirs is skipped by
// inspection; run reports the outcome unknown, since the process may be
// running still. A follow answered interrupted, by a daemon whose
// journal knows the add and that died in it, resends the add under its
// id and the daemon resumes it. Every capability in needCaps must be in
// the daemon's hello, on every connection. The hello of the connection
// that delivered the result is returned with it.
//
// This is the client's send path, and the sender lifetime is enforced
// here: a message with SubmittedAt is neither sent nor resent past it.
// The relay in the daemon has a send loop of its own, with unbounded
// backoff and its record on disk, under the same SenderLifetime.
func stream(ctx context.Context, h client.Host, needCaps []string, m protocol.Message, r Reporter, o streamOpts) (hello, res protocol.Message, err error) {
	f := &progressFilter{fn: r.Progress}
	sent := false // the command may have reached a daemon on this execution
	ever := false // it was written to a daemon at some point: nothing is refused as unsent after
	const attempts = 3
	for attempt := 1; ; attempt++ {
		c, err := client.Dial(ctx, h)
		if err != nil {
			// A redial after a started attempt is a transport failure
			// like any other and spends the same budget.
			if attempt == 1 || attempt == attempts || ctx.Err() != nil {
				return hello, res, notSent(ever, err)
			}
			r.Note(fmt.Sprintf("%s: reconnect failed (%v); retrying", h.Name, err))
			if err := pause(ctx); err != nil {
				return hello, res, err
			}
			continue
		}
		for _, cap := range needCaps {
			if !protocol.Has(c.Hello.Capabilities, cap) {
				c.Close()
				return hello, res, notSent(ever, fmt.Errorf("%s: daemon %s does not support %s", h.Name, c.Hello.Version, cap))
			}
		}
		if o.environment != "" && c.Hello.EnvironmentID != o.environment {
			c.Close()
			return hello, res, notSent(ever, fmt.Errorf("%s: answers as environment %s, not %s the request was resolved for", h.Name, c.Hello.EnvironmentID, o.environment))
		}
		hello = c.Hello
		follow := sent && protocol.Has(c.Hello.Capabilities, protocol.CapFollow)
		req := m
		if follow {
			req = protocol.Message{Type: protocol.TypeFollow, ID: m.ID, After: f.mark, Attempt: o.attempt}
		} else if !m.SubmittedAt.IsZero() && time.Since(m.SubmittedAt) > SenderLifetime {
			// The lifetime bounds sends and resends of the command; a
			// follow executes nothing and is asked at any age.
			c.Close()
			return hello, res, notSent(ever, ErrSubmissionExpired)
		}
		f.numbered = protocol.Has(c.Hello.Capabilities, protocol.CapFollow)
		f.reset()
		sent, ever = true, true
		res, err = exchange(ctx, c, req, o.cancel, f.pass)
		c.Close()
		unknown := res.Error == protocol.ErrUnknownCommand || res.Error == protocol.ErrInterrupted || (o.attempt > 0 && res.Error == protocol.ErrUnknownAttempt)
		if follow && res.Type == protocol.TypeResult && !res.OK && unknown {
			if !o.restart {
				return hello, res, ErrOutcomeUnknown
			}
			// A new execution numbers from 1 again; a resumed one too,
			// since the daemon that resumes it has no memory of the
			// numbers the one that died gave.
			if res.Error == protocol.ErrInterrupted {
				r.Note(fmt.Sprintf("%s: daemon restarted during %s %s at %s; sending it again to resume", h.Name, m.Type, m.ID, res.Stage))
			} else {
				r.Note(fmt.Sprintf("%s: daemon no longer knows %s %s; sending it again", h.Name, m.Type, m.ID))
			}
			f.mark, f.seen, sent = 0, 0, false
			attempt--
			continue
		}
		// A result, ok or not, ends it: the daemon's refusals come with
		// the result message. Only a transport failure, which has no
		// message, is retried.
		if err == nil || res.Type != "" || ctx.Err() != nil || attempt == attempts {
			return hello, res, err
		}
		r.Note(fmt.Sprintf("%s: connection lost (%v); reconnecting to follow %s", h.Name, err, m.Type))
		if err := pause(ctx); err != nil {
			return hello, res, err
		}
	}
}

// NotSent is a failure before the command was written to any daemon:
// the host could not be reached, its daemon lacks a capability, it
// answers as another environment, or the submission has expired. The
// command did not run, so nothing is unknown about it.
type NotSent struct{ Err error }

func (e *NotSent) Error() string { return e.Err.Error() }
func (e *NotSent) Unwrap() error { return e.Err }

// notSent wraps a refusal when the command was never sent; after a
// send the daemon may hold it, and the error is what it was.
func notSent(sent bool, err error) error {
	if sent {
		return err
	}
	return &NotSent{Err: err}
}

// streamOpts is what differs between the commands on a stream.
type streamOpts struct {
	// restart resends the command when a follow finds the id unknown.
	restart bool
	// cancel, when it receives or is closed, sends a cancel for the
	// command on the current connection, and again on each reconnect,
	// and the stream keeps waiting for the result.
	cancel <-chan struct{}
	// environment, when set, is the environment id the daemon must
	// answer as, on every connection; another is a refusal before the
	// command is sent.
	environment string
	// attempt, on a prompt message, is the attempt number a follow
	// carries; a follow answered unknown attempt resends the message.
	attempt int
}

// ErrOutcomeUnknown is a run whose daemon no longer knows the id after a
// lost connection: the process may be running still, may have finished,
// or may never have started.
var ErrOutcomeUnknown = errors.New("outcome unknown: the daemon no longer knows the run; it may be running still, finished, or never started")

// exchange sends one request on the connection and reads until its
// result, passing progress to onProgress. A cancel that arrives is sent
// after the request, never before it, so it cannot reach the daemon
// ahead of the command it stops. Cancelling ctx closes the connection.
func exchange(ctx context.Context, c *client.Conn, req protocol.Message, cancel <-chan struct{}, onProgress func(protocol.Message)) (protocol.Message, error) {
	defer c.CloseOnDone(ctx)()
	if err := c.Write(req); err != nil {
		return protocol.Message{}, err
	}
	if cancel != nil {
		stop := make(chan struct{})
		defer close(stop)
		go func() {
			select {
			case <-cancel:
				_ = c.Write(protocol.Message{Type: protocol.TypeCancel, ID: req.ID})
			case <-stop:
			}
		}()
	}
	for {
		m, err := c.Read()
		if err != nil {
			if ctx.Err() != nil {
				return protocol.Message{}, ctx.Err()
			}
			return protocol.Message{}, err
		}
		if m.ID != req.ID {
			continue
		}
		switch m.Type {
		case protocol.TypeProgress:
			onProgress(m)
		case protocol.TypeError:
			return m, errors.New(m.Error)
		case protocol.TypeResult:
			if !m.OK {
				return m, errors.New(m.Error)
			}
			return m, nil
		}
	}
}

// pause waits a second between attempts, or returns when ctx ends.
func pause(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(time.Second):
		return nil
	}
}

// progressFilter passes each progress message on once across
// reconnects. Numbered progress, from a daemon with follow, passes when
// its n is past the mark; unnumbered progress, from an older daemon that
// replays a command's stream from the start, is counted per connection
// and passes past the count seen. Both counters advance on every message
// passed, so a redial that lands on the other kind of daemon still
// filters.
type progressFilter struct {
	numbered bool   // the current connection numbers its progress
	mark     uint64 // highest n passed
	seen     int    // messages passed in all
	n        int    // messages received on this connection
	fn       func(protocol.Message)
}

func (f *progressFilter) reset() { f.n = 0 }

func (f *progressFilter) pass(p protocol.Message) {
	f.n++
	if f.numbered {
		if p.N <= f.mark {
			return
		}
		f.mark = p.N
	} else {
		if f.n <= f.seen {
			return
		}
		if p.N > f.mark {
			f.mark = p.N
		}
	}
	f.seen++
	if f.fn != nil {
		f.fn(p)
	}
}

// failed turns a stream's outcome into the error the caller reports: a
// result with a stage names it; a refusal or a transport failure is
// passed through.
func failed(command string, res protocol.Message, err error) error {
	if err == nil {
		return nil
	}
	if res.Type == protocol.TypeResult && res.Stage != "" {
		return &StageError{Command: command, Stage: res.Stage, Msg: res.Error}
	}
	if res.Type != "" {
		return &StageError{Command: command, Msg: res.Error}
	}
	if errors.Is(err, context.Canceled) {
		return err
	}
	return err
}
