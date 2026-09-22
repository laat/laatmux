package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"strings"
	"time"

	"github.com/laat/laatmux/internal/config"
	"github.com/laat/laatmux/internal/daemon"
	"github.com/laat/laatmux/internal/home"
	"github.com/laat/laatmux/internal/worktree"
)

func cmdServe(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	listen := fs.String("listen", "unix:"+home.DefaultSocket(), "unix:<path> or tcp:<host:port>")
	// The servers to watch come from the config file by default. The flag
	// overrides it with a comma-separated list; --tmux-socket is the older
	// spelling and takes the same value.
	var servers string
	fs.StringVar(&servers, "tmux-servers", "", `comma-separated tmux servers to watch (a -L name, "default", or a -S path); default from config, else laatmux`)
	fs.StringVar(&servers, "tmux-socket", "", "alias of --tmux-servers")
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
	if servers != "" {
		specs = nil
		for _, v := range strings.Split(servers, ",") {
			specs = append(specs, strings.TrimSpace(v))
		}
	}
	watched, err := config.ParseServers(specs)
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
		return err
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
	agents := map[string][]string{}
	if local, ok := cfg.Local(); ok {
		hostname = local.Name
		if dirs, err := local.Dirs(); err == nil {
			store = worktree.New(dirs.Expand(), cfg.Repos)
		}
	}
	for name, a := range cfg.Agents {
		agents[name] = a.Cmd
	}
	rt := home.Runtime{Address: network + ":" + ln.Addr().String(), PID: os.Getpid(), Version: version, EnvironmentID: envID, StartedAt: time.Now()}
	if err := home.WriteRuntime(rt); err != nil {
		return err
	}
	defer home.RemoveRuntime(os.Getpid())

	logger := log.New(os.Stderr, "laatmux ", log.LstdFlags)
	labels := make([]string, len(watched))
	for i, s := range watched {
		labels[i] = s.Label()
	}
	logger.Printf("serve %s env=%s tmux=%s listen=%s", version, envID, strings.Join(labels, ","), rt.Address)
	if store != nil {
		logger.Printf("worktrees: repos=%s worktrees=%s known=%d", store.Dirs.Repos, store.Dirs.Worktrees, len(store.Repos))
	} else {
		logger.Printf("worktrees: host %s has no repos and worktrees directories configured; add disabled", hostname)
	}
	d := daemon.New(daemon.Config{
		Targets: daemon.Targets(watched...), Interval: *interval, CaptureLines: *lines,
		EnvironmentID: envID, Host: hostname, Version: version, Logger: logger,
		Store: store, Agents: agents,
	})
	errc := make(chan error, 2)
	go func() { errc <- d.Run(ctx) }()
	go func() { errc <- d.Serve(ctx, ln) }()
	select {
	case <-ctx.Done():
		return nil
	case err := <-errc:
		return err
	}
}
