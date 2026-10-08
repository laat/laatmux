package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/laat/laatmux/internal/client"
	"github.com/laat/laatmux/internal/config"
	"github.com/laat/laatmux/internal/home"
	"github.com/laat/laatmux/internal/merged"
	"github.com/laat/laatmux/internal/peer"
	"github.com/laat/laatmux/internal/protocol"
	"github.com/laat/laatmux/internal/source"
	"github.com/laat/laatmux/internal/tmux"
	"github.com/laat/laatmux/internal/worktree"
)

// snapshotTimeout bounds a snapshot request: the daemon answers once its
// first poll is complete, which over ssh includes starting it.
const snapshotTimeout = 20 * time.Second

// splitRepoBranch parses <repo>/<branch>. Labels cannot contain "/", so
// the first component is the repository and the rest is the branch,
// slashes included. A branch that is not UTF-8 is refused here, before
// any daemon is asked: rm and run would send another name, and no
// record path finds has it, since a host lists a worktree checked out on
// one with the branch quoted. The refusal names that listed form, which
// findWorktree answers with the root. A branch with U+FFFD goes on: a
// record of a real one has that name, and findWorktree answers it the
// same way; without a record, the daemon refuses it.
func splitRepoBranch(target string) (repo, branch string, err error) {
	repo, branch, ok := strings.Cut(target, "/")
	if !ok || repo == "" || branch == "" {
		return "", "", fmt.Errorf("%q is not <repo>/<branch>", target)
	}
	if err := worktree.CheckWire(branch); err != nil && !utf8.ValidString(branch) {
		return "", "", fmt.Errorf("%w; a worktree checked out on it is listed as %s, which rm, run and path take to say its root", err, repo+"/"+tmux.Printable(branch))
	}
	return repo, branch, nil
}

// resolveRepo picks the repository: the flag, by name or source, else the
// one the current directory belongs to on the local host. Identity is the
// source, so the directory's git origin is matched against the known
// sources first; that also finds a checkout whose label has since
// changed. Only a directory with no origin at all falls back to its place
// under the local host's repos or worktrees directory, where the next
// path component is the label. An origin that is not a known source is
// an error, not a fall back to the label: the label may belong to another
// source by now.
func resolveRepo(ctx context.Context, cfg config.Config, flag string) (config.Repo, error) {
	if flag != "" {
		r, ok := cfg.Repo(flag)
		if !ok {
			return config.Repo{}, fmt.Errorf("unknown repository %q; configured: %s", flag, repoList(cfg))
		}
		return r, nil
	}
	cwd, err := os.Getwd()
	if err != nil {
		return config.Repo{}, err
	}
	origin, err := originOf(ctx, cwd)
	if err != nil {
		return config.Repo{}, err
	}
	if origin != "" {
		if r, ok := cfg.RepoBySource(origin); ok {
			return r, nil
		}
		return config.Repo{}, fmt.Errorf("%s has origin %s, which is not a configured repository; use --repo (configured: %s)", tmux.Printable(cwd), origin, repoList(cfg))
	}
	if label, ok := labelUnder(cfg, cwd); ok {
		if r, ok := cfg.RepoByName(label); ok {
			return r, nil
		}
	}
	return config.Repo{}, fmt.Errorf("%s is not inside a known repository; use --repo (configured: %s)", tmux.Printable(cwd), repoList(cfg))
}

// labelUnder is the path component after one of the local host's repos
// directories or its worktrees directory when dir is under one of them.
// The more specific directory is tried first: with worktrees nested
// under repos, a worktree's label is the component after worktrees, not
// "worktrees", and with ~/code/group listed beside ~/code, a checkout's
// in the group is the component after the group.
func labelUnder(cfg config.Config, dir string) (string, bool) {
	local, ok := cfg.Local()
	if !ok {
		return "", false
	}
	d, err := local.Dirs()
	if err != nil {
		return "", false
	}
	d = d.Expand()
	// Cleaned, so a directory written with a trailing / is matched, and
	// is longer than the one it is in only by the path between them.
	var bases []string
	for _, b := range append(d.Repos, d.Worktrees) {
		bases = append(bases, filepath.Clean(b))
	}
	sort.SliceStable(bases, func(i, j int) bool { return len(bases[i]) > len(bases[j]) })
	for _, base := range bases {
		if rest, ok := strings.CutPrefix(dir+"/", base+"/"); ok {
			label, _, _ := strings.Cut(rest, "/")
			return label, label != ""
		}
	}
	return "", false
}

// originOf is the git origin of the repository dir is in, "" when git
// positively reports none: the key is unset, outside any repository too
// (exit 1), or the .git file dir is under names a repository that is
// gone (exit 128). That last is decided by the .git file itself, on any
// git (goneGitfile). git's message, in the C locale so it is the English
// one, is the fallback for what the file does not show, such as a
// directory with a HEAD that is no repository; only gits before 2.56 say
// "not a git repository" there, 2.56 says "gitfile does not point to a
// valid repository", which is not matched. Anything else, git missing or
// a repository it cannot read, is an error, so a directory whose
// identity cannot be inspected is never resolved from its label instead.
func originOf(ctx context.Context, dir string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", "-C", dir, "config", "--get", "remote.origin.url")
	cmd.Env = worktree.GitEnv()
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err == nil {
		return strings.TrimSpace(string(out)), nil
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		switch {
		case exit.ExitCode() == 1:
			return "", nil
		case exit.ExitCode() == 128 && (goneGitfile(dir) || strings.Contains(stderr.String(), "not a git repository")):
			return "", nil
		}
	}
	// git's message can repeat dir, as it is; its lines are kept.
	msg := strings.TrimSpace(stderr.String())
	if msg == "" {
		msg = err.Error()
	}
	return "", fmt.Errorf("%s: cannot read git origin: %s", tmux.Printable(dir), tmux.PrintableLines(msg))
}

// goneGitfile reports whether git's search for the repository of dir
// ends at a .git file naming a directory without a HEAD, which no git
// takes for a repository: the worktree's repository is gone. The search
// goes up from dir's physical path, as git's does, to the first level
// with a .git, and ends undecided where git might stop short of a .git
// file: at a .git that is not a regular file (git moves past a directory
// that is no repository, which this does not tell), or at a level with
// a HEAD of its own, which git may take for a bare repository. A dir
// that does not exist is undecided too, git failed to enter it. git's
// search starts at dir whatever this process's environment names, as
// worktree.GitEnv gives it no GIT_DIR or other variable naming a
// repository; a GIT_CEILING_DIRECTORIES that ends it sooner has git find
// no repository, which is no origin too. The file is read as git reads
// it: "gitdir: " and a path, relative to the file's directory, trailing
// line ends dropped, at most 1 MiB.
func goneGitfile(dir string) bool {
	d, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return false
	}
	for {
		dotgit := filepath.Join(d, ".git")
		fi, err := os.Stat(dotgit)
		switch {
		case err == nil:
			if !fi.Mode().IsRegular() || fi.Size() > 1<<20 {
				return false
			}
			b, err := os.ReadFile(dotgit)
			if err != nil {
				return false
			}
			target, ok := strings.CutPrefix(string(b), "gitdir: ")
			target = strings.TrimRight(target, "\r\n")
			if !ok || target == "" {
				return false
			}
			if !filepath.IsAbs(target) {
				target = d + "/" + target
			}
			// Not Join, whose lexical .. can differ from the file
			// system's past a symlink; Lstat, as a HEAD symlink into
			// refs/ counts for git whether or not it resolves. A
			// target that is a file, or below one, has no HEAD either.
			_, err = os.Lstat(target + "/HEAD")
			return errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ENOTDIR)
		case !errors.Is(err, fs.ErrNotExist):
			return false
		}
		if _, err := os.Lstat(filepath.Join(d, "HEAD")); !errors.Is(err, fs.ErrNotExist) {
			return false
		}
		parent := filepath.Dir(d)
		if parent == d {
			return false
		}
		d = parent
	}
}

func repoList(cfg config.Config) string {
	if len(cfg.Repos) == 0 {
		return "none"
	}
	names := make([]string, len(cfg.Repos))
	for i, r := range cfg.Repos {
		names[i] = r.Name
	}
	return strings.Join(names, ", ")
}

// hostFor picks the host for a repository: the flag, else the last-used
// host for it, else the config's default order. The host must be able to
// hold worktrees. last.json is read without the flag, for the last-used
// host, and with withLast, for a caller that takes the repository's other
// defaults from it or writes it back after the host's work, as add does:
// a file it cannot read stops that caller here, before the host is asked.
// A command given its host and wanting nothing more is not stopped by a
// file it does not use; the defaults it gets back are then zero.
func hostFor(cfg config.Config, flag string, repo config.Repo, withLast bool) (config.Host, home.LastRepo, error) {
	var lr home.LastRepo
	if flag == "" || withLast {
		last, err := home.ReadLast()
		if err != nil {
			return config.Host{}, home.LastRepo{}, err
		}
		lr = last.Get(repo.Source)
	}
	h, err := cfg.DefaultHost(flag, lr.Host)
	if err != nil {
		return config.Host{}, lr, err
	}
	if !h.CanAdd() {
		return config.Host{}, lr, fmt.Errorf("host %s has no repos and worktrees directories configured", h.Name)
	}
	return h, lr, nil
}

// snapshot returns the host's daemon's hello and a snapshot of its
// records, through the local daemon's merged stream when it has one: the
// records are already there while a sidebar holds the stream open, and
// on a cold daemon the wait is the connection the direct dial would have
// made. A host the local daemon's config lacks, or a daemon without the
// capability, falls back to dialling the host. The connection is closed;
// commands open their own.
func snapshot(ctx context.Context, h peer.Host, needCap string) (hello, snap protocol.Message, err error) {
	if c, ok := merged.Dial(ctx); ok {
		hello, snap, ok, err := mergedSnapshot(ctx, c, h, needCap)
		c.Close()
		if ok {
			return hello, snap, err
		}
	}
	c, err := client.Dial(ctx, h)
	if err != nil {
		return hello, snap, err
	}
	defer c.Close()
	if err := needCaps(h, c.Hello, needCap); err != nil {
		return hello, snap, err
	}
	sctx, cancel := context.WithTimeout(ctx, snapshotTimeout)
	defer cancel()
	snap, err = c.Snapshot(sctx)
	if err != nil {
		return hello, snap, fmt.Errorf("%s: %w", h.Name, err)
	}
	return c.Hello, snap, nil
}

// mergedSnapshot waits on the merged stream for the one host until it is
// listed or has failed. Not ok when the stream has no such host.
func mergedSnapshot(ctx context.Context, c *client.Conn, h peer.Host, needCap string) (hello, snap protocol.Message, ok bool, err error) {
	m := merged.New()
	waiting, err := m.Read(ctx, c, snapshotTimeout, func(waiting []string) bool { return !slices.Contains(waiting, h.Name) })
	if err != nil {
		return hello, snap, true, err
	}
	for _, n := range waiting {
		if n == h.Name {
			return hello, snap, true, fmt.Errorf("%s: no snapshot from the local daemon after %s", h.Name, snapshotTimeout)
		}
	}
	hello, snap, ok, err = m.HostSnapshot(h.Name)
	if !ok || err != nil {
		return hello, snap, ok, err
	}
	return hello, snap, true, needCaps(h, hello, needCap)
}

// needCaps checks the hello for status and the capability the command
// needs.
func needCaps(h peer.Host, hello protocol.Message, needCap string) error {
	for _, cap := range []string{protocol.CapStatus, needCap} {
		if cap != "" && !protocol.Has(hello.Capabilities, cap) {
			return fmt.Errorf("%s: daemon %s does not support %s", h.Name, hello.Version, cap)
		}
	}
	return nil
}

// findWorktree returns the record for a branch of a repository, by source:
// the record carries the daemon's label, which may differ from this
// machine's for the same source. Two clones of one repository can each
// have a worktree for the branch; that is an error naming both roots
// rather than a guess. A record whose branch is only shown, one laatmux
// cannot carry checked out by hand, is no branch to name: what matches
// it is the shown form, and that is an error naming the root, which rm
// --root takes. So is a record with U+FFFD in its branch from an older
// daemon, which sends no flag: it is such a branch, mangled.
func findWorktree(ws []protocol.Worktree, repo config.Repo, branch string) (protocol.Worktree, bool, error) {
	var found []protocol.Worktree
	for _, w := range ws {
		if w.Branch == branch && branch != "" && source.Same(w.Source, repo.Source) {
			found = append(found, w)
		}
	}
	for _, w := range found {
		if w.BranchDisplayOnly || worktree.CheckWire(w.Branch) != nil {
			return protocol.Worktree{}, false, fmt.Errorf("%s is how the host shows the branch of the worktree at %s; laatmux cannot carry the branch's name, which is not valid UTF-8 or has U+FFFD, so no command names the worktree by it: rm takes the root with --root", tmux.Printable(repo.Name+"/"+branch), tmux.Printable(w.Root))
		}
	}
	switch len(found) {
	case 0:
		return protocol.Worktree{}, false, nil
	case 1:
		return found[0], true, nil
	}
	roots := make([]string, len(found))
	for i, w := range found {
		roots[i] = tmux.Printable(w.Root)
	}
	sort.Strings(roots)
	return protocol.Worktree{}, false, fmt.Errorf("%s has worktrees at %s, in two clones of the repository", tmux.Printable(repo.Name+"/"+branch), strings.Join(roots, " and "))
}

// findRecord is findWorktree over the worktrees, then over the main
// checkouts, which another clone of the repository can have on the
// branch a worktree is for: path prints a main checkout's root, and rm
// and run refuse it (onMain).
func findRecord(ws []protocol.Worktree, repo config.Repo, branch string) (protocol.Worktree, bool, error) {
	worktrees, mains := splitMains(ws)
	if w, ok, err := findWorktree(worktrees, repo, branch); ok || err != nil {
		return w, ok, err
	}
	// A main checkout is named by the branch it has as shown: no
	// command takes its root, so findWorktree's word about --root does
	// not hold for it.
	var found []protocol.Worktree
	for _, w := range mains {
		if w.Branch == branch && branch != "" && source.Same(w.Source, repo.Source) {
			found = append(found, w)
		}
	}
	switch len(found) {
	case 0:
		return protocol.Worktree{}, false, nil
	case 1:
		return found[0], true, nil
	}
	return protocol.Worktree{}, false, twoMains(tmux.Printable(repo.Name+"/"+branch), found)
}

// twoMains says the branch a target names is checked out in the main
// checkouts of two clones of the repository, which no label tells
// apart: both clones of a repository the config lists carry its name.
func twoMains(target string, found []protocol.Worktree) error {
	roots := make([]string, len(found))
	for i, w := range found {
		roots[i] = tmux.Printable(w.Root)
	}
	sort.Strings(roots)
	n := len(roots)
	clones, at := "two clones", strings.Join(roots, " and ")
	if n > 2 {
		clones, at = strconv.Itoa(n)+" clones", strings.Join(roots[:n-1], ", ")+" and "+roots[n-1]
	}
	return fmt.Errorf("%s is checked out in the main checkouts at %s, %s of the repository", target, at, clones)
}

// onMain is rm's and run's refusal of a main checkout, which git keeps
// and run's root is not: what names it, its host and its root, and what
// the command takes instead.
func onMain(repo config.Repo, w protocol.Worktree, host, takes string) error {
	return fmt.Errorf("%s on %s is the main checkout, at %s; %s", tmux.Printable(repo.Name+"/"+w.Branch), host, tmux.Printable(w.Root), takes)
}

// noWorktree is run's and path's error for a branch with no worktree on
// the host, <repo>/<branch> as tmux.Printable shows it: git takes a C1
// control character and a byte that is not UTF-8 in a branch.
func noWorktree(repo config.Repo, branch, host string) error {
	return fmt.Errorf("no worktree for %s on %s", tmux.Printable(repo.Name+"/"+branch), host)
}
