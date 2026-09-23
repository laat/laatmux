// Package command is the client side of add, rm and shell: one
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

// stream sends a command with progress to the host and returns its
// result, dialing again with the same id when the transport fails
// mid-way: the daemon replays what it already sent and follows, and the
// reporter only sees messages not seen before. The hello of the
// connection that delivered the result is returned with it.
func stream(ctx context.Context, h client.Host, needCap string, m protocol.Message, r Reporter) (hello, res protocol.Message, err error) {
	f := &replayFilter{fn: r.Progress}
	const attempts = 3
	for attempt := 1; ; attempt++ {
		f.reset()
		c, err := client.Dial(ctx, h)
		if err != nil {
			// A redial after a started attempt is a transport failure
			// like any other and spends the same budget.
			if attempt == 1 || attempt == attempts || ctx.Err() != nil {
				return hello, res, err
			}
			r.Note(fmt.Sprintf("%s: reconnect failed (%v); retrying", h.Name, err))
			if err := pause(ctx); err != nil {
				return hello, res, err
			}
			continue
		}
		if !protocol.Has(c.Hello.Capabilities, needCap) {
			c.Close()
			return hello, res, fmt.Errorf("%s: daemon %s does not support %s", h.Name, c.Hello.Version, needCap)
		}
		hello = c.Hello
		res, err = c.Stream(ctx, m, f.pass)
		c.Close()
		// A result, ok or not, ends it: Stream returns the daemon's
		// refusals with the result message. Only a transport failure,
		// which has no message, is retried.
		if err == nil || res.Type != "" || ctx.Err() != nil || attempt == attempts {
			return hello, res, err
		}
		r.Note(fmt.Sprintf("%s: connection lost (%v); reconnecting to follow %s", h.Name, err, m.Type))
		if err := pause(ctx); err != nil {
			return hello, res, err
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

// replayFilter passes each progress message on once across reconnects:
// the daemon replays a command's stream from the start, so messages are
// counted per connection and only those past the high-water mark are new.
type replayFilter struct {
	seen, n int
	fn      func(protocol.Message)
}

func (f *replayFilter) reset() { f.n = 0 }

func (f *replayFilter) pass(p protocol.Message) {
	f.n++
	if f.n > f.seen {
		f.seen = f.n
		if f.fn != nil {
			f.fn(p)
		}
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
