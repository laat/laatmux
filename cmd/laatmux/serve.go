package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/laat/laatmux/internal/config"
	"github.com/laat/laatmux/internal/daemon"
	"github.com/laat/laatmux/internal/github"
	"github.com/laat/laatmux/internal/home"
	"github.com/laat/laatmux/internal/peer"
	"github.com/laat/laatmux/internal/procs"
	"github.com/laat/laatmux/internal/protocol"
	"github.com/laat/laatmux/internal/tmux"
	"github.com/laat/laatmux/internal/workspace"
	"github.com/laat/laatmux/internal/worktree"
)

func cmdServe(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	listen := fs.String("listen", "unix:"+home.DefaultSocket(), "unix:<path> or tcp:<host:port>")
	// The servers to watch come from the config file by default. The flag
	// overrides it with a comma-separated list.
	servers := fs.String("tmux-servers", "", `comma-separated tmux servers to watch (a -L name, "default", or a -S path); default from config, else laatmux`)
	interval := fs.Duration("interval", daemon.DefaultInterval, "poll interval")
	lines := fs.Int("capture-lines", daemon.DefaultCapture, "screen lines captured per pane")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	specs := cfg.TmuxServers
	if *servers != "" {
		specs = nil
		for _, v := range strings.Split(*servers, ",") {
			specs = append(specs, strings.TrimSpace(v))
		}
	}
	watched, err := tmux.ParseServers(specs)
	if err != nil {
		return err
	}
	lock, err := home.TryLock()
	if err != nil {
		return err
	}
	defer lock.Release()
	envID, err := home.EnvironmentID()
	if err != nil {
		return err
	}
	network, addr, ok := strings.Cut(*listen, ":")
	if !ok {
		return fmt.Errorf("bad --listen %q", *listen)
	}
	if network == "unix" {
		os.Remove(addr)
	}
	ln, err := net.Listen(network, addr)
	if err != nil {
		// A unix socket's error names its path, under the state
		// directory by default, as it is.
		return tmux.PrintablePath(err)
	}
	defer ln.Close()
	if network == "unix" {
		os.Chmod(addr, 0o600)
		defer os.Remove(addr)
	}
	// This host's own entry names it and says where its checkouts and
	// worktrees go. Without directories there are no worktree records and
	// no add; the log says so once.
	hostname, _ := os.Hostname()
	var store *worktree.Store
	if local, ok := cfg.Local(); ok {
		hostname = local.Name
		if dirs, err := local.Dirs(); err == nil {
			store = worktree.New(dirs.Expand(), nil)
			store.SetListed(worktree.ListedFrom(cfg))
		}
	}
	rt := home.Runtime{Address: network + ":" + ln.Addr().String(), PID: os.Getpid(), Version: version, EnvironmentID: envID, StartedAt: time.Now()}
	if self, ok := procs.Lookup(os.Getpid()); ok {
		rt.ProcessStart = self.StartID
	}
	logger := log.New(os.Stderr, "laatmux ", log.LstdFlags)
	if store != nil {
		// Two checkouts whose names make one label are logged once.
		store.Log = logger
	}
	labels := make([]string, len(watched))
	for i, s := range watched {
		labels[i] = tmux.Printable(s.Label())
	}
	logger.Printf("serve %s env=%s tmux=%s listen=%s", version, envID, strings.Join(labels, ","), tmux.Printable(rt.Address))
	if store != nil {
		repos := make([]string, len(store.Dirs.Repos))
		for i, r := range store.Dirs.Repos {
			repos[i] = tmux.Printable(r)
		}
		logger.Printf("worktrees: repos=%s worktrees=%s known=%d", strings.Join(repos, ","), tmux.Printable(store.Dirs.Worktrees), len(store.Repos()))
	} else {
		logger.Printf("worktrees: host %s has no repos and worktrees directories configured; add disabled", hostname)
	}
	reread := configReread()
	// The shutdown message ends the daemon the way a signal does.
	ctx, shutdown := context.WithCancel(ctx)
	defer shutdown()
	d := daemon.New(daemon.Config{
		Targets: daemon.Targets(watched...), Interval: *interval, CaptureLines: *lines,
		EnvironmentID: envID, Host: hostname, Version: version, Logger: logger,
		Store: store, Reread: reread, Agents: agentCommands(cfg), Shutdown: shutdown,
		Commands: filepath.Join(home.Dir(), "commands"),
		Pending:  filepath.Join(home.Dir(), "pending"),
		// The images paste-image sends from the laptop's clipboard.
		Paste: filepath.Join(home.Dir(), "paste"),
		// What the user has seen, kept across restarts, and what this
		// machine's tmux clients show.
		Attention: filepath.Join(home.Dir(), "attention.json"),
		Clients:   listClients,
		// PR and check state through gh, kept across restarts.
		GitHub:      github.GH,
		Branches:    filepath.Join(home.Dir(), "branches.json"),
		GitHubHosts: cfg.GitHubHosts,
		// The merged stream: the hosts are re-read from the file on every
		// merged subscription and when the file changes, and the local
		// sessions listed from the default server.
		Hosts:    configHosts,
		Sessions: localSessions,
	})
	// A daemon New could not finish, its pending directory unopenable
	// say, ends here, before it is announced: the runtime file is not
	// written and no client is served a hello without the relay.
	if err := d.Err(); err != nil {
		return err
	}
	if err := home.WriteRuntime(rt); err != nil {
		return err
	}
	defer home.RemoveRuntime(os.Getpid())
	errc := make(chan error, 2)
	go func() { errc <- d.Run(ctx) }()
	go func() { errc <- d.Serve(ctx, ln) }()
	// Whichever way serve ends, a signal or a failure, the runs are
	// stopped the way cancel stops them, so a restart for an upgrade
	// leaves no orphan; the wait is bounded by the kill delay plus a
	// margin.
	defer func() {
		sctx, cancel := context.WithTimeout(context.Background(), daemon.DefaultKillDelay+5*time.Second)
		defer cancel()
		d.StopRuns(sctx)
	}()
	select {
	case <-ctx.Done():
		return nil
	case err := <-errc:
		if ctx.Err() != nil {
			return nil
		}
		return err
	}
}

// configReread is the daemon's read of this machine's config file, the
// store's, the adds' and the relay's: the file is looked at every
// worktree interval and read again when it has changed, so a
// repository edited in is labelled by its entry without a restart, and
// a copy rule, a repository's setup or an agent added or edited is used
// by the next add.
func configReread() func() (daemon.ConfigRead, bool, error) {
	var watch config.Watch
	return func() (daemon.ConfigRead, bool, error) {
		cfg, changed, err := watch.Changed()
		if !changed || err != nil {
			return daemon.ConfigRead{}, false, err
		}
		return daemon.ConfigRead{Listed: worktree.ListedFrom(cfg), Agents: agentCommands(cfg)}, true, nil
	}
}

// configHosts is the daemon's read of the hosts the config lists. A
// file read empty while it is being written, an editor saving it in
// place say, is config.ErrWriting, and the daemon keeps the hosts it
// has, rather than taking the default config's lone local host and
// dropping every other from the views.
func configHosts() ([]peer.Host, error) {
	cfg, err := config.LoadSettled()
	if err != nil {
		return nil, err
	}
	hosts := make([]peer.Host, 0, len(cfg.Hosts))
	for _, h := range cfg.Hosts {
		hosts = append(hosts, h.Host)
	}
	return hosts, nil
}

// agentCommands is the config's agents as the daemon starts them: the
// command by label.
func agentCommands(cfg config.Config) map[string][]string {
	agents := make(map[string][]string, len(cfg.Agents))
	for name, a := range cfg.Agents {
		agents[name] = a.Cmd
	}
	return agents
}

// localSessions is the daemon's listing of this machine's workspace
// sessions and plain attachments, on the default server. A listing a
// user's after-list-sessions hook failed after has its sessions, with
// the *tmux.HookError; any other failure has none.
func localSessions(ctx context.Context) ([]protocol.Session, error) {
	locals, err := workspace.List(ctx)
	return workspace.Records(locals), err
}
