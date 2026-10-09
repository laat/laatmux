package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"

	"github.com/laat/laatmux/internal/client"
	"github.com/laat/laatmux/internal/command"
	"github.com/laat/laatmux/internal/config"
	"github.com/laat/laatmux/internal/merged"
	"github.com/laat/laatmux/internal/peer"
	"github.com/laat/laatmux/internal/protocol"
	"github.com/laat/laatmux/internal/rows"
	"github.com/laat/laatmux/internal/source"
	"github.com/laat/laatmux/internal/term"
	"github.com/laat/laatmux/internal/tmux"
	"github.com/laat/laatmux/internal/workspace"
)

// cmdPrune removes the worktrees nothing uses whose work is in the
// repository's default branch: the ones ls shows with no session, no
// agent and nothing else in them, that are clean, and whose commits
// are all in the default branch or whose PR is merged. Their
// ignored files go with them, as with rm, and the plan says how many.
// The listing is the merged stream's, as ls reads it; what the
// worktree holds is read on its host, by the facts message; the PR
// state is this machine's branch records, which {pr_state} draws. The
// plan is printed, a line per worktree with why it goes or stays; -n
// stops there, and without --yes the plan is confirmed once, in a
// terminal. Each removal is an rm, held to the commit the plan read and
// to nothing running in the worktree since.
func cmdPrune(ctx context.Context, args []string) error {
	a, err := parsePruneArgs(args)
	if err != nil {
		return err
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if a.host != "" {
		// By the name the merged stream has it, which an ssh alias
		// found it by is not.
		h, ok := cfg.Find(a.host)
		if !ok {
			return fmt.Errorf("unknown host %q", a.host)
		}
		if h.Paused {
			return &peer.PausedError{Name: h.Name}
		}
		a.host = h.Name
	}
	m := merged.New()
	m.Configure(cfg)
	c, err := dialMergedOrExplain(ctx)
	if err != nil {
		return err
	}
	defer c.Close()
	// With --host only that host is waited on: another one that does
	// not answer holds nothing up.
	waiting, err := m.Read(ctx, c, snapshotTimeout, func(waiting []string) bool {
		return len(waiting) == 0 || a.host != "" && !slices.Contains(waiting, a.host)
	})
	if err != nil {
		return err
	}
	m.TimedOut(waiting, snapshotTimeout)
	c.Close()
	return prune(ctx, cfg, a, m.Status(""), os.Stdout)
}

// matchRepo is --repo as a worktree is matched by it: a repository
// this machine's config lists, by its source; else a host's label or a
// source as the listing has them, for a checkout a host found by its
// origin. Not ok when neither knows the name.
func matchRepo(cfg config.Config, flag string, ws []protocol.Worktree) (func(protocol.Worktree) bool, bool) {
	if flag == "" {
		return func(protocol.Worktree) bool { return true }, true
	}
	if r, ok := cfg.Repo(flag); ok {
		return func(w protocol.Worktree) bool { return source.Same(w.Source, r.Source) }, true
	}
	match := func(w protocol.Worktree) bool { return w.Repo == flag || w.Source != "" && source.Same(w.Source, flag) }
	return match, slices.ContainsFunc(ws, match)
}

// pruneArgs is prune's command line.
type pruneArgs struct {
	host, repo string
	dryRun     bool
	branches   bool
	yes        bool
}

var pruneUsage = errors.New("usage: laatmux prune [--host h] [--repo r] [-n|--dry-run] [--branches] [--yes]")

func parsePruneArgs(args []string) (pruneArgs, error) {
	var a pruneArgs
	fs := flag.NewFlagSet("prune", flag.ContinueOnError)
	fs.StringVar(&a.host, "host", "", "only this host's worktrees")
	fs.StringVar(&a.repo, "repo", "", "only this repository's worktrees, by name or source")
	fs.BoolVar(&a.dryRun, "n", false, "print the plan only")
	fs.BoolVar(&a.dryRun, "dry-run", false, "print the plan only")
	fs.BoolVar(&a.branches, "branches", false, "delete each removed worktree's local branch too")
	fs.BoolVar(&a.yes, "yes", false, "remove without asking")
	if err := fs.Parse(args); err != nil {
		return a, err
	}
	if fs.NArg() > 0 {
		return a, pruneUsage
	}
	return a, nil
}

// pruneItem is one worktree in the plan: the host it is on, its line
// as the plan names it, what it is decided on, and the decision.
type pruneItem struct {
	host   string
	name   string
	in     pruneInput
	remove bool
	reason string
}

// pruneInput is what prune decides one worktree on: its record, the
// local workspace session for it, why what runs in it cannot be told
// from here, the facts its host read for it or why there are none, and
// this machine's branch record for its branch, nil when there is none.
type pruneInput struct {
	Worktree protocol.Worktree
	Local    *protocol.Session
	Unseen   string
	Facts    *protocol.RootFacts
	NoFacts  string
	Branch   *protocol.BranchStatus
}

// keepBefore is why a worktree stays whatever its host would read
// about it, "" when that is what decides: a main checkout, which git
// keeps; a detached HEAD, which has no branch to judge it by; a
// branch laatmux cannot carry, which no command names; a local
// workspace session, which rm kills with what runs in it, a shell
// over ssh on a remote host that the host does not see among its
// panes; and a listing that cannot say nothing runs in it.
func keepBefore(in pruneInput) string {
	w := in.Worktree
	switch {
	case w.Main:
		return "the main checkout"
	case w.Branch == "":
		return "detached HEAD"
	case w.BranchDisplayOnly:
		return "a branch laatmux cannot carry; rm --root removes it"
	case in.Local != nil:
		return "local session " + tmux.Printable(in.Local.Name) + " is open"
	case in.Unseen != "":
		return in.Unseen
	}
	return ""
}

// decide is whether the worktree goes, and why or why not. It goes
// when it is clean and either has no commit its repository's default
// branch lacks, or its branch's PR is merged with HEAD at the PR's
// last commit: a squash or a rebase merge leaves the branch's commits
// ahead of the default branch, and a commit made after the merge is in
// no PR. A PR merged into another branch, one under it in a stack say,
// counts: its work is in that branch on origin, and the plan names
// it. Everything else stays, said with why: a worktree git keeps from
// removal, locked or with submodules, too, as rm would fail on it.
func decide(in pruneInput) (remove bool, reason string) {
	w, f := in.Worktree, in.Facts
	if r := keepBefore(in); r != "" {
		return false, r
	}
	switch {
	case f == nil:
		return false, in.NoFacts
	case f.Error != "":
		return false, tmux.Printable(f.Error)
	case f.Branch != w.Branch:
		return false, "the branch is " + branchOrDetachedName(f.Branch) + " now"
	case f.InUse != "":
		return false, "in use, by " + tmux.Printable(f.InUse)
	case f.Changed > 0:
		return false, "dirty: " + plural(f.Changed, "changed file")
	case f.Locked && f.LockReason != "":
		return false, "locked: " + tmux.Printable(f.LockReason)
	case f.Locked:
		return false, "locked"
	case f.Submodules:
		return false, "has submodules, which git removes only by force; rm --force removes it"
	case f.Base == "":
		return false, "no default branch to compare with: no origin/HEAD, main or master"
	}
	def := strings.TrimPrefix(f.Base, "origin/")
	switch {
	case def == w.Branch:
		return false, "the default branch"
	case f.Ahead == 0:
		return true, clean(f) + ", no commits beyond " + tmux.Printable(f.Base)
	}
	ahead := plural(f.Ahead, "commit") + " ahead of " + tmux.Printable(f.Base)
	var pr *protocol.PullRequest
	if in.Branch != nil {
		pr = in.Branch.PR
	}
	if pr != nil && pr.State == "merged" {
		switch {
		case in.Branch.HeadOID == "":
			return false, fmt.Sprintf("%s; PR #%d is merged, its last commit not known", ahead, pr.Number)
		case in.Branch.HeadOID == f.Head:
			into := ""
			if pr.Base != "" && pr.Base != def {
				into = " into " + tmux.Printable(pr.Base)
			}
			return true, fmt.Sprintf("%s, PR #%d merged%s", clean(f), pr.Number, into)
		}
		return false, fmt.Sprintf("%s; PR #%d is merged, but HEAD is not its last commit", ahead, pr.Number)
	}
	switch {
	case !f.OnOrigin:
		ahead += ", not on origin"
	case f.Pushed:
		ahead += ", pushed"
	default:
		ahead += ", not all pushed"
	}
	switch {
	case pr == nil:
	case pr.Draft && pr.State == "open":
		ahead += fmt.Sprintf(", draft PR #%d", pr.Number)
	default:
		ahead += fmt.Sprintf(", PR #%d %s", pr.Number, pr.State)
	}
	return false, ahead
}

// clean is how a removal's reason calls a clean worktree: with the
// ignored files and directories that go with it, when there are any.
func clean(f *protocol.RootFacts) string {
	var ignored []string
	if f.Ignored > 0 {
		ignored = append(ignored, plural(f.Ignored, "ignored file"))
	}
	if f.IgnoredDirs > 0 {
		ignored = append(ignored, plural(f.IgnoredDirs, "ignored directory"))
	}
	if len(ignored) == 0 {
		return "clean"
	}
	return "clean, " + strings.Join(ignored, " and ")
}

// branchOrDetachedName is a branch as a reason names it.
func branchOrDetachedName(b string) string {
	if b == "" {
		return "detached"
	}
	return tmux.Printable(b)
}

// plural is n and the noun, made plural past one: directory, directories.
func plural(n int, noun string) string {
	switch {
	case n == 1:
		return "1 " + noun
	case strings.HasSuffix(noun, "y"):
		return fmt.Sprintf("%d %sies", n, strings.TrimSuffix(noun, "y"))
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

// pruneCandidates is the worktree lines of the tree that ls shows with
// no session and nothing under them: no agent, no managed session on
// the host, no pane and no run; not a main checkout, nor a task's
// line, which stands for an add. Only a host listed decides, as only
// its records are all of them; repo is whether the worktree is of the
// repository --repo names.
func pruneCandidates(tree []rows.Row, listed func(host string) bool, repo func(protocol.Worktree) bool) []pruneItem {
	var out []pruneItem
	for _, n := range tree {
		w := n.Worktree
		if n.Kind != rows.KindWorktree || n.Pending != nil || w == nil || n.Orphaned || w.Main {
			continue
		}
		if n.Agent != nil || n.Children > 0 || w.Session != "" || !listed(n.Host) || !repo(*w) {
			continue
		}
		out = append(out, pruneItem{host: n.Host, name: n.Name, in: pruneInput{Worktree: *w, Local: n.Local, Branch: n.Branch}})
	}
	return out
}

// hostFacts asks a host's daemon for the facts at the roots; it must
// answer as the environment the records are of.
var hostFacts = func(ctx context.Context, h peer.Host, env string, roots []string) ([]protocol.RootFacts, error) {
	c, err := client.Dial(ctx, h)
	if err != nil {
		return nil, err
	}
	defer c.Close()
	if !protocol.Has(c.Hello.Capabilities, protocol.CapPrune) {
		return nil, fmt.Errorf("daemon %s has no prune; laatmux upgrade %s installs one that has", c.Hello.Version, h.Name)
	}
	if c.Hello.EnvironmentID != env {
		return nil, fmt.Errorf("answers as environment %s, not %s its worktrees were listed for", c.Hello.EnvironmentID, env)
	}
	res, err := c.Request(ctx, protocol.Message{Type: protocol.TypeFacts, ID: command.ID("facts"), Roots: roots})
	if err != nil {
		return nil, err
	}
	return res.Facts, nil
}

// confirm asks once whether to go on, on stderr, and reads the answer
// from the terminal: y or yes is yes, anything else no. Not a terminal
// is an error naming --yes; ctx ending, an interrupt, stops the wait.
var confirm = func(ctx context.Context, prompt string) (bool, error) {
	if !term.IsTerminal(os.Stdin) {
		return false, errors.New("not asking without a terminal; give --yes to remove them, or -n for the plan alone")
	}
	fmt.Fprint(os.Stderr, prompt)
	answer := make(chan string, 1)
	go func() {
		line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
		answer <- line
	}()
	select {
	case <-ctx.Done():
		fmt.Fprintln(os.Stderr)
		return false, ctx.Err()
	case line := <-answer:
		a := strings.ToLower(strings.TrimSpace(line))
		return a == "y" || a == "yes", nil
	}
}

// pruneLocals lists this machine's sessions before each removal, a
// variable for tests.
var pruneLocals = workspace.List

// prune is the command over the merged status read: the hosts not
// looked at said, the plan printed, and the removals once confirmed.
func prune(ctx context.Context, cfg config.Config, a pruneArgs, s merged.Status, out io.Writer) error {
	repo, ok := matchRepo(cfg, a.repo, s.Input.Worktrees)
	if !ok {
		return fmt.Errorf("unknown repository %q; configured: %s, and no host lists one by that name or source", a.repo, repoList(cfg))
	}
	// What runs in a worktree is known from here only through a
	// listing of the local sessions that worked, and through records
	// that carry their worktree: a merging daemon of a build before
	// attribution drops it, and the panes and runs.
	attributed := map[string]bool{}
	for _, h := range s.Input.Hosts {
		attributed[h.Name] = h.Attribution
	}
	listed := map[string]merged.Host{}
	unseen := func(host string) string {
		switch {
		case s.SessionsErr != "":
			return "local sessions not listed: " + tmux.Printable(s.SessionsErr)
		case !listed[host].Attribution:
			return fmt.Sprintf("the daemon on %s, %s, does not say which worktree each agent is in; laatmux upgrade %s installs one that does", host, listed[host].Version, host)
		case !attributed[host]:
			return "the records of " + host + " come without the worktree each agent is in: the local daemon is an older build; laatmux stop ends it, and the next command starts this one"
		}
		return ""
	}
	for _, h := range s.Hosts {
		if a.host != "" && h.Name != a.host {
			continue
		}
		switch {
		case h.Paused:
			fmt.Fprintf(out, "%s  paused; its worktrees are not looked at\n", h.Name)
		case h.Connected && h.Listed && h.Worktrees:
			listed[h.Name] = h
		case h.Error != "":
			fmt.Fprintf(out, "%s  DOWN  %s; its worktrees are not looked at\n", h.Name, h.Down())
		case h.Connected && h.Listed:
			// A daemon without the worktrees capability has none.
		default:
			fmt.Fprintf(out, "%s  not listed yet; its worktrees are not looked at\n", h.Name)
		}
	}
	items := pruneCandidates(rows.Tree(s.Input), func(host string) bool { _, ok := listed[host]; return ok }, repo)
	if len(items) == 0 {
		fmt.Fprintln(out, "no worktree without a session to look at")
		return nil
	}
	for i := range items {
		items[i].in.Unseen = unseen(items[i].host)
	}
	hosts := map[string]config.Host{}
	for _, h := range s.Hosts {
		host := h.Name
		if _, ok := listed[host]; !ok {
			continue
		}
		var roots []string
		var asked []*pruneItem
		for i := range items {
			it := &items[i]
			if it.host == host && keepBefore(it.in) == "" {
				roots = append(roots, it.in.Worktree.Root)
				asked = append(asked, it)
			}
		}
		if len(asked) == 0 {
			continue
		}
		ch, ok := cfg.Find(host)
		noFacts := ""
		var facts []protocol.RootFacts
		switch {
		case !ok:
			noFacts = "host " + host + " is not in this machine's config"
		case !protocol.Has(h.Caps, protocol.CapPrune):
			noFacts = fmt.Sprintf("the daemon on %s, %s, has no prune; laatmux upgrade %s installs one that has", host, h.Version, host)
		default:
			hosts[host] = ch
			var err error
			if facts, err = hostFacts(ctx, ch.Host, h.EnvID, roots); err != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				noFacts = host + ": " + err.Error()
			}
		}
		byRoot := map[string]*protocol.RootFacts{}
		for i := range facts {
			byRoot[facts[i].Root] = &facts[i]
		}
		for _, it := range asked {
			it.in.Facts, it.in.NoFacts = byRoot[it.in.Worktree.Root], noFacts
			if it.in.Facts == nil && noFacts == "" {
				it.in.NoFacts = host + " did not answer for it"
			}
		}
	}
	var remove []*pruneItem
	for i := range items {
		it := &items[i]
		it.remove, it.reason = decide(it.in)
		if it.remove {
			remove = append(remove, it)
		}
	}
	fmt.Fprint(out, planText(items))
	if len(remove) == 0 {
		fmt.Fprintln(out, "nothing to remove")
		return nil
	}
	if a.dryRun {
		return nil
	}
	if !a.yes {
		what := plural(len(remove), "worktree")
		if a.branches {
			what += " with their local branches"
		}
		ok, err := confirm(ctx, "remove "+what+"? [y/N] ")
		if err != nil {
			return err
		}
		if !ok {
			fmt.Fprintln(out, "nothing removed")
			return nil
		}
	}
	failed := 0
	for _, it := range remove {
		w := it.in.Worktree
		rm := command.Rm{Host: hosts[it.host], Repo: config.Repo{Name: w.Repo, Source: w.Source}, Branch: w.Branch, Root: w.Root,
			Environment: w.EnvironmentID, Head: it.in.Facts.Head, Unused: true, DeleteBranch: a.branches}
		// The host refuses a worktree something runs in since the plan;
		// a local session opened for it meanwhile is this machine's to
		// see, and rm would kill it.
		locals, err := pruneLocals(ctx)
		if err == nil || tmux.HookOnly(err) {
			if l, ok := workspace.Find(locals, protocol.SessionKey(w.EnvironmentID, w.Root), ""); ok {
				err = fmt.Errorf("local session %s is open since the plan", tmux.Printable(l.Name))
			} else {
				err = nil
			}
		} else {
			err = fmt.Errorf("local sessions not listed: %w", err)
		}
		if err != nil {
			failed++
			fmt.Fprintf(out, "%s on %s not removed: %v\n", rm.Describe(), it.host, err)
			continue
		}
		res, err := rm.Run(ctx, prunePrinter{out})
		if err == nil || res.Root != "" {
			fmt.Fprintf(out, "removed %s on %s (%s)\n", rm.Describe(), it.host, tmux.Printable(w.Root))
		}
		for _, name := range res.Killed {
			fmt.Fprintf(out, "killed local session %s\n", tmux.Printable(name))
		}
		if err != nil {
			if ctx.Err() != nil {
				return err
			}
			failed++
			fmt.Fprintf(out, "%s on %s not removed: %v\n", rm.Describe(), it.host, err)
		}
	}
	if failed > 0 {
		return fmt.Errorf("%d of %s not removed", failed, plural(len(remove), "worktree"))
	}
	return nil
}

// planText is the plan, a line per worktree: remove or keep, the
// worktree and its host, and why.
func planText(items []pruneItem) string {
	var b strings.Builder
	for _, it := range items {
		verb := "keep"
		if it.remove {
			verb = "remove"
		}
		fmt.Fprintf(&b, "%-6s  %-36s %s\n", verb, tmux.Printable(it.name)+" ("+it.host+")", it.reason)
	}
	return b.String()
}

// prunePrinter is the Reporter of prune's removals, printing to the
// command's output as rm prints: the branch stage's line among them.
type prunePrinter struct{ w io.Writer }

func (p prunePrinter) Progress(m protocol.Message) { fmt.Fprintln(p.w, command.ProgressLine(m)) }
func (p prunePrinter) Note(s string)               { fmt.Fprintln(os.Stderr, "laatmux:", s) }
