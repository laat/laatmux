package command

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/laat/laatmux/internal/client"
	"github.com/laat/laatmux/internal/config"
	"github.com/laat/laatmux/internal/home"
	"github.com/laat/laatmux/internal/protocol"
	"github.com/laat/laatmux/internal/workspace"
)

// Add is one add: a worktree for the branch on the host, an agent
// started in it, and the local workspace session opened. The inputs are
// resolved by the caller; see the CLI's flags and the dashboard's
// pickers.
type Add struct {
	Host   config.Host
	Repo   config.Repo
	Branch string
	// Agent is the configured agent to start; Cmd, when set, is the
	// command instead and Agent is "".
	Agent string
	Cmd   []string
	// ID is the command id; "" picks a fresh one. A caller that retries
	// keeps the id so the daemon resumes rather than restarts.
	ID string
	// Prompt is what the agent is started with, delivered by the host
	// as its config says; Generated says Branch is a proposal for the
	// host to make unique. Either needs the host's task capability.
	// SubmittedAt is when the add was submitted, kept across resends;
	// zero means now, and it is set on the first send.
	Prompt      string
	Generated   bool
	SubmittedAt time.Time
}

// Added is what an add left behind.
type Added struct {
	Done    bool   // the host's add succeeded; Root and Managed are there
	Root    string // the worktree root on the host
	Branch  string // the branch used: the one allocated for a generated name
	Managed string // the managed session on the host
	Session string // the local workspace session
	Created bool   // the local session was made rather than found
	// Prompt is the delivery state of the prompt, one of the protocol's
	// Delivery constants, with Reason when it is not delivered or
	// unknown. The add succeeded whatever it says; the worktree and the
	// agent are there, and the state is what the user reads.
	Prompt string
	Reason string
	// Sent is that the add was written to a daemon, Answered that a
	// result came back, and Stage the stage a failed one names. Sent
	// without an answer is a lost result, of which nothing is known;
	// not sent is a refusal before the host, and the prompt went
	// nowhere.
	Sent     bool
	Answered bool
	Stage    string
}

// Complete reports whether nothing about the add needs the user: it
// succeeded and its prompt is delivered, or there was none.
func (a Added) Complete() bool {
	return a.Done && (a.Prompt == protocol.DeliveryDelivered || a.Prompt == protocol.DeliveryNone || a.Prompt == "")
}

// Run sends the add to the host, following its progress through r,
// then records the host and agent as the repository's last used and
// makes sure the local workspace session exists. A failed stage is a
// *StageError. When the host's side succeeded and the local steps then
// failed, the result carries the root and managed session with the
// error, so the caller can say what exists.
func (a Add) Run(ctx context.Context, r Reporter) (Added, error) {
	if a.Host.Name == "" || a.Repo.Source == "" || a.Branch == "" {
		return Added{}, errors.New("add needs a host, a repository and a branch")
	}
	id := a.ID
	if id == "" {
		id = ID("add")
	}
	req := a.Request(id)
	hello, res, err := stream(ctx, a.Host.Host, a.Needs(), req, r, streamOpts{restart: true})
	var ns *NotSent
	out := Added{Done: res.OK, Root: res.Root, Branch: res.Branch, Managed: res.Session, Prompt: res.Prompt, Reason: res.Error, Sent: !errors.As(err, &ns), Answered: res.Type == protocol.TypeResult, Stage: res.Stage}
	if out.Branch == "" {
		out.Branch = a.Branch
	}
	if err != nil {
		// What the host said stays with the error: a launch that
		// failed after the agent may have started reports the delivery
		// unknown, and the caller must not lose that to the failure.
		if out.Prompt == "" || out.Prompt == protocol.DeliveryNone {
			out.Reason = ""
		}
		return out, failed("add", res, err)
	}
	if err := home.UpdateLast(func(l *home.Last) {
		cur := l.Get(a.Repo.Source)
		cur.Host = a.Host.Name
		if a.Agent != "" {
			cur.Agent = a.Agent
		}
		l.Set(a.Repo.Source, cur)
	}); err != nil {
		return out, err
	}
	out.Session, out.Created, err = workspace.Ensure(ctx, workspace.Spec{
		Host: a.Host.Host, Managed: res.Session,
		Name:   workspace.SessionName(a.Host.Name, a.Repo.Name, out.Branch),
		Key:    workspace.Key(hello.EnvironmentID, res.Root),
		Source: a.Repo.Source, Branch: out.Branch,
	})
	return out, err
}

// Request is the add message under id, as the daemon takes it. The
// submission time is the first send's, kept on every resend, so the
// host can judge the add's age against its journal and the sender
// against its lifetime.
func (a Add) Request(id string) protocol.Message {
	submitted := a.SubmittedAt
	if submitted.IsZero() {
		submitted = time.Now()
	}
	return protocol.Message{
		Type: protocol.TypeAdd, ID: id, Repo: a.Repo.Source, Branch: a.Branch, AgentName: a.Agent, Cmd: a.Cmd,
		Prompt: a.Prompt, Generated: a.Generated, SubmittedAt: submitted,
	}
}

// Needs is the capabilities the host's daemon must have for this add:
// add, and task when a prompt or a generated branch is involved.
func (a Add) Needs() []string {
	if a.Prompt != "" || a.Generated {
		return []string{protocol.CapAdd, protocol.CapTask}
	}
	return []string{protocol.CapAdd}
}

// Deliver is one delivery attempt of a pending prompt to the agent an
// add started on the host: the prompt message under the add's id with
// the attempt number, which the host's journal serializes and answers
// from the record when it has seen the number before.
type Deliver struct {
	Host    config.Host
	ID      string // the add's command id
	Attempt int    // from 1, one more than the last the host has
	Prompt  string
	// Environment, when set, is the environment id the host must
	// answer as.
	Environment string
}

// Run sends the attempt and returns the delivery state with its
// reason. A refusal, recovery expired say, is an error.
func (p Deliver) Run(ctx context.Context, r Reporter) (state, reason string, err error) {
	if p.Host.Name == "" || p.ID == "" || p.Attempt < 1 || p.Prompt == "" {
		return "", "", errors.New("deliver needs a host, an id, an attempt number and the prompt")
	}
	req := protocol.Message{Type: protocol.TypePrompt, ID: p.ID, Attempt: p.Attempt, Prompt: p.Prompt}
	_, res, err := stream(ctx, p.Host.Host, []string{protocol.CapTask}, req, r, streamOpts{restart: true, environment: p.Environment, attempt: p.Attempt})
	if err != nil {
		return "", "", failed("prompt", res, err)
	}
	return res.Prompt, res.Error, nil
}

// Describe is the one line that says what the add is: "add
// <repo>/<branch> on <host> with <agent>".
func (a Add) Describe() string {
	s := fmt.Sprintf("add %s/%s on %s", a.Repo.Name, a.Branch, a.Host.Name)
	if a.Generated {
		s += " (or the next free name)"
	}
	if a.Agent != "" {
		s += " with " + a.Agent
	} else if len(a.Cmd) > 0 {
		s += " running " + strings.Join(a.Cmd, " ")
	}
	if a.Prompt != "" {
		s += " and a prompt"
	}
	return s
}

// Submit hands the add to this machine's daemon, which runs it in the
// background against the host: the daemon writes the task to disk and
// answers accepted, and the add's progress and outcome are a pending
// record in the merged stream from then on. The repository's last-used
// host and agent are recorded here, at submit, since the submit is the
// choice. A daemon without the relay capability is an error saying so,
// not a foreground add in disguise.
func (a Add) Submit(ctx context.Context) (string, error) {
	if a.Host.Name == "" || a.Repo.Source == "" || a.Branch == "" {
		return "", errors.New("add needs a host, a repository and a branch")
	}
	id := a.ID
	if id == "" {
		id = ID("add")
	}
	c, err := client.Dial(ctx, client.Host{Name: "local"})
	if err != nil {
		return "", err
	}
	defer c.Close()
	if !protocol.Has(c.Hello.Capabilities, protocol.CapRelay) {
		return "", fmt.Errorf("the local daemon %s has no relay capability; the add can only run in the foreground", c.Hello.Version)
	}
	req := a.Request(id)
	req.Relay, req.Name = a.Host.Name, a.Repo.Name
	// The id is this submit's whatever happens: the daemon accepts it
	// again and starts it if it is not running, so a lost answer is
	// asked for again on a fresh connection, and an error after that
	// still names the id the daemon may hold.
	if res, err := c.Request(ctx, req); err != nil {
		if res.Type != "" || ctx.Err() != nil {
			return "", err
		}
		c.Close()
		c, err = client.Dial(ctx, client.Host{Name: "local"})
		if err != nil {
			return id, fmt.Errorf("%s: the answer was lost and the daemon could not be reached again; laatmux tasks says whether it holds %s", err, id)
		}
		defer c.Close()
		if _, err := c.Request(ctx, req); err != nil {
			return id, err
		}
	}
	err = home.UpdateLast(func(l *home.Last) {
		cur := l.Get(a.Repo.Source)
		cur.Host = a.Host.Name
		if a.Agent != "" {
			cur.Agent = a.Agent
		}
		l.Set(a.Repo.Source, cur)
	})
	return id, err
}

// Dismiss drops a pending record that needs the user from this
// machine's daemon.
func Dismiss(ctx context.Context, id string) error {
	c, err := client.Dial(ctx, client.Host{Name: "local"})
	if err != nil {
		return err
	}
	defer c.Close()
	if !protocol.Has(c.Hello.Capabilities, protocol.CapRelay) {
		return fmt.Errorf("the local daemon %s has no relay capability", c.Hello.Version)
	}
	_, err = c.Request(ctx, protocol.Message{Type: protocol.TypeDismiss, ID: id})
	return err
}

// DismissAt asks this machine's daemon, when one is running, to drop
// the tasks at a worktree rm removed. Best effort: a daemon that is not
// running holds no task in memory, and marks one gone once its host's
// listing lacks the worktree; one without the relay has none.
func DismissAt(ctx context.Context, environmentID, root string) error {
	nc, err := client.DialLocal(ctx, false)
	if err != nil {
		return nil
	}
	c, err := client.Connect(ctx, client.Host{Name: "local"}, nc, nc, func() { nc.Close() })
	if err != nil {
		return err
	}
	defer c.Close()
	if !protocol.Has(c.Hello.Capabilities, protocol.CapRelay) {
		return nil
	}
	_, err = c.Request(ctx, protocol.Message{Type: protocol.TypeDismiss, EnvironmentID: environmentID, Root: root})
	return err
}

// DeliverPending asks this machine's daemon to deliver a pending
// record's prompt now, as its next attempt, and returns the delivery
// state with its reason.
func DeliverPending(ctx context.Context, id string) (state, reason string, err error) {
	c, err := client.Dial(ctx, client.Host{Name: "local"})
	if err != nil {
		return "", "", err
	}
	defer c.Close()
	if !protocol.Has(c.Hello.Capabilities, protocol.CapRelay) {
		return "", "", fmt.Errorf("the local daemon %s has no relay capability", c.Hello.Version)
	}
	res, err := c.Request(ctx, protocol.Message{Type: protocol.TypePrompt, ID: id})
	if err != nil {
		return "", "", err
	}
	return res.Prompt, res.Error, nil
}
