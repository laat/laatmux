package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"

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
	// relay is the local daemon's relay capability: with it a submit
	// hands the add to the daemon and ends the view; without it the
	// add runs in the foreground with its log, as before.
	relay bool
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
	// recover is the notice to show once the log has ended, delivered
	// or not: a prompt that did not reach the agent, with its text;
	// recovered is the message the notice covers, put back after it.
	recover   *view.Notice
	recovered string
	// last is what the foreground add left, for compose to jump to.
	last command.Added
	// switcher replaces the tmux switch, for tests, jumper a row's jump,
	// and refocus the return of focus after a click in the sidebar.
	switcher func(session string) error
	jumper   func(r rows.Row) error
	refocus  func()
	// quitting is that the notice up is the last thing shown: its
	// dismissal ends the view.
	quitting bool
}

// running is a command under way: its log, and what to do when it ends.
type running struct {
	log  *view.Log
	done func(m *view.Model) (exit bool)
	// prompt is the prompt of an add under way, kept in a file should
	// the wait for it be quit, since the view ends with nothing shown;
	// quit is set then, so the add, should it end before the view does,
	// keeps no second copy.
	prompt string
	quit   *atomic.Bool
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
			if r := m.Selection(); r != nil && r.Pending != nil {
				d.askDismiss(m, *r)
				break
			}
			d.askRm(m, a.Key.Rune == 'X')
		case 'p':
			d.deliverPrompt(m)
		case 's':
			d.settle(m)
		case 'S':
			return d.shell(m)
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

// jump is the jump command's logic on a row; jumpAction is the view's
// way to it, with the focus handled.
func (d *dash) jump(m *view.Model, r rows.Row) bool {
	exit, _ := d.jumpRow(m, r)
	return exit
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
			if d.run != nil && d.run.prompt != "" {
				// The view ends with the add's outcome unknown, so the
				// prompt is kept and said to be, in a notice that ends
				// the view when dismissed: a message would never be
				// drawn.
				d.run.quit.Store(true)
				m.Overlay, d.run, d.quitting = quitNotice(d.run.prompt), nil, true
				return false
			}
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
			// keeping in sight. A prompt to recover comes up over it.
			m.Message = err.Error()
			if d.recover != nil {
				m.Overlay, d.recover, d.recovered = d.recover, nil, m.Message
			}
			return false
		}
		return run.done(m)
	case *view.Notice:
		// The key that dismissed it cleared the message it covered. The
		// jump the notice held off is made now, when there is a session.
		m.Overlay = nil
		if d.quitting {
			return true
		}
		m.Message, d.recovered = d.recovered, ""
		if d.last.Session != "" && m.Message == "" {
			session := d.last.Session
			d.last.Session = ""
			if err := d.jumpTo(session); err != nil {
				m.Message = err.Error()
				return false
			}
			return d.exitOnJump
		}
		return false
	}
	return false
}

// jumpTo switches the client to the session, through switcher when a
// test set one.
func (d *dash) jumpTo(session string) error {
	if d.switcher != nil {
		return d.switcher(session)
	}
	return switchTo(d.ctx, session)
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
	// update of it would fail after the daemon has the task.
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
	preRepo, preHost, branch := "", "", ""
	if r := m.Selection(); r != nil && r.Worktree != nil && r.Worktree.Session == "" && !r.Stale {
		preRepo, preHost, branch = localRepoArg(d.cfg, *r.Worktree), r.Host, r.Worktree.Branch
	} else if repo, err := resolveRepo(d.ctx, d.cfg, ""); err == nil {
		preRepo = repo.Name
	}
	form := buildForm(d.cfg, f, last, preRepo, preHost, branch, d.st.hostCaps)
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
		if r.Name == preRepo || config.SameSource(r.Source, preRepo) {
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

// submitForm runs what the form asked for: with the relay, the add is
// handed to the local daemon and the view ends once it is accepted; a
// refusal, a host whose daemon does not support tasks say, keeps the
// form up with the error, its text intact, while an error after the
// daemon may hold the task ends the view with the id, so nothing is
// submitted twice. Without the relay, the add runs in the foreground
// with its log, and the new workspace session is jumped to.
func (d *dash) submitForm(m *view.Model, f *addForm, o *view.Form) bool {
	add := command.Add{
		Host: f.hosts[o.Chips[1].Selected], Repo: f.repos[o.Chips[0].Selected], Copy: d.cfg.Copy, Agent: f.agents[o.Chips[2].Selected],
		Branch: strings.TrimSpace(o.Branch()), Prompt: o.Prompt(), Generated: o.Generated(),
	}
	if d.relay {
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
			// The daemon may hold the task: the form goes, so nothing
			// is submitted twice, and the view stays with the message,
			// which a popup closing would take with it.
			d.add = nil
			m.Message = "submitted " + id + "; " + err.Error() + "; laatmux tasks says whether the daemon holds it"
			return false
		}
		d.add = nil
		m.Message = "accepted " + id
		return true
	}
	d.add = nil
	d.runAdd(m, add)
	return false
}

// runAdd runs the add in the foreground with its log; on success the
// new workspace session is jumped to.
func (d *dash) runAdd(m *view.Model, add command.Add) {
	var res command.Added
	quit := new(atomic.Bool)
	defer func() {
		if d.run != nil {
			d.run.prompt, d.run.quit = add.Prompt, quit
		}
	}()
	d.start(m, add.Describe(), func(r command.Reporter) error {
		var err error
		res, err = add.Run(d.ctx, r)
		d.last = res
		// A prompt that did not reach the agent, or may not have, is
		// shown with its text whatever else happened, and kept in a
		// file, since the foreground path keeps none of it otherwise.
		if d.recover = undelivered(add, res); d.recover != nil && !quit.Load() {
			if path, err := keepPrompt(command.ID("prompt"), add.Prompt); err == nil {
				// The path on a line of its own, to be copied.
				d.recover.Lines = append([]string{"kept in", path, ""}, d.recover.Lines...)
				d.recover.Verbatim += 3
			} else {
				d.recover.Lines = append([]string{"not kept in a file: " + err.Error(), ""}, d.recover.Lines...)
				d.recover.Verbatim += 2
			}
		}
		if err != nil && res.Done {
			// The host's side is done; what failed is local, and the
			// message must say the worktree and agent exist.
			return fmt.Errorf("%s/%s ready on %s (%s); local session: %w", add.Repo.Name, res.Branch, add.Host.Name, res.Root, err)
		}
		return err
	}, func(m *view.Model) bool {
		// The delivery state is what the user reads: the notice stays
		// until dismissed; a jump would leave it behind.
		if d.recover != nil {
			m.Overlay, d.recover = d.recover, nil
			return false
		}
		if err := switchTo(d.ctx, res.Session); err != nil {
			m.Message = err.Error()
			return false
		}
		return d.exitOnJump
	})
}

// undelivered is the notice a foreground add whose prompt did not
// reach the agent, or may not have, leaves up until dismissed: the
// state, the reason, the session when there is one, and the prompt
// itself, wrapped and scrollable, to be copied into the agent, since
// the foreground path keeps no file of it. nil when there is nothing
// to recover: no prompt, or one delivered.
func undelivered(add command.Add, res command.Added) *view.Notice {
	if add.Prompt == "" || res.Prompt == protocol.DeliveryDelivered || res.Prompt == protocol.DeliveryNone {
		return nil
	}
	var lines []string
	switch {
	case res.Prompt == "" && !res.Sent:
		// Refused before any daemon had it.
		lines = []string{"the add was refused before it reached the host; the prompt was not sent", ""}
	case res.Prompt == "" && !res.Answered:
		// No result came: the host may have taken the add and the
		// agent may have the prompt.
		lines = []string{"outcome unknown: no result came from the host; the agent may have the prompt", ""}
	case res.Prompt == "" && res.Stage != "" && res.Stage != protocol.StageAgent:
		// A failure before the agent stage: the prompt was never sent.
		lines = []string{"the add failed at " + res.Stage + ", before the prompt was sent", ""}
	case res.Prompt == "":
		lines = []string{"the add failed; whether the prompt was sent is unknown", ""}
	case res.Reason != "":
		lines = []string{"prompt " + res.Prompt + ": " + res.Reason, ""}
	default:
		lines = []string{"prompt " + res.Prompt, ""}
	}
	sure := res.Prompt == protocol.DeliveryNotDelivered
	switch {
	case res.Managed != "" && sure:
		lines = append(lines, "session "+res.Managed+" is running in "+res.Root+" without it. The prompt was:")
	case res.Managed != "":
		lines = append(lines, "session "+res.Managed+" is running in "+res.Root+"; whether it has the prompt is unknown. The prompt was:")
	case res.Root != "" && sure:
		lines = append(lines, "the worktree "+res.Root+" is there without an agent. The prompt was:")
	case res.Root != "":
		lines = append(lines, "the worktree "+res.Root+" is there. The prompt was:")
	default:
		lines = append(lines, "The prompt was:")
	}
	lines = append(lines, "")
	lines = append(lines, strings.Split(add.Prompt, "\n")...)
	n := view.NewNotice(add.Describe(), lines, "enter or esc returns")
	n.Verbatim = len(lines) - strings.Count(add.Prompt, "\n") - 1
	return n
}

// quitNotice is what a Ctrl-C on a foreground add leaves: the add may
// run on or may never have been sent, since the view's end cancels
// it, so the prompt is kept in a file and named, or shown when it
// could not be.
func quitNotice(prompt string) *view.Notice {
	lines := []string{"the add may run on, or may never have been sent; laatmux ls says which", ""}
	verbatim := 0
	if path, err := keepPrompt(command.ID("prompt"), prompt); err == nil {
		lines = append(lines, "the prompt is kept in", path)
	} else {
		lines = append(lines, "the prompt could not be kept in a file: "+err.Error(), "", "The prompt was:", "")
		verbatim = len(lines)
		lines = append(lines, strings.Split(prompt, "\n")...)
	}
	n := view.NewNotice("add interrupted", lines, "enter or esc quits")
	n.Verbatim, n.Final = verbatim, true
	return n
}

// keepPrompt writes an undelivered prompt to a file of its own under
// the state directory, readable by the user alone, and returns the
// path: the notice is copied from by hand, lossily, and the popup that
// shows it closes. The file is the user's to delete.
func keepPrompt(id, prompt string) (string, error) {
	dir := filepath.Join(home.Dir(), "undelivered")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	path := filepath.Join(dir, id+".txt")
	if err := os.WriteFile(path, []byte(prompt), 0o600); err != nil {
		return "", err
	}
	return path, nil
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
	m.Ask(fmt.Sprintf("dismiss %s on %s (%s)? y/n", r.Name, r.Host, r.State()), "dismiss")
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
	what := p.Repo + "/" + p.Branch + " on " + p.Host
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
		if repo, ok := recordRepo(d.cfg, r.Worktree.Source); ok {
			rm.Repo = repo
		} else if repo, ok := d.cfg.RepoByName(r.Worktree.Repo); ok && r.Worktree.Source == "" {
			rm.Repo = repo
		}
		if rm.Repo.Source == "" {
			rm.Branch = ""
		}
	case r.Stale:
		rm.Environment, rm.Root = workspace.SplitKey(r.Local.Key)
		if repo, ok := recordRepo(d.cfg, r.Local.Source); ok && r.Local.Branch != "" {
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
	if r.Pending != nil {
		// The row is the task's until it hands over: s settles the
		// worktree row it becomes.
		m.Message = r.Name + ": a pending task; s settles its worktree row once it hands over"
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
	row := *r
	if row.Pending != nil {
		// A task's row goes by the task's own rules for its session.
		var err error
		if row, err = pendingTarget(row); err != nil {
			m.Message = err.Error()
			return false
		}
	}
	l, err := d.localFor(row)
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
