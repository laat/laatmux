package main

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/laat/laatmux/internal/command"
	"github.com/laat/laatmux/internal/config"
	"github.com/laat/laatmux/internal/home"
	"github.com/laat/laatmux/internal/protocol"
	"github.com/laat/laatmux/internal/rows"
	"github.com/laat/laatmux/internal/view"
	"github.com/laat/laatmux/internal/workspace"
	"github.com/laat/laatmux/internal/worktree"
)

// dash is the dashboard's actions on the view: the keys beyond the
// shared ones, the pickers they open, and the commands they run. The
// commands are the same implementations the CLI calls, in
// internal/command, with the progress drawn in a log overlay in place
// of the list.
type dash struct {
	ctx        context.Context
	cfg        config.Config
	st         *merged
	exitOnJump bool
	// add is the picker sequence in progress, nil when none.
	add *addFlow
	// rm is the removal a confirm line asks about.
	rm command.Rm
	// run is the command whose log is on screen, nil when none.
	run *running
}

// running is a command under way: its log, and what to do when it ends.
type running struct {
	log  *view.Log
	done func(m *view.Model) (exit bool)
}

// act handles a dashboard key, a confirm answer or an overlay ending.
// True ends the view.
func (d *dash) act(m *view.Model, a view.Action) bool {
	switch a.Kind {
	case view.ActionOther:
		if a.Key.Kind != view.KeyRune {
			return false
		}
		switch a.Key.Rune {
		case 'a':
			d.startAdd(m)
		case 'x', 'X':
			d.askRm(m, a.Key.Rune == 'X')
		case 's':
			d.settle(m)
		case 'S':
			return d.shell(m)
		}
	case view.ActionConfirm:
		if m.ConfirmTag == "rm" {
			m.ConfirmTag = ""
			d.startRm(m)
		}
	case view.ActionOverlay:
		return d.overlayDone(m)
	}
	return false
}

// jump is the Enter key: the jump command's logic on the selected row.
func (d *dash) jump(m *view.Model, r rows.Row) bool {
	if err := jumpRow(d.ctx, d.cfg, r); err != nil {
		m.Message = err.Error()
		return false
	}
	return d.exitOnJump
}

// overlayDone reads what the finished overlay decided and moves on:
// the next picker of an add, the add itself, or the outcome of a
// command.
func (d *dash) overlayDone(m *view.Model) bool {
	switch o := m.Overlay.(type) {
	case *view.Picker:
		m.Overlay = nil
		if o.Chosen < 0 || d.add == nil {
			d.add = nil
			return false
		}
		d.add.chose(o.Chosen)
		d.advanceAdd(m)
	case *view.Prompt:
		m.Overlay = nil
		if o.Cancelled || d.add == nil {
			d.add = nil
			return false
		}
		d.add.branch = strings.TrimSpace(o.Text)
		d.runAdd(m)
	case *view.Log:
		if o.Quit {
			return true
		}
		m.Overlay = nil
		run := d.run
		d.run = nil
		if run == nil {
			return false
		}
		if err := o.Err(); err != nil {
			// The error was on screen until the key; the list returns
			// with the message repeating it, since a refusal is worth
			// keeping in sight.
			m.Message = err.Error()
			return false
		}
		return run.done(m)
	}
	return false
}

// start runs a command in the background with its progress in a log
// overlay, then calls done on the view's goroutine when it succeeded.
func (d *dash) start(m *view.Model, title string, run func(command.Reporter) error, done func(m *view.Model) bool) {
	log := view.NewLog(title)
	d.run = &running{log: log, done: done}
	m.Overlay = log
	r := logReporter{log: log, st: d.st}
	go func() {
		err := run(r)
		log.End(err)
		d.st.notify()
	}()
}

// logReporter draws a command's progress into its log and wakes the
// view.
type logReporter struct {
	log *view.Log
	st  *merged
}

func (r logReporter) Progress(m protocol.Message) {
	r.log.Append(command.ProgressLine(m))
	r.st.notify()
}

func (r logReporter) Note(s string) {
	r.log.Append("laatmux: " + s)
	r.st.notify()
}

// addFlow is the a key: pickers for the repository, the host and the
// agent, then a prompt for the branch, then the add. Each picker is
// skipped when there is one candidate, and preselects the default the
// CLI would take: the repository of the directory the popup was opened
// from, the host and agent last used for that repository, else the
// first. a on a worktree row that has no session pre-fills the
// repository, host and branch from the record.
type addFlow struct {
	repos  []config.Repo
	hosts  []config.Host
	agents []string
	step   int // the step whose picker is up: 0 repository, 1 host, 2 agent
	last   home.Last
	repo   config.Repo
	host   config.Host
	agent  string
	branch string
	// preselections, from the selected row or the working directory
	preRepo, preHost string
}

func (d *dash) startAdd(m *view.Model) {
	f := &addFlow{repos: d.cfg.Repos, agents: d.cfg.AgentNames()}
	for _, h := range d.cfg.Hosts {
		if h.CanAdd() {
			f.hosts = append(f.hosts, h)
		}
	}
	// A step with nothing to pick from refuses before any picker is
	// up, so nothing is chosen for an add that cannot be sent. So does
	// last.json that cannot be read: the CLI's add fails on it before
	// sending, and the add's own update of it would fail after the
	// worktree and agent exist on the host.
	switch {
	case len(f.repos) == 0:
		m.Message = "no repositories configured"
		return
	case len(f.hosts) == 0:
		m.Message = "no host has repos and worktrees configured"
		return
	case len(f.agents) == 0:
		m.Message = "no agents configured"
		return
	}
	last, err := home.ReadLast()
	if err != nil {
		m.Message = "last.json: " + err.Error()
		return
	}
	f.last = last
	if r := m.Selection(); r != nil && r.Worktree != nil && r.Worktree.Session == "" && !r.Stale {
		f.preRepo, f.preHost, f.branch = localRepoArg(d.cfg, *r.Worktree), r.Host, r.Worktree.Branch
	} else if repo, err := resolveRepo(d.ctx, d.cfg, ""); err == nil {
		f.preRepo = repo.Name
	}
	d.add = f
	d.advanceAdd(m)
}

// chose records the picker's choice for the current step.
func (f *addFlow) chose(i int) {
	switch f.step {
	case 0:
		f.repo = f.repos[i]
	case 1:
		f.host = f.hosts[i]
	case 2:
		f.agent = f.agents[i]
	}
	f.step++
}

// advanceAdd opens the picker for the next step, skipping steps with
// one candidate, then the branch prompt.
func (d *dash) advanceAdd(m *view.Model) {
	f := d.add
	for {
		switch f.step {
		case 0:
			if len(f.repos) == 1 {
				f.chose(0)
				continue
			}
			var cs []view.Choice
			pre := 0
			for i, r := range f.repos {
				cs = append(cs, view.Choice{Label: r.Name, Detail: r.Source})
				if r.Name == f.preRepo || r.Source == f.preRepo {
					pre = i
				}
			}
			m.Overlay = view.NewPicker("add: repository", cs, pre)
			return
		case 1:
			if len(f.hosts) == 1 {
				f.chose(0)
				continue
			}
			want := f.preHost
			if want == "" {
				if h, err := d.cfg.DefaultHost("", f.last.Get(f.repo.Source).Host); err == nil {
					want = h.Name
				}
			}
			var cs []view.Choice
			pre := 0
			for i, h := range f.hosts {
				detail := h.Worktrees
				if h.SSH != "" {
					detail = "ssh " + h.SSH + "  " + detail
				}
				cs = append(cs, view.Choice{Label: h.Name, Detail: detail})
				if h.Name == want {
					pre = i
				}
			}
			m.Overlay = view.NewPicker("add: host", cs, pre)
			return
		case 2:
			if len(f.agents) == 1 {
				f.chose(0)
				continue
			}
			want := ""
			if name, _, err := d.cfg.DefaultAgent("", f.last.Get(f.repo.Source).Agent); err == nil {
				want = name
			}
			var cs []view.Choice
			pre := 0
			for i, name := range f.agents {
				cs = append(cs, view.Choice{Label: name, Detail: strings.Join(d.cfg.Agents[name].Cmd, " ")})
				if name == want {
					pre = i
				}
			}
			m.Overlay = view.NewPicker("add: agent", cs, pre)
			return
		default:
			title := fmt.Sprintf("add %s on %s with %s: branch", f.repo.Name, f.host.Name, f.agent)
			m.Overlay = view.NewPrompt(title, f.branch, func(s string) error {
				return worktree.CheckBranch(d.ctx, strings.TrimSpace(s))
			})
			return
		}
	}
}

// runAdd runs the add the pickers built; on success the new workspace
// session is jumped to.
func (d *dash) runAdd(m *view.Model) {
	f := d.add
	d.add = nil
	add := command.Add{Host: f.host, Repo: f.repo, Branch: f.branch, Agent: f.agent}
	var res command.Added
	d.start(m, add.Describe(), func(r command.Reporter) error {
		var err error
		res, err = add.Run(d.ctx, r)
		if err != nil && res.Root != "" {
			// The host's side is done; what failed is local, and the
			// message must say the worktree and agent exist.
			return fmt.Errorf("%s/%s ready on %s (%s); local session: %w", f.repo.Name, f.branch, f.host.Name, res.Root, err)
		}
		return err
	}, func(m *view.Model) bool {
		if err := switchTo(d.ctx, res.Session); err != nil {
			m.Message = err.Error()
			return false
		}
		return d.exitOnJump
	})
}

// askRm puts the confirm line up for the selected workspace: a worktree
// row, or a stale row whose session still names its root. The question
// names what goes and where.
func (d *dash) askRm(m *view.Model, force bool) {
	r := m.Selection()
	if r == nil {
		return
	}
	rm, err := d.rmFor(*r)
	if err != nil {
		m.Message = err.Error()
		return
	}
	rm.Force = force
	d.rm = rm
	verb := "remove"
	if force {
		verb = "force-remove"
	}
	m.Ask(fmt.Sprintf("%s %s on %s (%s)? y/n", verb, rm.Describe(), rm.Host.Name, rm.Root), "rm")
}

// rmFor is the rm for a row: the worktree's repository, branch and root
// from its record, or from a stale session's tags and key. A repository
// this machine's config does not know is removed by root alone, as
// --root does.
func (d *dash) rmFor(r rows.Row) (command.Rm, error) {
	if r.Host == "" {
		return command.Rm{}, errors.New(r.Name + ": no configured host claims this record")
	}
	h, ok := d.cfg.Find(r.Host)
	if !ok {
		return command.Rm{}, fmt.Errorf("unknown host %q", r.Host)
	}
	rm := command.Rm{Host: h}
	switch {
	case r.Worktree != nil:
		rm.Root, rm.Branch, rm.Environment = r.Worktree.Root, r.Worktree.Branch, r.Worktree.EnvironmentID
		if repo, ok := d.cfg.RepoBySource(r.Worktree.Source); ok {
			rm.Repo = repo
		} else if repo, ok := d.cfg.RepoByName(r.Worktree.Repo); ok && r.Worktree.Source == "" {
			rm.Repo = repo
		}
		if rm.Repo.Source == "" {
			rm.Branch = ""
		}
	case r.Stale:
		rm.Environment, rm.Root = workspace.SplitKey(r.Local.Key)
		if repo, ok := d.cfg.RepoBySource(r.Local.Source); ok && r.Local.Branch != "" {
			rm.Repo, rm.Branch = repo, r.Local.Branch
		}
	default:
		return command.Rm{}, errors.New(r.Name + ": not a worktree; rm removes worktrees")
	}
	if rm.Root == "" {
		return command.Rm{}, errors.New(r.Name + ": no root known")
	}
	return rm, nil
}

// startRm runs the confirmed rm; the list returns on success with a
// message, and a refusal stays on screen with the hint to use X.
func (d *dash) startRm(m *view.Model) {
	rm := d.rm
	d.rm = command.Rm{}
	what := fmt.Sprintf("%s on %s", rm.Describe(), rm.Host.Name)
	var res command.Removed
	d.start(m, "rm "+what, func(r command.Reporter) error {
		var err error
		res, err = rm.Run(d.ctx, r)
		if err != nil && res.Root != "" {
			// The host's side is done; what failed is the local
			// session, and the message must not read as a refusal.
			return fmt.Errorf("removed %s; local session: %w", what, err)
		}
		return forceHint(err, rm.Force)
	}, func(m *view.Model) bool {
		msg := "removed " + what
		if len(res.Killed) > 0 {
			msg += "; killed " + strings.Join(res.Killed, ", ")
		}
		m.Message = msg
		return false
	})
}

// forceHint adds what X does to a refusal that asks for force: git's
// message for a dirty or locked worktree says to use --force, and here
// that is X.
func forceHint(err error, force bool) error {
	if err == nil || force || !strings.Contains(err.Error(), "force") {
		return err
	}
	return fmt.Errorf("%w  (X force-removes)", err)
}

// settle toggles the settled tag on the selected row's workspace
// session. The merged stream carries the change back within a second
// and the row moves to or from the settled group.
func (d *dash) settle(m *view.Model) {
	r := m.Selection()
	if r == nil {
		return
	}
	if r.Local == nil || !r.Local.Workspace() {
		m.Message = r.Name + ": no local workspace session; enter creates one"
		return
	}
	if err := workspace.SetSettled(d.ctx, r.Local.Name, !r.Settled); err != nil {
		m.Message = err.Error()
		return
	}
	if r.Settled {
		m.Message = "unsettled " + r.Local.Name
	} else {
		m.Message = "settled " + r.Local.Name
	}
}

// shell opens the shell window in the selected workspace, creating the
// workspace session first when the row has none, and jumps to it.
func (d *dash) shell(m *view.Model) bool {
	r := m.Selection()
	if r == nil {
		return false
	}
	l, err := d.localFor(*r)
	if err != nil {
		m.Message = err.Error()
		return false
	}
	if err := command.Shell(d.ctx, d.cfg, l); err != nil {
		m.Message = err.Error()
		return false
	}
	if err := switchTo(d.ctx, l.Name); err != nil {
		m.Message = err.Error()
		return false
	}
	return d.exitOnJump
}

// localFor is the row's workspace session, made from the worktree
// record when it does not exist yet. An existing session is routed by
// the host that answers for its key's environment id now, not by the
// host tag it was made with, which a renamed host leaves behind, and
// not by the row's host: an observed agent on this machine's default
// server sits in a local window of a workspace whose worktree may be
// on another host, and the shell belongs where the worktree is.
func (d *dash) localFor(r rows.Row) (workspace.Local, error) {
	if r.Stale {
		return workspace.Local{}, errors.New(r.Name + ": its worktree is gone")
	}
	if r.Local != nil && r.Local.Workspace() {
		l := *r.Local
		env, _ := workspace.SplitKey(l.Key)
		if name := d.st.hostOf(env); name != "" {
			l.Host = name
		}
		return l, nil
	}
	if r.Worktree == nil || r.Worktree.Session == "" {
		return workspace.Local{}, errors.New(r.Name + ": not a workspace")
	}
	h, ok := d.cfg.Find(r.Host)
	if !ok {
		return workspace.Local{}, fmt.Errorf("unknown host %q", r.Host)
	}
	spec := worktreeSpec(d.cfg, h, *r.Worktree)
	name, _, err := workspace.Ensure(d.ctx, spec)
	if err != nil {
		return workspace.Local{}, err
	}
	return workspace.Local{Name: name, Key: spec.Key, Host: h.Name, Source: spec.Source, Branch: spec.Branch}, nil
}
