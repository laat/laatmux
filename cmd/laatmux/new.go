package main

import (
	"context"
	"errors"
	"flag"
	"fmt"

	"github.com/laat/laatmux/internal/client"
	"github.com/laat/laatmux/internal/config"
	"github.com/laat/laatmux/internal/protocol"
	"github.com/laat/laatmux/internal/tmux"
)

func cmdNew(ctx context.Context, args []string) error {
	a, err := parseNewArgs(args)
	if err != nil {
		return err
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	h, ok := cfg.Find(a.host)
	if !ok {
		return fmt.Errorf("unknown host %q", a.host)
	}
	res, err := newSession(ctx, h, protocol.Message{Name: a.name, Cwd: a.cwd, Cmd: a.cmd})
	if err != nil {
		return err
	}
	fmt.Printf("%s/%s %s\n", h.Name, res.Session, res.PaneID)
	return nil
}

// newSession asks the host's daemon to make a managed session, as new
// does and as a jump does for a worktree with none (newHome): m's name,
// cwd and command, the session tagged with the host's name here. A
// daemon without the capability is a *noNewError, and nothing is asked.
func newSession(ctx context.Context, h config.Host, m protocol.Message) (protocol.Message, error) {
	c, err := client.Dial(ctx, h.Host)
	if err != nil {
		return protocol.Message{}, err
	}
	defer c.Close()
	if !protocol.Has(c.Hello.Capabilities, protocol.CapNew) {
		return protocol.Message{}, &noNewError{host: h.Name, version: c.Hello.Version}
	}
	m.Type, m.Host = protocol.TypeNew, h.Name
	return c.Request(ctx, m)
}

// noNewError is a host whose daemon does not support new: an older
// build, or one with no managed server.
type noNewError struct{ host, version string }

func (e *noNewError) Error() string {
	return fmt.Sprintf("%s: daemon %s does not support new", e.host, e.version)
}

// noNew reports whether err is a daemon without new.
func noNew(err error) bool {
	var e *noNewError
	return errors.As(err, &e)
}

type newArgs struct {
	name, host, cwd string
	cmd             []string
}

// parseNewArgs reads the new command line. A name tmux would not store
// as given, and a command that starts with an environment assignment,
// are refused here, before a host is dialled; the daemon refuses both
// again.
func parseNewArgs(args []string) (newArgs, error) {
	var a newArgs
	fs := flag.NewFlagSet("new", flag.ContinueOnError)
	fs.StringVar(&a.host, "host", "", "host name from config; default local")
	fs.StringVar(&a.cwd, "cwd", "", "working directory on the host (required)")
	// Flags may come before or after the name: flag.Parse stops at the first
	// non-flag, so parse once for leading flags and again for trailing ones.
	if err := fs.Parse(args); err != nil {
		return a, err
	}
	if fs.NArg() < 1 {
		return a, errors.New("usage: laatmux new <name> [--host h] --cwd <path> [-- <cmd>...]")
	}
	a.name = fs.Arg(0)
	if err := fs.Parse(fs.Args()[1:]); err != nil {
		return a, err
	}
	a.cmd = fs.Args()
	if err := tmux.CheckSessionName(a.name); err != nil {
		return a, err
	}
	if a.cwd == "" {
		return a, errors.New("--cwd is required")
	}
	if err := config.CheckCmd(a.cmd); err != nil {
		return a, err
	}
	return a, nil
}
