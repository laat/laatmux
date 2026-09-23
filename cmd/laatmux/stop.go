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
// leave for another process to inherit. A daemon from before the
// shutdown message gets SIGTERM at the pid of the runtime record that
// was dialled, read once, so the record's address and pid are one
// daemon's. The wait is for the startup lock to leave that daemon's
// hands, released when it exits, reaped or not, or taken by a
// replacement that a client started meanwhile. A daemon that holds the
// lock but answers on no socket is shutting down already, its listener
// goes before its runs are stopped, and is waited for the same way. No
// daemon running is not an error.
func cmdStop(ctx context.Context, args []string) error {
	if len(args) > 0 {
		return errors.New("usage: laatmux stop")
	}
	rt, err := home.ReadRuntime()
	if err != nil && !errors.Is(err, os.ErrNotExist) && !errors.Is(err, home.ErrStale) {
		return err
	}
	var nc net.Conn
	if err == nil {
		nc, err = client.DialAddress(rt.Address)
	}
	if err != nil {
		holder, herr := home.Holder()
		if herr != nil {
			return herr
		}
		if holder == 0 {
			fmt.Println("no daemon running")
			return nil
		}
		return waitGone(ctx, holder, fmt.Sprintf("pid %d, shutting down", holder))
	}
	return stopDaemon(ctx, nc, rt)
}

// stopDaemon ends the daemon on nc, the one the runtime record rt was
// read for, and waits for it to be gone.
func stopDaemon(ctx context.Context, nc net.Conn, rt home.Runtime) error {
	c, err := client.Connect(ctx, client.Host{Name: "local"}, nc, nc, func() { nc.Close() })
	if err != nil {
		return err
	}
	defer c.Close()
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
