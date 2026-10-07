package command

import (
	"context"
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
	// No test reaches the user's tmux or laatmux: the tmux sockets, the
	// state directory with the runtime file that names the daemon stream
	// and Add.Run dial, and the config, a file that is not there, are
	// under a directory of the run's own, and the variables that name
	// the user's session are cleared. A test that wants a daemon or a
	// config sets LAATMUX_HOME or LAATMUX_CONFIG itself.
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

// A test that sets no LAATMUX_HOME or LAATMUX_CONFIG has the run's, and
// the binary started as client.StartDaemon starts it runs no test.
func TestRunIsItsOwn(t *testing.T) {
	if runDir == "" {
		t.Fatal("TestMain gave the run no directory")
	}
	for what, p := range map[string]string{"state directory": home.Dir(), "config": config.Path()} {
		if !strings.HasPrefix(p, runDir+"/") {
			t.Errorf("the %s is %s, not under the run's %s", what, p, runDir)
		}
	}
	if os.Getenv("LAATMUX_TEST_SERVED") != "" {
		// The start below, under a TestMain that ran the suite: not again.
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	serve := exec.CommandContext(ctx, os.Args[0], "serve")
	serve.Env = append(os.Environ(), "LAATMUX_TEST_SERVED=1")
	if out, err := serve.CombinedOutput(); err == nil || string(out) != noDaemon+"\n" {
		t.Errorf("started as a daemon: %v\n%s", err, out)
	}
}
