package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/laat/laatmux/internal/client"
	"github.com/laat/laatmux/internal/config"
	"github.com/laat/laatmux/internal/protocol"
	"github.com/laat/laatmux/internal/rows"
	"github.com/laat/laatmux/internal/tmux"
	"github.com/laat/laatmux/internal/workspace"
)

// A jump to a pane: an agent's tile, an agent or a pane under a
// worktree. It is routed by the pane's server and session, not by the
// worktree's. A pane in a managed session: the jump to the workspace
// session, or the plain attachment to another managed session, then the
// host daemon's select command, which makes the pane current on the
// managed server, and the attach pane made current here, since the user
// may have left the session on a shell window. A pane on this machine's
// default server: switch-client, then select-window and select-pane. A
// pane on a remote host's default server, or another observed server, is
// refused as jump refuses it.

// paneTarget is what a row's pane jump needs of the record.
type paneTarget struct {
	server, session, paneID string
}

// paneOf is the pane a row jumps to, ok for a row that has one.
func paneOf(r rows.Row) (paneTarget, bool) {
	switch {
	case r.Pending != nil:
		return paneTarget{}, false
	case r.Agent != nil && (r.Kind == rows.KindTile || r.Kind == rows.KindAgent):
		return paneTarget{rows.Server(*r.Agent), r.Agent.Session, r.Agent.PaneID}, true
	case r.Pane != nil && r.Kind == rows.KindPane:
		return paneTarget{r.Pane.Server, r.Pane.Session, r.Pane.PaneID}, true
	}
	return paneTarget{}, false
}

// jumpPane goes to the row's pane. The message returned says what the
// jump could not do beyond reaching the session, a pane gone say; the
// error is a jump that could not be made at all.
//
// line is the depth-1 line whose workspace session attaches to the
// pane's managed session, nil for none.
func jumpPane(ctx context.Context, cfg config.Config, line *rows.Row, r rows.Row, p paneTarget) (message string, err error) {
	if r.Host == "" {
		return "", errors.New(r.Name + ": no configured host claims this record")
	}
	h, ok := cfg.Find(r.Host)
	if !ok {
		return "", fmt.Errorf("unknown host %q", r.Host)
	}
	srv := tmux.Parse(p.server)
	how, err := jumpMode(h.Host, srv, p.session)
	if err != nil {
		return "", err
	}
	if how == jumpSwitch {
		if err := switchTo(ctx, p.session); err != nil {
			return "", err
		}
		if err := workspace.Server.SelectPane(ctx, p.paneID); err != nil {
			return "pane " + p.paneID + ": " + err.Error(), nil
		}
		return "", nil
	}
	name, _, err := workspace.Ensure(ctx, paneSpec(cfg, h, line, r, p))
	if err != nil {
		return "", err
	}
	if err := switchTo(ctx, name); err != nil {
		return "", err
	}
	if attach := workspace.AttachPane(ctx, name); attach != "" {
		_ = workspace.Server.SelectPane(ctx, attach)
	}
	if r.HostDown {
		// A host the merged stream has as down is not dialled: the
		// session is reached, the pane left as it is.
		return h.Name + " is down; pane " + p.paneID + " not selected", nil
	}
	sctx, cancel := context.WithTimeout(ctx, selectTimeout)
	defer cancel()
	if err := selectRemote(sctx, h.Host, p.paneID); err != nil {
		return "pane " + p.paneID + ": " + err.Error(), nil
	}
	return "", nil
}

// selectTimeout bounds the select round trip, which runs on the view's
// own goroutine: a host that stops answering holds the view no longer.
const selectTimeout = 5 * time.Second

// paneSpec is the local session a managed pane's jump attaches, routed
// by the pane's session, whichever record names it: by spec, the
// workspace session of the line whose home the session is, or whose
// agent laatmux made at the root is in it with the home lost, or the
// task's before the host lists the worktree, as those lines' own jumps
// attach it; or the plain attachment to the pane's managed session. A
// pane of one worktree in another's session so goes to the other's
// workspace session, and a plain attachment never takes a workspace
// session's name.
func paneSpec(cfg config.Config, h config.Host, line *rows.Row, r rows.Row, p paneTarget) workspace.Spec {
	switch {
	case line != nil && line.Worktree != nil && line.Worktree.Session != "":
		return worktreeSpec(cfg, h, *line.Worktree)
	case line != nil && line.Worktree != nil && line.Pending == nil:
		// The home lost: the session named after the worktree, as the
		// line's jump names it.
		w := *line.Worktree
		w.Session = p.session
		spec := worktreeSpec(cfg, h, w)
		spec.Name = worktreeSessionName(h, w)
		return spec
	case line != nil && line.Pending != nil:
		// The task's, as its own jump attaches it, with the worktree's
		// record when the host lists one without a home.
		pd := line.Pending
		w := protocol.Worktree{ID: pd.WorktreeID(), EnvironmentID: pd.EnvironmentID, Root: pd.Root, Repo: pd.Repo, Branch: pd.Branch, Source: pd.Source}
		if line.Worktree != nil {
			w = *line.Worktree
		}
		w.Session = pd.Session
		return worktreeSpec(cfg, h, w)
	}
	return workspace.Spec{Host: h.Host, Managed: p.session, Name: h.Name + "/" + p.session}
}

// selectRemote asks the host's daemon to make the pane current on its
// managed server, over the same connection every command takes. A
// daemon without the capability leaves the pane as it is, which is no
// error.
var selectRemote = func(ctx context.Context, h client.Host, paneID string) error {
	c, err := client.Dial(ctx, h)
	if err != nil {
		return err
	}
	defer c.Close()
	if !protocol.Has(c.Hello.Capabilities, protocol.CapSelect) {
		return nil
	}
	// Request closes the connection when the context ends, so a daemon
	// that stops answering holds the view no longer than the bound.
	_, err = c.Request(ctx, protocol.Message{Type: protocol.TypeSelect, ID: "select-" + paneID, PaneID: paneID})
	return err
}
