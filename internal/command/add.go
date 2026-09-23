package command

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/laat/laatmux/internal/config"
	"github.com/laat/laatmux/internal/home"
	"github.com/laat/laatmux/internal/protocol"
	"github.com/laat/laatmux/internal/workspace"
)

// Add is one add: a worktree for the branch on the host, an agent
// started in it, and the local workspace session opened. The inputs are
// resolved by the caller; see the CLI's flags and the dashboard's
// pickers.
type Add struct {
	Host   config.Host
	Repo   config.Repo
	Branch string
	// Agent is the configured agent to start; Cmd, when set, is the
	// command instead and Agent is "".
	Agent string
	Cmd   []string
	// ID is the command id; "" picks a fresh one. A caller that retries
	// keeps the id so the daemon resumes rather than restarts.
	ID string
}

// Added is what an add left behind.
type Added struct {
	Root    string // the worktree root on the host
	Managed string // the managed session on the host
	Session string // the local workspace session
	Created bool   // the local session was made rather than found
}

// Run sends the add to the host, following its progress through r,
// then records the host and agent as the repository's last used and
// makes sure the local workspace session exists. A failed stage is a
// *StageError.
func (a Add) Run(ctx context.Context, r Reporter) (Added, error) {
	if a.Host.Name == "" || a.Repo.Source == "" || a.Branch == "" {
		return Added{}, errors.New("add needs a host, a repository and a branch")
	}
	id := a.ID
	if id == "" {
		id = ID("add")
	}
	req := protocol.Message{Type: protocol.TypeAdd, ID: id, Repo: a.Repo.Source, Branch: a.Branch, AgentName: a.Agent, Cmd: a.Cmd}
	hello, res, err := stream(ctx, a.Host.Host, protocol.CapAdd, req, r)
	if err != nil {
		return Added{}, failed("add", res, err)
	}
	out := Added{Root: res.Root, Managed: res.Session}
	if err := home.UpdateLast(func(l *home.Last) {
		cur := l.Get(a.Repo.Source)
		cur.Host = a.Host.Name
		if a.Agent != "" {
			cur.Agent = a.Agent
		}
		l.Set(a.Repo.Source, cur)
	}); err != nil {
		return out, err
	}
	out.Session, out.Created, err = workspace.Ensure(ctx, workspace.Spec{
		Host: a.Host.Host, Managed: res.Session,
		Name:   workspace.SessionName(a.Host.Name, a.Repo.Name, a.Branch),
		Key:    workspace.Key(hello.EnvironmentID, res.Root),
		Source: a.Repo.Source, Branch: a.Branch,
	})
	return out, err
}

// Describe is the one line that says what the add is: "add
// <repo>/<branch> on <host> with <agent>".
func (a Add) Describe() string {
	s := fmt.Sprintf("add %s/%s on %s", a.Repo.Name, a.Branch, a.Host.Name)
	if a.Agent != "" {
		s += " with " + a.Agent
	} else if len(a.Cmd) > 0 {
		s += " running " + strings.Join(a.Cmd, " ")
	}
	return s
}
