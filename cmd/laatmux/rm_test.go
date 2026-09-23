package main

import (
	"strings"
	"testing"

	"github.com/laat/laatmux/internal/client"
	"github.com/laat/laatmux/internal/command"
	"github.com/laat/laatmux/internal/config"
	"github.com/laat/laatmux/internal/workspace"
)

// rm inside a workspace session takes the workspace from the session's
// tags: the root from the key, the repository and branch when the
// source is configured here, else the root alone; the host from its
// tag. Not a workspace, or a host the config lacks, is refused.
func TestRmCurrent(t *testing.T) {
	cfg := config.Config{
		Hosts: []config.Host{{Host: client.Host{Name: "mac"}}, {Host: client.Host{Name: "vm", SSH: "vm"}}},
		Repos: []config.Repo{{Source: "git@x:o/proj.git", Name: "proj"}},
	}
	cases := []struct {
		name string
		cur  workspace.Local
		want command.Rm
		err  string
	}{
		{"known source", workspace.Local{Name: "vm/proj/x", Key: "env//r/proj/x", Host: "vm", Source: "git@x:o/proj.git", Branch: "x"},
			command.Rm{Host: cfg.Hosts[1], Repo: cfg.Repos[0], Branch: "x", Root: "/r/proj/x"}, ""},
		{"unknown source", workspace.Local{Name: "mac/other/y", Key: "env//r/other/y", Host: "mac", Source: "git@x:o/other.git", Branch: "y"},
			command.Rm{Host: cfg.Hosts[0], Root: "/r/other/y"}, ""},
		{"no branch tag", workspace.Local{Name: "mac/proj/z", Key: "env//r/proj/z", Host: "mac", Source: "git@x:o/proj.git"},
			command.Rm{Host: cfg.Hosts[0], Root: "/r/proj/z"}, ""},
		{"not a workspace", workspace.Local{Name: "notes"}, command.Rm{}, "not a workspace session"},
		{"attachment", workspace.Local{Name: "vm/work", Attach: "vm/work", Host: "vm"}, command.Rm{}, "not a workspace session"},
		{"unknown host", workspace.Local{Name: "box/proj/x", Key: "env//r/proj/x", Host: "box"}, command.Rm{}, "not configured"},
		{"no root", workspace.Local{Name: "vm/proj/x", Key: "env", Host: "vm"}, command.Rm{}, "no root"},
	}
	for _, c := range cases {
		got, err := rmCurrent(cfg, c.cur)
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
		if got.Host.Name != c.want.Host.Name || got.Repo != c.want.Repo || got.Branch != c.want.Branch || got.Root != c.want.Root || got.Force {
			t.Errorf("%s: got %+v want %+v", c.name, got, c.want)
		}
	}
}
