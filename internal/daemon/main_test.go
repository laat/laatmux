package daemon

import (
	"os"
	"os/exec"
	"testing"
)

// No test reaches the user's tmux: every server's socket, the default
// and the managed one included, is under a directory of the run's own,
// and the variables that name the user's session are cleared. A test
// that wants a server of its own sets TMUX_TMPDIR itself.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("/tmp", "lmxt")
	if err != nil {
		panic(err)
	}
	os.Setenv("TMUX_TMPDIR", dir)
	os.Unsetenv("TMUX")
	os.Unsetenv("TMUX_PANE")
	code := m.Run()
	for _, name := range []string{"default", "laatmux"} {
		exec.Command("tmux", "-L", name, "kill-server").Run()
	}
	os.RemoveAll(dir)
	os.Exit(code)
}
