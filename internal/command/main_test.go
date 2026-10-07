package command

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/laat/laatmux/internal/config"
	"github.com/laat/laatmux/internal/home"
)

func TestMain(m *testing.M) {
	// First: a daemon client.StartDaemon starts is this binary with
	// "serve", an argument a test binary ignores: it would run the whole
	// suite again, detached, and a test that starts a daemon would start
	// another. No test here wants one; the start ends at once, as a
	// daemon that never comes up.
	if len(os.Args) > 1 && os.Args[1] == "serve" {
		fmt.Fprintln(os.Stderr, noDaemon)
		os.Exit(1)
	}
	// A child of TestRunIsItsOwn that got past the check above ends
	// here, with a status of its own: run on, it would run the suite,
	// and that test would start another child.
	if os.Getenv("LAATMUX_TEST_SERVED") != "" {
		fmt.Fprintln(os.Stderr, "started as a daemon, and not stopped")
		os.Exit(2)
	}
	// No test reaches the user's tmux or laatmux: the tmux sockets, the
	// state directory with the runtime file that names the daemon
	// client.Dial connects to, and the config, a file that is not there,
	// are under a directory of the run's own, and the variables that
	// name the user's session are cleared. A test that wants a daemon or
	// a config sets LAATMUX_HOME or LAATMUX_CONFIG itself.
	dir, err := os.MkdirTemp("/tmp", "lmxk")
	if err != nil {
		panic(err)
	}
	runDir = dir
	os.Setenv("TMUX_TMPDIR", dir)
	os.Unsetenv("TMUX")
	os.Unsetenv("TMUX_PANE")
	os.Setenv("LAATMUX_HOME", filepath.Join(dir, "home"))
	os.Setenv("LAATMUX_CONFIG", filepath.Join(dir, "config.yaml"))
	code := m.Run()
	exec.Command("tmux", "-L", "default", "kill-server").Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

// runDir is the directory TestMain gives the run.
var runDir string

// noDaemon is what the binary says, started as a daemon.
const noDaemon = "the internal/command tests start no daemon"

// A test that sets no LAATMUX_HOME, LAATMUX_CONFIG or TMUX_TMPDIR has
// the run's, and the binary started as client.StartDaemon starts it,
// with or without arguments after "serve", runs no test.
func TestRunIsItsOwn(t *testing.T) {
	if runDir == "" {
		t.Fatal("TestMain gave the run no directory")
	}
	for what, p := range map[string]string{"state directory": home.Dir(), "config": config.Path()} {
		if !strings.HasPrefix(p, runDir+"/") {
			t.Errorf("the %s is %s, not under the run's %s", what, p, runDir)
		}
	}
	if d := os.Getenv("TMUX_TMPDIR"); d != runDir {
		t.Errorf("TMUX_TMPDIR is %q, not the run's %s", d, runDir)
	}
	for _, v := range []string{"TMUX", "TMUX_PANE"} {
		if s, ok := os.LookupEnv(v); ok {
			t.Errorf("%s is set: %q", v, s)
		}
	}
	// As StartDaemon starts it: its own executable, with "serve" and
	// what LAATMUX_SERVE_ARGS adds. The guard's status, 1, and its line
	// first; a binary built with -cover may add one of its own at exit.
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"serve"}, {"serve", "--listen", "tcp:127.0.0.1:0"}} {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		serve := exec.CommandContext(ctx, self, args...)
		serve.Env = append(os.Environ(), "LAATMUX_TEST_SERVED=1")
		out, err := serve.CombinedOutput()
		cancel()
		var exit *exec.ExitError
		if !errors.As(err, &exit) || exit.ExitCode() != 1 || !strings.HasPrefix(string(out), noDaemon+"\n") {
			t.Errorf("started with %q: %v\n%s", args, err, out)
		}
	}
}
