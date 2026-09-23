package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"syscall"
	"time"

	"github.com/laat/laatmux/internal/home"
)

// stopWait bounds how long stop waits for the daemon to exit: a clean
// shutdown cancels its runs, SIGTERM then SIGKILL five seconds later,
// and waits for them a few seconds more.
const stopWait = 20 * time.Second

// cmdStop ends this machine's daemon cleanly, SIGTERM as a service
// manager would send it, and waits for it to exit; the next client
// starts one again, which after an upgrade is the new build. No daemon
// running is not an error.
func cmdStop(ctx context.Context, args []string) error {
	if len(args) > 0 {
		return errors.New("usage: laatmux stop")
	}
	rt, err := home.ReadRuntime()
	if err != nil {
		if errors.Is(err, os.ErrNotExist) || errors.Is(err, home.ErrStale) {
			fmt.Println("no daemon running")
			return nil
		}
		return err
	}
	if err := syscall.Kill(rt.PID, syscall.SIGTERM); err != nil {
		return fmt.Errorf("stop daemon %d: %w", rt.PID, err)
	}
	deadline := time.Now().Add(stopWait)
	for home.Alive(rt.PID) {
		if time.Now().After(deadline) {
			return fmt.Errorf("daemon %s (pid %d) still running after %s", rt.Version, rt.PID, stopWait)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
	fmt.Printf("stopped daemon %s (pid %d)\n", rt.Version, rt.PID)
	return nil
}
