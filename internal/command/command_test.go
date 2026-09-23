package command

import (
	"errors"
	"strings"
	"testing"

	"github.com/laat/laatmux/internal/client"
	"github.com/laat/laatmux/internal/config"
	"github.com/laat/laatmux/internal/protocol"
	"github.com/laat/laatmux/internal/workspace"
)

// Progress replayed after a reconnect is passed on once.
func TestStreamDedupe(t *testing.T) {
	var got []string
	f := &replayFilter{fn: func(p protocol.Message) { got = append(got, p.Detail) }}
	for _, d := range []string{"a", "b"} {
		f.pass(protocol.Message{Detail: d})
	}
	f.reset() // reconnect: the daemon replays from the start
	for _, d := range []string{"a", "b", "c"} {
		f.pass(protocol.Message{Detail: d})
	}
	if strings.Join(got, "") != "abc" {
		t.Fatalf("got %v", got)
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
}
