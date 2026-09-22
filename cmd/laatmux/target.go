package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
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
	if out, err := exec.CommandContext(ctx, "git", "-C", cwd, "config", "--get", "remote.origin.url").Output(); err == nil {
		origin := strings.TrimSpace(string(out))
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
// worktrees directory when dir is under one of them.
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
	for _, base := range []string{d.Repos, d.Worktrees} {
		if rest, ok := strings.CutPrefix(dir+"/", base+"/"); ok {
			label, _, _ := strings.Cut(rest, "/")
			return label, label != ""
		}
	}
	return "", false
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

// snapshot dials the host and returns the daemon's hello and its first
// snapshot. The connection is closed; commands open their own.
func snapshot(ctx context.Context, h client.Host, needCap string) (hello, snap protocol.Message, err error) {
	c, err := client.Dial(ctx, h)
	if err != nil {
		return hello, snap, err
	}
	defer c.Close()
	for _, cap := range []string{protocol.CapStatus, needCap} {
		if cap != "" && !protocol.Has(c.Hello.Capabilities, cap) {
			return hello, snap, fmt.Errorf("%s: daemon %s does not support %s", h.Name, c.Hello.Version, cap)
		}
	}
	sctx, cancel := context.WithTimeout(ctx, snapshotTimeout)
	defer cancel()
	snap, err = c.Snapshot(sctx)
	if err != nil {
		return hello, snap, fmt.Errorf("%s: %w", h.Name, err)
	}
	return c.Hello, snap, nil
}

// findWorktree returns the record for a branch of a repository, by the
// host's label for it.
func findWorktree(ws []protocol.Worktree, repo, branch string) (protocol.Worktree, bool) {
	for _, w := range ws {
		if w.Repo == repo && w.Branch == branch && branch != "" {
			return w, true
		}
	}
	return protocol.Worktree{}, false
}

// stream sends a command with progress to the host and returns its result,
// dialing again with the same id when the transport fails mid-way: the
// daemon replays what it already sent and follows, and onProgress is only
// called for messages not seen before. The hello of the connection that
// delivered the result is returned with it.
func stream(ctx context.Context, h client.Host, needCap string, m protocol.Message, onProgress func(protocol.Message)) (hello, res protocol.Message, err error) {
	f := &replayFilter{fn: onProgress}
	const attempts = 3
	for attempt := 1; ; attempt++ {
		f.reset()
		c, err := client.Dial(ctx, h)
		if err != nil {
			// A redial after a started attempt is a transport failure
			// like any other and spends the same budget.
			if attempt == 1 || attempt == attempts || ctx.Err() != nil {
				return hello, res, err
			}
			fmt.Fprintf(os.Stderr, "laatmux: %s: reconnect failed (%v); retrying\n", h.Name, err)
			if err := pause(ctx); err != nil {
				return hello, res, err
			}
			continue
		}
		if !protocol.Has(c.Hello.Capabilities, needCap) {
			c.Close()
			return hello, res, fmt.Errorf("%s: daemon %s does not support %s", h.Name, c.Hello.Version, needCap)
		}
		hello = c.Hello
		res, err = c.Stream(ctx, m, f.pass)
		c.Close()
		// A result, ok or not, ends it: Stream returns the daemon's
		// refusals with the result message. Only a transport failure,
		// which has no message, is retried.
		if err == nil || res.Type != "" || ctx.Err() != nil || attempt == attempts {
			return hello, res, err
		}
		fmt.Fprintf(os.Stderr, "laatmux: %s: connection lost (%v); reconnecting to follow %s\n", h.Name, err, m.Type)
		if err := pause(ctx); err != nil {
			return hello, res, err
		}
	}
}

// pause waits a second between attempts, or returns when ctx ends.
func pause(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(time.Second):
		return nil
	}
}

// replayFilter passes each progress message on once across reconnects:
// the daemon replays a command's stream from the start, so messages are
// counted per connection and only those past the high-water mark are new.
type replayFilter struct {
	seen, n int
	fn      func(protocol.Message)
}

func (f *replayFilter) reset() { f.n = 0 }

func (f *replayFilter) pass(p protocol.Message) {
	f.n++
	if f.n > f.seen {
		f.seen = f.n
		if f.fn != nil {
			f.fn(p)
		}
	}
}
