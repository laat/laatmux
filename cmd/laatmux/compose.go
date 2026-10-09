package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/laat/laatmux/internal/config"
	"github.com/laat/laatmux/internal/home"
	"github.com/laat/laatmux/internal/merged"
	"github.com/laat/laatmux/internal/term"
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
// the dashboard's a does. The submit hands the add to the local
// daemon's relay and the popup closes on accepted.
func cmdCompose(ctx context.Context, args []string) error {
	if len(args) > 0 {
		return errors.New("usage: laatmux compose")
	}
	// The watch's first read: the theme follows the file from there, and
	// the form reads it again as a picker opens.
	var w config.Watch
	cfg, _, err := w.Changed()
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
	// The merged stream is followed while the form is up, so the note
	// about a host's daemon reflects the hello that arrives after the
	// snapshot on a cold daemon.
	st := merged.New()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go st.Follow(ctx, c)
	// The candidates are the known set, which the merged stream carries:
	// its snapshot is waited for, and this machine's host listed in it,
	// whose checkouts the directory's repository is preselected from,
	// for a moment at most; what the hosts send later is a candidate
	// once the picker opens.
	awaitLocal(ctx, st, localWait)
	f := &addForm{hosts: addHosts(cfg), agents: cfg.AgentNames(), known: st.Known, reload: config.LoadSettled}
	f.take(cfg)
	// No repository known is the form with a picker for a pasted
	// source, as the dashboard's a has.
	switch {
	case len(f.hosts) == 0:
		return errors.New("no host has repos and worktrees configured")
	case len(f.agents) == 0:
		return errors.New("no agents configured")
	}
	last, err := home.ReadLast()
	if err != nil {
		return err
	}
	// Opened from a workspace session, the form is for its repository
	// on its host; elsewhere for the repository of the directory it was
	// opened in.
	preRepo, preHost := workspacePreset(ctx, f.hosts)
	if preRepo == "" {
		if repo, err := resolveRepo(ctx, cfg, func() merged.Known { return f.set }, ""); err == nil {
			preRepo = repo.Source
		}
	}
	form := buildForm(cfg, f, last, preRepo, preHost, "", st.HostCaps)
	form.Validate = func(b string) error { return worktree.CheckBranch(ctx, strings.TrimSpace(b)) }
	t, err := term.Open(os.Stdin, os.Stdout)
	if err != nil {
		return err
	}
	bg := &background{ask: func() (bool, bool) { return t.Background(backgroundWait) }}
	t.Theme, _ = lookWith(cfg, bg.get)
	// The view reads the keys from here: the terminal is not asked
	// again.
	bg.ask = nil
	m := &view.Model{Layout: view.Compact, Overlay: form}
	d := &dash{ctx: ctx, cfg: cfg, st: st, exitOnJump: true, add: f}
	c2 := &composer{d: d, f: f}
	cmds := make(chan func(*view.Model) view.Action)
	// The form is the whole view: a config that does not load is said in
	// its note, until a read succeeds.
	watchConfig(ctx, &w, configPoll, cmds, func(_ *view.Model, cfg config.Config) {
		t.Theme, _ = lookWith(cfg, bg.get)
		d.formConfig(nil)
		f.takePaused(cfg)
	}, func(_ *view.Model, err error) { d.formConfig(err) })
	err = view.Run(ctx, t, m, view.Host{
		Changed:  st.Changed(),
		Refresh:  func(*view.Model) {},
		Act:      c2.act,
		Commands: cmds,
	})
	// The terminal is restored before the outcome is printed, so it is
	// not lost with the alternate screen.
	t.Close()
	if c2.outcome != "" {
		fmt.Println(c2.outcome)
	}
	return err
}

// localWait bounds compose's wait for the merged stream's snapshot with
// this machine's host listed: a popup is up within it whatever the
// daemon is doing, a first poll on a cold one say.
const localWait = 3 * time.Second

// awaitLocal waits, for at most wait, until the state has the merged
// stream's snapshot and the local host in it is listed, has failed or
// is paused.
func awaitLocal(ctx context.Context, st *merged.State, wait time.Duration) {
	t := time.NewTimer(wait)
	defer t.Stop()
	for {
		s, waiting := st.Status(""), st.Waiting()
		if s.Loaded && !slices.ContainsFunc(s.Hosts, func(h merged.Host) bool { return h.Local && slices.Contains(waiting, h.Name) }) {
			return
		}
		select {
		case <-st.Changed():
		case <-t.C:
			return
		case <-ctx.Done():
			return
		}
	}
}

// composer is compose's view host: the form, then whatever the submit
// puts up, until the view ends.
type composer struct {
	d       *dash
	f       *addForm
	outcome string // printed once the terminal is restored
}

// act handles an overlay ending. The form's submit either ends the
// view on accepted, puts the form back up on a refusal, or leaves a
// message an answer the daemon may have taken carries, shown in an
// ended log until a key since the popup closes with the process.
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
	}
	return false
}
