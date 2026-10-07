package main

import (
	"context"

	"github.com/laat/laatmux/internal/client"
	"github.com/laat/laatmux/internal/daemon"
	"github.com/laat/laatmux/internal/protocol"
	"github.com/laat/laatmux/internal/tmux"
	"github.com/laat/laatmux/internal/workspace"
)

// clientVars is what list-clients reads per client: its current window
// and pane, and the tags of that pane and its session. A pane option
// resolves through the pane and its window, a session option through the
// client's session.
var clientVars = []string{
	"#{client_name}", "#{window_id}", "#{pane_id}", "#{pane_dead}",
	"#{" + sidebarTag + "}", "#{@laatmux_attach_pane}", "#{@laatmux_attach_target}",
	"#{@laatmux_host}", "#{@laatmux_attach}", "#{@laatmux_workspace}",
}

// listClients is what each client of the default server shows, for the
// daemon's seen rule. No server is no clients. The clients are read
// through tmux.Fields, so a tag is read whole whatever it holds.
func listClients(ctx context.Context) ([]daemon.ClientView, error) {
	recs, err := workspace.Server.Records(ctx, tmux.NewFields(clientVars...), "list-clients")
	if err != nil {
		if tmux.NoServer(err) {
			return nil, nil
		}
		return nil, err
	}
	var views []daemon.ClientView
	for _, f := range recs {
		if f[0] == "" {
			continue
		}
		v := daemon.ClientView{Client: f[0], Pane: f[2], Dead: f[3] == "1", AttachPane: f[5] != "", Target: f[6],
			Host: f[7], Attach: f[8], Workspace: workspace.DecodeKey(f[9])}
		if f[4] != "" {
			// A focused sidebar pane stands for the pane beside it: the
			// user reading the sidebar is looking at the attach or the
			// agent next to it.
			beside, err := besideSidebar(ctx, f[1])
			if err != nil {
				return nil, err
			}
			v.Pane, v.Dead, v.AttachPane, v.Target = beside.Pane, beside.Dead, beside.AttachPane, beside.Target
		}
		views = append(views, v)
	}
	return views, nil
}

// besideSidebar is the pane a window's sidebar stands for: its live
// attach pane, or its one other pane. None is a view of nothing.
func besideSidebar(ctx context.Context, window string) (daemon.ClientView, error) {
	recs, err := workspace.Server.Records(ctx, tmux.NewFields(
		"#{pane_id}", "#{pane_dead}", "#{"+sidebarTag+"}", "#{@laatmux_attach_pane}", "#{@laatmux_attach_target}",
	), "list-panes", "-t", window)
	if err != nil {
		// The window or the server went between the two commands:
		// nothing is shown.
		return daemon.ClientView{}, nil
	}
	return pickBeside(recs), nil
}

// pickBeside chooses from the window's panes, records of besideSidebar's
// values: the live attach pane, else the one pane that is not a sidebar.
func pickBeside(recs [][]string) daemon.ClientView {
	var others []daemon.ClientView
	for _, f := range recs {
		if f[2] != "" {
			continue
		}
		v := daemon.ClientView{Pane: f[0], Dead: f[1] == "1", AttachPane: f[3] != "", Target: f[4]}
		if v.AttachPane && !v.Dead {
			return v
		}
		others = append(others, v)
	}
	if len(others) == 1 {
		return others[0]
	}
	return daemon.ClientView{}
}

// sidebarSeen sends the local daemon a poke, from the hooks a client's
// move runs: it lists what the clients show now rather than at its next
// second. A daemon without attention, or none at all, is left alone.
func sidebarSeen(ctx context.Context) error {
	// A daemon not running is not started: the poke is only a hurry.
	nc, err := client.DialLocal(ctx, false)
	if err != nil {
		return nil
	}
	defer nc.Close()
	pc := protocol.NewConn(nc)
	hello, err := pc.Read()
	if err != nil || !protocol.Has(hello.Capabilities, protocol.CapAttention) {
		return nil
	}
	return pc.Write(protocol.Message{Type: protocol.TypePoke})
}
