package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"text/tabwriter"

	"github.com/laat/laatmux/internal/config"
	"github.com/laat/laatmux/internal/home"
	"github.com/laat/laatmux/internal/merged"
	"github.com/laat/laatmux/internal/tmux"
)

// cmdRepos shows the known repositories (merged.Known): each one's name
// and source, whether the config lists it, which hosts have a checkout
// of it, and where it lands on each host: the main checkout under repos
// and the worktree directory under worktrees. A checkout a host has is
// printed as the host found it; one a clone would make, and the
// worktree directory, as configured, so a "~" is the host's own.
func cmdRepos(ctx context.Context, args []string) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if len(args) > 0 {
		return errors.New("usage: laatmux repos")
	}
	known := readKnown(ctx, cfg)
	var last home.Last
	if len(known.Repos) > 0 {
		if last, err = home.ReadLast(); err != nil {
			return err
		}
	}
	writeRepos(os.Stdout, cfg, known, last)
	return nil
}

// writeRepos is cmdRepos's output. A host with more than one repos
// directory has a line first: a checkout is looked for in each, and a
// repository found in none is cloned into the first, the checkout each
// repository's line names. So do the hosts whose checkouts were not
// read.
func writeRepos(out io.Writer, cfg config.Config, known merged.Known, last home.Last) {
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
	switch {
	case !known.Discovered:
		head = append(head, "the hosts' checkouts not read, no local daemon answered: the config's repos alone")
	case len(known.Unread) > 0:
		head = append(head, "checkouts not read from "+strings.Join(known.Unread, ", ")+": their repositories show where the config or a worktree has them")
	}
	for _, l := range head {
		fmt.Fprintln(out, l)
	}
	if len(known.Repos) == 0 {
		if known.Discovered {
			fmt.Fprintf(out, "no repos configured in %s, and none checked out on any host\n", tmux.Printable(config.Path()))
		} else {
			fmt.Fprintf(out, "no repos configured in %s\n", tmux.Printable(config.Path()))
		}
		return
	}
	if len(head) > 0 {
		fmt.Fprintln(out)
	}
	w := tabwriter.NewWriter(out, 0, 8, 2, ' ', 0)
	defer w.Flush()
	for i, r := range known.Repos {
		if i > 0 {
			fmt.Fprintln(w)
		}
		fmt.Fprintf(w, "%s\t%s\t%s\n", r.Name, r.Source, reposMark(r))
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
				// The host's first checkout of it, as its scan found them.
				checkout := d.Checkout(r.Name)
				for _, f := range r.Found {
					if f.Host == h.Name && f.Root != "" {
						checkout = f.Root
						break
					}
				}
				fmt.Fprintf(w, "  %s\t%s\t%s", h.Name, tmux.Printable(checkout), tmux.Printable(d.Worktree(r.Name, "<branch>")))
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

// reposMark is a repository's mark on its line: configured, discovered
// on the hosts with a checkout of it, or both.
func reposMark(r merged.KnownRepo) string {
	var marks []string
	if r.Configured {
		marks = append(marks, "configured")
	}
	var hosts []string
	for _, f := range r.Found {
		if !slices.Contains(hosts, f.Host) {
			hosts = append(hosts, f.Host)
		}
	}
	if len(hosts) > 0 {
		marks = append(marks, "discovered on "+strings.Join(hosts, ", "))
	}
	return strings.Join(marks, ", ")
}
