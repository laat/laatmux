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
	c2 := &composer{d: d, f: f}
	err = view.Run(ctx, t, m, view.Host{
		Changed: st.change,
		Refresh: func(*view.Model) {},
		Act:     c2.act,
	})
	// The terminal is restored before the outcome is printed, so it is
	// not lost with the alternate screen.
	t.Close()
	if c2.outcome != "" {
		fmt.Println(c2.outcome)
	}
	return err
}

// composer is compose's view host: the form, then whatever the submit
// puts up, until the view ends.
type composer struct {
	d       *dash
	f       *addForm
	outcome string // printed once the terminal is restored
}

// act handles an overlay ending. The form's submit either ends the
// view on accepted, puts the form back up on a refusal, runs the add
// in the foreground with its log, or leaves a message an answer the
// daemon may have taken carries, shown in an ended log until a key
// since the popup closes with the process. The log's end goes through
// the dashboard's handling, which may put up the notice of a prompt
// that did not reach the agent; when that is dismissed the view ends,
// jumping first to the session the add made when there is one, as the
// foreground add does, so the prompt can be pasted into the agent.
func (c *composer) act(m *view.Model, a view.Action) bool {
	if a.Kind != view.ActionOverlay {
		return false
	}
	d := c.d
	switch o := m.Overlay.(type) {
	case *view.Form:
		m.Overlay = nil
		if o.Cancelled {
			return true
		}
		done := d.submitForm(m, c.f, o)
		c.outcome = m.Message
		if done {
			return true
		}
		if m.Overlay == nil && m.Message != "" {
			m.Overlay = ended(m.Message)
			m.Message = ""
		}
		return false
	case *view.Log:
		done := d.overlayDone(m)
		if m.Message != "" {
			c.outcome = m.Message
		}
		return done || m.Overlay == nil
	case *view.Notice:
		// The dashboard's handling jumps when there is a session and no
		// error to show; the outcome is what it left.
		d.overlayDone(m)
		if m.Message != "" {
			c.outcome = m.Message
		}
		return true
	}
	return false
}
