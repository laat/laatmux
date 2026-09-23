package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"sort"

	"github.com/laat/laatmux/internal/client"
	"github.com/laat/laatmux/internal/config"
	"github.com/laat/laatmux/internal/home"
	"github.com/laat/laatmux/internal/protocol"
	"github.com/laat/laatmux/internal/rows"
	"github.com/laat/laatmux/internal/tmux"
	"github.com/laat/laatmux/internal/view"
	"github.com/laat/laatmux/internal/workspace"
)

// cmdDashboard is the list view filling whatever it runs in, meant for
// display-popup -E: Enter jumps to the selected row and exits, so the
// popup closes; q exits without. It is one client of the local daemon's
// merged stream. The default layout is compact because a popup is wide
// and short; the title line under each row uses the room.
func cmdDashboard(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("dashboard", flag.ContinueOnError)
	layoutFlag := fs.String("layout", string(view.Compact), "tiles or compact")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return errors.New("usage: laatmux dashboard [--layout tiles|compact]")
	}
	layout, err := view.ParseLayout(*layoutFlag)
	if err != nil {
		return err
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	c, err := dialMergedOrExplain(ctx)
	if err != nil {
		return err
	}
	m := &view.Model{Layout: layout, Titles: true, LocalHost: localHostName(cfg),
		Hint: "enter jump  v layout  / filter  f settled  q quit"}
	return runView(ctx, cfg, c, m, true)
}

// runView runs the view on the terminal against the merged stream. With
// exitOnJump a successful jump ends the view, which is what a popup
// wants; a sidebar pane stays. A jump that fails puts its message in
// the footer either way.
func runView(ctx context.Context, cfg config.Config, c *client.Conn, m *view.Model, exitOnJump bool) error {
	current := ""
	if cur, err := workspace.Current(ctx); err == nil {
		current = cur.Name
	}
	st := newMerged()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go st.followMerged(ctx, c)
	t, err := view.Open(os.Stdin, os.Stdout)
	if err != nil {
		return err
	}
	defer t.Close()
	return view.Run(ctx, t, m, view.Host{
		Changed: st.change,
		Refresh: func(m *view.Model) { st.fill(m, current) },
		Act: func(m *view.Model, a view.Action) bool {
			if a.Kind != view.ActionJump {
				return false
			}
			if err := jumpRow(ctx, cfg, *m.Selection()); err != nil {
				m.Message = err.Error()
				return false
			}
			return exitOnJump
		},
	})
}

// dialMergedOrExplain connects to the local daemon's merged stream. A
// daemon without the capability is an older build still running; the
// sidebar and the dashboard exist for the merged stream, so they refuse
// with what to do rather than dialling every host from every window.
func dialMergedOrExplain(ctx context.Context) (*client.Conn, error) {
	c, err := client.Dial(ctx, client.Host{Name: "local"})
	if err != nil {
		return nil, fmt.Errorf("local daemon: %w", err)
	}
	if !protocol.Has(c.Hello.Capabilities, protocol.CapMerged) {
		c.Close()
		how := "stop it and the next client starts the current build"
		if rt, err := home.ReadRuntime(); err == nil {
			how = fmt.Sprintf("stop it with: kill %d; the next client starts the current build", rt.PID)
		}
		return nil, fmt.Errorf("local daemon %s has no merged stream (an older build, or no hosts in the config); %s", c.Hello.Version, how)
	}
	return c, nil
}

// localHostName is the configured name of this machine.
func localHostName(cfg config.Config) string {
	if h, ok := cfg.Local(); ok {
		return h.Name
	}
	return ""
}

// fill sets the model's rows and header from the merged state: the rows
// from every record and the local sessions, and a header line for the
// local daemon being down, each host that is not connected and listed,
// and a failed session listing.
func (m *merged) fill(v *view.Model, current string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	v.Rows = rows.Build(m.input(m.localsLocked(), current))
	v.Header = v.Header[:0]
	if m.daemonErr != "" {
		v.Header = append(v.Header, "local daemon  DOWN  "+m.daemonErr)
	}
	names := make([]string, 0, len(m.hosts))
	for n := range m.hosts {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		st := m.hosts[n]
		switch {
		case st.Connected && st.Listed:
		case st.Connected:
			v.Header = append(v.Header, n+"  connected  (snapshot pending)")
		case st.Error != "":
			v.Header = append(v.Header, n+"  DOWN  "+st.Error)
		default:
			v.Header = append(v.Header, n+"  connecting")
		}
	}
	if m.sessionsErr != "" {
		v.Header = append(v.Header, "local sessions not listed: "+m.sessionsErr)
	}
}

// jumpRow is the jump command's logic run in-process against the merged
// records. A workspace row switches to its local session, creating it
// from the record when missing; a managed session that is no worktree's
// does the same through a plain attachment; an observed agent on this
// machine's default server is a switch-client; one on a remote host's
// default server is refused as jump refuses it. A worktree with no
// session cannot be jumped to: the message is the add line that would
// start one. A stale row's session exists locally and is switched to.
// The view runs inside the default tmux server, so switch-client is
// always allowed.
func jumpRow(ctx context.Context, cfg config.Config, r rows.Row) error {
	if r.Stale {
		return workspace.Switch(ctx, r.Local.Name)
	}
	if r.Host == "" {
		return errors.New(r.Name + ": no configured host claims this record")
	}
	h, ok := cfg.Find(r.Host)
	if !ok {
		return fmt.Errorf("unknown host %q", r.Host)
	}
	var spec workspace.Spec
	switch {
	case r.Worktree != nil:
		if r.Worktree.Session == "" {
			return errors.New(addHint(cfg, h, *r.Worktree))
		}
		spec = worktreeSpec(cfg, h, *r.Worktree)
	case r.Agent != nil:
		how, err := jumpMode(h.Host, tmux.Parse(rows.Server(*r.Agent)), r.Agent.Session)
		if err != nil {
			return err
		}
		if how == jumpSwitch {
			return workspace.Switch(ctx, r.Agent.Session)
		}
		spec = workspace.Spec{Host: h.Host, Managed: r.Agent.Session, Name: h.Name + "/" + r.Agent.Session}
	default:
		return errors.New(r.Name + ": nothing to jump to")
	}
	name, _, err := workspace.Ensure(ctx, spec)
	if err != nil {
		return err
	}
	return workspace.Switch(ctx, name)
}
