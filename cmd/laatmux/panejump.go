package main

import (
	"context"
	"errors"
	"fmt"

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
func jumpPane(ctx context.Context, cfg config.Config, r rows.Row, p paneTarget) (message string, err error) {
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
	// The workspace session when the pane is in the worktree's home
	// session, or the one a task's jump made before the host listed
	// the worktree; the plain attachment to its managed session
	// otherwise.
	var name string
	switch {
	case r.Worktree == nil && r.Local != nil && r.Local.Workspace():
		name = r.Local.Name
	default:
		var spec workspace.Spec
		if r.Worktree != nil && r.Worktree.Session == p.session {
			spec = worktreeSpec(cfg, h, *r.Worktree)
		} else {
			spec = workspace.Spec{Host: h.Host, Managed: p.session, Name: h.Name + "/" + p.session}
		}
		var err error
		if name, _, err = workspace.Ensure(ctx, spec); err != nil {
			return "", err
		}
	}
	if err := switchTo(ctx, name); err != nil {
		return "", err
	}
	if attach := workspace.AttachPane(ctx, name); attach != "" {
		_ = workspace.Server.SelectPane(ctx, attach)
	}
	if err := selectRemote(ctx, h.Host, p.paneID); err != nil {
		return "pane " + p.paneID + ": " + err.Error(), nil
	}
	return "", nil
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
	if err := c.Write(protocol.Message{Type: protocol.TypeSelect, ID: "select-" + paneID, PaneID: paneID}); err != nil {
		return err
	}
	for {
		m, err := c.Read()
		if err != nil {
			return err
		}
		if m.Type == protocol.TypeResult {
			if !m.OK {
				return errors.New(m.Error)
			}
			return nil
		}
	}
}
