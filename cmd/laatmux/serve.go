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

	"github.com/laat/laatmux/internal/daemon"
	"github.com/laat/laatmux/internal/home"
)

func cmdServe(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	listen := fs.String("listen", "unix:"+home.DefaultSocket(), "unix:<path> or tcp:<host:port>")
	sock := tmuxServerFlag(fs)
	interval := fs.Duration("interval", daemon.DefaultInterval, "poll interval")
	lines := fs.Int("capture-lines", daemon.DefaultCapture, "screen lines captured per pane")
	if err := fs.Parse(args); err != nil {
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
	hostname, _ := os.Hostname()
	rt := home.Runtime{Address: network + ":" + ln.Addr().String(), PID: os.Getpid(), Version: version, EnvironmentID: envID, StartedAt: time.Now()}
	if err := home.WriteRuntime(rt); err != nil {
		return err
	}
	defer home.RemoveRuntime(os.Getpid())

	logger := log.New(os.Stderr, "laatmux ", log.LstdFlags)
	logger.Printf("serve %s env=%s tmux=%q listen=%s", version, envID, *sock, rt.Address)
	d := daemon.New(daemon.Config{
		Server: parseServer(*sock), Interval: *interval, CaptureLines: *lines,
		EnvironmentID: envID, Host: hostname, Version: version, Logger: logger,
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
