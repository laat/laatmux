package command

import (
	"context"
	"errors"
	"net"
	"os"
	"strings"
	"sync"
	"testing"

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
	_, _, err := stream(context.Background(), host, protocol.CapRm, req, Discard{}, streamOpts{restart: true, environment: "env"})
	if err == nil || !strings.Contains(err.Error(), "answers as environment other, not env") {
		t.Fatalf("mismatch on the first connection: %v", err)
	}
	if got := f.commands(); len(got) != 0 {
		t.Fatalf("command sent to the wrong environment: %+v", got)
	}

	f = startFake(t, 1, protocol.Message{EnvironmentID: "env", Capabilities: caps}, protocol.Message{EnvironmentID: "other", Capabilities: caps})
	_, _, err = stream(context.Background(), host, protocol.CapRm, req, Discard{}, streamOpts{restart: true, environment: "env"})
	if err == nil || !strings.Contains(err.Error(), "answers as environment other, not env") {
		t.Fatalf("mismatch on the reconnect: %v", err)
	}
	if got := f.commands(); len(got) != 1 || got[0].Type != protocol.TypeRm {
		t.Fatalf("after the reconnect: %+v", got)
	}

	f = startFake(t, 1, protocol.Message{EnvironmentID: "env", Capabilities: caps})
	hello, res, err := stream(context.Background(), host, protocol.CapRm, req, Discard{}, streamOpts{restart: true, environment: "env"})
	if err != nil || !res.OK || hello.EnvironmentID != "env" {
		t.Fatalf("same environment throughout: %+v %+v %v", hello, res, err)
	}
	if got := f.commands(); len(got) != 2 || got[1].Type != protocol.TypeFollow {
		t.Fatalf("reconnect did not follow: %+v", got)
	}
}
