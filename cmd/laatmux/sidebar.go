package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
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
// removes the hooks and the panes. The hooks run `attach` and `reap`.
//
// Every check-and-create runs under an exclusive flock on
// $LAATMUX_HOME/sidebar.lock: two attaches for the same window, or an
// attach racing on, would each see no tagged pane and make two.

// sidebarTag is the pane option that marks a sidebar pane.
const sidebarTag = "@laatmux_sidebar"

// sidebarHooks are the server hooks on, each at an index laatmux owns,
// so off removes exactly what on set and the user's hooks at other
// indexes stay. The commands run in the background so tmux does not
// wait on them. In an after-new-window or after-new-session hook the
// formats expand for the window the command made, so window_id is the
// new window in both; the hook_window and hook_session formats are
// empty there on tmux 3.6.
var sidebarHooks = []struct{ hook, cmd string }{
	{"after-new-window[9101]", "sidebar attach '#{window_id}'"},
	{"after-new-session[9102]", "sidebar attach '#{window_id}'"},
	{"pane-exited[9103]", "sidebar reap"},
	{"after-kill-pane[9104]", "sidebar reap"},
}

func cmdSidebar(ctx context.Context, args []string) error {
	usage := errors.New("usage: laatmux sidebar [toggle|on|off]\n       laatmux sidebar pane | attach <window> | reap")
	sub := "toggle"
	if len(args) > 0 {
		sub = args[0]
		args = args[1:]
	}
	if sub != "attach" && len(args) > 0 {
		return usage
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	switch sub {
	case "toggle", "on", "off":
		return sidebarSwitch(ctx, cfg, sub)
	case "pane":
		return sidebarPane(ctx, cfg)
	case "attach":
		if len(args) != 1 {
			return usage
		}
		return sidebarAttach(ctx, cfg, args[0])
	case "reap":
		return sidebarReap(ctx)
	}
	return usage
}

// sidebarSwitch turns the sidebar on or off. Toggle reads the hooks:
// present means on.
func sidebarSwitch(ctx context.Context, cfg config.Config, sub string) error {
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
		for _, h := range sidebarHooks {
			if _, err := workspace.Server.Run(ctx, "set-hook", "-gu", h.hook); err != nil {
				return err
			}
		}
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
	// Hooks go in before the walk, so a window made during the walk is
	// caught by its hook rather than missed by both; attach skipping a
	// window that has a pane makes the overlap harmless.
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	for _, h := range sidebarHooks {
		cmd := fmt.Sprintf("run-shell -b %s", tmux.ShellJoin([]string{tmux.ShellJoin([]string{exe}) + " " + h.cmd}))
		if _, err := workspace.Server.Run(ctx, "set-hook", "-g", h.hook, cmd); err != nil {
			return err
		}
	}
	out, err := workspace.Server.Run(ctx, "list-windows", "-a", "-F", "#{window_id}")
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

// sidebarAttach adds a pane to one window, from a hook. Under the lock
// it reads the hooks and does nothing when they are gone: an attach that
// was queued behind off must not put a pane back.
func sidebarAttach(ctx context.Context, cfg config.Config, window string) error {
	unlock, err := sidebarLock()
	if err != nil {
		return err
	}
	defer unlock()
	on, err := sidebarHooksSet(ctx)
	if err != nil || !on {
		return err
	}
	return sidebarAdd(ctx, cfg, window)
}

// sidebarAdd splits a sidebar pane off the left edge of the window, full
// height, at the configured width, unless the window has one. The split
// is detached so focus stays where it was, and the new pane is tagged
// by the id split-window printed, not as the window's active pane: an
// after-split-window hook of the user's runs between the two commands
// of a sequence and may select or split another pane, which would then
// carry the tag and be killed by off. The lock is held from the check to
// the tag, so no attach sees the pane untagged. Called with the lock
// held.
func sidebarAdd(ctx context.Context, cfg config.Config, window string) error {
	out, err := workspace.Server.Run(ctx, "list-panes", "-t", window, "-F", "#{"+sidebarTag+"}")
	if err != nil {
		return err
	}
	if strings.TrimSpace(string(out)) != "" {
		return nil
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	out, err = workspace.Server.Run(ctx,
		"split-window", "-d", "-h", "-b", "-f", "-l", strconv.Itoa(cfg.Sidebar.Columns()), "-t", window,
		"-P", "-F", "#{pane_id}", tmux.ShellJoin([]string{exe, "sidebar", "pane"}))
	if err != nil {
		return err
	}
	id := strings.TrimSpace(string(out))
	if !strings.HasPrefix(id, "%") {
		return fmt.Errorf("split-window printed %q, not a pane id", id)
	}
	_, err = workspace.Server.Run(ctx, "set-option", "-p", "-t", id, sidebarTag, "1")
	return err
}

// sidebarReap kills a sidebar pane that is alone in its window, so a
// window whose real pane exited closes at once instead of surviving as
// a sidebar. A dead pane kept by remain-on-exit, such as a workspace's
// attach pane, still counts as the window's: jump respawns it.
func sidebarReap(ctx context.Context) error {
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
		if p.sidebar && !alive[p.window] {
			_, _ = workspace.Server.Run(ctx, "kill-pane", "-t", p.id)
		}
	}
	return nil
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
	out, err := workspace.Server.Run(ctx, "list-panes", "-a", "-F", strings.Join([]string{"#{window_id}", "#{pane_id}", "#{" + sidebarTag + "}", "#{pane_dead}", "#{remain-on-exit}"}, tmux.Sep))
	if err != nil {
		if tmux.NoServer(err) {
			return nil, nil
		}
		return nil, err
	}
	var panes []paneInfo
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		f := strings.Split(line, tmux.Sep)
		if len(f) != 5 {
			continue
		}
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
// under.
func sidebarLock() (func(), error) {
	if err := os.MkdirAll(home.Dir(), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(home.Dir(), "sidebar.lock"), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
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
	m := &view.Model{Layout: layout, LocalHost: localHostName(cfg)}
	return runView(ctx, cfg, c, m, false)
}
