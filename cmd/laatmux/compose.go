package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/laat/laatmux/internal/config"
	"github.com/laat/laatmux/internal/home"
	"github.com/laat/laatmux/internal/protocol"
	"github.com/laat/laatmux/internal/view"
	"github.com/laat/laatmux/internal/worktree"
)

// ended is a log that has ended with the message as its error, so the
// view waits for a key before it goes on.
func ended(msg string) *view.Log {
	l := view.NewLog("laatmux compose")
	l.End(errors.New(msg))
	return l
}

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
	// The merged stream is followed while the form is up, so the note
	// about a host's daemon reflects the hello that arrives after the
	// snapshot on a cold daemon.
	st := newMerged()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go st.followMerged(ctx, c)
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
				if done {
					return true
				}
				// A refusal put the form back up; without the relay
				// the log is up; an answer the daemon may have taken
				// stays until a key, since the popup closes with it.
				if m.Overlay == nil && m.Message != "" {
					m.Overlay = ended(m.Message)
					m.Message = ""
				}
				return false
			case *view.Log:
				done := d.overlayDone(m)
				if m.Message != "" {
					outcome = m.Message
				}
				return done || m.Overlay == nil
			}
			return false
		},
	})
	// The terminal is restored before the outcome is printed, so it is
	// not lost with the alternate screen.
	t.Close()
	if outcome != "" {
		fmt.Println(outcome)
	}
	return err
}
