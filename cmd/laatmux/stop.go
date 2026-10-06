package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"syscall"
	"time"

	"github.com/laat/laatmux/internal/client"
	"github.com/laat/laatmux/internal/home"
	"github.com/laat/laatmux/internal/peer"
	"github.com/laat/laatmux/internal/procs"
	"github.com/laat/laatmux/internal/protocol"
)

// stopWait bounds how long stop waits for the daemon to exit: a clean
// shutdown cancels its runs, SIGTERM then SIGKILL five seconds later,
// and waits for them a few seconds more. A variable so a test can
// shorten it.
var stopWait = 20 * time.Second

// cmdStop ends this machine's daemon cleanly and waits for it to exit;
// the next client starts one again, which after an upgrade is the new
// build. The daemon is reached over its socket, without starting one,
// and asked to shut down, so the process that ends is the one that
// answered the hello: not a pid a file remembers, which a crash can
// leave for another process to inherit, but for the one exception
// below. The runtime record is read once and its address dialled, and
// the daemon that answers must be the one the record names, which its
// hello's pid says; a socket path is reused by the next daemon, so a
// record read just before its daemon left can reach the replacement,
// and then the record is read again. A daemon from before the shutdown
// message has no pid in its hello and gets SIGTERM at the record's
// pid, the daemon that answered on the record's address.
//
// A daemon that holds the startup lock but cannot be asked is
// starting, the lock comes before the listener and the record, or it
// is the record's daemon and is wedged (nothing listens at the record's
// address, the hello never comes, or it is of another protocol) or
// shutting down (its listener goes before its runs are stopped, the
// record standing). The first is kept being tried while it holds the
// lock. The second gets SIGTERM, the exception: that the pid is that
// daemon's process is established as an agent's identity is, by pid
// and the start the kernel keeps for the process, which the daemon
// wrote into its record; a pid reused since the daemon died is a
// process with another start, whatever the files say. The lock file
// naming the pid on two readings a moment apart (a probe holding the
// free lock for an instant, or a daemon between taking the lock and
// rewriting the file, shows the previous content) is the cheap check
// before it. This build's daemon ignores the SIGTERM while it shuts
// down, since main keeps the signal caught until serve returns.
// The wait after the request is for the lock to leave the daemon's
// hands, released when it exits, reaped or not, or taken by a
// replacement. No daemon running is not an error.
func cmdStop(ctx context.Context, args []string) error {
	if len(args) > 0 {
		return errors.New("usage: laatmux stop")
	}
	start := time.Now()
	deadline := start.Add(stopWait)
	// What the last round found, for the report when the deadline
	// passes: whether the record stands and names the daemon the loop
	// is after, and why it could not be stopped.
	named, why := false, ""
	answered := false // the round's dial reached a daemon and had its answer
	var rt home.Runtime
	lastPID := 0 // the record's pid the round before, for a reason carried over
	for {
		if time.Now().After(deadline) {
			holder, err := lockHolder()
			if err != nil {
				return err
			}
			if holder == 0 {
				fmt.Println("no daemon running")
				return nil
			}
			took := time.Since(start).Round(time.Second)
			if named && holder != rt.PID {
				return fmt.Errorf("daemon (pid %d) holds the lock while the runtime record's (pid %d) %s, after %s", holder, rt.PID, why, took)
			}
			if named && rt.ProcessStart == "" {
				return fmt.Errorf("daemon (pid %d) holds the lock but %s after %s; its runtime record is from a build before process_start, or its daemon could not read its own start, so stop does not signal it: if ps shows pid %d as laatmux serve, kill %d ends it", holder, why, took, holder, holder)
			}
			return fmt.Errorf("daemon (pid %d) holds the lock but %s after %s", holder, why, took)
		}
		var err error
		rt, err = home.ReadRuntime()
		if err != nil && !errors.Is(err, os.ErrNotExist) && !errors.Is(err, home.ErrStale) {
			return err
		}
		// Every way round the loop is bounded and paced the same: a
		// record whose daemon is not the one that answers, and a
		// holder that answers on no socket.
		named = err == nil // the record stands and names rt.PID
		last, lastAnswered, samePID := why, answered, rt.PID == lastPID
		why, answered, lastPID = "answers on no socket", false, rt.PID
		if named {
			if nc, err := client.DialAddress(rt.Address); err == nil {
				err := stopDaemon(ctx, nc, rt, deadline)
				switch {
				case errors.Is(err, errMoved):
					why, answered = "is answered by another daemon", true
				case errors.Is(err, errNoHello) && errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil:
					// The loop's deadline passed during the hello wait:
					// reported on the next round for what the round
					// before found, when it had an answer for the same
					// record, or for the hello that never came.
					why = "gives no hello before the wait ran out"
					if lastAnswered && samePID {
						why = last
					}
					continue
				case errors.Is(err, errNoHello):
					why, answered = "gives no hello stop can take ("+strings.TrimPrefix(err.Error(), errNoHello.Error()+": ")+")", true
				default:
					return err
				}
			}
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		holder, err := lockHolder()
		if err != nil {
			return err
		}
		if named && holder == rt.PID && isDaemon(rt) {
			// The record's daemon holds the lock and cannot be asked:
			// read the lock again a moment later, then end it as a
			// daemon before the shutdown message is. Another daemon
			// answering at the record's address changes nothing: the
			// identified holder of this state directory's lock is the
			// daemon to end.
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(100 * time.Millisecond):
			}
			if sent, err := signalDaemon(ctx, rt, why); err != nil {
				return err
			} else if sent {
				return waitGone(ctx, rt.PID, fmt.Sprintf("%s (pid %d)", rt.Version, rt.PID))
			}
		}
		if holder == 0 {
			fmt.Println("no daemon running")
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// signalDaemon sends SIGTERM to the record's daemon when, on this
// reading, it holds the lock and the process is identified as it. The
// signal goes through a handle to the process taken before the identity
// is checked the last time: on Linux a pidfd where the kernel gives one
// (5.3 and later, os.FindProcess falling back to the pid silently where
// it does not), which a reuse of the pid after the check does not
// reach; on macOS the pid, with nothing narrower on offer. A process
// gone by the signal is sent, as waitGone finds the lock free.
func signalDaemon(ctx context.Context, rt home.Runtime, why string) (bool, error) {
	proc, err := os.FindProcess(rt.PID)
	if err != nil {
		return false, err
	}
	defer proc.Release()
	again, err := home.Holder()
	if err != nil {
		return false, err
	}
	if again != rt.PID || !isDaemon(rt) {
		return false, nil
	}
	if ctx.Err() != nil {
		return false, ctx.Err()
	}
	if err := proc.Signal(syscall.SIGTERM); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return false, fmt.Errorf("stop daemon %s (pid %d), which %s: %w", rt.Version, rt.PID, why, err)
	}
	return true, nil
}

// lockHolder is the pid the startup lock's holder wrote into the lock
// file, 0 when the lock is free. A daemon between taking the lock and
// writing its pid, or a lock file being rewritten, reads as no holder
// for an instant; a second reading a moment later tells it from a lock
// that is free.
func lockHolder() (int, error) {
	holder, err := home.Holder()
	if err == nil && holder == 0 {
		time.Sleep(100 * time.Millisecond)
		holder, err = home.Holder()
	}
	return holder, err
}

// isDaemon reports whether the process the runtime record names is the
// daemon that wrote it: the one with the pid and the start the kernel
// keeps for it, which the daemon wrote into the record (ProcessStart)
// as an agent's Identity is its pid and start. A pid reused since the
// daemon died is a process with another start; a record from a build
// before the field names no daemon this way, and is waited out.
func isDaemon(rt home.Runtime) bool {
	p, ok := procs.Lookup(rt.PID)
	return ok && rt.ProcessStart != "" && p.StartID == rt.ProcessStart
}

// errMoved is a daemon reached on a record's address that is not the
// daemon the record names; errNoHello one that accepted the connection
// but gave no hello stop takes: none within the wait, or one of
// another protocol.
var (
	errMoved   = errors.New("the daemon that answered is not the one the runtime record names")
	errNoHello = errors.New("no hello")
)

// stopDaemon ends the daemon on nc, the one the runtime record rt was
// read for, and waits for it to be gone. A hello whose pid is another
// daemon's is errMoved; a hello without a pid, from a daemon before the
// field, is taken as the record's when the record still stands. The
// hello is waited for until deadline at most, the loop's, so a daemon
// that never sends one does not hold stop past it.
func stopDaemon(ctx context.Context, nc net.Conn, rt home.Runtime, deadline time.Time) error {
	hctx, cancel := context.WithDeadline(ctx, deadline)
	c, err := client.Connect(hctx, peer.Host{Name: "local"}, nc, nc, func() { nc.Close() })
	cancel()
	if err != nil {
		return fmt.Errorf("%w: %w", errNoHello, err)
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
