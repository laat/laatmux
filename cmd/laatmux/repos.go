package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/laat/laatmux/internal/config"
	"github.com/laat/laatmux/internal/home"
	"github.com/laat/laatmux/internal/tmux"
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
	var last home.Last
	if len(cfg.Repos) > 0 {
		if last, err = home.ReadLast(); err != nil {
			return err
		}
	}
	writeRepos(os.Stdout, cfg, last)
	return nil
}

// writeRepos is cmdRepos's output. A host with more than one repos
// directory has a line first: a checkout is looked for in each, and a
// repository found in none is cloned into the first, the checkout each
// repository's line names.
func writeRepos(out io.Writer, cfg config.Config, last home.Last) {
	var head []string
	if len(cfg.Copy) > 0 {
		head = append(head, "copy, every worktree on this machine: "+strings.Join(cfg.Copy, "  "))
	}
	for _, h := range cfg.Hosts {
		if len(h.Repos) < 2 {
			continue
		}
		dirs := make([]string, len(h.Repos))
		for i, d := range h.Repos {
			dirs[i] = tmux.Printable(d)
		}
		head = append(head, fmt.Sprintf("checkouts on %s: found in %s, cloned into %s", h.Name, strings.Join(dirs, "  "), dirs[0]))
	}
	for _, l := range head {
		fmt.Fprintln(out, l)
	}
	if len(cfg.Repos) == 0 {
		fmt.Fprintf(out, "no repos configured in %s\n", tmux.Printable(config.Path()))
		return
	}
	if len(head) > 0 {
		fmt.Fprintln(out)
	}
	w := tabwriter.NewWriter(out, 0, 8, 2, ' ', 0)
	defer w.Flush()
	for i, r := range cfg.Repos {
		if i > 0 {
			fmt.Fprintln(w)
		}
		fmt.Fprintf(w, "%s\t%s\n", r.Name, r.Source)
		if len(r.Copy) > 0 {
			fmt.Fprintf(w, "  copy\t%s\n", strings.Join(r.Copy, "  "))
		}
		if len(r.Setup) > 0 {
			fmt.Fprintf(w, "  setup\t%s\n", strings.Join(r.Setup, "  "))
		}
		lr := last.Get(r.Source)
		for _, h := range cfg.Hosts {
			if d, err := h.Dirs(); err != nil {
				// A host can lose its directories after being used, and
				// last.json still names it; say so rather than hide it.
				fmt.Fprintf(w, "  %s\tno repos and worktrees configured, cannot add", h.Name)
			} else {
				fmt.Fprintf(w, "  %s\t%s\t%s", h.Name, tmux.Printable(d.Checkout(r.Name)), tmux.Printable(d.Worktree(r.Name, "<branch>")))
			}
			if lr.Host == h.Name {
				fmt.Fprint(w, "\tlast used")
				if lr.Agent != "" {
					fmt.Fprint(w, ", agent "+lr.Agent)
				}
			}
			fmt.Fprintln(w)
		}
	}
}
