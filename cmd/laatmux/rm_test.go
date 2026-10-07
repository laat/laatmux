package main

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/laat/laatmux/internal/command"
	"github.com/laat/laatmux/internal/config"
	"github.com/laat/laatmux/internal/home"
	"github.com/laat/laatmux/internal/peer"
	"github.com/laat/laatmux/internal/protocol"
	"github.com/laat/laatmux/internal/tmux"
)

// rm inside a workspace session takes the workspace from the session:
// the host must answer as the key's environment; the worktree record
// for the root gives the repository and branch when the host has one,
// else the tags when the source is configured here, else the root
// alone; not a workspace, or a key without a root, is refused.
func TestRmCurrent(t *testing.T) {
	cfg := config.Config{
		Hosts: []config.Host{{Host: peer.Host{Name: "mac"}}, {Host: peer.Host{Name: "vm", SSH: "vm"}}},
		Repos: []config.Repo{{Source: "git@x:o/proj.git", Name: "proj"}},
	}
	mac, vm := cfg.Hosts[0], cfg.Hosts[1]
	proj := cfg.Repos[0]
	records := []protocol.Worktree{
		{ID: "env/worktree//r/proj/x", EnvironmentID: "env", Repo: "proj", Source: proj.Source, Branch: "renamed", Root: "/r/proj/x"},
		{ID: "env/worktree//r/proj/d", EnvironmentID: "env", Repo: "proj", Source: proj.Source, Branch: "", Root: "/r/proj/d"},
		{ID: "env/worktree//r/other/y", EnvironmentID: "env", Repo: "other", Source: "git@x:o/other.git", Branch: "y", Root: "/r/other/y"},
		{ID: "other/worktree//r/proj/x", EnvironmentID: "other", Repo: "proj", Source: proj.Source, Branch: "x", Root: "/r/proj/x"},
	}
	cases := []struct {
		name string
		cur  protocol.Session
		h    config.Host
		env  string
		ws   []protocol.Worktree
		want command.Rm
		err  string
	}{
		{"record wins over the tags", protocol.Session{Name: "vm/proj/x", Key: "env//r/proj/x", Host: "vm", Source: proj.Source, Branch: "x"}, vm, "env", records,
			command.Rm{Host: vm, Repo: proj, Branch: "renamed", Root: "/r/proj/x", Environment: "env"}, ""},
		{"detached record: repository and root", protocol.Session{Name: "vm/proj/d", Key: "env//r/proj/d", Host: "vm", Source: proj.Source, Branch: "d"}, vm, "env", records,
			command.Rm{Host: vm, Repo: proj, Root: "/r/proj/d", Environment: "env"}, ""},
		{"record of a source not configured here: root alone", protocol.Session{Name: "vm/other/y", Key: "env//r/other/y", Host: "vm", Source: "git@x:o/other.git", Branch: "y"}, vm, "env", records,
			command.Rm{Host: vm, Root: "/r/other/y", Environment: "env"}, ""},
		{"no record: the tags", protocol.Session{Name: "vm/proj/z", Key: "env//r/proj/z", Host: "vm", Source: proj.Source, Branch: "z"}, vm, "env", records,
			command.Rm{Host: vm, Repo: proj, Branch: "z", Root: "/r/proj/z", Environment: "env"}, ""},
		{"no record, unknown source", protocol.Session{Name: "mac/other/q", Key: "env//r/other/q", Host: "mac", Source: "git@x:o/other.git", Branch: "q"}, mac, "env", nil,
			command.Rm{Host: mac, Root: "/r/other/q", Environment: "env"}, ""},
		{"no record, no branch tag", protocol.Session{Name: "mac/proj/z", Key: "env//r/proj/z", Host: "mac", Source: proj.Source}, mac, "env", nil,
			command.Rm{Host: mac, Root: "/r/proj/z", Environment: "env"}, ""},
		{"host answers as another environment", protocol.Session{Name: "vm/proj/x", Key: "env//r/proj/x", Host: "vm", Source: proj.Source, Branch: "x"}, vm, "other", records,
			command.Rm{}, "answers as other"},
		{"record on another environment is not this one", protocol.Session{Name: "vm/proj/x", Key: "other//r/proj/x", Host: "vm"}, vm, "other", records[3:],
			command.Rm{Host: vm, Repo: proj, Branch: "x", Root: "/r/proj/x", Environment: "other"}, ""},
		{"not a workspace", protocol.Session{Name: "notes"}, mac, "env", nil, command.Rm{}, "not a workspace session"},
		{"attachment", protocol.Session{Name: "vm/work", Attach: "vm/work", Host: "vm"}, vm, "env", nil, command.Rm{}, "not a workspace session"},
		{"no root", protocol.Session{Name: "vm/proj/x", Key: "env", Host: "vm"}, vm, "env", nil, command.Rm{}, "no root"},
	}
	for _, c := range cases {
		got, err := rmCurrent(cfg, c.cur, c.h, c.env, c.ws)
		if c.err != "" {
			if err == nil || !strings.Contains(err.Error(), c.err) {
				t.Errorf("%s: err %v, want %q", c.name, err, c.err)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		if got.Host.Name != c.want.Host.Name || got.Repo.Source != c.want.Repo.Source || got.Branch != c.want.Branch || got.Root != c.want.Root || got.Environment != c.want.Environment || got.Force {
			t.Errorf("%s:\n got %+v\nwant %+v", c.name, got, c.want)
		}
	}
}

// rm on a machine without tmux removes the worktree on the host and has
// no local session to clean up, rather than failing after the host's
// side is done: whether the host has the worktree's record or the root
// is looked for among the local sessions. With the default server's
// socket there, the local sessions may hold the root, and rm stops
// before it sends a removal without it.
func TestRmWithoutTmux(t *testing.T) {
	const src = "git@x:o/proj.git"
	var rms []protocol.Message
	var mu sync.Mutex
	startFakeDaemon(t, []string{protocol.CapStatus, protocol.CapMerged, protocol.CapFollow, protocol.CapRm}, func(pc *protocol.Conn, m protocol.Message) bool {
		switch m.Type {
		case protocol.TypeSubscribe:
			pc.Write(protocol.Message{Type: protocol.TypeSnapshot, Seq: 1, Hosts: []protocol.HostStatus{
				{Name: "mac", EnvironmentID: "lenv", Connected: true, Listed: true, Capabilities: []string{protocol.CapStatus, protocol.CapWorktrees, protocol.CapRm}},
			}, Worktrees: []protocol.Worktree{{ID: "lenv/worktree//w/proj/b", EnvironmentID: "lenv", Repo: "proj", Branch: "b", Root: "/w/proj/b", Source: src}}})
		case protocol.TypeRm:
			mu.Lock()
			rms = append(rms, m)
			mu.Unlock()
			pc.Write(protocol.Message{Type: protocol.TypeResult, ID: m.ID, OK: true, Root: "/w/proj/" + m.Branch})
		}
		return true
	})
	cfgPath := filepath.Join(os.Getenv("LAATMUX_HOME"), "config.yaml")
	if err := os.WriteFile(cfgPath, []byte("hosts:\n  - name: mac\n    repos: /r\n    worktrees: /w\nrepos:\n  - "+src+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LAATMUX_CONFIG", cfgPath)
	t.Setenv("PATH", t.TempDir())
	tmpdir := t.TempDir()
	t.Setenv("TMUX_TMPDIR", tmpdir)
	for _, branch := range []string{"b", "gone"} {
		args := []string{"proj/" + branch, "--host", "mac"}
		if err := cmdRm(context.Background(), args); err != nil {
			t.Errorf("rm %v: %v", args, err)
		}
	}
	sockDir := filepath.Join(tmpdir, "tmux-"+strconv.Itoa(os.Getuid()))
	if err := os.MkdirAll(sockDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sockDir, "default"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := cmdRm(context.Background(), []string{"proj/gone", "--host", "mac"}); !tmux.NotInstalled(err) {
		t.Errorf("rm proj/gone with the socket there: %v, want the not-found error", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(rms) != 2 || rms[0].Root != "/w/proj/b" || rms[1].Root != "" || rms[1].Branch != "gone" {
		t.Errorf("rms sent: %+v", rms)
	}
}

// The command line: a target or a root given as the empty string is
// the usage, not the workspace of the current session; flags come
// before or after the target.
func TestParseRmArgs(t *testing.T) {
	cases := []struct {
		args []string
		want rmArgs
		bad  bool
	}{
		{nil, rmArgs{}, false},
		{[]string{"--force"}, rmArgs{force: true}, false},
		{[]string{"proj/x"}, rmArgs{target: "proj/x", targetGiven: true}, false},
		{[]string{"proj/x", "--host", "vm", "--force"}, rmArgs{target: "proj/x", targetGiven: true, host: "vm", force: true}, false},
		{[]string{"--root", "/r/x", "--host", "vm"}, rmArgs{root: "/r/x", rootGiven: true, host: "vm"}, false},
		{[]string{""}, rmArgs{}, true},
		{[]string{"--root", ""}, rmArgs{}, true},
		{[]string{"--root", "", "--host", "vm"}, rmArgs{}, true},
		{[]string{"proj/x", "--root", "/r/x"}, rmArgs{}, true},
		{[]string{"proj/x", "extra"}, rmArgs{}, true},
	}
	for _, c := range cases {
		got, err := parseRmArgs(c.args)
		if c.bad {
			if err == nil {
				t.Errorf("%v: accepted as %+v", c.args, got)
			}
			continue
		}
		if err != nil || got != c.want {
			t.Errorf("%v: got %+v %v, want %+v", c.args, got, err, c.want)
		}
	}
}

// The add command line: a branch first, or none with a prompt, and
// the command override after -- never read as the branch.
func TestParseAddArgs(t *testing.T) {
	cases := []struct {
		args []string
		want addArgs
		bad  bool
	}{
		{[]string{"fix"}, addArgs{branch: "fix"}, false},
		{[]string{"fix", "--host", "vm", "-p", "do it"}, addArgs{branch: "fix", host: "vm", prompt: "do it"}, false},
		{[]string{"-p", "Fix the tests", "--agent", "claude"}, addArgs{branch: "fix-the-tests", generated: true, prompt: "Fix the tests", agent: "claude"}, false},
		{[]string{"-p", "Fix the tests", "--", "claude", "--flag"}, addArgs{branch: "fix-the-tests", generated: true, prompt: "Fix the tests", cmd: []string{"claude", "--flag"}}, false},
		{[]string{"fix", "--", "sleep", "3600"}, addArgs{branch: "fix", cmd: []string{"sleep", "3600"}}, false},
		{[]string{"--detach", "-p", "Fix it"}, addArgs{branch: "fix-it", generated: true, prompt: "Fix it", detach: true}, false},
		{nil, addArgs{}, true},
		{[]string{""}, addArgs{}, true},
		{[]string{"-p", "!!!"}, addArgs{}, true},
		{[]string{"fix", "extra"}, addArgs{}, true},
		{[]string{"fix", "--", "FOO=1", "claude"}, addArgs{}, true},
	}
	for _, c := range cases {
		got, err := parseAddArgs(c.args)
		if c.bad {
			if err == nil {
				t.Errorf("%v: accepted as %+v", c.args, got)
			}
			continue
		}
		if err != nil || got.branch != c.want.branch || got.generated != c.want.generated || got.prompt != c.want.prompt || got.host != c.want.host || got.agent != c.want.agent || got.detach != c.want.detach || strings.Join(got.cmd, " ") != strings.Join(c.want.cmd, " ") {
			t.Errorf("%v: got %+v %v, want %+v", c.args, got, err, c.want)
		}
	}
}

// new refuses a command that starts with an environment assignment
// before it reads the config or dials a daemon; without the check the
// unknown host would be the error.
func TestNewRefusesAssignment(t *testing.T) {
	t.Setenv("LAATMUX_CONFIG", filepath.Join(t.TempDir(), "config.yaml"))
	err := cmdNew(context.Background(), []string{"s", "--host", "nosuch", "--cwd", "/w", "--", "FOO=1", "claude"})
	if err == nil || !strings.Contains(err.Error(), "FOO=1 is an environment assignment") {
		t.Fatalf("new: %v", err)
	}
}

// tasks show never builds a path from the id: one that is not a plain
// name maps to a hashed file, which does not exist.
func TestShowTaskNoTraversal(t *testing.T) {
	t.Setenv("LAATMUX_HOME", t.TempDir())
	os.MkdirAll(filepath.Join(home.Dir(), "pending"), 0o700)
	os.WriteFile(filepath.Join(home.Dir(), "secret.json"), []byte(`{"prompt_text":"leak"}`), 0o600)
	err := showTask("../secret")
	if err == nil || !strings.Contains(err.Error(), "no pending record") {
		t.Fatalf("traversal: %v", err)
	}
}

// The task state line: a host gone from the snapshot's host list comes
// first, whatever the record says.
func TestTaskState(t *testing.T) {
	p := protocol.Pending{ID: "t", Done: true, OK: true, Prompt: protocol.DeliveryNotDelivered, Error: "not ready", Listed: true}
	if got := TaskState(p, true); !strings.HasPrefix(got, "prompt not delivered") {
		t.Fatalf("configured: %q", got)
	}
	if got := TaskState(p, false); !strings.HasPrefix(got, "host removed") {
		t.Fatalf("removed: %q", got)
	}
	p.Mismatch = "vm answers as environment x"
	if got := TaskState(p, true); !strings.HasPrefix(got, "host replaced") {
		t.Fatalf("mismatch: %q", got)
	}
	p.Mismatch, p.AttemptOpen, p.Attempt, p.AttemptError = "", true, 2, "old refusal"
	if got := TaskState(p, true); !strings.HasPrefix(got, "delivering the prompt, attempt 2") {
		t.Fatalf("open attempt: %q", got)
	}
	// An error is one line with no control byte in it: a failed setup's
	// is the last lines of its output, git's can span lines.
	setup := "setup: npm ci: exit status 1: \x1b[31merror\x1b[0m | done"
	for _, c := range []struct {
		p    protocol.Pending
		want string
	}{
		{protocol.Pending{Done: true, Stage: protocol.StageSetup, Error: setup}, strconv.Quote(setup)},
		{protocol.Pending{Done: true, OK: true, Prompt: protocol.DeliveryNotDelivered, Error: "a\nb"}, "prompt not delivered: " + strconv.Quote("a\nb")},
		{protocol.Pending{Done: true, OK: true, Prompt: protocol.DeliveryUnknown, Error: "a\tb"}, "prompt delivery unknown: " + strconv.Quote("a\tb")},
		{protocol.Pending{Done: true, OK: true, Prompt: protocol.DeliveryNotDelivered, AttemptError: "a\x1bb"}, "prompt " + protocol.DeliveryNotDelivered + "; last attempt refused: " + strconv.Quote("a\x1bb")},
		{protocol.Pending{Done: true, OK: true, ListingError: "a\x1bb"}, "done, awaiting the listing: " + strconv.Quote("a\x1bb")},
		{protocol.Pending{Unreachable: "a\x1bb"}, "host unreachable, retrying: " + strconv.Quote("a\x1bb")},
	} {
		if got := TaskState(c.p, true); got != c.want {
			t.Errorf("TaskState = %q, want %q", got, c.want)
		}
	}
}
