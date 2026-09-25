package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/laat/laatmux/internal/config"
	"github.com/laat/laatmux/internal/home"
	"github.com/laat/laatmux/internal/protocol"
	"github.com/laat/laatmux/internal/view"
	"github.com/laat/laatmux/internal/worktree"
)

// cmdCompose is the task form alone, for a popup binding such as
// display-popup -E -d '#{pane_current_path}' 'laatmux compose': the
// dashboard's model without the list, exiting on submit or cancel. The
// repository defaults to the directory the popup was opened from, as
// the dashboard's a does. With the local daemon's relay the submit
// hands the add to it and the popup closes on accepted; without it the
// add runs in the foreground with its log, then the new session is
// jumped to.
func cmdCompose(ctx context.Context, args []string) error {
	if len(args) > 0 {
		return errors.New("usage: laatmux compose")
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	c, err := dialMergedOrExplain(ctx)
	if err != nil {
		return err
	}
	relay := protocol.Has(c.Hello.Capabilities, protocol.CapRelay)
	// One snapshot, for the host rows' cached capabilities, then the
	// stream is followed for the note to stay current.
	st := newMerged()
	if _, err := st.readMerged(ctx, c, 2*time.Second, func(m *merged) bool { return len(m.hosts) > 0 }); err != nil {
		c.Close()
		return err
	}
	f := &addForm{repos: cfg.Repos, agents: cfg.AgentNames()}
	for _, h := range cfg.Hosts {
		if h.CanAdd() {
			f.hosts = append(f.hosts, h)
		}
	}
	switch {
	case len(f.repos) == 0:
		return errors.New("no repositories configured")
	case len(f.hosts) == 0:
		return errors.New("no host has repos and worktrees configured")
	case len(f.agents) == 0:
		return errors.New("no agents configured")
	}
	last, err := home.ReadLast()
	if err != nil {
		return err
	}
	preRepo := ""
	if repo, err := resolveRepo(ctx, cfg, ""); err == nil {
		preRepo = repo.Name
	}
	form := buildForm(cfg, f, last, preRepo, "", "", st.hostCaps)
	form.Validate = func(b string) error { return worktree.CheckBranch(ctx, strings.TrimSpace(b)) }
	t, err := view.Open(os.Stdin, os.Stdout)
	if err != nil {
		return err
	}
	defer t.Close()
	m := &view.Model{Layout: view.Compact, Overlay: form}
	d := &dash{ctx: ctx, cfg: cfg, st: st, exitOnJump: true, relay: relay, add: f}
	var outcome string
	err = view.Run(ctx, t, m, view.Host{
		Changed: st.change,
		Refresh: func(*view.Model) {},
		Act: func(m *view.Model, a view.Action) bool {
			if a.Kind != view.ActionOverlay {
				return false
			}
			switch o := m.Overlay.(type) {
			case *view.Form:
				m.Overlay = nil
				if o.Cancelled {
					return true
				}
				done := d.submitForm(m, f, o)
				outcome = m.Message
				// Without the relay the log is up now; with it the
				// answer ends the view, accepted or refused.
				return done || m.Overlay == nil
			case *view.Log:
				return d.overlayDone(m) || m.Overlay == nil
			}
			return false
		},
	})
	if outcome != "" {
		fmt.Println(outcome)
	}
	return err
}
