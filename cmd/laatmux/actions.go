package main

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"runtime"
	"strings"

	"github.com/laat/laatmux/internal/command"
	"github.com/laat/laatmux/internal/config"
	"github.com/laat/laatmux/internal/home"
	"github.com/laat/laatmux/internal/merged"
	"github.com/laat/laatmux/internal/protocol"
	"github.com/laat/laatmux/internal/rows"
	"github.com/laat/laatmux/internal/source"
	"github.com/laat/laatmux/internal/term"
	"github.com/laat/laatmux/internal/tmux"
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
	st         *merged.State
	exitOnJump bool
	// submit hands an add to the local daemon; a test replaces it, as
	// it does dismiss and deliver, a pending task's x and p.
	submit  func(command.Add) (string, error)
	dismiss func(id string) error
	deliver func(id string) (state, reason string, err error)
	// pending is the task a confirm line asks to dismiss.
	pending *protocol.Pending
	// add is the form in progress, nil when none.
	add *addForm
	// rm is the removal a confirm line asks about.
	rm command.Rm
	// run is the command whose log is on screen, nil when none.
	run *running
	// jumper replaces a row's jump, for tests, and refocus is the return
	// of focus after a click in the sidebar.
	jumper func(r rows.Row) error
	// client is the tmux client the next jump switches, from a sidebar
	// command with -c; "" is the view's own.
	client  string
	refocus func()
}

// running is a command under way: what to do when it ends; its log is
// the overlay.
type running struct {
	done func(m *view.Model) (exit bool)
}

// act handles a dashboard key, a confirm answer or an overlay ending.
// True ends the view.
func (d *dash) act(m *view.Model, a view.Action) bool {
	switch a.Kind {
	case view.ActionOther:
		if a.Key.Kind != term.KeyRune {
			return false
		}
		switch a.Key.Rune {
		case 'a':
			d.startAdd(m)
		case 'x', 'X':
			if r := m.Selection(); r != nil && r.Pending != nil {
				d.askDismiss(m, *r)
				break
			}
			d.askRm(m, a.Key.Rune == 'X')
		case 'p':
			d.deliverPrompt(m)
		case 'z':
			d.settle(m)
		case 'S':
			return d.shell(m)
		case 'o', 'O':
			d.openBranch(m, a.Key.Rune == 'O')
		}
	case view.ActionConfirm:
		switch m.ConfirmTag {
		case "rm":
			m.ConfirmTag = ""
			d.startRm(m)
		case "dismiss":
			m.ConfirmTag = ""
			d.startDismiss(m)
		}
	case view.ActionOverlay:
		return d.overlayDone(m)
	}
	return false
}

// jumpRow runs the jump and says whether it happened: jumped is false
// for a task still running and for a jump refused, whose message is in
// the footer; exit is that the view ends.
func (d *dash) jumpRow(m *view.Model, r rows.Row) (exit, jumped bool) {
	if r.Pending != nil && !r.Pending.Done {
		// A task still running has nothing to jump to yet.
		return false, false
	}
	jump := d.jumper
	if jump == nil {
		jump = func(r rows.Row) error { return jumpRow(d.ctx, d.cfg, r) }
	}
	if p, ok := paneOf(r); ok && d.jumper == nil {
		// A tile, or an agent or a pane in the tree: to the pane, the
		// session reached whatever the pane's fate. A worktree's row goes
		// by its own line when that line attaches the pane's session, as
		// z and S on it do, though another line attaches the session
		// too: one whose root agent was moved into it, or into whose
		// session this worktree's root agent was moved.
		line := m.LineFor(r.Host, p.session)
		if r.Worktree != nil {
			if l := m.OwnerLine(r.Worktree.ID); l != nil && l.Home() == p.session {
				line = l
			}
		}
		msg, err := jumpPane(d.ctx, d.cfg, line, r, p)
		if err != nil {
			m.Message = err.Error()
			return false, false
		}
		m.Message = msg
		return d.exitOnJump, true
	}
	if r.Kind == rows.KindRun {
		// A run's line: the worktree's session, as its line's jump
		// reaches it, through the root agent with the home lost.
		if l := m.OwnerLine(r.Worktree.ID); l != nil {
			r = *l
		} else {
			r.Kind, r.Run = rows.KindWorktree, nil
		}
	}
	if err := jump(r); err != nil {
		m.Message = err.Error()
		return false, false
	}
	return d.exitOnJump, true
}

// overlayDone reads what the finished overlay decided and moves on:
// the next picker of an add, the add itself, or the outcome of a
// command.
func (d *dash) overlayDone(m *view.Model) bool {
	switch o := m.Overlay.(type) {
	case *view.Form:
		m.Overlay = nil
		f := d.add
		if o.Cancelled || f == nil {
			d.add = nil
			return false
		}
		return d.submitForm(m, f, o)
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
	d.run = &running{done: done}
	m.Overlay = log
	r := logReporter{log: log, st: d.st}
	go func() {
		err := run(r)
		log.End(err)
		d.st.Notify()
	}()
}

// logReporter draws a command's progress into its log and wakes the
// view.
type logReporter struct {
	log *view.Log
	st  *merged.State
}

func (r logReporter) Progress(m protocol.Message) {
	r.log.Append(command.ProgressLine(m))
	r.st.Notify()
}

func (r logReporter) Note(s string) {
	r.log.Append("laatmux: " + s)
	r.st.Notify()
}

// addForm is the a key: the task form, with the candidates its chips
// were built from, so a choice maps back to the config's entries. The
// chips preselect what add would take: the repository of the directory
// the popup was opened from, the host and agent last used for it, else
// the config's defaults. a on a worktree row that has no session
// pre-fills the repository, host and branch from the record, the
// branch explicit.
type addForm struct {
	repos  []config.Repo
	hosts  []config.Host
	agents []string
}

func (d *dash) startAdd(m *view.Model) {
	f := &addForm{repos: d.cfg.Repos, agents: d.cfg.AgentNames()}
	for _, h := range d.cfg.Hosts {
		if h.CanAdd() {
			f.hosts = append(f.hosts, h)
		}
	}
	// A field with nothing to choose from refuses before the form is
	// up. So does last.json that cannot be read: the submit's own
	// update of it would fail after the daemon has the task. The error
	// names the file.
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
		m.Message = err.Error()
		return
	}
	// The repository and host of the selected row's worktree, from a
	// tile or any tree line under one, with the branch when the worktree
	// has no session yet, so an agent can be started in it; a
	// repository line names its repository.
	preRepo, preHost, branch := "", "", ""
	switch r := m.Selection(); {
	case r != nil && r.Worktree != nil && !r.Orphaned:
		preRepo, preHost = localRepoArg(d.cfg, *r.Worktree), r.Host
		if r.Worktree.Session == "" {
			branch = r.Worktree.Branch
		}
	case r != nil && r.Kind == rows.KindRepo:
		// By its source, as a worktree's row: the line's name is a
		// host's label when this machine has none, which another local
		// repository could share, so a source this machine does not
		// know preselects nothing. A repository known by a label alone
		// goes by it.
		if r.ID() == rows.LabelRepoNode(r.Name) {
			preRepo = r.Name
		}
		for _, repo := range d.cfg.Repos {
			if rows.RepoNode(repo.Source) == r.ID() {
				preRepo = repo.Name
				break
			}
		}
	default:
		if repo, err := resolveRepo(d.ctx, d.cfg, ""); err == nil {
			preRepo = repo.Name
		}
	}
	form := buildForm(d.cfg, f, last, preRepo, preHost, branch, d.st.HostCaps)
	form.Validate = func(b string) error { return worktree.CheckBranch(d.ctx, strings.TrimSpace(b)) }
	d.add = f
	m.Overlay = form
}

// buildForm makes the task form over the candidates, preselecting the
// repository named, the host and agent last used for it, else the
// config's defaults. caps, when set, gives a host's cached daemon
// capabilities for the note about tasks not being supported.
func buildForm(cfg config.Config, f *addForm, last home.Last, preRepo, preHost, branch string, caps func(host string) ([]string, bool)) *view.Form {
	var chips [3]view.Chip
	chips[0].Title = "repository"
	for i, r := range f.repos {
		chips[0].Choices = append(chips[0].Choices, view.Choice{Label: r.Name, Detail: r.Source})
		if r.Name == preRepo || source.Same(r.Source, preRepo) {
			chips[0].Selected = i
		}
	}
	repo := f.repos[chips[0].Selected]
	chips[1].Title = "host"
	wantHost := preHost
	if wantHost == "" {
		if h, err := cfg.DefaultHost("", last.Get(repo.Source).Host); err == nil {
			wantHost = h.Name
		}
	}
	for i, h := range f.hosts {
		detail := h.Worktrees
		if h.SSH != "" {
			detail = "ssh " + h.SSH + "  " + detail
		}
		chips[1].Choices = append(chips[1].Choices, view.Choice{Label: h.Name, Detail: detail})
		if h.Name == wantHost {
			chips[1].Selected = i
		}
	}
	chips[2].Title = "agent"
	wantAgent := ""
	if name, _, err := cfg.DefaultAgent("", last.Get(repo.Source).Agent); err == nil {
		wantAgent = name
	}
	for i, name := range f.agents {
		chips[2].Choices = append(chips[2].Choices, view.Choice{Label: name, Detail: strings.Join(cfg.Agents[name].Cmd, " ")})
		if name == wantAgent {
			chips[2].Selected = i
		}
	}
	form := view.NewForm("add a task", chips, branch)
	form.Propose = worktree.ProposeBranch
	// A repository chosen later brings its own last-used host and
	// agent, unless the user has set those chips themselves; a host
	// pre-filled from a worktree's record is as good as set.
	var userSet [3]bool
	userSet[1] = preHost != ""
	form.Changed = func(form *view.Form, chip int) {
		if chip != 0 {
			userSet[chip] = true
			return
		}
		// As at build: the last used, else the config's default, else
		// the first candidate.
		repo := f.repos[form.Chips[0].Selected]
		if !userSet[1] {
			form.Chips[1].Selected = 0
			if h, err := cfg.DefaultHost("", last.Get(repo.Source).Host); err == nil {
				for i, c := range f.hosts {
					if c.Name == h.Name {
						form.Chips[1].Selected = i
					}
				}
			}
		}
		if !userSet[2] {
			form.Chips[2].Selected = 0
			if name, _, err := cfg.DefaultAgent("", last.Get(repo.Source).Agent); err == nil {
				for i, a := range f.agents {
					if a == name {
						form.Chips[2].Selected = i
					}
				}
			}
		}
	}
	if caps != nil {
		form.Note = func(form *view.Form) string {
			host := form.Chips[1].Label()
			if c, ok := caps(host); ok && !protocol.Has(c, protocol.CapTask) {
				return "tasks not supported by " + host + "'s daemon"
			}
			return ""
		}
	}
	return form
}

// submitForm hands the add the form asked for to the local daemon's
// relay, and the view ends once it is accepted; a refusal, a host whose
// daemon does not support tasks say, keeps the form up with the error,
// its text intact, while an error after the daemon may hold the task
// drops the form and keeps the view with the id in the message, so
// nothing is submitted twice.
func (d *dash) submitForm(m *view.Model, f *addForm, o *view.Form) bool {
	add := command.Add{
		Host: f.hosts[o.Chips[1].Selected], Repo: f.repos[o.Chips[0].Selected], Copy: d.cfg.Copy, Agent: f.agents[o.Chips[2].Selected],
		Branch: strings.TrimSpace(o.Branch()), Prompt: o.Prompt(), Generated: o.Generated(),
	}
	submit := d.submit
	if submit == nil {
		submit = func(a command.Add) (string, error) { return a.Submit(d.ctx) }
	}
	id, err := submit(add)
	switch {
	case err != nil && id == "":
		o.Reopen(err.Error())
		m.Overlay = o
		return false
	case err != nil:
		// The daemon may hold the task: the form goes, so nothing is
		// submitted twice, and the view stays with the message, which
		// a popup closing would take with it.
		d.add = nil
		m.Message = "submitted " + id + "; " + err.Error() + "; laatmux tasks says whether the daemon holds it"
		return false
	}
	d.add = nil
	m.Message = "accepted " + id
	return true
}

// askRm puts the confirm line up for the selected workspace: a worktree
// row, or an orphaned row whose session still names its root. The question
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
	with := ""
	if r.Worktree != nil {
		// From an agent's tile or line: the worktree goes, and every
		// agent in it with it, counted as the tree joins them.
		switch n := m.AgentsUnder(r.Worktree.ID); {
		case n == 1:
			with = " with its agent"
		case n > 1:
			with = fmt.Sprintf(" with its %d agents", n)
		}
	}
	m.Ask(fmt.Sprintf("%s %s on %s (%s)%s? y/n", verb, rm.Describe(), rm.Host.Name, tmux.Printable(rm.Root), with), "rm")
}

// Dismissable is a pending task that x drops: one that needs the user,
// and three more that would otherwise be stuck, one the host has no
// trace of, one whose host is gone from the config, and one whose host
// answers as another machine. The daemon has the last word: it refuses
// a task still running on the machine it was accepted for.
func Dismissable(r rows.Row) bool {
	p := r.Pending
	if p == nil {
		return false
	}
	// The daemon's own tests, in its order: never sent nor taken; the
	// host gone or the machine replaced as it has recorded; a task with
	// an outcome and no attempt open. The view's own sight of a
	// replaced machine waits for the relay's record while the add runs.
	return (!p.Sent && !p.Taken) || r.Removed || p.Mismatch != "" || (p.Done && !p.AttemptOpen && r.NeedsUser())
}

// Deliverable is a pending task whose prompt p delivers now: the add
// succeeded, the prompt did not reach the agent or may not have, and no
// attempt is open. The prompt is retained until it is delivered.
func Deliverable(r rows.Row) bool {
	p := r.Pending
	if p == nil || r.Removed || r.Replaced || p.Mismatch != "" {
		// The relay cannot reach the machine the task was accepted on.
		return false
	}
	return p.Done && p.OK && !p.Delivered() && !p.Gone && !p.AttemptOpen && p.AttemptError != protocol.ErrRecoveryExpired &&
		(p.Prompt == protocol.DeliveryNotDelivered || p.Prompt == protocol.DeliveryUnknown)
}

// askDismiss puts the confirm line for dropping a pending task, or
// says why x does nothing on it.
func (d *dash) askDismiss(m *view.Model, r rows.Row) {
	if !Dismissable(r) {
		p := r.Pending
		switch {
		case r.Replaced && p.Mismatch == "" && !p.Done:
			m.Message = r.Name + ": the relay has not yet seen the machine change; x dismisses it once it has"
		case !p.Done:
			m.Message = r.Name + ": the add is still running; x dismisses it once it needs you"
		case p.AttemptOpen:
			m.Message = r.Name + ": a delivery attempt is open; x dismisses it once it has an outcome"
		default:
			m.Message = r.Name + ": nothing to dismiss; it hands over to its worktree row once listed"
		}
		return
	}
	p := *r.Pending
	d.pending = &p
	// The name is the task's <repo>/<branch>, and git takes a C1
	// control character and a byte that is not UTF-8 in a branch.
	m.Ask(fmt.Sprintf("dismiss %s on %s (%s)? y/n", tmux.Printable(r.Name), r.Host, r.State()), "dismiss")
}

// startDismiss drops the confirmed task through the local daemon.
func (d *dash) startDismiss(m *view.Model) {
	p := d.pending
	d.pending = nil
	if p == nil {
		return
	}
	dismiss := d.dismiss
	if dismiss == nil {
		dismiss = func(id string) error { return command.Dismiss(d.ctx, id) }
	}
	what := tmux.Printable(p.Repo+"/"+p.Branch) + " on " + p.Host
	d.start(m, "dismiss "+what, func(command.Reporter) error {
		return dismiss(p.ID)
	}, func(m *view.Model) bool {
		m.Message = "dismissed " + what
		return false
	})
}

// deliverPrompt is p: a pending task's retained prompt delivered now,
// with the state it reached in the footer.
func (d *dash) deliverPrompt(m *view.Model) {
	r := m.Selection()
	if r == nil {
		return
	}
	if r.Pending == nil {
		m.Message = r.Name + ": p delivers a pending task's prompt"
		return
	}
	p := *r.Pending
	if !Deliverable(*r) {
		switch {
		case r.Removed || r.Replaced || p.Mismatch != "":
			m.Message = r.Name + ": " + r.State()
			if Dismissable(*r) {
				m.Message += "; x dismisses the task"
			}
		case p.Delivered():
			m.Message = r.Name + ": nothing to deliver (the prompt is " + p.Prompt + ")"
		case p.Done && !p.OK, p.Gone, p.AttemptError == protocol.ErrRecoveryExpired:
			// The prompt has nowhere to go; the file still has it, if
			// the add had one, which a failure before the agent stage
			// does not say.
			m.Message = r.Name + ": " + r.State() + "; laatmux tasks show " + p.ID + " prints the prompt, if one was kept"
		default:
			m.Message = r.Name + ": nothing to deliver (" + r.State() + ")"
		}
		return
	}
	deliver := d.deliver
	if deliver == nil {
		deliver = func(id string) (string, string, error) { return command.DeliverPending(d.ctx, id) }
	}
	var state, reason string
	d.start(m, "prompt for "+r.Name, func(command.Reporter) error {
		var err error
		state, reason, err = deliver(p.ID)
		return err
	}, func(m *view.Model) bool {
		if state == "" {
			state = "sent"
		}
		m.Message = "prompt " + state
		if reason != "" {
			m.Message += ": " + reason
		}
		return false
	})
}

// rmFor is the rm for a row: the worktree's repository, branch and root
// from its record, or from an orphaned session's tags and key. A repository
// this machine's config does not know is removed by root alone, as
// --root does.
func (d *dash) rmFor(r rows.Row) (command.Rm, error) {
	switch r.Kind {
	case rows.KindPane, rows.KindRun:
		return command.Rm{}, errors.New(r.Name + ": a pane or a run; x removes worktrees, from their line or an agent's")
	case rows.KindRepo, rows.KindGroup, rows.KindFold:
		return command.Rm{}, errors.New(r.Name + ": x removes worktrees, from their line or an agent's")
	}
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
		}
		if rm.Repo.Source == "" {
			rm.Branch = ""
		}
	case r.Orphaned:
		rm.Environment, rm.Root = protocol.SplitSessionKey(r.Local.Key)
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

// openBranch opens the selected row's PR in the browser, or with checks
// its checks page, which for a branch with no PR is its commit's.
func (d *dash) openBranch(m *view.Model, checks bool) {
	r := m.Selection()
	if r == nil {
		return
	}
	b := r.Branch
	url := ""
	switch {
	case b == nil:
		m.Message = r.Name + ": no PR or checks known"
		return
	case checks:
		url = b.ChecksURL
	case b.PR != nil:
		url = b.PR.URL
	default:
		m.Message = r.Name + ": no PR; O opens the branch's checks"
		return
	}
	if url == "" {
		m.Message = r.Name + ": no checks page known"
		return
	}
	if err := openURL(url); err != nil {
		m.Message = "open " + url + ": " + err.Error()
		return
	}
	m.Message = "opening " + url
}

// openURL opens a URL in this machine's browser, without waiting for the
// browser: the view goes on meanwhile.
var openURL = func(url string) error {
	name := "xdg-open"
	if runtime.GOOS == "darwin" {
		name = "open"
	}
	cmd := exec.Command(name, url)
	if err := cmd.Start(); err != nil {
		return err
	}
	go cmd.Wait()
	return nil
}

// settle toggles the settled tag on the workspace session of the
// selected line, or of the line holding the selected row. The merged
// stream carries the change back within a second: in the agent view
// the workspace's tiles move into or out of the stale fold, but for a
// blocked or done one and the viewer's own, which stay in place,
// sorted with the stale rows, to be unsettled; in the tree the line
// stays where it is, dim with the stale icon unless an agent there
// wants the user.
func (d *dash) settle(m *view.Model) {
	r := m.Selection()
	if r == nil {
		return
	}
	// The settled state is the line's, and its children show it: a
	// tile, an agent, a pane or a run toggles it in the session of the
	// line holding it, whatever local session it has of its own. So
	// does one under a task standing for a listed worktree, whose line
	// carries the workspace session but, being a task's row, not the
	// state, and a managed agent of no worktree in a line's home
	// session, whose pane jump lands in that line's workspace session.
	line, resolved := r, false
	if l := ownerLine(m, *r); l != nil && r.Pending == nil {
		line, resolved = l, true
	}
	if r.Pending != nil || line.Pending != nil && r.Worktree == nil {
		// The row is the task's until it hands over, and so is the
		// add's agent before the host lists the worktree: z settles
		// the worktree row it becomes.
		m.Message = line.Name + ": a pending task; z settles its worktree row once it hands over"
		return
	}
	if !resolved && r.Worktree == nil && (r.Local == nil || !r.Local.Workspace()) {
		// A row of no worktree that no line holds, and of no workspace
		// session: a repository line, the stale fold, or an agent in
		// other sessions, in a plain session or in a managed one that is
		// no line's home. It is not a workspace, as S says, whatever
		// enter on it does: enter folds the line and the fold, and takes
		// the agent's pane jump to its session.
		m.Message = r.Name + ": not a workspace"
		return
	}
	if line.Local == nil || !line.Local.Workspace() {
		// A managed agent of no worktree in the session its line has by
		// name alone, with no home: the line's own jump does not make the
		// workspace session, refusing or going to its agent's plain
		// session, and the agent's pane jump makes it (paneSpec).
		hint := "enter creates one"
		if !resolved || r.Worktree != nil || line.Home() != "" {
			hint = noWorkspaceHint(d.cfg, *line, resolved)
		}
		m.Message = line.Name + ": no local workspace session; " + hint
		return
	}
	// The direction is the session's own state, which the rows' copies
	// are made from: a line's and its children's; that of an observed
	// agent on this machine's default server in a window of a workspace
	// session whose worktree does not take it as a child (on another
	// host, say), which stands in other sessions with that session as
	// its own; and that of a managed agent of no worktree in a line's
	// home session, which stands there too. A task's row carries the
	// session but not the state.
	settled := line.Local.Settled
	if err := workspace.SetSettled(d.ctx, line.Local.Name, !settled); err != nil {
		m.Message = err.Error()
		return
	}
	if settled {
		m.Message = "unsettled " + line.Local.Name
	} else {
		m.Message = "settled " + line.Local.Name
	}
}

// noWorkspaceHint says what enter on a line with no local workspace
// session does about one, by where the line's jump goes: it makes the
// session; or it jumps to the session of the line's agent on this
// machine's default server; or it is refused, and the refusal is the
// hint. A worktree with no home whose jump goes by such an agent, or by
// one on another host's default server, gets its workspace session from
// add, which starts a managed session at the root: the hint ends with
// the add line, or what add needs first. A task still running has
// nothing to jump to until it is done. A line of no worktree, which
// settle does not pass, gets no add line: what add needs is the
// worktree's.
func noWorkspaceHint(cfg config.Config, line rows.Row, resolved bool) string {
	enter := "enter"
	if resolved {
		enter = "enter on the line"
	}
	if line.Pending != nil && !line.Pending.Done {
		return enter + " creates one once the task is done"
	}
	var hint string
	switch _, session, err := jumpTarget(cfg, line); {
	case err == nil && session == "":
		return enter + " creates one"
	case err == nil:
		hint = fmt.Sprintf("%s jumps to %s, its agent's session", enter, session)
	default:
		hint = err.Error()
	}
	if h, ok := cfg.Find(line.Host); ok && line.Host != "" && line.Agent != nil && line.Worktree != nil {
		// Enter went by the agent, which only the jump of a worktree
		// with no home does: add gives it a home and the session.
		hint += "; " + addsSession(cfg, h, *line.Worktree)
	}
	return hint
}

// addsSession says how add makes a workspace session for a worktree
// with no home: by its branch, which a detached worktree has to have
// checked out first, on a host this machine's config gives the
// directories add needs, for a repository that config lists, which
// --repo takes, with an agent in that config for add to start, and
// last.json readable JSON, which add reads before it picks the host.
// It names every one of these the worktree lacks, not only the first.
func addsSession(cfg config.Config, h config.Host, w protocol.Worktree) string {
	var needs []string
	if w.Branch == "" {
		needs = append(needs, "a branch is checked out in "+tmux.Printable(w.Root))
	}
	if !h.CanAdd() {
		needs = append(needs, "host "+h.Name+" has repos and worktrees directories in the config")
	}
	if _, ok := cfg.RepoBySource(w.Source); w.Source != "" && !ok {
		needs = append(needs, w.Source+" is a repository in the config")
	}
	if len(cfg.Agents) == 0 {
		needs = append(needs, "an agent is in the config")
	}
	last, err := home.ReadLast()
	if err != nil {
		needs = append(needs, home.LastPath()+" is readable JSON")
	}
	switch n := len(needs); {
	case n == 0:
		return addCommand(cfg, h, w, last) + " makes one"
	case n > 2:
		// A list: the directories' own "and" would run into the joins.
		needs[n-1] = "and " + needs[n-1]
		return "laatmux add makes one once " + strings.Join(needs, ", ")
	}
	return "laatmux add makes one once " + strings.Join(needs, " and ")
}

// shell opens the shell window in the selected workspace, creating the
// workspace session first when the row has none, and jumps to it.
func (d *dash) shell(m *view.Model) bool {
	r := m.Selection()
	if r == nil {
		return false
	}
	row, err := shellRow(m, *r)
	if err != nil {
		m.Message = err.Error()
		return false
	}
	l, err := d.localFor(row)
	if err != nil {
		m.Message = err.Error()
		return false
	}
	// localFor has put the configured name on the session, by its
	// environment when the tag named a renamed host; one it could not
	// name is not the local host, as the CLI has it.
	h, ok := d.cfg.Find(l.Host)
	switch {
	case l.Host == "":
		m.Message = fmt.Sprintf("workspace session %s carries no host tag, and no connected host answers as its environment", l.Name)
		return false
	case !ok:
		m.Message = fmt.Sprintf("workspace session %s is on host %q, which is not configured", l.Name, l.Host)
		return false
	}
	if err := command.Shell(d.ctx, h, l); err != nil {
		m.Message = err.Error()
		return false
	}
	if err := switchTo(d.ctx, l.Name); err != nil {
		m.Message = err.Error()
		return false
	}
	return d.exitOnJump
}

// shellRow is the row whose workspace session the shell opens: an
// agent's tile, an agent, a pane or a run goes by the line holding it,
// whatever local session it has of its own, as z does: an agent
// observed in a window of another worktree's workspace session has that
// session as its own, and the shell belongs to its worktree's. A
// managed agent of no worktree in a line's home session goes by that
// line, whose workspace session its pane jump lands in. The line's jump
// agent is what the lost-home case counts on. The tile of a
// task standing for a listed worktree goes by the task line holding the
// worktree, the newest standing task's, which carries the same session;
// a task's row by the task's own rules for its session.
func shellRow(m *view.Model, row rows.Row) (rows.Row, error) {
	if l := ownerLine(m, row); l != nil {
		row = *l
	}
	if row.Pending != nil {
		return pendingTarget(row)
	}
	return row, nil
}

// ownerLine is the line holding a tile, an agent, a pane or a run: the
// one holding its worktree's children; for a managed agent of no
// worktree, the line whose workspace session attaches to the agent's
// session, a worktree's whose home it is or, with the home lost, whose
// root agent is in it, or the task line holding the add's agent before
// the host lists the worktree. Nil for a line and for a row no line
// holds.
func ownerLine(m *view.Model, row rows.Row) *rows.Row {
	switch {
	case row.Kind == rows.KindWorktree || row.Kind == rows.KindTask:
		return nil
	case row.Worktree != nil:
		return m.OwnerLine(row.Worktree.ID)
	case row.Pending == nil && row.Agent != nil && row.Agent.Server == protocol.ServerLaatmux:
		// As the pane jump routes it, so z and S act on the workspace
		// session enter lands in. Only on the managed server: an
		// observed session of the same name is not the line's.
		return m.LineFor(row.Host, row.Agent.Session)
	}
	return nil
}

// localFor is the row's workspace session, made from the worktree
// record when it does not exist yet. An existing session is routed by
// the host that answers for its key's environment id now, not by the
// host tag it was made with, which a renamed host leaves behind, and
// not by the row's host: an observed agent on this machine's default
// server sits in a local window of a workspace whose worktree may be
// on another host, and the shell belongs where the worktree is.
func (d *dash) localFor(r rows.Row) (protocol.Session, error) {
	if r.Orphaned {
		return protocol.Session{}, errors.New(r.Name + ": its worktree is gone")
	}
	if r.Local != nil && r.Local.Workspace() {
		l := *r.Local
		env, _ := protocol.SplitSessionKey(l.Key)
		if name := d.st.HostOf(env); name != "" {
			l.Host = name
		}
		return l, nil
	}
	spec, err := localSpec(d.cfg, r)
	if err != nil {
		return protocol.Session{}, err
	}
	name, _, err := workspace.Ensure(d.ctx, spec)
	if err != nil {
		return protocol.Session{}, err
	}
	return protocol.Session{Name: name, Key: spec.Key, Host: spec.Host.Name, Source: spec.Source, Branch: spec.Branch}, nil
}

// localSpec is the workspace session a row without one gets for its
// shell: the one the row's own jump makes, through the worktree's root
// agent when the home is lost; a row whose jump is no workspace
// session, a switch on this machine's default server or a plain
// attachment, has none.
func localSpec(cfg config.Config, r rows.Row) (workspace.Spec, error) {
	if r.Worktree == nil {
		return workspace.Spec{}, errors.New(r.Name + ": not a workspace")
	}
	h, ok := cfg.Find(r.Host)
	if !ok {
		return workspace.Spec{}, fmt.Errorf("unknown host %q", r.Host)
	}
	spec, session, err := rowSpec(cfg, h, r)
	if err != nil || session != "" || spec.Key == "" {
		return workspace.Spec{}, errors.New(r.Name + ": not a workspace")
	}
	return spec, nil
}
