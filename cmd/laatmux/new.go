package main

import (
	"context"
	"errors"
	"flag"
	"fmt"

	"github.com/laat/laatmux/internal/client"
	"github.com/laat/laatmux/internal/config"
	"github.com/laat/laatmux/internal/protocol"
)

func cmdNew(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("new", flag.ContinueOnError)
	host := fs.String("host", "", "host name from config; default local")
	cwd := fs.String("cwd", "", "working directory on the host (required)")
	// Flags may come before or after the name: flag.Parse stops at the first
	// non-flag, so parse once for leading flags and again for trailing ones.
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() < 1 {
		return errors.New("usage: laatmux new <name> [--host h] --cwd <path> [-- <cmd>...]")
	}
	name := fs.Arg(0)
	if err := fs.Parse(fs.Args()[1:]); err != nil {
		return err
	}
	cmd := fs.Args()
	if *cwd == "" {
		return errors.New("--cwd is required")
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	h, ok := cfg.Find(*host)
	if !ok {
		return fmt.Errorf("unknown host %q", *host)
	}
	c, err := client.Dial(ctx, h)
	if err != nil {
		return err
	}
	defer c.Close()
	if !protocol.Has(c.Hello.Capabilities, protocol.CapNew) {
		return fmt.Errorf("%s: daemon %s does not support new", h.Name, c.Hello.Version)
	}
	res, err := c.Request(ctx, protocol.Message{Type: protocol.TypeNew, Name: name, Cwd: *cwd, Cmd: cmd, Host: h.Name})
	if err != nil {
		return err
	}
	fmt.Printf("%s/%s %s\n", h.Name, res.Session, res.PaneID)
	return nil
}
