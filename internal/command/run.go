package command

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/laat/laatmux/internal/config"
	"github.com/laat/laatmux/internal/protocol"
)

// Run is one run: a command in a worktree root on its host, with the
// output streamed back through the Reporter as output progress, one line
// per message with FD saying which stream it came from. The root names
// the worktree; the repository and branch, when known, are sent for the
// daemon to check against it.
type Run struct {
	Host   config.Host
	Repo   config.Repo // zero when unknown
	Branch string
	Root   string
	Cmd    []string
	ID     string
	// Cancel, when it receives or is closed, asks the daemon to stop the
	// process; Run then waits for the result, which says cancelled.
	Cancel <-chan struct{}
}

// Ran is what a run left behind: the exit status of the process.
type Ran struct {
	Exit int
}

// Run sends the run to the host and follows it to the result. A refusal
// or a cancelled run is a *StageError with the daemon's message; a lost
// connection whose follow finds the daemon no longer knows the run is
// ErrOutcomeUnknown, since the process may be running still.
func (r Run) Run(ctx context.Context, rep Reporter) (Ran, error) {
	if r.Host.Name == "" || r.Root == "" {
		return Ran{}, errors.New("run needs a host and a worktree root")
	}
	if len(r.Cmd) == 0 {
		return Ran{}, errors.New("run needs a command")
	}
	id := r.ID
	if id == "" {
		id = ID("run")
	}
	req := protocol.Message{Type: protocol.TypeRun, ID: id, Repo: r.Repo.Source, Branch: r.Branch, Root: r.Root, Cmd: r.Cmd}
	_, res, err := stream(ctx, r.Host.Host, []string{protocol.CapRun}, req, rep, streamOpts{cancel: r.Cancel})
	if err != nil {
		if errors.Is(err, ErrOutcomeUnknown) {
			return Ran{}, err
		}
		return Ran{}, failed("run", res, err)
	}
	return Ran{Exit: res.Exit}, nil
}

// Describe is the one line that says what the run is: "run <cmd> in
// <repo>/<branch> on <host>".
func (r Run) Describe() string {
	where := r.Root
	if r.Repo.Name != "" && r.Branch != "" {
		where = r.Repo.Name + "/" + r.Branch
	}
	return fmt.Sprintf("run %s in %s on %s", strings.Join(r.Cmd, " "), where, r.Host.Name)
}

// Cancelled reports whether the error is a run the daemon stopped on
// request, or on its own shutdown.
func Cancelled(err error) bool {
	var se *StageError
	return errors.As(err, &se) && se.Stage == "" && se.Msg == protocol.ErrCancelled
}
