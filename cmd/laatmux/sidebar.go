package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"

	"github.com/laat/laatmux/internal/config"
	"github.com/laat/laatmux/internal/home"
	"github.com/laat/laatmux/internal/tmux"
	"github.com/laat/laatmux/internal/view"
	"github.com/laat/laatmux/internal/workspace"
)

// The sidebar: a pane on the left of every window in the default tmux
// server running `laatmux sidebar pane`, one client of the local daemon's
// merged stream each. `on` installs hooks on the server so new windows
// and sessions get a pane, and a window whose real panes are gone loses
// its sidebar; then it adds a pane to every window that lacks one. `off`
// removes the hooks and the panes. The hooks run `attach`, `reap` and
// `fit`.
//
// Every check-and-create runs under an exclusive flock on
// $LAATMUX_HOME/sidebar.lock: two attaches for the same window, or an
// attach racing on, would each see no tagged pane and make two.

// sidebarTag is the pane option that marks a sidebar pane.
const sidebarTag = "@laatmux_sidebar"

// sessionsTag is the server option naming the sessions `on --session`
// limited the sidebar to, space-separated session ids; empty is every
// session. The hooks stay global and attach reads it: a hook set on the
// session itself would shadow the user's global hooks there.
const sessionsTag = "@laatmux_sidebar_sessions"

// sidebarHooks are the server hooks on, each at an index laatmux owns,
// so off removes exactly what on set and the user's hooks at other
// indexes stay. The commands run in the background so tmux does not
// wait on them. In an after-new-window or after-new-session hook the
// formats expand for the window the command made, so window_id is the
// new window in both; the hook_window and hook_session formats are
// empty there on tmux 3.6. window-resized expands window_id for the
// resized window too.
var sidebarHooks = []struct{ hook, cmd string }{
	{"after-new-window[9101]", "sidebar attach '#{window_id}' '#{session_id}'"},
	{"after-new-session[9102]", "sidebar attach '#{window_id}' '#{session_id}'"},
	{"pane-exited[9103]", "sidebar reap"},
	{"after-kill-pane[9104]", "sidebar reap"},
	{"window-resized[9105]", "sidebar fit '#{window_id}'"},
	// What a client shows decides what the user has seen: a move to
	// another session, window or pane has the daemon look at once.
	{"client-session-changed[9106]", "sidebar seen"},
	{"session-window-changed[9107]", "sidebar seen"},
	{"window-pane-changed[9108]", "sidebar seen"},
}

func cmdSidebar(ctx context.Context, args []string) error {
	usage := errors.New("usage: laatmux sidebar [toggle|on [--session]|off]\n       laatmux sidebar next | prev | jump N | view agents|tree | scope all|session|project [-t window] [-c client] [--all]\n       laatmux sidebar pane | attach <window> | fit <window> | reap | seen")
	sub := "toggle"
	if len(args) > 0 {
		sub = args[0]
		args = args[1:]
	}
	switch sub {
	case "next", "prev", "jump", "view", "scope":
		return sidebarControl(ctx, sub, args)
	}
	session := false
	if sub == "on" && len(args) == 1 && args[0] == "--session" {
		session, args = true, nil
	}
	if sub != "attach" && sub != "fit" && len(args) > 0 {
		return usage
	}
	if sub == "attach" && len(args) == 2 {
		// The window and its session, from the hooks.
		return sidebarAttach(ctx, args[0], args[1])
	}
	// The config is read only where its width and layout are needed, so
	// off and reap still clean up while the file is broken.
	switch sub {
	case "toggle", "on", "off":
		return sidebarSwitch(ctx, sub, session)
	case "pane":
		cfg, err := config.Load()
		if err != nil {
			return err
		}
		return sidebarPane(ctx, cfg)
	case "attach":
		if len(args) != 1 {
			return usage
		}
		return sidebarAttach(ctx, args[0], "")
	case "fit":
		if len(args) != 1 {
			return usage
		}
		// A broken config is reported where a sidebar is made, by on
		// and attach; fit runs on every resize, and its error would open
		// over the user's pane each time.
		cfg, err := config.Load()
		if err != nil {
			return nil
		}
		return sidebarFit(ctx, cfg, args[0])
	case "reap":
		return sidebarReap(ctx)
	case "seen":
		return sidebarSeen(ctx)
	}
	return usage
}

// sidebarSwitch turns the sidebar on or off. Toggle reads the hooks:
// present means on. With session, on puts panes in the current
// session's windows only and names the session in the sessions option,
// which attach reads for the windows the global hooks report, so other
// sessions get none; a plain on clears the option, so every session
// gets panes again.
func sidebarSwitch(ctx context.Context, sub string, session bool) error {
	unlock, err := sidebarLock()
	if err != nil {
		return err
	}
	defer unlock()
	if sub == "toggle" {
		on, err := sidebarHooksSet(ctx)
		if err != nil {
			return err
		}
		if on {
			sub = "off"
		} else {
			sub = "on"
		}
	}
	if sub == "off" {
		return sidebarOff(ctx)
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	// Hooks go in before the walk, so a window made during the walk is
	// caught by its hook rather than missed by both; attach skipping a
	// window that has a pane makes the overlap harmless.
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	target, err := scopeSidebar(ctx, session)
	if err != nil {
		return err
	}
	if err := setSidebarHooks(ctx, exe); err != nil {
		return err
	}
	if cfg.Sidebar.JumpKeys {
		if err := bindJumpKeys(ctx, exe); err != nil {
			return err
		}
	} else {
		unbindJumpKeys(ctx)
	}
	list := []string{"list-windows", "-a", "-F", "#{window_id}"}
	if session {
		list = []string{"list-windows", "-t", target, "-F", "#{window_id}"}
	}
	out, err := workspace.Server.Run(ctx, list...)
	if err != nil {
		if tmux.NoServer(err) {
			return nil
		}
		return err
	}
	for _, w := range strings.Fields(string(out)) {
		if err := sidebarAdd(ctx, cfg, w); err != nil {
			return err
		}
	}
	return nil
}

// sidebarOff unsets the hooks, the jump keys and the sessions option,
// and kills every tagged pane.
func sidebarOff(ctx context.Context) error {
	for _, h := range sidebarHooks {
		if _, err := workspace.Server.Run(ctx, "set-hook", "-gu", h.hook); err != nil {
			if tmux.NoServer(err) {
				// Nothing to turn off; hooks die with the server.
				return nil
			}
			return err
		}
	}
	unbindJumpKeys(ctx)
	_, _ = workspace.Server.Run(ctx, "set-option", "-su", sessionsTag)
	panes, err := sidebarPanes(ctx)
	if err != nil {
		return err
	}
	for _, p := range panes {
		if p.sidebar {
			_, _ = workspace.Server.Run(ctx, "kill-pane", "-t", p.id)
		}
	}
	return nil
}

// scopeSidebar sets the sessions option for on: with session, the
// current session added to it, and the panes a plain on put in other
// sessions killed, since those sessions get none now; without, the
// option cleared, so every session gets panes. It returns the current
// session's id with session.
func scopeSidebar(ctx context.Context, session bool) (string, error) {
	if !session {
		if _, err := workspace.Server.Run(ctx, "set-option", "-su", sessionsTag); err != nil && !tmux.NoServer(err) {
			return "", err
		}
		return "", nil
	}
	if os.Getenv("TMUX") != "" && !workspace.Inside(ctx) {
		// The default server would name another session for a shell
		// nested on the laatmux server, and kill the panes elsewhere.
		return "", errors.New("sidebar on --session: run it in a session of the default tmux server")
	}
	out, err := workspace.Server.Run(ctx, "display-message", "-p", "#{session_id}")
	if err != nil {
		return "", err
	}
	target := strings.TrimSpace(string(out))
	sessions, _ := sidebarSessions(ctx)
	if !slices.Contains(sessions, target) {
		sessions = append(sessions, target)
	}
	if _, err := workspace.Server.Run(ctx, "set-option", "-s", sessionsTag, strings.Join(sessions, " ")); err != nil {
		return "", err
	}
	if recs, err := workspace.Server.Records(ctx, tmux.NewFields("#{session_id}", "#{pane_id}", "#{"+sidebarTag+"}"), "list-panes", "-a"); err == nil {
		for _, f := range recs {
			if f[2] != "" && !slices.Contains(sessions, f[0]) {
				_, _ = workspace.Server.Run(ctx, "kill-pane", "-t", f[1])
			}
		}
	}
	return target, nil
}

// sidebarSessions is the sessions option's ids, none for every session.
func sidebarSessions(ctx context.Context) ([]string, error) {
	out, err := workspace.Server.Run(ctx, "show-options", "-sv", sessionsTag)
	if err != nil {
		return nil, err
	}
	return strings.Fields(string(out)), nil
}

// runShellExe is exe as the first word of a run-shell command. tmux
// expands that command as a format before the shell sees it, so the
// path is a format literal under the quoting: a # in it, from a
// worktree's branch, is kept.
func runShellExe(exe string) string {
	return tmux.ShellJoin([]string{tmux.FormatLiteral(exe)})
}

// jumpKeyCmd is the command a jump key runs: the window and the client
// the key's own, so any number of clients attached tell apart.
func jumpKeyCmd(exe string, n int) string {
	return fmt.Sprintf("run-shell -b %s", tmux.ShellJoin([]string{runShellExe(exe) + fmt.Sprintf(" sidebar jump %d -t '#{window_id}' -c '#{client_name}'", n)}))
}

// bindJumpKeys binds M-1..M-9 in tmux's root table to the sidebar's
// jump; jump_keys off leaves them unbound.
func bindJumpKeys(ctx context.Context, exe string) error {
	for n := 1; n <= 9; n++ {
		if _, err := workspace.Server.Run(ctx, "bind-key", "-n", fmt.Sprintf("M-%d", n), jumpKeyCmd(exe, n)); err != nil {
			return err
		}
	}
	return nil
}

// unbindJumpKeys takes laatmux's jump bindings off: M-1..M-9 in the
// root table bound to a sidebar jump of that number, leaving a key
// bound to anything else alone.
func unbindJumpKeys(ctx context.Context) {
	out, err := workspace.Server.Run(ctx, "list-keys", "-T", "root")
	if err != nil {
		return
	}
	for _, l := range strings.Split(string(out), "\n") {
		f := strings.Fields(l)
		// bind-key -T root M-1 run-shell -b "... sidebar jump 1 -t ...".
		if len(f) < 5 || f[0] != "bind-key" || f[1] != "-T" || f[2] != "root" {
			continue
		}
		key := f[3]
		if len(key) != 3 || !strings.HasPrefix(key, "M-") || key[2] < '1' || key[2] > '9' {
			continue
		}
		if f[4] == "run-shell" && strings.Contains(l, " sidebar jump "+key[2:]+" -t ") {
			_, _ = workspace.Server.Run(ctx, "unbind-key", "-n", key)
		}
	}
}

// setSidebarHooks sets the hooks, each running exe in the background,
// global all: a hook on a session would shadow the user's global hooks
// of that name there.
func setSidebarHooks(ctx context.Context, exe string) error {
	for _, h := range sidebarHooks {
		cmd := fmt.Sprintf("run-shell -b %s", tmux.ShellJoin([]string{runShellExe(exe) + " " + h.cmd}))
		if _, err := workspace.Server.Run(ctx, "set-hook", "-g", h.hook, cmd); err != nil {
			return err
		}
	}
	return nil
}

// sidebarAttach adds a pane to one window, from a hook. Under the lock
// it reads the hooks and does nothing when they are gone: an attach that
// was queued behind off must not put a pane back. With the sidebar on
// for some sessions, a window in another gets none; a hook from an
// older build names no session, and its window gets a pane whatever
// its session.
func sidebarAttach(ctx context.Context, window, session string) error {
	unlock, err := sidebarLock()
	if err != nil {
		return err
	}
	defer unlock()
	on, err := sidebarHooksSet(ctx)
	if err != nil || !on {
		return err
	}
	if sessions, err := sidebarSessions(ctx); err == nil && len(sessions) > 0 && session != "" && !slices.Contains(sessions, session) {
		return nil
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	return sidebarAdd(ctx, cfg, window)
}

// sidebarAdd splits a sidebar pane off the window as sidebarSplit says,
// the left edge at sidebarWidth's width, the configured one or half a
// narrow window, or the top edge at the strip's height, unless the
// window has one. The split
// is detached so focus stays where it was, and the new pane is tagged
// by the id split-window printed, not as the window's active pane: an
// after-split-window hook of the user's runs between the two commands
// of a sequence and may select or split another pane, which would then
// carry the tag and be killed by off. The lock is held from the check to
// the tag, so no attach sees the pane untagged. A dead sidebar pane, kept
// by a remain-on-exit the pane inherited before its own was set, is
// killed and replaced. Called with the lock held.
func sidebarAdd(ctx context.Context, cfg config.Config, window string) error {
	recs, err := workspace.Server.Records(ctx, tmux.NewFields("#{pane_id}", "#{"+sidebarTag+"}", "#{pane_dead}", "#{window_width}"), "list-panes", "-t", window)
	if err != nil {
		return err
	}
	windowWidth := 0
	for _, f := range recs {
		windowWidth, _ = strconv.Atoi(f[3])
		if f[1] == "" {
			continue
		}
		if f[2] != "1" {
			return nil
		}
		_, _ = workspace.Server.Run(ctx, "kill-pane", "-t", f[0])
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	out, err := workspace.Server.Run(ctx, append(sidebarSplit(cfg, windowWidth), "-t", window, "-P", "-F", "#{pane_id}", tmux.ShellJoin([]string{exe, "sidebar", "pane"}))...)
	if err != nil {
		return err
	}
	id := strings.TrimSpace(string(out))
	if !strings.HasPrefix(id, "%") {
		return fmt.Errorf("split-window printed %q, not a pane id", id)
	}
	// remain-on-exit off on the pane itself: a global on would keep a
	// sidebar that exited as a dead tagged pane, which attach would take
	// for a live one.
	_, err = workspace.Server.Run(ctx, "set-option", "-p", "-t", id, sidebarTag, "1",
		tmux.Next, "set-option", "-p", "-t", id, "remain-on-exit", "off")
	return err
}

// sidebarReap kills a sidebar pane that is alone in its window, so a
// window whose real pane exited closes at once instead of surviving as
// a sidebar. A dead pane kept by remain-on-exit, such as a workspace's
// attach pane, still counts as the window's: jump respawns it. It takes
// the lock so it never runs between a split and its tag, where it
// would see a live untagged pane, leave the window, and not run again.
func sidebarReap(ctx context.Context) error {
	unlock, err := sidebarLock()
	if err != nil {
		return err
	}
	defer unlock()
	panes, err := sidebarPanes(ctx)
	if err != nil {
		return err
	}
	alive := map[string]bool{}
	for _, p := range panes {
		if !p.sidebar && (!p.dead || p.remain) {
			alive[p.window] = true
		}
	}
	for _, p := range panes {
		// A dead sidebar pane is never wanted: its process is gone and
		// the tag would keep attach from adding a live one.
		if p.sidebar && (!alive[p.window] || p.dead) {
			_, _ = workspace.Server.Run(ctx, "kill-pane", "-t", p.id)
		}
	}
	reapSockets(ctx)
	return nil
}

// sidebarWidth is the sidebar's width in a window of the given width:
// the configured columns or percentage, 10% clamped to 25..50 unset,
// or half the window when that is narrower, so neither pane is
// squeezed to a column. 0 is a window not known, and gets the
// configured width.
func sidebarWidth(cfg config.Config, windowWidth int) int {
	if windowWidth <= 0 {
		return cfg.Sidebar.Columns(0)
	}
	return min(cfg.Sidebar.Columns(windowWidth), max(windowWidth/2, 1))
}

// sidebarSplit is the split-window that adds the pane: off the left
// edge, full height, at the width; or, with position top, off the top
// edge, full width, at the height.
func sidebarSplit(cfg config.Config, windowWidth int) []string {
	if cfg.Sidebar.Top() {
		return []string{"split-window", "-d", "-v", "-b", "-f", "-l", strconv.Itoa(cfg.Sidebar.Lines())}
	}
	return []string{"split-window", "-d", "-h", "-b", "-f", "-l", strconv.Itoa(sidebarWidth(cfg, windowWidth))}
}

// sidebarFit puts the window's sidebar pane back to its width, or a
// strip on top to its height. tmux
// shares a window's change of width out among its panes: a session made
// detached is 80 columns wide, its sidebar split off at the width, and
// a client switching to it widens the sidebar by a share of the extra
// columns. So does resizing the terminal. The width is sidebarWidth's,
// the configured one or half a narrow window. A window without a live
// sidebar pane is left alone.
//
// The width is the sidebar's own: a border dragged by hand is put back
// at the next resize. Telling a drag from a resize would take a record
// of the window's width, which queued fits and a sidebar turned off and
// on again leave stale.
//
// It runs under the lock: a switch that lands between attach's split
// and its tag scales an untagged pane, and fit, waiting for the tag,
// then sees it. resize-pane unzooms a zoomed window, so the zoomed pane
// is zoomed again in the same command sequence, and tmux draws the
// window once.
func sidebarFit(ctx context.Context, cfg config.Config, window string) error {
	unlock, err := sidebarLock()
	if err != nil {
		return err
	}
	defer unlock()
	size := "#{pane_width}"
	if cfg.Sidebar.Top() {
		size = "#{pane_height}"
	}
	recs, err := workspace.Server.Records(ctx, tmux.NewFields("#{pane_id}", "#{"+sidebarTag+"}", "#{pane_dead}", size, "#{window_zoomed_flag}", "#{pane_active}", "#{window_width}"), "list-panes", "-t", window)
	if err != nil {
		// Best effort, on every resize: a window killed while fit
		// waited on the lock, or no server, is nothing to fit, and an
		// error would open over the user's pane.
		return nil
	}
	sidebar, zoomed, have, windowWidth := "", "", "", 0
	for _, f := range recs {
		windowWidth, _ = strconv.Atoi(f[6])
		if f[4] == "1" && f[5] == "1" {
			zoomed = f[0]
		}
		if f[1] != "" && f[2] != "1" {
			sidebar, have = f[0], f[3]
		}
	}
	want, axis := strconv.Itoa(sidebarWidth(cfg, windowWidth)), "-x"
	if cfg.Sidebar.Top() {
		want, axis = strconv.Itoa(cfg.Sidebar.Lines()), "-y"
	}
	if sidebar == "" || have == want {
		return nil
	}
	args := []string{"resize-pane", "-t", sidebar, axis, want}
	if zoomed != "" {
		args = append(args, tmux.Next, "resize-pane", "-Z", "-t", zoomed)
	}
	_, err = workspace.Server.Run(ctx, args...)
	return err
}

type paneInfo struct {
	window, id string
	sidebar    bool
	dead       bool
	remain     bool
}

// sidebarPanes lists every pane on the default server with what reap
// and off need. No server is no panes.
func sidebarPanes(ctx context.Context) ([]paneInfo, error) {
	recs, err := workspace.Server.Records(ctx, tmux.NewFields("#{window_id}", "#{pane_id}", "#{"+sidebarTag+"}", "#{pane_dead}", "#{remain-on-exit}"), "list-panes", "-a")
	if err != nil {
		if tmux.NoServer(err) {
			return nil, nil
		}
		return nil, err
	}
	var panes []paneInfo
	for _, f := range recs {
		panes = append(panes, paneInfo{window: f[0], id: f[1], sidebar: f[2] != "", dead: f[3] == "1", remain: f[4] == "on"})
	}
	return panes, nil
}

// sidebarHooksSet reports whether laatmux's hooks are on the server. No
// server is no hooks.
func sidebarHooksSet(ctx context.Context) (bool, error) {
	out, err := workspace.Server.Run(ctx, "show-hooks", "-g")
	if err != nil {
		if tmux.NoServer(err) {
			return false, nil
		}
		return false, err
	}
	return strings.Contains(string(out), sidebarHooks[0].hook+" "), nil
}

// sidebarLock takes the exclusive lock every check-and-create runs
// under. os's errors name the state directory as it is; the sidebar's
// commands print them.
func sidebarLock() (func(), error) {
	if err := os.MkdirAll(home.Dir(), 0o700); err != nil {
		return nil, tmux.PrintablePath(err)
	}
	f, err := os.OpenFile(filepath.Join(home.Dir(), "sidebar.lock"), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, tmux.PrintablePath(err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return nil, err
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
	}, nil
}

// sidebarPane is what runs in a sidebar pane: the list view in the
// configured layout, marking the session the pane sits in, staying after
// a jump. q exits, which closes the pane; that is how one window's
// sidebar is dismissed until a new window is made or on runs again.
func sidebarPane(ctx context.Context, cfg config.Config) error {
	layout, err := view.ParseLayout(cfg.Sidebar.Layout)
	if err != nil {
		return err
	}
	c, err := dialMergedOrExplain(ctx)
	if err != nil {
		return err
	}
	vw, err := view.ParseView(cfg.Sidebar.View)
	if err != nil {
		return err
	}
	m := &view.Model{Layout: layout, View: vw, Tabs: true, Follow: true, LocalHost: localHostName(cfg), AskQuit: true,
		ItemWidth: cfg.Sidebar.ItemWidth(),
		Hint:      "tab view  s/h/l fold  f all  F scope  v layout  / filter  z settle  p/x task  ? help  q quit",
		HelpTitle: "laatmux sidebar", Help: []string{
			"p            deliver a task's prompt",
			"x X          dismiss a task, remove its worktree",
			"q Ctrl-C     quit the sidebar, after a question",
		}}
	if cfg.Sidebar.Top() {
		// The strip: the agent view alone, chips along the top, no tab
		// line; the layout and view are not the stored defaults'.
		m.Layout, m.View, m.Tabs = view.Strip, view.ViewAgents, false
		return runView(ctx, cfg, c, m, viewOptions{listen: true, fixedLayout: true, fixedView: true})
	}
	return runView(ctx, cfg, c, m, viewOptions{listen: true})
}
