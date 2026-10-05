package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/laat/laatmux/internal/config"
	"github.com/laat/laatmux/internal/merged"
	"github.com/laat/laatmux/internal/protocol"
	"github.com/laat/laatmux/internal/rows"
)

// render is ls's listing of the merged state: a line per host that is
// not connected and listed, the tree, and a failed session listing
// after it.
func render(s merged.Status) string {
	var b strings.Builder
	if s.DaemonErr != "" {
		fmt.Fprintf(&b, "local daemon  DOWN  %s\n", s.DaemonErr)
	}
	for _, st := range s.Hosts {
		n := st.Name
		switch {
		case st.Connected && st.Listed:
			fmt.Fprintf(&b, "%s  connected  %s\n", n, st.Version)
		case st.Connected:
			fmt.Fprintf(&b, "%s  connected  %s  (snapshot pending)\n", n, st.Version)
		case st.Error != "":
			fmt.Fprintf(&b, "%s  DOWN  %s\n", n, st.Down())
		default:
			fmt.Fprintf(&b, "%s  connecting\n", n)
		}
	}
	renderTree(&b, rows.Tree(s.Input), time.Now())
	if s.SessionsErr != "" {
		// An incomplete listing says so after the tree, rather than
		// looking complete.
		fmt.Fprintf(&b, "\nlocal sessions not listed: %s\n", s.SessionsErr)
	}
	return b.String()
}

// renderTree prints the tree: a repository per line, its worktrees with
// their host under it, and under each its agents, panes and runs; a
// task where its worktree will be; other sessions last.
func renderTree(b *strings.Builder, nodes []rows.Row, now time.Time) {
	for _, n := range nodes {
		switch n.Kind {
		case rows.KindRepo, rows.KindGroup:
			fmt.Fprintf(b, "\n%s\n", n.Name)
		case rows.KindWorktree, rows.KindTask:
			renderLine(b, n, now)
		case rows.KindAgent:
			if n.Depth == 1 {
				// A session in other sessions.
				fmt.Fprintf(b, "  %-36s %s\n", n.Name+" ("+where(n)+")", agentText(n, now))
				continue
			}
			fmt.Fprintf(b, "    %s\n", agentText(n, now))
		case rows.KindPane:
			cmd := n.Name
			if n.Pane != nil && n.Pane.Command != "" {
				cmd = "$ " + n.Pane.Command
			}
			fmt.Fprintf(b, "    %s\n", cmd)
		case rows.KindRun:
			fmt.Fprintf(b, "    ▶ %s  %s\n", n.Name, rows.Ago(now.Sub(n.Run.StartedAt)))
		}
	}
}

// where is a node's host, with the server for an agent observed off the
// managed server, as jump --server takes it, and a note when the host
// is down.
func where(n rows.Row) string {
	s := n.Host
	if a := n.Agent; a != nil && n.Kind == rows.KindAgent {
		if srv := a.Server; srv != protocol.ServerLaatmux {
			s += "/" + srv
		}
	}
	if n.HostDown {
		s += ", host down"
	}
	return s
}

// renderLine is a worktree or task line: its label, host, and what it
// is instead of stats, a task's state say.
func renderLine(b *strings.Builder, n rows.Row, now time.Time) {
	label, _ := n.Labels()
	if n.Orphaned {
		label = n.Name
	}
	note := ""
	switch {
	case n.Pending != nil:
		note = n.Mark() + " " + n.State()
		if d := n.Detail(); d != "" {
			note += "  " + d
		}
	case n.Orphaned:
		_, root := protocol.SplitSessionKey(n.Local.Key)
		note = "worktree gone " + root
	case n.Worktree != nil && n.Children == 0:
		note = n.State()
	}
	if n.Settled {
		note = strings.TrimSpace(note + "  settled")
	}
	fmt.Fprintf(b, "%s\n", strings.TrimRight(fmt.Sprintf("  %-36s %s", label+" ("+where(n)+")", note), " "))
}

// agentText is an agent's mark, state, name, age and title.
func agentText(n rows.Row, now time.Time) string {
	a := n.Agent
	state := string(a.Activity)
	switch {
	case a.Liveness == protocol.Gone:
		state = "gone"
	case n.Done:
		state = "done"
	case n.Stale:
		state = "stale"
	}
	title := strings.TrimSpace(a.Title)
	if r := []rune(title); len(r) > 48 {
		// By rune: a cut in the middle of one prints as garbage.
		title = string(r[:48])
	}
	return fmt.Sprintf("%s %-8s %-6s %s  %s", n.Mark(), state, n.AgentName(), rows.Ago(now.Sub(a.ActivityAt)), title)
}

func cmdLs(ctx context.Context, args []string) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	m := merged.New()
	m.Configure(cfg)
	// The local daemon merges the hosts' streams; the dial starts it
	// when it is not running, and a daemon that cannot be started, or
	// one of an older build without the stream, is the error.
	c, err := dialMergedOrExplain(ctx)
	if err != nil {
		return err
	}
	defer c.Close()
	pending, err := m.Read(ctx, c, snapshotTimeout, func(pending []string) bool { return len(pending) == 0 })
	if err != nil {
		return err
	}
	m.TimedOut(pending, snapshotTimeout)
	fmt.Print(render(m.Status("")))
	return nil
}

func cmdWatch(ctx context.Context, args []string) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	m := merged.New()
	m.Configure(cfg)
	c, err := dialMergedOrExplain(ctx)
	if err != nil {
		return err
	}
	go m.Follow(ctx, c)
	t := time.NewTicker(5 * time.Second) // refresh relative times
	defer t.Stop()
	for {
		// The local sessions, settled and orphaned among them, come
		// with the merged stream.
		fmt.Print("\033[H\033[2J" + render(m.Status("")))
		select {
		case <-ctx.Done():
			return nil
		case <-m.Changed():
		case <-t.C:
		}
	}
}
