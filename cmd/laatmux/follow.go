package main

import (
	"context"
	"time"

	"github.com/laat/laatmux/internal/config"
	"github.com/laat/laatmux/internal/merged"
	"github.com/laat/laatmux/internal/palette"
	"github.com/laat/laatmux/internal/view"
)

// A view that stays up follows the config file as the daemon follows
// its repos: every configPoll it looks at the file (config.Watch), and
// takes the file read again when it has changed, on the view's
// goroutine. The dashboard and a sidebar pane take the hosts, agents,
// repositories and defaults their keys and the task form use, the rows'
// order and stale settings, the line templates, the icons, the theme and
// the jump key labels (configTaker); compose takes the theme, its form
// reading the file as a picker opens. What a view cannot take stays as
// it started: the pane's place and size, which `sidebar on` and the
// hooks set (fit puts a width changed in the file on the next resize);
// the view, layout and scope it started in, which its keys and
// sidebar.json own from then on; and the terminal's background, asked
// once before the view reads the keys, since an answer asked later
// would come through them.

// configPoll is how often a view looks at the config file.
const configPoll = 2 * time.Second

// watchConfig looks at the config file every interval until ctx ends,
// w having made the first read, and sends the view take with the file
// read again when it has changed; failed, when set, with the error of a
// file that changed and does not load, once per change, the view
// keeping what it has.
func watchConfig(ctx context.Context, w *config.Watch, every time.Duration, cmds chan<- func(*view.Model) view.Action, take func(*view.Model, config.Config), failed func(*view.Model, error)) {
	go func() {
		t := time.NewTicker(every)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
			cfg, changed, err := w.Changed()
			var f func(*view.Model) view.Action
			switch {
			case err != nil && failed != nil:
				f = func(m *view.Model) view.Action { failed(m, err); return view.Action{} }
			case changed:
				f = func(m *view.Model) view.Action { take(m, cfg); return view.Action{} }
			default:
				continue
			}
			select {
			case cmds <- f:
			case <-ctx.Done():
				return
			}
		}
	}()
}

// formConfig puts a config file that does not load in the note of the
// task form that is up, which covers the view's footer, and of one
// opened later, until a read succeeds; nil, a read that did, clears it.
func (d *dash) formConfig(err error) {
	d.configErr = formConfigErr(err)
	if d.add != nil {
		d.add.configErr = d.configErr
	}
}

// formConfigErr is the task form's note for a config that does not
// load, "" for nil.
func formConfigErr(err error) string {
	if err == nil {
		return ""
	}
	return "config: " + err.Error() + "; the form keeps what it offers"
}

// background is the terminal's background as a view asks it: once, the
// first time the theme needs it, before the view reads the keys; after
// that the answer, or none, which the theme takes as COLORFGBG or dark.
type background struct {
	ask      func() (dark, ok bool) // nil once asked, or once the view reads the keys
	dark, ok bool
}

func (b *background) get() (bool, bool) {
	if b.ask != nil {
		b.dark, b.ok = b.ask()
		b.ask = nil
	}
	return b.dark, b.ok
}

// configTaker is how the dashboard and a sidebar pane take the config:
// at start and each time the file changes.
type configTaker struct {
	d       *dash
	st      *merged.State
	o       viewOptions
	bg      *background
	theme   func(palette.Theme) // the terminal's, which the next draw uses
	current string              // the session the rows mark, for the refill
	// failure is the footer failed put up, which a take clears while
	// the footer still says it.
	failure string
}

// failed puts a config file that changed and does not load in the
// footer, and in the note of a task form that is up, which covers the
// footer; the view keeps what it has.
func (c *configTaker) failed(m *view.Model, err error) {
	c.failure = "config: " + err.Error() + "; the view keeps the config it had"
	m.Message = c.failure
	c.d.formConfig(err)
}

// take gives the view what it draws and acts with from cfg: the dash's
// config, which the jumps, the removals and a task form made from now
// on read, with a failure's footer and note cleared; the merged state's repository names, order and stale
// settings; the theme and icons; the templates; the agent icons; the
// jump key labels; this machine's name; and in a sidebar pane the
// strip's chip width. refill fills the rows again, for a config taken
// while the view runs; the first fill is the view's.
func (c *configTaker) take(m *view.Model, cfg config.Config, refill bool) {
	c.d.cfg = cfg
	c.d.formConfig(nil)
	if c.failure != "" && m.Message == c.failure {
		// The file that did not load is put right.
		m.Message = ""
	}
	c.failure = ""
	c.st.Configure(cfg)
	th, icons := lookWith(cfg, c.bg.get)
	c.theme(th)
	m.Icons = icons
	m.SetTemplates(templatesFor(cfg, c.o))
	m.AgentIcons = agentIcons(cfg)
	m.JumpKeys = jumpKeysShown(cfg, c.o)
	m.LocalHost = localHostName(cfg)
	if c.o.listen {
		// A sidebar pane: the strip's chips, which a pane on the left
		// does not draw.
		m.ItemWidth = cfg.Sidebar.ItemWidth()
	}
	if refill {
		fill(m, c.st.Status(c.current))
	}
}
