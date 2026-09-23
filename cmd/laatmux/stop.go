package main

import (
	"context"
	"errors"
	"fmt"
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
// shutdown message gets SIGTERM at the pid its runtime file names,
// which is the daemon that just answered on the socket that file
// names. The wait is for the startup lock to leave that daemon's hands,
// released when it exits, reaped or not, or taken by a replacement
// that a client started meanwhile. No daemon running is not an error.
func cmdStop(ctx context.Context, args []string) error {
	if len(args) > 0 {
		return errors.New("usage: laatmux stop")
	}
	nc, err := client.DialLocal(ctx, false)
	if err != nil {
		fmt.Println("no daemon running")
		return nil
	}
	c, err := client.Connect(ctx, client.Host{Name: "local"}, nc, nc, func() { nc.Close() })
	if err != nil {
		return err
	}
	defer c.Close()
	rt, err := home.ReadRuntime()
	if err != nil {
		return err
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
	deadline := time.Now().Add(stopWait)
	for {
		holder, err := home.Holder()
		if err != nil {
			return err
		}
		if holder != rt.PID {
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
