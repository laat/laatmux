package main

import (
	"strings"
	"testing"

	"github.com/laat/laatmux/internal/client"
	"github.com/laat/laatmux/internal/config"
	"github.com/laat/laatmux/internal/workspace"
)

// A split in a plain session is the plain split in the pane's directory;
// in a workspace session it lands at the worktree root, started there
// for a local host and through ssh with cd for a remote one. The
// direction flag is passed through and the default left to tmux.
func TestSplitArgs(t *testing.T) {
	plain := workspace.Local{Name: "notes"}
	ws := workspace.Local{Name: "vm/proj/x", Key: "env//home/u/wt/proj/x", Host: "vm"}
	local := config.Host{Host: client.Host{Name: "mac"}}
	remote := config.Host{Host: client.Host{Name: "vm", SSH: "vm"}}
	cases := []struct {
		name string
		dir  string
		l    workspace.Local
		h    config.Host
		want string
	}{
		{"plain", "-h", plain, config.Host{}, "split-window -h -t %3 -c /cur"},
		{"plain default direction", "", plain, config.Host{}, "split-window -t %3 -c /cur"},
		{"local workspace", "-v", ws, local, "split-window -v -t %3 -c /home/u/wt/proj/x"},
		{"remote workspace", "-h", ws, remote, "split-window -h -t %3 " + workspace.ShellCommand(remote.Host, "/home/u/wt/proj/x")},
	}
	for _, c := range cases {
		if got := strings.Join(splitArgs(c.dir, "%3", "/cur", c.l, c.h), " "); got != c.want {
			t.Errorf("%s:\n got %s\nwant %s", c.name, got, c.want)
		}
	}
}

// The command after "--" is taken verbatim, flags and target before it.
func TestSplitDashes(t *testing.T) {
	before, cmd, ok := splitDashes([]string{"proj/x", "--host", "vm", "--", "sh", "-c", "echo -- hi"})
	if !ok || strings.Join(before, " ") != "proj/x --host vm" || strings.Join(cmd, " ") != "sh -c echo -- hi" {
		t.Fatalf("%v %v %v", before, cmd, ok)
	}
	if _, _, ok := splitDashes([]string{"proj/x"}); ok {
		t.Fatal("no dashes taken as ok")
	}
	if before, cmd, ok := splitDashes([]string{"--", "make"}); !ok || len(before) != 0 || len(cmd) != 1 {
		t.Fatalf("%v %v %v", before, cmd, ok)
	}
}
