package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"strings"
	"time"

	"github.com/laat/laatmux/internal/daemon"
	"github.com/laat/laatmux/internal/detect"
	"github.com/laat/laatmux/internal/procs"
	"github.com/laat/laatmux/internal/tmux"
)

func cmdExplain(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("explain", flag.ContinueOnError)
	sock := tmuxServerFlag(fs)
	lines := fs.Int("capture-lines", daemon.DefaultCapture, "screen lines")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() < 1 {
		return errors.New("usage: laatmux explain [--tmux-socket s] <pane-id>")
	}
	srv := parseServer(*sock)
	panes, err := srv.ListPanes(ctx)
	if err != nil {
		return err
	}
	var pane *tmux.Pane
	for i := range panes {
		if panes[i].ID == fs.Arg(0) {
			pane = &panes[i]
		}
	}
	if pane == nil {
		return fmt.Errorf("pane %s not found", fs.Arg(0))
	}
	fmt.Printf("pane %s session=%s tty=%s pane_pid=%d current_command=%s\ntitle: %q\n", pane.ID, pane.Session, pane.TTY, pane.PID, pane.CurrentCommand, pane.Title)
	list, err := procs.ListTTY(pane.TTY)
	if err != nil {
		fmt.Println("procs:", err)
	}
	for _, p := range list {
		argv := ""
		if len(p.Argv) > 0 {
			argv = strings.Join(p.Argv, " ")
			if len(argv) > 80 {
				argv = argv[:80] + "…"
			}
		}
		fmt.Printf("  pid=%d ppid=%d pgid=%d tpgid=%d comm=%s start=%s %s\n", p.PID, p.PPID, p.PGID, p.TPGID, p.Comm, p.Start.Format(time.TimeOnly), argv)
	}
	id, found, _ := procs.Find(pane.TTY)
	fmt.Printf("identity: found=%v agent=%q pid=%d leader=%d comm=%s\n", found, id.Agent, id.PID, id.LeaderPID, id.Comm)
	screen, err := srv.Capture(ctx, pane.ID, *lines)
	if err != nil {
		return err
	}
	in := detect.Input{Agent: id.Agent, Title: pane.Title, Screen: screen}
	res := detect.Detect(in)
	fmt.Printf("detect: state=%s rule=%q reason=%s idle=%v blocker=%v working=%v skip=%v\n", res.State, res.Rule, res.Reason, res.VisibleIdle, res.VisibleBlocker, res.VisibleWorking, res.Skip)
	fmt.Println(detect.Explain(in))
	return nil
}
