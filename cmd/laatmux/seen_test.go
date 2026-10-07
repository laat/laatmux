package main

import (
	"context"
	"io"
	"os/exec"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/laat/laatmux/internal/daemon"
	"github.com/laat/laatmux/internal/tmux"
	"github.com/laat/laatmux/internal/workspace"
)

// A focused sidebar pane stands for the window's live attach pane, else
// its one other pane; a dead attach pane is not live, so a window with
// one and a shell has two other panes, and stands for none.
func TestPickBeside(t *testing.T) {
	side := []string{"%1", "0", "1", "", ""}
	for _, c := range []struct {
		name string
		recs [][]string
		want daemon.ClientView
	}{
		{"attach", [][]string{side, {"%2", "0", "", "1", "proj/x"}}, daemon.ClientView{Pane: "%2", AttachPane: true, Target: "proj/x"}},
		{"shell", [][]string{side, {"%3", "0", "", "", ""}}, daemon.ClientView{Pane: "%3"}},
		{"dead attach", [][]string{side, {"%2", "1", "", "1", "proj/x"}, {"%3", "0", "", "", ""}}, daemon.ClientView{}},
		{"attach among two", [][]string{side, {"%3", "0", "", "", ""}, {"%2", "0", "", "1", "proj/x"}}, daemon.ClientView{Pane: "%2", AttachPane: true, Target: "proj/x"}},
		{"alone", [][]string{side}, daemon.ClientView{}},
	} {
		if got := pickBeside(c.recs); got != c.want {
			t.Errorf("%s: %+v, want %+v", c.name, got, c.want)
		}
	}
}

// Each client is read with the tags of its pane and session: one on a
// live attach pane, one on a dead one, and one on a sidebar pane, which
// stands for the attach pane beside it. A target holds no tmux.Sep or newline, Ensure
// refusing a managed session with a U+2063 and tmux storing a newline
// in a session name escaped, so the targets have them only to show
// that the listings do not split a value: split at Sep, the dead
// client's line had more fields than it reads and was dropped, and the
// sidebar window's pane line was cut at the newline, its target read
// short. The user's after-list-clients and after-list-panes hooks
// print a line after the records, which is no client and no pane.
func TestListClients(t *testing.T) {
	isolatedDefault(t)
	ctx := context.Background()
	run := func(args ...string) string {
		t.Helper()
		return strings.TrimSpace(string(must(workspace.Server.Run(ctx, args...))))
	}
	targets := map[string]string{"u": "proj/u", "v": "proj/" + tmux.Sep + "x", "w": "proj/y\n" + tmux.Sep + "z"}
	panes := map[string]string{}
	for _, s := range []string{"u", "v", "w"} {
		panes[s] = run("new-session", "-d", "-s", s, "-P", "-F", "#{pane_id}", "sleep 1000")
		run("set-option", "-p", "-t", panes[s], "@laatmux_attach_pane", "1", tmux.Next, "set-option", "-p", "-t", panes[s], "@laatmux_attach_target", targets[s],
			tmux.Next, "set-option", "-t", s, "@laatmux_host", "mac")
	}
	run("set-option", "-t", "u", "@laatmux_attach", "mac/u", tmux.Next, "set-option", "-t", "v", "@laatmux_attach", "mac/v")
	run("set-option", "-t", "w", "@laatmux_workspace", "env1/root/w")
	side := run("split-window", "-h", "-t", panes["w"], "-P", "-F", "#{pane_id}", "sleep 1000")
	run("set-option", "-p", "-t", side, sidebarTag, "1", tmux.Next, "select-pane", "-t", side)
	// v's attach pane exits, kept dead by remain-on-exit.
	run("set-option", "-p", "-t", panes["v"], "remain-on-exit", "on", tmux.Next, "respawn-pane", "-k", "-t", panes["v"], "true")
	for i := 0; run("display-message", "-p", "-t", panes["v"], "#{pane_dead}") != "1"; i++ {
		if i == 50 {
			t.Fatalf("pane %s did not die", panes["v"])
		}
		time.Sleep(100 * time.Millisecond)
	}
	for _, s := range []string{"u", "v", "w"} {
		c := exec.Command("tmux", "-L", "default", "-C", "attach", "-t", s)
		in, err := c.StdinPipe()
		if err != nil {
			t.Fatal(err)
		}
		c.Stdout = io.Discard
		if err := c.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { in.Close(); _ = c.Process.Kill(); _ = c.Wait() })
	}
	clients := map[string]string{}
	for i := 0; i < 50 && len(clients) != 3; i++ {
		time.Sleep(100 * time.Millisecond)
		clear(clients)
		for _, l := range strings.Split(run("list-clients", "-F", "#{client_session} #{client_name}"), "\n") {
			if s, c, ok := strings.Cut(l, " "); ok {
				clients[s] = c
			}
		}
	}
	if len(clients) != 3 {
		t.Fatalf("clients: %v", clients)
	}
	for _, hook := range []string{"after-list-clients", "after-list-panes"} {
		run("set-hook", "-g", hook, "display-message -p hook")
	}
	views, err := listClients(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := []daemon.ClientView{
		{Client: clients["u"], Pane: panes["u"], AttachPane: true, Target: targets["u"], Host: "mac", Attach: "mac/u"},
		{Client: clients["v"], Pane: panes["v"], Dead: true, AttachPane: true, Target: targets["v"], Host: "mac", Attach: "mac/v"},
		{Client: clients["w"], Pane: panes["w"], AttachPane: true, Target: targets["w"], Host: "mac", Workspace: "env1/root/w"},
	}
	slices.SortFunc(views, func(a, b daemon.ClientView) int { return strings.Compare(a.Client, b.Client) })
	slices.SortFunc(want, func(a, b daemon.ClientView) int { return strings.Compare(a.Client, b.Client) })
	if !slices.Equal(views, want) {
		t.Errorf("clients:\n%+v\nwant\n%+v", views, want)
	}
}

// The github line trusts only an answer with a viewer: a body with data
// null and errors, which gh returns with exit 0, is the error.
func TestViewerStatus(t *testing.T) {
	for body, want := range map[string]string{
		`{"data":{"viewer":{"login":"laat"}}}`:                           "ok",
		`{"data":null,"errors":[{"message":"API rate limit exceeded"}]}`: "gh api graphql: API rate limit exceeded",
		`{"data":{"viewer":null}}`:                                       "gh api graphql: no viewer",
	} {
		if got := viewerStatus([]byte(body)); got != want {
			t.Errorf("%s: %q, want %q", body, got, want)
		}
	}
}
