package command

import (
	"context"
	"errors"

	"github.com/laat/laatmux/internal/config"
	"github.com/laat/laatmux/internal/protocol"
	"github.com/laat/laatmux/internal/workspace"
)

// Rm is one rm: the worktree and its managed session on the host, then
// the local workspace session. Either the repository and branch or the
// root names the worktree; the root is sent whenever it is known, since
// it is what reaches a managed session whose worktree is already gone,
// and alone it removes a detached worktree.
type Rm struct {
	Host   config.Host
	Repo   config.Repo // zero when only the root is known
	Branch string
	Root   string
	Force  bool
	ID     string
}

// Removed is what an rm left behind: the root it acted on, "" when
// nothing was registered for the branch and no root was known, and the
// local sessions it killed.
type Removed struct {
	Root   string
	Killed []string
}

// Run sends the rm to the host, following its progress through r, then
// kills the local workspace session for the root. A refusal, git's for
// a dirty worktree say, is a *StageError with its message.
func (m Rm) Run(ctx context.Context, r Reporter) (Removed, error) {
	if m.Host.Name == "" {
		return Removed{}, errors.New("rm needs a host")
	}
	if (m.Repo.Source == "" || m.Branch == "") && m.Root == "" {
		return Removed{}, errors.New("rm needs a repository and branch, or a root")
	}
	id := m.ID
	if id == "" {
		id = ID("rm")
	}
	req := protocol.Message{Type: protocol.TypeRm, ID: id, Repo: m.Repo.Source, Branch: m.Branch, Root: m.Root, Force: m.Force}
	hello, res, err := stream(ctx, m.Host.Host, protocol.CapRm, req, r)
	if err != nil {
		return Removed{}, failed("rm", res, err)
	}
	out := Removed{Root: res.Root}
	if out.Root == "" {
		out.Root = m.Root
	}
	if out.Root == "" {
		return out, nil
	}
	// The local workspace session is the client's to clean up.
	locals, err := workspace.List(ctx)
	if err != nil {
		return out, err
	}
	key := workspace.Key(hello.EnvironmentID, out.Root)
	for _, l := range locals {
		if l.Key == key {
			if err := workspace.Kill(ctx, l.Name); err != nil {
				return out, err
			}
			out.Killed = append(out.Killed, l.Name)
		}
	}
	return out, nil
}

// Describe names what the rm removes: <repo>/<branch>, or the root.
func (m Rm) Describe() string {
	if m.Repo.Name != "" && m.Branch != "" {
		return m.Repo.Name + "/" + m.Branch
	}
	return m.Root
}

// RootOf is the root rm sends for a branch when the worktree's record
// is not in the host's records: the key of the local workspace session
// found by its source and branch tags, which survive a renamed host or
// label. A session found by name instead is accepted only when it
// carries no identity tags at all, from before they existed: tags that
// name another source or branch mean the name has moved on to another
// workspace, and its root must not be sent with this one's identity.
// "" when there is no such session on this host.
func RootOf(locals []workspace.Local, environmentID string, h config.Host, repo config.Repo, branch string) string {
	l, ok := workspace.FindWorktree(locals, environmentID, repo.Source, branch)
	if !ok {
		l, ok = workspace.ByName(locals, workspace.SessionName(h.Name, repo.Name, branch))
		ok = ok && l.Source == "" && l.Branch == ""
	}
	if !ok || !l.Workspace() {
		return ""
	}
	if env, root := workspace.SplitKey(l.Key); env == environmentID {
		return root
	}
	return ""
}
