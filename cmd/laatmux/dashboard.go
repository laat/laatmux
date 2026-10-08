package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/laat/laatmux/internal/client"
	"github.com/laat/laatmux/internal/config"
	"github.com/laat/laatmux/internal/home"
	"github.com/laat/laatmux/internal/merged"
	"github.com/laat/laatmux/internal/peer"
	"github.com/laat/laatmux/internal/protocol"
	"github.com/laat/laatmux/internal/rows"
	"github.com/laat/laatmux/internal/term"
	"github.com/laat/laatmux/internal/tmux"
	"github.com/laat/laatmux/internal/view"
	"github.com/laat/laatmux/internal/workspace"
)

// cmdDashboard is the list view filling whatever it runs in, meant for
// display-popup -E: Enter jumps to the selected row and exits, so the
// popup closes; q exits without. It is one client of the local daemon's
// merged stream. The default layout is compact because a popup is wide
// and short; the title line under each row uses the room. Beyond the
// shared keys it has actions: a adds through pickers, x and X remove,
// z settles, S opens a shell; see actions.go.
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
	fixedLayout := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "layout" {
			fixedLayout = true
		}
	})
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	c, err := dialMergedOrExplain(ctx)
	if err != nil {
		return err
	}
	if err := needRelay(c); err != nil {
		return err
	}
	// The dashboard starts in the view and, without --layout, the layout
	// last chosen, from sidebar.json.
	m := &view.Model{Layout: layout, View: view.ViewAgents, Tabs: true, Titles: true, Follow: true, LocalHost: localHostName(cfg),
		Hint:      "enter jump  tab view  a add  x rm  p prompt  z settle  S shell  o/O PR  s/h/l fold  f all  F scope  v layout  / filter  ? help  q quit",
		HelpTitle: "laatmux dashboard", Help: []string{
			"a            add a worktree",
			"x X          remove the worktree, X with force",
			"p            deliver a task's prompt",
			"S            open a shell in the workspace session",
			"o O          open the PR, its checks",
			"q Ctrl-C     quit",
		}}
	// The dashboard starts at all: a scope the CLI set for the sidebar
	// panes would empty a popup opened from an unrelated shell.
	return runView(ctx, cfg, c, m, viewOptions{exitOnJump: true, actions: true, fixedLayout: fixedLayout, fixedScope: true})
}

// templatesFor is the host's templates: the dashboard's defaults for
// the dashboard, the sidebar's for a pane.
func templatesFor(cfg config.Config, o viewOptions) view.Templates {
	return templates(cfg, o.actions)
}

// jumpKeysShown is whether the {jump_key} labels are drawn: the keys
// reach a sidebar pane over its socket, and a popup gets the keys
// itself and drops them, while a window's binding goes to the window's
// sidebar, so only a pane that listens shows them.
func jumpKeysShown(cfg config.Config, o viewOptions) bool {
	return cfg.Sidebar.JumpKeys && o.listen
}

// runView runs the view on the terminal against the merged stream. With
// exitOnJump a successful jump ends the view, which is what a popup
// wants; a sidebar pane stays. With actions the dashboard's keys are
// live. A jump that fails puts its message in the footer either way.
// viewOptions is how a view runs: exitOnJump for the popup, actions for
// the dashboard's keys, fixedLayout when a flag chose the layout over
// the stored default, and commands from a sidebar pane's socket.
type viewOptions struct {
	exitOnJump, actions, fixedLayout, fixedView bool
	fixedScope                                  bool // the dashboard: at all, whatever the file says
	listen                                      bool // a sidebar pane: its socket
}

func runView(ctx context.Context, cfg config.Config, c *client.Conn, m *view.Model, o viewOptions) error {
	exitOnJump, actions := o.exitOnJump, o.actions
	current := ""
	// A lookup a user's hook failed after has the session all the same;
	// the view has no line for the hook's error (warnHook).
	if cur, err := workspace.Current(ctx); err == nil || tmux.HookOnly(err) {
		current = cur.Name
	}
	st := merged.New()
	st.Configure(cfg)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go st.Follow(ctx, c)
	t, err := term.Open(os.Stdin, os.Stdout)
	if err != nil {
		return err
	}
	defer t.Close()
	t.Theme, m.Icons = look(cfg, t)
	m.Machine, _ = os.Hostname()
	m.SetTemplates(templatesFor(cfg, o))
	m.AgentIcons = agentIcons(cfg)
	m.JumpKeys = jumpKeysShown(cfg, o)
	host := settingsHost{dashboard: o.actions, fixedLayout: o.fixedLayout, fixedView: o.fixedView, fixedScope: o.fixedScope}
	seen := startSettings(cfg, m, host)
	cmds := make(chan func(*view.Model) view.Action)
	watchSettings(ctx, seen, cmds, host)
	d := &dash{ctx: ctx, cfg: cfg, st: st, exitOnJump: exitOnJump, reload: config.Load}
	if o.listen {
		// The pane's socket: a command names the client its jump
		// switches, kept on the dash until the jump takes it.
		// A socket that cannot be made, a path too long for one say,
		// leaves the pane useful without the CLI's control.
		stop, err := listenPane(ctx, cmds, d.paneCommand)
		if err != nil {
			m.Message = "sidebar socket: " + err.Error()
		} else {
			defer stop()
		}
	}
	return view.Run(ctx, t, m, view.Host{
		Changed:  st.Changed(),
		Commands: cmds,
		Refresh:  func(m *view.Model) { fill(m, st.Status(current)) },
		Act: func(m *view.Model, a view.Action) bool {
			switch {
			case a.Kind == view.ActionSettings:
				if err := saveSettings(m, time.Now(), host); err != nil {
					m.Message = "sidebar.json: " + err.Error()
				}
				return false
			case a.Kind == view.ActionJump:
				return d.jumpAction(m, a)
			case actions:
				return d.act(m, a)
			case taskAction(m, a), a.Kind == view.ActionOther && a.Key.Kind == term.KeyRune && a.Key.Rune == 'z':
				// The sidebar takes a task's p and x, and what follows
				// from them, and z, which settles; none of the
				// dashboard's other keys.
				return d.act(m, a)
			}
			return false
		},
	})
}

// jumpAction runs a jump: to the row the action names. In a view that
// stays, the sidebar, a click made the view's pane the active one, as
// tmux's click binding selects the pane clicked; the pane that was
// active before is made so again first, so typing goes back where it
// was, both when the jump stays in this session and in this window when
// the viewer comes back to it.
func (d *dash) jumpAction(m *view.Model, a view.Action) bool {
	r, named := a.Row, a.Row != nil
	if !named {
		r = m.Selection()
	}
	if r == nil {
		return false
	}
	if d.client != "" {
		// A jump from the socket switches the client the command
		// named, this once.
		base := d.ctx
		d.ctx = withClient(base, d.client)
		d.client = ""
		defer func() { d.ctx = base }()
	}
	exit, jumped := d.jumpRow(m, *r)
	if !jumped {
		// A click or a digit that jumped nowhere, on a task still
		// running or refused with a message, selects the row, as it did
		// before jumps left the selection following: the message is
		// about that row, and p and x act on it. Enter was on the
		// selection already, and leaves following as it was. The focus
		// stays on the view, where the message is.
		if named {
			m.Select(r.ID())
		}
		return exit
	}
	if a.Mouse && !d.exitOnJump {
		refocus := d.refocus
		if refocus == nil {
			refocus = func() { lastPane(d.ctx) }
		}
		refocus()
	}
	return exit
}

// paneCommand is a sidebar command as the view runs it: the model's
// event, and, when it is a jump, the client the command named kept on
// the dash for that jump to switch; a command that jumped nowhere, with
// a question up say, leaves no client for a later jump by key.
func (d *dash) paneCommand(c view.Command) func(*view.Model) view.Action {
	return func(m *view.Model) view.Action {
		a := m.Command(c)
		if a.Kind == view.ActionJump {
			d.client = c.Client
		}
		return a
	}
}

// lastPane makes the pane active before the view's own the active one
// in the view's window, while the view's pane is the active one: tmux
// checks and switches in one command, so two clicks handled one after
// the other do not toggle the focus back into the view. It rests on
// tmux's default click binding having made the view's pane active for
// the click; a view focused with the keyboard before the click gives
// the focus to the pane active before it, which is where typing went
// before the view was focused. A window whose view pane was active all
// along has no other to go back to, and tmux's refusal is ignored. The
// server is the pane's own, the one TMUX names.
func lastPane(ctx context.Context) {
	if pane := os.Getenv("TMUX_PANE"); pane != "" {
		_, _ = tmux.Server{}.Run(ctx, "if-shell", "-F", "-t", pane, "#{pane_active}", "last-pane -t "+pane)
	}
}

// taskAction is an action on a pending task: p or x on a task's row,
// the answer to the dismiss question, or the end of the log they put
// up. The sidebar, without the dashboard's keys, takes these.
func taskAction(m *view.Model, a view.Action) bool {
	switch a.Kind {
	case view.ActionOther:
		r := m.Selection()
		return r != nil && r.Pending != nil && a.Key.Kind == term.KeyRune && (a.Key.Rune == 'p' || a.Key.Rune == 'x' || a.Key.Rune == 'X')
	case view.ActionConfirm:
		return m.ConfirmTag == "dismiss"
	case view.ActionOverlay:
		_, ok := m.Overlay.(*view.Log)
		return ok
	}
	return false
}

// dialMergedOrExplain connects to the local daemon's merged stream,
// starting the daemon when it is not running. A dial that fails is a
// daemon that did not answer, a wedged one or one of another protocol,
// or one that did not start, and the error says what to do for each:
// the dial's own reason tells them apart, and nothing here probes the
// runtime record or the startup lock, since a record a crash left may
// name a pid since reused, and laatmux stop is the one command that
// checks a pid is the daemon's. One without the capability is an older
// build still running, and the error says how to replace it. Every
// client of the stream, ls and watch among them, refuses rather than
// dial the hosts itself.
func dialMergedOrExplain(ctx context.Context) (*client.Conn, error) {
	c, err := client.Dial(ctx, peer.Host{Name: "local"})
	if err != nil {
		return nil, fmt.Errorf("local daemon: %w; one running that does not answer is stopped with: laatmux stop; one that did not start says why in %s, and laatmux serve run by hand shows it, or names the pid of one holding the lock", err, tmux.Printable(filepath.Join(home.Dir(), "daemon.log")))
	}
	if !protocol.Has(c.Hello.Capabilities, protocol.CapMerged) {
		c.Close()
		return nil, fmt.Errorf("local daemon %s has no merged stream, an older build; stop it with: laatmux stop; the next client starts the current build", c.Hello.Version)
	}
	return c, nil
}

// needRelay is the add form's requirement on the daemon it submits to:
// a daemon with the merged stream and no relay is an older build that
// ran on without its pending directory, which the current build does
// not; the dashboard, compose and tasks refuse it, where ls, watch and
// the sidebar, which submit nothing, take the stream alone.
func needRelay(c *client.Conn) error {
	if !protocol.Has(c.Hello.Capabilities, protocol.CapRelay) {
		c.Close()
		return fmt.Errorf("local daemon %s has no relay, an older build; stop it with: laatmux stop; the next client starts the current build", c.Hello.Version)
	}
	return nil
}

// localHostName is the configured name of this machine.
func localHostName(cfg config.Config) string {
	if h, ok := cfg.Local(); ok {
		return h.Name
	}
	return ""
}

// fill sets the model's rows and header from the merged state: the rows
// from every record and the local sessions, the day's handoffs, and a
// header line for the local daemon being down, each host that is not
// connected and listed, and a failed session listing.
func fill(v *view.Model, s merged.Status) {
	tree := rows.Tree(s.Input)
	v.Set(tree, rows.Agents(s.Input, tree), s.Handoffs)
	v.Loading = !s.Loaded
	v.Header = v.Header[:0]
	if s.DaemonErr != "" {
		v.Header = append(v.Header, view.HeaderLine{Text: "local daemon  DOWN  " + s.DaemonErr, Down: true})
	}
	for _, st := range s.Hosts {
		n := st.Name
		switch {
		case st.Connected && st.Listed:
		case st.Connected:
			v.Header = append(v.Header, view.HeaderLine{Text: n + "  connected  (snapshot pending)"})
		case st.Error != "":
			v.Header = append(v.Header, view.HeaderLine{Text: n + "  DOWN  " + st.Down(), Down: true})
		default:
			v.Header = append(v.Header, view.HeaderLine{Text: n + "  connecting"})
		}
	}
	if s.SessionsErr != "" {
		v.Header = append(v.Header, view.HeaderLine{Text: "local sessions not listed: " + s.SessionsErr})
	}
}

// jumpRow is the jump command's logic run in-process against the merged
// records. A workspace row switches to its local session, creating it
// from the record when missing; a managed session that is no worktree's
// does the same through a plain attachment; an observed agent on this
// machine's default server is a switch-client; one on a remote host's
// default server is refused as jump refuses it. A worktree with no
// session cannot be jumped to: the message says how add would start
// one. A orphaned row's session exists locally and is switched to.
// The view is meant to run inside the default tmux server, where
// switch-client is allowed; run elsewhere, a dashboard in a plain
// terminal say, the message says how to attach instead.
func jumpRow(ctx context.Context, cfg config.Config, r rows.Row) error {
	if r.Orphaned {
		return switchTo(ctx, r.Local.Name)
	}
	spec, session, err := jumpTarget(cfg, r)
	if err != nil {
		return err
	}
	if session != "" {
		return switchTo(ctx, session)
	}
	// A session Ensure read from a listing a user's hook failed after is
	// there; the view has no line for the hook's error (warnHook).
	name, _, err := workspace.Ensure(ctx, spec)
	if err != nil && !tmux.HookOnly(err) {
		return err
	}
	return switchTo(ctx, name)
}

// jumpTarget is where jumpRow takes a row that is not orphaned: what
// rowSpec decides on the row's host, for a pending task's row by the
// task's target.
func jumpTarget(cfg config.Config, r rows.Row) (workspace.Spec, string, error) {
	if r.Pending != nil {
		var err error
		if r, err = pendingTarget(r); err != nil {
			return workspace.Spec{}, "", err
		}
	}
	if r.Host == "" {
		return workspace.Spec{}, "", errors.New(r.Name + ": no configured host claims this record")
	}
	h, ok := cfg.Find(r.Host)
	if !ok {
		return workspace.Spec{}, "", fmt.Errorf("unknown host %q", r.Host)
	}
	return rowSpec(cfg, h, r)
}

// worktreeSessionName is the workspace session name for a worktree: by
// branch, or by the root's base name when detached.
func worktreeSessionName(h config.Host, w protocol.Worktree) string {
	if w.Branch != "" {
		return workspace.SessionName(h.Name, w.Repo, w.Branch)
	}
	return h.Name + "/" + w.Repo + "@" + tmux.EncodeBranch(filepath.Base(w.Root))
}

// rowSpec is where a row's jump goes: the workspace session to make or
// find, or the session on this machine's default server to switch to.
func rowSpec(cfg config.Config, h config.Host, r rows.Row) (spec workspace.Spec, session string, err error) {
	switch {
	case r.Worktree != nil && r.Worktree.Main && r.Agent == nil:
		// A main checkout's line goes to its agent's session, the
		// default server's: no workspace session is made for it.
		return spec, "", errors.New(mainNoAgent(h, *r.Worktree))
	case r.Worktree != nil && (r.Worktree.Session != "" || r.Agent == nil):
		if r.Worktree.Session == "" {
			return spec, "", errors.New(addHint(cfg, h, *r.Worktree))
		}
		return worktreeSpec(h, *r.Worktree), "", nil
	case r.Worktree != nil && r.Agent.Server == protocol.ServerLaatmux:
		// A worktree whose own session lost the home, a split in it gone
		// elsewhere say: the row's agent is the one laatmux made at the
		// root, and the worktree's workspace session attaches to that
		// session as it did while it was the home, so the worktree keeps
		// one local session whether or not it has a home. The local
		// session is named after the worktree, as add names the one it
		// makes, not after the agent's session, which panes moved in by
		// hand could make another worktree's too.
		w := *r.Worktree
		w.Session = r.Agent.Session
		spec := worktreeSpec(h, w)
		spec.Name = worktreeSessionName(h, w)
		return spec, "", nil
	case r.Agent != nil:
		// An agent's row, or a worktree's without a home session whose
		// agent is on a default server.
		how, err := jumpMode(h.Host, tmux.Parse(r.Agent.Server), r.Agent.Session)
		if err != nil {
			return spec, "", err
		}
		if how == jumpSwitch {
			return spec, r.Agent.Session, nil
		}
		return attachSpec(h, r.Agent.Session), "", nil
	}
	return spec, "", errors.New(r.Name + ": nothing to jump to")
}

// pendingTarget is a pending task's row made ready for the jump: a
// task whose host is gone or answers as another machine is refused,
// and one whose worktree row is not listed yet, or is listed from
// before the add gave it a session, jumps by what the host reported,
// the managed session at the root.
func pendingTarget(r rows.Row) (rows.Row, error) {
	p := r.Pending
	switch {
	case r.Removed:
		return r, errors.New(r.Name + ": host removed from the config")
	case p.Gone:
		return r, errors.New(r.Name + ": the worktree is gone; x dismisses the task")
	case p.Done && !p.OK:
		// A failed add stands for no worktree row; one listed at the
		// root is drawn beside it.
		return r, errors.New(r.Name + ": " + r.State() + "; x dismisses the task")
	case p.Mismatch != "" || r.Replaced:
		// The name reaches another machine now: its session of the
		// same name is not this task's.
		return r, errors.New(r.Name + ": host replaced: " + r.Detail())
	case r.Worktree != nil && r.Worktree.Session != "":
	case r.Worktree != nil && r.Agent != nil && r.Agent.Server == protocol.ServerLaatmux:
		// Listed without a home, with the agent laatmux made at the
		// root in a managed session: the jump goes through that agent,
		// as the worktree line's does, wherever the agent went.
	case p.Session == "" || p.Root == "" || p.EnvironmentID == "":
		if r.Worktree == nil {
			return r, errors.New(r.Name + ": no session yet")
		}
	default:
		// The worktree row is not listed yet, or is listed from before
		// the add gave it a session: the jump goes by what the host
		// reported, the managed session at the root.
		w := protocol.Worktree{ID: p.WorktreeID(), EnvironmentID: p.EnvironmentID, Root: p.Root, Repo: p.Repo, Branch: p.Branch, Source: p.Source}
		if r.Worktree != nil {
			w = *r.Worktree
		}
		w.Session = p.Session
		r.Worktree = &w
	}
	return r, nil
}

// switchTo makes the session current for the client the view runs in,
// or the one the context names, a `sidebar jump -c` client; or says how
// to attach when the view is not inside the default server.
func switchTo(ctx context.Context, name string) error {
	if !workspace.Inside(ctx) {
		return fmt.Errorf("%s is on the default tmux server; attach with: %s", name, workspace.AttachHint(name))
	}
	if c, ok := ctx.Value(clientKey{}).(string); ok && c != "" {
		return workspace.SwitchClient(ctx, c, name)
	}
	return workspace.Switch(ctx, name)
}

// clientKey carries the tmux client a jump switches through a context.
type clientKey struct{}

// withClient is ctx with the client a jump switches, "" for the view's
// own.
func withClient(ctx context.Context, client string) context.Context {
	if client == "" {
		return ctx
	}
	return context.WithValue(ctx, clientKey{}, client)
}
