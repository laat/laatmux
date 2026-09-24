package main

import (
	"strings"
	"testing"

	"github.com/laat/laatmux/internal/client"
	"github.com/laat/laatmux/internal/command"
	"github.com/laat/laatmux/internal/config"
	"github.com/laat/laatmux/internal/protocol"
	"github.com/laat/laatmux/internal/workspace"
)

// rm inside a workspace session takes the workspace from the session:
// the host must answer as the key's environment; the worktree record
// for the root gives the repository and branch when the host has one,
// else the tags when the source is configured here, else the root
// alone; not a workspace, or a key without a root, is refused.
func TestRmCurrent(t *testing.T) {
	cfg := config.Config{
		Hosts: []config.Host{{Host: client.Host{Name: "mac"}}, {Host: client.Host{Name: "vm", SSH: "vm"}}},
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
		cur  workspace.Local
		h    config.Host
		env  string
		ws   []protocol.Worktree
		want command.Rm
		err  string
	}{
		{"record wins over the tags", workspace.Local{Name: "vm/proj/x", Key: "env//r/proj/x", Host: "vm", Source: proj.Source, Branch: "x"}, vm, "env", records,
			command.Rm{Host: vm, Repo: proj, Branch: "renamed", Root: "/r/proj/x", Environment: "env"}, ""},
		{"detached record: repository and root", workspace.Local{Name: "vm/proj/d", Key: "env//r/proj/d", Host: "vm", Source: proj.Source, Branch: "d"}, vm, "env", records,
			command.Rm{Host: vm, Repo: proj, Root: "/r/proj/d", Environment: "env"}, ""},
		{"record of a source not configured here: root alone", workspace.Local{Name: "vm/other/y", Key: "env//r/other/y", Host: "vm", Source: "git@x:o/other.git", Branch: "y"}, vm, "env", records,
			command.Rm{Host: vm, Root: "/r/other/y", Environment: "env"}, ""},
		{"no record: the tags", workspace.Local{Name: "vm/proj/z", Key: "env//r/proj/z", Host: "vm", Source: proj.Source, Branch: "z"}, vm, "env", records,
			command.Rm{Host: vm, Repo: proj, Branch: "z", Root: "/r/proj/z", Environment: "env"}, ""},
		{"no record, unknown source", workspace.Local{Name: "mac/other/q", Key: "env//r/other/q", Host: "mac", Source: "git@x:o/other.git", Branch: "q"}, mac, "env", nil,
			command.Rm{Host: mac, Root: "/r/other/q", Environment: "env"}, ""},
		{"no record, no branch tag", workspace.Local{Name: "mac/proj/z", Key: "env//r/proj/z", Host: "mac", Source: proj.Source}, mac, "env", nil,
			command.Rm{Host: mac, Root: "/r/proj/z", Environment: "env"}, ""},
		{"host answers as another environment", workspace.Local{Name: "vm/proj/x", Key: "env//r/proj/x", Host: "vm", Source: proj.Source, Branch: "x"}, vm, "other", records,
			command.Rm{}, "answers as other"},
		{"record on another environment is not this one", workspace.Local{Name: "vm/proj/x", Key: "other//r/proj/x", Host: "vm"}, vm, "other", records[3:],
			command.Rm{Host: vm, Repo: proj, Branch: "x", Root: "/r/proj/x", Environment: "other"}, ""},
		{"not a workspace", workspace.Local{Name: "notes"}, mac, "env", nil, command.Rm{}, "not a workspace session"},
		{"attachment", workspace.Local{Name: "vm/work", Attach: "vm/work", Host: "vm"}, vm, "env", nil, command.Rm{}, "not a workspace session"},
		{"no root", workspace.Local{Name: "vm/proj/x", Key: "env", Host: "vm"}, vm, "env", nil, command.Rm{}, "no root"},
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
