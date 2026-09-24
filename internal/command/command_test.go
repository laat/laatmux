package command

import (
	"context"
	"errors"
	"net"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/laat/laatmux/internal/client"
	"github.com/laat/laatmux/internal/config"
	"github.com/laat/laatmux/internal/home"
	"github.com/laat/laatmux/internal/protocol"
	"github.com/laat/laatmux/internal/workspace"
)

// Progress replayed after a reconnect is passed on once: by position
// from a daemon that replays from the start, by number from one that
// replays from the mark, and a redial that lands on the other kind
// still filters.
func TestStreamDedupe(t *testing.T) {
	var got []string
	f := &progressFilter{fn: func(p protocol.Message) { got = append(got, p.Detail) }}
	for _, d := range []string{"a", "b"} {
		f.pass(protocol.Message{Detail: d})
	}
	f.reset() // reconnect: the daemon replays from the start
	for _, d := range []string{"a", "b", "c"} {
		f.pass(protocol.Message{Detail: d})
	}
	if strings.Join(got, "") != "abc" {
		t.Fatalf("positional: got %v", got)
	}

	got = nil
	f = &progressFilter{numbered: true, fn: func(p protocol.Message) { got = append(got, p.Detail) }}
	for i, d := range []string{"a", "b"} {
		f.pass(protocol.Message{N: uint64(i + 1), Detail: d})
	}
	f.reset() // reconnect: follow after 2, but a slow replay overlaps
	for i, d := range []string{"b", "c", "d"} {
		f.pass(protocol.Message{N: uint64(i + 2), Detail: d})
	}
	if strings.Join(got, "") != "abcd" || f.mark != 4 {
		t.Fatalf("numbered: got %v mark %d", got, f.mark)
	}
	// A gap counts as the lines it stands for: the mark moves to its n.
	f.pass(protocol.Message{N: 9, State: protocol.StateGap, Detail: "5 lines dropped"})
	f.pass(protocol.Message{N: 10, Detail: "e"})
	if strings.Join(got, "") != "abcd5 lines droppede" || f.mark != 10 {
		t.Fatalf("gap: got %v mark %d", got, f.mark)
	}
	// The next connection is an older daemon: it replays everything
	// unnumbered, and only what is past the count seen passes.
	f.numbered = false
	f.reset()
	for _, d := range []string{"a", "b", "c", "d", "5 lines dropped", "e", "f"} {
		f.pass(protocol.Message{Detail: d})
	}
	if strings.Join(got, "") != "abcd5 lines droppedef" {
		t.Fatalf("downgrade: got %v", got)
	}
}

// A result that names a stage is a StageError saying so; a refusal
// without one carries the daemon's message alone; a transport failure
// is passed through.
func TestFailed(t *testing.T) {
	err := failed("add", protocol.Message{Type: protocol.TypeResult, Stage: "worktree", Error: "boom"}, errors.New("boom"))
	var se *StageError
	if !errors.As(err, &se) || se.Stage != "worktree" || err.Error() != "add failed at worktree: boom" {
		t.Errorf("stage: %v", err)
	}
	err = failed("rm", protocol.Message{Type: protocol.TypeResult, Error: "dirty"}, errors.New("dirty"))
	if !errors.As(err, &se) || se.Stage != "" || err.Error() != "dirty" {
		t.Errorf("refusal: %v", err)
	}
	lost := errors.New("connection lost")
	if err := failed("rm", protocol.Message{}, lost); err != lost {
		t.Errorf("transport: %v", err)
	}
}

// The root for a branch whose record is gone comes from the local
// session's tags, or from an untagged session by name, and never from
// a session tagged for another workspace or on another environment.
func TestRootOf(t *testing.T) {
	h := config.Host{Host: client.Host{Name: "vm", SSH: "vm"}}
	repo := config.Repo{Source: "git@x:o/proj.git", Name: "proj"}
	cases := []struct {
		name   string
		locals []workspace.Local
		want   string
	}{
		{"by tags", []workspace.Local{{Name: "other", Key: "env//r/x", Source: repo.Source, Branch: "x"}}, "/r/x"},
		{"by name untagged", []workspace.Local{{Name: "vm/proj/x", Key: "env//r/x"}}, "/r/x"},
		{"by name tagged for another", []workspace.Local{{Name: "vm/proj/x", Key: "env//r/y", Source: repo.Source, Branch: "y"}}, ""},
		{"other environment", []workspace.Local{{Name: "vm/proj/x", Key: "env2//r/x"}}, ""},
		{"none", nil, ""},
	}
	for _, c := range cases {
		if got := RootOf(c.locals, "env", h, repo, "x"); got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
}

func TestDescribe(t *testing.T) {
	a := Add{Host: config.Host{Host: client.Host{Name: "vm"}}, Repo: config.Repo{Name: "proj"}, Branch: "x", Agent: "claude"}
	if got := a.Describe(); got != "add proj/x on vm with claude" {
		t.Error(got)
	}
	a.Agent, a.Cmd = "", []string{"sh", "-c", "true"}
	if got := a.Describe(); got != "add proj/x on vm running sh -c true" {
		t.Error(got)
	}
	if got := (Rm{Root: "/r/x"}).Describe(); got != "/r/x" {
		t.Error(got)
	}
	r := Run{Host: config.Host{Host: client.Host{Name: "vm"}}, Repo: config.Repo{Name: "proj"}, Branch: "x", Root: "/r/x", Cmd: []string{"go", "test"}}
	if got := r.Describe(); got != "run go test in proj/x on vm" {
		t.Error(got)
	}
	r.Repo = config.Repo{}
	if got := r.Describe(); got != "run go test in /r/x on vm" {
		t.Error(got)
	}
}

// A cancelled run is told apart from other refusals.
func TestCancelled(t *testing.T) {
	if !Cancelled(&StageError{Command: "run", Msg: protocol.ErrCancelled}) {
		t.Error("cancelled not seen")
	}
	if Cancelled(&StageError{Command: "run", Msg: "no such worktree"}) || Cancelled(errors.New(protocol.ErrCancelled)) {
		t.Error("false positive")
	}
}

// fakeDaemon answers as the local daemon: the runtime file names it, its
// hello carries the environment and capabilities given per connection,
// and it records the commands it gets. drop ends a connection after the
// command instead of answering, which is a transport failure to the
// client.
type fakeDaemon struct {
	mu       sync.Mutex
	hellos   []protocol.Message // one per connection, in order; the last repeats
	got      []protocol.Message
	drop     int // connections to drop after the command, from the first
	conns    int
	listener net.Listener
	// answer, when set, is the reply to a command; the default is an ok
	// result with a root.
	answer func(protocol.Message) protocol.Message
}

func startFake(t *testing.T, drop int, hellos ...protocol.Message) *fakeDaemon {
	t.Helper()
	t.Setenv("LAATMUX_HOME", t.TempDir())
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	if err := home.WriteRuntime(home.Runtime{Address: "tcp:" + ln.Addr().String(), PID: os.Getpid(), Version: "fake"}); err != nil {
		t.Fatal(err)
	}
	f := &fakeDaemon{hellos: hellos, drop: drop, listener: ln}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			f.mu.Lock()
			n := f.conns
			f.conns++
			hello := f.hellos[min(n, len(f.hellos)-1)]
			drop := n < f.drop
			f.mu.Unlock()
			go func() {
				defer c.Close()
				pc := protocol.NewConn(c)
				hello.Type, hello.Protocol = protocol.TypeHello, protocol.Version
				pc.Write(hello)
				for {
					m, err := pc.Read()
					if err != nil {
						return
					}
					if m.Type == protocol.TypeHello {
						continue
					}
					f.mu.Lock()
					f.got = append(f.got, m)
					f.mu.Unlock()
					if drop {
						return
					}
					if f.answer != nil {
						pc.Write(f.answer(m))
						continue
					}
					pc.Write(protocol.Message{Type: protocol.TypeResult, ID: m.ID, OK: true, Root: "/r/x"})
				}
			}()
		}
	}()
	return f
}

func (f *fakeDaemon) commands() []protocol.Message {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]protocol.Message(nil), f.got...)
}

// The environment a command was resolved for is held on every
// connection: a daemon answering as another is refused before the
// command is sent, on the first connection and on a reconnect after a
// dropped one alike; the same environment throughout goes through.
func TestStreamHoldsEnvironment(t *testing.T) {
	caps := []string{protocol.CapStatus, protocol.CapRm, protocol.CapFollow}
	req := protocol.Message{Type: protocol.TypeRm, ID: "r1", Root: "/r/x"}
	host := client.Host{Name: "local"}

	f := startFake(t, 0, protocol.Message{EnvironmentID: "other", Capabilities: caps})
	_, _, err := stream(context.Background(), host, []string{protocol.CapRm}, req, Discard{}, streamOpts{restart: true, environment: "env"})
	if err == nil || !strings.Contains(err.Error(), "answers as environment other, not env") {
		t.Fatalf("mismatch on the first connection: %v", err)
	}
	if got := f.commands(); len(got) != 0 {
		t.Fatalf("command sent to the wrong environment: %+v", got)
	}

	f = startFake(t, 1, protocol.Message{EnvironmentID: "env", Capabilities: caps}, protocol.Message{EnvironmentID: "other", Capabilities: caps})
	_, _, err = stream(context.Background(), host, []string{protocol.CapRm}, req, Discard{}, streamOpts{restart: true, environment: "env"})
	if err == nil || !strings.Contains(err.Error(), "answers as environment other, not env") {
		t.Fatalf("mismatch on the reconnect: %v", err)
	}
	if got := f.commands(); len(got) != 1 || got[0].Type != protocol.TypeRm {
		t.Fatalf("after the reconnect: %+v", got)
	}

	f = startFake(t, 1, protocol.Message{EnvironmentID: "env", Capabilities: caps})
	hello, res, err := stream(context.Background(), host, []string{protocol.CapRm}, req, Discard{}, streamOpts{restart: true, environment: "env"})
	if err != nil || !res.OK || hello.EnvironmentID != "env" {
		t.Fatalf("same environment throughout: %+v %+v %v", hello, res, err)
	}
	if got := f.commands(); len(got) != 2 || got[1].Type != protocol.TypeFollow {
		t.Fatalf("reconnect did not follow: %+v", got)
	}
}

// A follow answered interrupted, by a daemon whose journal knows the
// add and that died in it, resends the add under its id; the sender
// lifetime is enforced before any send, so an add submitted more than
// seven days ago is outcome unknown without a connection.
func TestStreamResendsOnInterrupted(t *testing.T) {
	caps := []string{protocol.CapStatus, protocol.CapAdd, protocol.CapFollow, protocol.CapTask}
	host := client.Host{Name: "local"}
	f := startFake(t, 1, protocol.Message{EnvironmentID: "env", Capabilities: caps})
	f.answer = func(m protocol.Message) protocol.Message {
		if m.Type == protocol.TypeFollow {
			return protocol.Message{Type: protocol.TypeResult, ID: m.ID, Error: protocol.ErrInterrupted, Stage: protocol.StageWorktree}
		}
		return protocol.Message{Type: protocol.TypeResult, ID: m.ID, OK: true, Root: "/r/x", Branch: "task-2", Prompt: protocol.DeliveryDelivered}
	}
	add := Add{Host: config.Host{Host: host}, Repo: config.Repo{Source: "s", Name: "proj"}, Branch: "task", Generated: true, Prompt: "p", Agent: "claude"}
	req := add.Request("a1")
	if req.SubmittedAt.IsZero() || !req.Generated || req.Prompt != "p" {
		t.Fatalf("request %+v", req)
	}
	var notes []string
	rep := noter{fn: func(s string) { notes = append(notes, s) }}
	_, res, err := stream(context.Background(), host, add.Needs(), req, rep, streamOpts{restart: true})
	if err != nil || !res.OK || res.Branch != "task-2" {
		t.Fatalf("%+v %v", res, err)
	}
	got := f.commands()
	if len(got) != 3 || got[0].Type != protocol.TypeAdd || got[1].Type != protocol.TypeFollow || got[2].Type != protocol.TypeAdd || !got[2].SubmittedAt.Equal(got[0].SubmittedAt) {
		t.Fatalf("commands %+v", got)
	}
	if len(notes) == 0 || !strings.Contains(notes[len(notes)-1], "resume") {
		t.Fatalf("notes %q", notes)
	}
	// The lifetime: nothing is sent.
	f = startFake(t, 0, protocol.Message{EnvironmentID: "env", Capabilities: caps})
	old := add
	old.SubmittedAt = time.Now().Add(-8 * 24 * time.Hour)
	if _, _, err := stream(context.Background(), host, old.Needs(), old.Request("a2"), Discard{}, streamOpts{restart: true}); !errors.Is(err, ErrSubmissionExpired) {
		t.Fatalf("lifetime: %v", err)
	}
	if got := f.commands(); len(got) != 0 {
		t.Fatalf("sent past the lifetime: %+v", got)
	}
	// The lifetime bounds sends and resends, not follows: an add sent
	// in time is followed past it, and the resend the follow asks for
	// is what the lifetime refuses.
	was := SenderLifetime
	SenderLifetime = 300 * time.Millisecond
	defer func() { SenderLifetime = was }()
	f = startFake(t, 1, protocol.Message{EnvironmentID: "env", Capabilities: caps})
	f.answer = func(m protocol.Message) protocol.Message {
		time.Sleep(400 * time.Millisecond)
		return protocol.Message{Type: protocol.TypeResult, ID: m.ID, Error: protocol.ErrInterrupted, Stage: protocol.StageFetch}
	}
	if _, _, err := stream(context.Background(), host, add.Needs(), add.Request("a4"), Discard{}, streamOpts{restart: true}); !errors.Is(err, ErrSubmissionExpired) {
		t.Fatalf("resend past the lifetime: %v", err)
	}
	if got := f.commands(); len(got) != 2 || got[0].Type != protocol.TypeAdd || got[1].Type != protocol.TypeFollow {
		t.Fatalf("follow past the lifetime: %+v", got)
	}
	SenderLifetime = was

	// A prompt or a generated branch needs task; a daemon without it is
	// refused before the send.
	f = startFake(t, 0, protocol.Message{EnvironmentID: "env", Capabilities: []string{protocol.CapStatus, protocol.CapAdd, protocol.CapFollow}})
	if _, _, err := stream(context.Background(), host, add.Needs(), add.Request("a3"), Discard{}, streamOpts{restart: true}); err == nil || !strings.Contains(err.Error(), "does not support task") {
		t.Fatalf("needs task: %v", err)
	}
	if got := f.commands(); len(got) != 0 {
		t.Fatalf("sent without task: %+v", got)
	}
	if n := (Add{}).Needs(); len(n) != 1 || n[0] != protocol.CapAdd {
		t.Fatalf("needs %v", n)
	}
}

// Deliver sends the prompt message under the add's id with the attempt
// number, follows it with the number after a drop, and resends it when
// the follow is answered unknown attempt.
func TestDeliver(t *testing.T) {
	caps := []string{protocol.CapStatus, protocol.CapFollow, protocol.CapTask}
	host := client.Host{Name: "local"}
	f := startFake(t, 1, protocol.Message{EnvironmentID: "env", Capabilities: caps})
	f.answer = func(m protocol.Message) protocol.Message {
		if m.Type == protocol.TypeFollow {
			return protocol.Message{Type: protocol.TypeResult, ID: m.ID, Attempt: m.Attempt, Error: protocol.ErrUnknownAttempt}
		}
		return protocol.Message{Type: protocol.TypeResult, ID: m.ID, Attempt: m.Attempt, OK: true, Prompt: protocol.DeliveryNotDelivered, Error: "session replaced"}
	}
	d := Deliver{Host: config.Host{Host: host}, ID: "a1", Attempt: 2, Prompt: "p", Environment: "env"}
	state, reason, err := d.Run(context.Background(), Discard{})
	if err != nil || state != protocol.DeliveryNotDelivered || reason != "session replaced" {
		t.Fatalf("%s %s %v", state, reason, err)
	}
	got := f.commands()
	if len(got) != 3 || got[0].Type != protocol.TypePrompt || got[0].Attempt != 2 || got[0].Prompt != "p" || got[1].Type != protocol.TypeFollow || got[1].Attempt != 2 || got[2].Type != protocol.TypePrompt {
		t.Fatalf("commands %+v", got)
	}
	f = startFake(t, 0, protocol.Message{EnvironmentID: "env", Capabilities: caps})
	f.answer = func(m protocol.Message) protocol.Message {
		return protocol.Message{Type: protocol.TypeResult, ID: m.ID, Attempt: m.Attempt, Error: protocol.ErrRecoveryExpired}
	}
	if _, _, err := d.Run(context.Background(), Discard{}); err == nil || !strings.Contains(err.Error(), protocol.ErrRecoveryExpired) {
		t.Fatalf("refusal: %v", err)
	}
	if _, _, err := (Deliver{}).Run(context.Background(), Discard{}); err == nil {
		t.Fatal("empty deliver accepted")
	}
}

// noter is a Reporter that keeps the notes.
type noter struct{ fn func(string) }

func (noter) Progress(protocol.Message) {}
func (n noter) Note(s string)           { n.fn(s) }

// A host result that failed still carries what the host said: the
// delivery state of a launch that may have started the agent reaches
// the caller with the error, and no local session is made.
func TestAddKeepsHostOutcomeOnError(t *testing.T) {
	caps := []string{protocol.CapStatus, protocol.CapAdd, protocol.CapFollow, protocol.CapTask}
	host := client.Host{Name: "local"}
	f := startFake(t, 0, protocol.Message{EnvironmentID: "env", Capabilities: caps})
	f.answer = func(m protocol.Message) protocol.Message {
		return protocol.Message{Type: protocol.TypeResult, ID: m.ID, Stage: protocol.StageAgent, Error: "tmux: set-option failed", Root: "/r/x", Branch: "x", Prompt: protocol.DeliveryUnknown}
	}
	add := Add{Host: config.Host{Host: host}, Repo: config.Repo{Source: "s", Name: "proj"}, Branch: "x", Prompt: "p", Agent: "claude"}
	out, err := add.Run(context.Background(), Discard{})
	if err == nil || out.Prompt != protocol.DeliveryUnknown || out.Root != "/r/x" || out.Session != "" {
		t.Fatalf("%+v %v", out, err)
	}
	var se *StageError
	if !errors.As(err, &se) || se.Stage != protocol.StageAgent {
		t.Fatalf("error %v", err)
	}
	if out.Complete() || (Added{Done: true}).Complete() != true || (Added{Done: true, Prompt: protocol.DeliveryUnknown}).Complete() {
		t.Fatal("Complete")
	}
}

// Submit hands the add to the local daemon with the host named and is
// answered accepted; the repository's last-used host and agent are
// recorded at submit. A daemon without relay refuses before sending.
func TestSubmit(t *testing.T) {
	caps := []string{protocol.CapStatus, protocol.CapMerged, protocol.CapRelay}
	f := startFake(t, 0, protocol.Message{EnvironmentID: "env", Capabilities: caps})
	add := Add{Host: config.Host{Host: client.Host{Name: "vm", SSH: "vm"}}, Repo: config.Repo{Source: "s", Name: "proj"}, Branch: "task", Generated: true, Prompt: "p", Agent: "claude"}
	id, err := add.Submit(context.Background())
	if err != nil || !strings.HasPrefix(id, "add-") {
		t.Fatalf("%s %v", id, err)
	}
	got := f.commands()
	if len(got) != 1 || got[0].Type != protocol.TypeAdd || got[0].Relay != "vm" || got[0].Name != "proj" || got[0].Repo != "s" || got[0].Prompt != "p" || !got[0].Generated || got[0].SubmittedAt.IsZero() {
		t.Fatalf("commands %+v", got)
	}
	if l, _ := home.ReadLast(); l.Get("s").Host != "vm" || l.Get("s").Agent != "claude" {
		t.Fatalf("last %+v", l)
	}
	if err := Dismiss(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	f.answer = func(m protocol.Message) protocol.Message {
		return protocol.Message{Type: protocol.TypeResult, ID: m.ID, OK: true, Prompt: protocol.DeliveryDelivered, Attempt: 1}
	}
	if state, _, err := DeliverPending(context.Background(), id); err != nil || state != protocol.DeliveryDelivered {
		t.Fatalf("%s %v", state, err)
	}
	if got := f.commands(); len(got) != 3 || got[1].Type != protocol.TypeDismiss || got[2].Type != protocol.TypePrompt || got[2].Attempt != 0 {
		t.Fatalf("commands %+v", got)
	}
	// The answer lost: the same id is asked for again on a fresh
	// connection, and the daemon accepts it again.
	f = startFake(t, 1, protocol.Message{EnvironmentID: "env", Capabilities: caps})
	id2, err := add.Submit(context.Background())
	if err != nil || id2 == "" {
		t.Fatalf("%s %v", id2, err)
	}
	if got := f.commands(); len(got) != 2 || got[0].ID != id2 || got[1].ID != id2 || got[1].Type != protocol.TypeAdd {
		t.Fatalf("resubmit %+v", got)
	}
	f = startFake(t, 0, protocol.Message{EnvironmentID: "env", Capabilities: []string{protocol.CapStatus, protocol.CapMerged}})
	if _, err := add.Submit(context.Background()); err == nil || !strings.Contains(err.Error(), "no relay capability") {
		t.Fatalf("without relay: %v", err)
	}
	if got := f.commands(); len(got) != 0 {
		t.Fatalf("sent without relay: %+v", got)
	}
}
