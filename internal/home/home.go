// Package home locates laatmux's per-host state: the environment id, the
// daemon's runtime file and socket, and the lock used to arbitrate startup.
package home

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Dir is the state directory. LAATMUX_HOME overrides the default
// ~/.local/state/laatmux, which is what a sandboxed dev loop needs.
func Dir() string {
	if v := os.Getenv("LAATMUX_HOME"); v != "" {
		return v
	}
	if v := os.Getenv("XDG_STATE_HOME"); v != "" {
		return filepath.Join(v, "laatmux")
	}
	h, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(os.TempDir(), "laatmux")
	}
	return filepath.Join(h, ".local", "state", "laatmux")
}

func ensure() error { return os.MkdirAll(Dir(), 0o700) }

// EnvironmentID is minted once per host and kept across restarts. Routes to
// a host may change; this id does not.
func EnvironmentID() (string, error) {
	if err := ensure(); err != nil {
		return "", err
	}
	p := filepath.Join(Dir(), "environment-id")
	if b, err := os.ReadFile(p); err == nil {
		if s := strings.TrimSpace(string(b)); len(s) >= 8 {
			return s, nil
		}
	}
	var buf [8]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", err
	}
	id := hex.EncodeToString(buf[:])
	// Publish atomically so two concurrent initializers agree on one winner.
	tmp := p + ".tmp." + strconv.Itoa(os.Getpid())
	if err := os.WriteFile(tmp, []byte(id+"\n"), 0o600); err != nil {
		return "", err
	}
	if err := os.Link(tmp, p); err != nil {
		os.Remove(tmp)
		if b, rerr := os.ReadFile(p); rerr == nil {
			return strings.TrimSpace(string(b)), nil
		}
		return "", err
	}
	os.Remove(tmp)
	return id, nil
}

// Runtime is what a running daemon publishes for local clients.
type Runtime struct {
	Address       string    `json:"address"` // "unix:/path" or "tcp:127.0.0.1:port"
	PID           int       `json:"pid"`
	Version       string    `json:"version"`
	EnvironmentID string    `json:"environment_id"`
	StartedAt     time.Time `json:"started_at"`
}

func runtimePath() string { return filepath.Join(Dir(), "runtime.json") }

// WriteRuntime publishes the runtime file atomically.
func WriteRuntime(r Runtime) error {
	if err := ensure(); err != nil {
		return err
	}
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	tmp := runtimePath() + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, runtimePath())
}

// ReadRuntime returns the runtime file if its daemon is alive.
func ReadRuntime() (Runtime, error) {
	b, err := os.ReadFile(runtimePath())
	if err != nil {
		return Runtime{}, err
	}
	var r Runtime
	if err := json.Unmarshal(b, &r); err != nil {
		return Runtime{}, err
	}
	if !Alive(r.PID) {
		return r, ErrStale
	}
	return r, nil
}

// ErrStale is a runtime file whose daemon is gone.
var ErrStale = errors.New("laatmux: runtime file is stale")

// RemoveRuntime deletes the runtime file if it belongs to pid.
func RemoveRuntime(pid int) {
	r, err := ReadRuntime()
	if err != nil && !errors.Is(err, ErrStale) {
		return
	}
	if r.PID == pid {
		os.Remove(runtimePath())
	}
}

// Alive reports whether a pid exists.
func Alive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// DefaultSocket is the unix socket path for the daemon.
func DefaultSocket() string { return filepath.Join(Dir(), "laatmux.sock") }

// Lock arbitrates daemon startup. Two clients starting the daemon at once
// must produce one daemon. The lock is a kernel-held flock on a stable file,
// so there is no window between creating a file and publishing a pid, and
// stale cleanup is never needed: a dead holder's lock is released by the
// kernel. The pid inside is diagnostic only.
type Lock struct{ f *os.File }

// TryLock takes the startup lock or reports who holds it.
func TryLock() (*Lock, error) {
	if err := ensure(); err != nil {
		return nil, err
	}
	p := filepath.Join(Dir(), "daemon.lock")
	f, err := os.OpenFile(p, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		b, _ := io.ReadAll(f)
		f.Close()
		holder := strings.TrimSpace(string(b))
		if holder == "" {
			holder = "unknown"
		}
		return nil, fmt.Errorf("laatmux: daemon lock held by pid %s", holder)
	}
	_ = f.Truncate(0)
	_, _ = f.Seek(0, 0)
	fmt.Fprintf(f, "%d\n", os.Getpid())
	return &Lock{f: f}, nil
}

// Holder is the pid of the daemon holding the startup lock, 0 when no
// one does. The holder wrote its own pid into the file when it took the
// lock, so the answer names the process that has it now, not a pid a
// runtime file remembers, which a crash can leave behind for another
// process to inherit. The lock is released by the kernel when the holder
// exits, reaped or not, so a zombie is gone here. The check takes the
// lock for an instant when it is free; a daemon starting in that instant
// loses it and its client waits out a start that is not coming, which a
// stop racing a start is anyway.
func Holder() (int, error) {
	if err := ensure(); err != nil {
		return 0, err
	}
	f, err := os.OpenFile(filepath.Join(Dir(), "daemon.lock"), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EAGAIN) {
			return 0, err
		}
		b, _ := io.ReadAll(f)
		pid, _ := strconv.Atoi(strings.TrimSpace(string(b)))
		return pid, nil
	}
	_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	return 0, nil
}

func (l *Lock) Release() {
	if l != nil && l.f != nil {
		_ = syscall.Flock(int(l.f.Fd()), syscall.LOCK_UN)
		l.f.Close()
	}
}
