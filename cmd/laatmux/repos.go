package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"text/tabwriter"

	"github.com/laat/laatmux/internal/config"
	"github.com/laat/laatmux/internal/home"
)

// cmdRepos shows each known repository's name and where it lands on each
// host: the main checkout under repos and the worktree directory under
// worktrees. Paths are printed as configured, so a "~" is the host's own.
func cmdRepos(ctx context.Context, args []string) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if len(args) > 0 {
		return errors.New("usage: laatmux repos")
	}
	if len(cfg.Repos) == 0 {
		fmt.Printf("no repos configured in %s\n", config.Path())
		return nil
	}
	last, err := home.ReadLast()
	if err != nil {
		return err
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 8, 2, ' ', 0)
	defer w.Flush()
	for i, r := range cfg.Repos {
		if i > 0 {
			fmt.Fprintln(w)
		}
		fmt.Fprintf(w, "%s\t%s\n", r.Name, r.Source)
		lr := last.Get(r.Source)
		for _, h := range cfg.Hosts {
			d, err := h.Dirs()
			if err != nil {
				fmt.Fprintf(w, "  %s\tno repos and worktrees configured, cannot add\n", h.Name)
				continue
			}
			fmt.Fprintf(w, "  %s\t%s\t%s", h.Name, d.Checkout(r.Name), d.Worktree(r.Name, "<branch>"))
			if lr.Host == h.Name {
				fmt.Fprint(w, "\tlast used")
				if lr.Agent != "" {
					fmt.Fprint(w, ", agent "+lr.Agent)
				}
			}
			fmt.Fprintln(w)
		}
	}
	return nil
}
