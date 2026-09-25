package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"

	"github.com/laat/laatmux/internal/client"
	"github.com/laat/laatmux/internal/config"
	"github.com/laat/laatmux/internal/home"
	"github.com/laat/laatmux/internal/protocol"
)

// snapshotTimeout bounds a snapshot request: the daemon answers once its
// first poll is complete, which over ssh includes starting it.
const snapshotTimeout = 20 * time.Second

// splitRepoBranch parses <repo>/<branch>. Labels cannot contain "/", so
// the first component is the repository and the rest is the branch,
// slashes included.
func splitRepoBranch(target string) (repo, branch string, err error) {
	repo, branch, ok := strings.Cut(target, "/")
	if !ok || repo == "" || branch == "" {
		return "", "", fmt.Errorf("%q is not <repo>/<branch>", target)
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
		return config.Repo{}, fmt.Errorf("%s has origin %s, which is not a configured repository; use --repo (configured: %s)", cwd, origin, repoList(cfg))
	}
	if label, ok := labelUnder(cfg, cwd); ok {
		if r, ok := cfg.RepoByName(label); ok {
			return r, nil
		}
	}
	return config.Repo{}, fmt.Errorf("%s is not inside a known repository; use --repo (configured: %s)", cwd, repoList(cfg))
}

// labelUnder is the path component after the local host's repos or
// worktrees directory when dir is under one of them. The more specific
// directory is tried first: with worktrees nested under repos, a
// worktree's label is the component after worktrees, not "worktrees".
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
	bases := []string{d.Repos, d.Worktrees}
	if len(d.Worktrees) > len(d.Repos) {
		bases = []string{d.Worktrees, d.Repos}
	}
	for _, base := range bases {
		if rest, ok := strings.CutPrefix(dir+"/", base+"/"); ok {
			label, _, _ := strings.Cut(rest, "/")
			return label, label != ""
		}
	}
	return "", false
}

// originOf is the git origin of the repository dir is in, "" when git
// positively reports none: the key is unset (exit 1) or dir is in no
// repository (exit 128 with git's message). Anything else, git missing or
// a repository it cannot read, is an error, so a directory whose identity
// cannot be inspected is never resolved from its label instead.
func originOf(ctx context.Context, dir string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", "-C", dir, "config", "--get", "remote.origin.url")
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
		case exit.ExitCode() == 128 && strings.Contains(stderr.String(), "not a git repository"):
			return "", nil
		}
	}
	msg := strings.TrimSpace(stderr.String())
	if msg == "" {
		msg = err.Error()
	}
	return "", fmt.Errorf("%s: cannot read git origin: %s", dir, msg)
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
// hold worktrees.
func hostFor(cfg config.Config, flag string, repo config.Repo) (config.Host, home.LastRepo, error) {
	last, err := home.ReadLast()
	if err != nil {
		return config.Host{}, home.LastRepo{}, err
	}
	lr := last.Get(repo.Source)
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
func snapshot(ctx context.Context, h client.Host, needCap string) (hello, snap protocol.Message, err error) {
	if c, ok := dialMerged(ctx); ok {
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
func mergedSnapshot(ctx context.Context, c *client.Conn, h client.Host, needCap string) (hello, snap protocol.Message, ok bool, err error) {
	m := newMerged()
	pending, err := m.readMerged(ctx, c, snapshotTimeout, func(m *merged) bool {
		st, ok := m.hosts[h.Name]
		return !ok || st.ready()
	})
	if err != nil {
		return hello, snap, true, err
	}
	for _, n := range pending {
		if n == h.Name {
			return hello, snap, true, fmt.Errorf("%s: no snapshot from the local daemon after %s", h.Name, snapshotTimeout)
		}
	}
	hello, snap, ok, err = m.hostSnapshot(h.Name)
	if !ok || err != nil {
		return hello, snap, ok, err
	}
	return hello, snap, true, needCaps(h, hello, needCap)
}

// needCaps checks the hello for status and the capability the command
// needs.
func needCaps(h client.Host, hello protocol.Message, needCap string) error {
	for _, cap := range []string{protocol.CapStatus, needCap} {
		if cap != "" && !protocol.Has(hello.Capabilities, cap) {
			return fmt.Errorf("%s: daemon %s does not support %s", h.Name, hello.Version, cap)
		}
	}
	return nil
}

// findWorktree returns the record for a branch of a repository, by source:
// the record carries the daemon's label, which may differ from this
// machine's for the same source. A record from a daemon that does not
// carry the source is matched by label instead. Two clones of one
// repository can each have a worktree for the branch; that is an error
// naming both roots rather than a guess.
func findWorktree(ws []protocol.Worktree, repo config.Repo, branch string) (protocol.Worktree, bool, error) {
	var found []protocol.Worktree
	for _, w := range ws {
		if w.Branch == branch && branch != "" && sameRepo(w, repo) {
			found = append(found, w)
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
		roots[i] = w.Root
	}
	sort.Strings(roots)
	return protocol.Worktree{}, false, fmt.Errorf("%s/%s has worktrees at %s; two clones of the repository each have the branch, so name the worktree by its root", repo.Name, branch, strings.Join(roots, " and "))
}

// recordRepo is this machine's entry for a host record's source, with
// the source spelled as the record has it: a request about the record
// names the repository as the host does, which an older host, comparing
// sources as strings, needs.
func recordRepo(cfg config.Config, source string) (config.Repo, bool) {
	r, ok := cfg.RepoBySource(source)
	if ok {
		r.Source = source
	}
	return r, ok
}

// sameRepo reports whether the record is of the repository: by source
// when the record has one, else by label.
func sameRepo(w protocol.Worktree, repo config.Repo) bool {
	if w.Source != "" {
		return config.SameSource(w.Source, repo.Source)
	}
	return w.Repo == repo.Name
}
