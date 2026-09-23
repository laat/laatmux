package main

import (
	"context"
	"errors"
	"fmt"
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
// starts one again, which after an upgrade is the new build. The daemon
// is the holder of the startup lock, found through it rather than
// through the runtime file: a pid the file remembers can belong to
// another process after a crash, and a daemon that has exited but not
// been reaped has released the lock though its pid is still there. No
// daemon running is not an error.
func cmdStop(ctx context.Context, args []string) error {
	if len(args) > 0 {
		return errors.New("usage: laatmux stop")
	}
	pid, err := home.Holder()
	if err != nil {
		return err
	}
	if pid == 0 {
		fmt.Println("no daemon running")
		return nil
	}
	what := fmt.Sprintf("pid %d", pid)
	if rt, err := home.ReadRuntime(); err == nil && rt.PID == pid && rt.Version != "" {
		what = rt.Version + " (" + what + ")"
	}
	if err := syscall.Kill(pid, syscall.SIGTERM); err != nil && !errors.Is(err, syscall.ESRCH) {
		return fmt.Errorf("stop daemon %s: %w", what, err)
	}
	deadline := time.Now().Add(stopWait)
	for {
		holder, err := home.Holder()
		if err != nil {
			return err
		}
		if holder == 0 {
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
