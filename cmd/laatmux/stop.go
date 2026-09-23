package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"syscall"
	"time"

	"github.com/laat/laatmux/internal/client"
	"github.com/laat/laatmux/internal/home"
	"github.com/laat/laatmux/internal/protocol"
)

// stopWait bounds how long stop waits for the daemon to exit: a clean
// shutdown cancels its runs, SIGTERM then SIGKILL five seconds later,
// and waits for them a few seconds more.
const stopWait = 20 * time.Second

// cmdStop ends this machine's daemon cleanly and waits for it to exit;
// the next client starts one again, which after an upgrade is the new
// build. The daemon is reached over its socket, without starting one,
// and asked to shut down, so the process that ends is the one that
// answered the hello: never a pid a file remembers, which a crash can
// leave for another process to inherit. The runtime record is read once
// and its address dialled, and the daemon that answers must be the one
// the record names, which its hello's pid says; a socket path is reused
// by the next daemon, so a record read just before its daemon left can
// reach the replacement, and then the record is read again. A daemon
// from before the shutdown message has no pid in its hello and gets
// SIGTERM at the record's pid, the daemon that answered on the record's
// address. A daemon that holds the startup lock but answers on no
// socket is starting, the lock comes before the listener, or shutting
// down, the listener goes before the runs are stopped: stop keeps
// trying to reach it while that holder has the lock. The wait after
// the request is for the lock to leave the daemon's hands, released
// when it exits, reaped or not, or taken by a replacement. No daemon
// running is not an error.
func cmdStop(ctx context.Context, args []string) error {
	if len(args) > 0 {
		return errors.New("usage: laatmux stop")
	}
	deadline := time.Now().Add(stopWait)
	for {
		rt, err := home.ReadRuntime()
		if err != nil && !errors.Is(err, os.ErrNotExist) && !errors.Is(err, home.ErrStale) {
			return err
		}
		if err == nil {
			if nc, err := client.DialAddress(rt.Address); err == nil {
				err := stopDaemon(ctx, nc, rt)
				if !errors.Is(err, errMoved) {
					return err
				}
				continue
			}
		}
		holder, err := home.Holder()
		if err != nil {
			return err
		}
		if holder == 0 {
			fmt.Println("no daemon running")
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("daemon (pid %d) holds the lock but answers on no socket after %s", holder, stopWait)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// errMoved is a daemon reached on a record's address that is not the
// daemon the record names.
var errMoved = errors.New("the daemon that answered is not the one the runtime record names")

// stopDaemon ends the daemon on nc, the one the runtime record rt was
// read for, and waits for it to be gone. A hello whose pid is another
// daemon's is errMoved; a hello without a pid, from a daemon before the
// field, is taken as the record's when the record still stands.
func stopDaemon(ctx context.Context, nc net.Conn, rt home.Runtime) error {
	c, err := client.Connect(ctx, client.Host{Name: "local"}, nc, nc, func() { nc.Close() })
	if err != nil {
		return err
	}
	defer c.Close()
	switch {
	case c.Hello.PID != 0 && c.Hello.PID != rt.PID:
		return errMoved
	case c.Hello.PID == 0:
		if now, err := home.ReadRuntime(); err != nil || now.PID != rt.PID {
			return errMoved
		}
	}
	what := fmt.Sprintf("%s (pid %d)", c.Hello.Version, rt.PID)
	if protocol.Has(c.Hello.Capabilities, protocol.CapShutdown) {
		rctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		if _, err := c.Request(rctx, protocol.Message{Type: protocol.TypeShutdown}); err != nil {
			return fmt.Errorf("shutdown daemon %s: %w", what, err)
		}
	} else if err := syscall.Kill(rt.PID, syscall.SIGTERM); err != nil && !errors.Is(err, syscall.ESRCH) {
		return fmt.Errorf("stop daemon %s: %w", what, err)
	}
	return waitGone(ctx, rt.PID, what)
}

// waitGone waits for the startup lock to leave the daemon's hands, then
// says the daemon is stopped.
func waitGone(ctx context.Context, pid int, what string) error {
	deadline := time.Now().Add(stopWait)
	for {
		holder, err := home.Holder()
		if err != nil {
			return err
		}
		if holder != pid {
			break
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("daemon %s still running after %s", what, stopWait)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
	fmt.Printf("stopped daemon %s\n", what)
	return nil
}
