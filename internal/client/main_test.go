package client

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/laat/laatmux/internal/config"
	"github.com/laat/laatmux/internal/home"
)

func TestMain(m *testing.M) {
	// First: a start whose first argument is not a -test. flag is a
	// start as laatmux, such as the daemon StartDaemon starts, this
	// binary with "serve". A test binary ignores a word such as that: it
	// would run the whole suite again, detached, and a test that starts
	// a daemon would start another. DialLocal starts one when no daemon
	// answers, which a test whose dial fails, under a mutant say, would
	// do. No test here wants one; the start ends at once, as a daemon
	// that never comes up. go test puts its -test. flags first, every
	// test that runs this binary passes one first, and Go's flags take
	// two dashes as well as one.
	if len(os.Args) > 1 && !strings.HasPrefix(os.Args[1], "-test.") && !strings.HasPrefix(os.Args[1], "--test.") {
		refuse()
	}
	// A child of TestRunIsItsOwn that got past the check above ends
	// here, with a status of its own: run on, it would run the suite,
	// and that test would start another child.
	if os.Getenv("LAATMUX_TEST_SERVED") != "" {
		fmt.Fprintln(os.Stderr, "a child of TestRunIsItsOwn, past the guard")
		os.Exit(2)
	}
	// No test reaches the user's tmux or laatmux: the tmux sockets, the
	// state directory with the runtime file that names the daemon Dial
	// connects to, and the config, a file that is not there, are under a
	// directory of the run's own, and the variables that name the user's
	// session are cleared. A test that wants a daemon or a config sets
	// LAATMUX_HOME or LAATMUX_CONFIG itself.
	dir, err := os.MkdirTemp("/tmp", "lmxc")
	if err != nil {
		panic(err)
	}
	runDir = dir
	os.Setenv("TMUX_TMPDIR", dir)
	os.Unsetenv("TMUX")
	os.Unsetenv("TMUX_PANE")
	os.Setenv("LAATMUX_HOME", filepath.Join(dir, "home"))
	os.Setenv("LAATMUX_CONFIG", filepath.Join(dir, "config.yaml"))
	// A start the guard ends reports to its log, which no one may read,
	// and a test that takes the failure in its stride passes: the start
	// leaves a marker here, and the run fails on it. Only on a marker
	// there when m.Run returns: a start no test waits for, such as a
	// detached daemon after a cancelled dial, can come later, and the
	// guard ends it all the same.
	marks := filepath.Join(dir, "refused")
	if err := os.Mkdir(marks, 0o700); err != nil {
		panic(err)
	}
	os.Setenv("LAATMUX_TEST_REFUSED", marks)
	code := m.Run()
	if refusedStarts(marks) && code == 0 {
		code = 1
	}
	os.RemoveAll(dir)
	os.Exit(code)
}

// runDir is the directory TestMain gives the run.
var runDir string

// refuse ends a start as laatmux: the guard's line on stderr, then a
// marker with the start's arguments under the directory
// LAATMUX_TEST_REFUSED names, the run's that made the start.
func refuse() {
	fmt.Fprintln(os.Stderr, refused)
	if dir := os.Getenv("LAATMUX_TEST_REFUSED"); dir != "" {
		os.WriteFile(filepath.Join(dir, strconv.Itoa(os.Getpid())), fmt.Appendf(nil, "%q\n", os.Args[1:]), 0o600)
	}
	os.Exit(1)
}

// refusedStarts says, on stderr, which starts left a marker under dir,
// and whether any did.
func refusedStarts(dir string) bool {
	marks, _ := os.ReadDir(dir)
	for _, e := range marks {
		b, _ := os.ReadFile(filepath.Join(dir, e.Name()))
		// Empty when the start was killed between the create and the
		// write.
		args := strings.TrimSpace(string(b))
		if args == "" {
			args = "(none recorded)"
		}
		fmt.Fprintln(os.Stderr, refusedRun, args)
	}
	return len(marks) > 0
}

// refused is what the binary says, started as laatmux; refusedRun is
// what the run says, failed for such a start.
const (
	refused    = "the internal/client tests start this binary only as a test run"
	refusedRun = "the run fails: a test started this binary as laatmux, and the guard ended it, with arguments"
)

// A test that sets no LAATMUX_HOME, LAATMUX_CONFIG or TMUX_TMPDIR has
// the run's, and the binary started as laatmux, by StartDaemon or
// anything else that puts an argument other than a -test. flag
// first, runs no test, and leaves a marker that fails the run that
// started it.
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
	if os.Getenv("LAATMUX_TEST_STRAY") != "" {
		// A test that reaches StartDaemon and takes a daemon that never
		// comes up in its stride. LAATMUX_TEST_SERVED ends the start if
		// the guard does not.
		stray := exec.Command(os.Args[0], "serve")
		stray.Env = append(os.Environ(), "LAATMUX_TEST_SERVED=1")
		stray.Run()
		return
	}
	// As StartDaemon starts it, its own executable with "serve" and
	// what LAATMUX_SERVE_ARGS adds, or with any other first argument
	// but a -test. flag, a flag of laatmux's such as --version
	// included: the guard's status, 1, its line first, and a marker
	// with the arguments under the directory LAATMUX_TEST_REFUSED names;
	// a binary built with -cover may add a line of its own at exit. A
	// -test. flag first, with one dash or two, gets past the guard, to
	// the status of a child of this test, 2.
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	start := func(args ...string) ([]byte, []string, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		dir := t.TempDir()
		serve := exec.CommandContext(ctx, self, args...)
		serve.Env = append(os.Environ(), "LAATMUX_TEST_SERVED=1", "LAATMUX_TEST_REFUSED="+dir)
		out, err := serve.CombinedOutput()
		var marks []string
		es, _ := os.ReadDir(dir)
		for _, e := range es {
			b, _ := os.ReadFile(filepath.Join(dir, e.Name()))
			marks = append(marks, strings.TrimSpace(string(b)))
		}
		return out, marks, err
	}
	var exit *exec.ExitError
	for _, args := range [][]string{{"serve"}, {"serve", "--listen", "tcp:127.0.0.1:0"}, {"sidebar", "pane"}, {"--version"}, {"foo"}} {
		out, marks, err := start(args...)
		if !errors.As(err, &exit) || exit.ExitCode() != 1 || !strings.HasPrefix(string(out), refused+"\n") {
			t.Errorf("started with %q: %v\n%s", args, err, out)
		}
		if want := fmt.Sprintf("%q", args); len(marks) != 1 || marks[0] != want {
			t.Errorf("started with %q: markers %q, not one of %s", args, marks, want)
		}
	}
	for _, flag := range []string{"-test.run=^$", "--test.run=^$"} {
		if out, marks, err := start(flag); !errors.As(err, &exit) || exit.ExitCode() != 2 || strings.Contains(string(out), refused) || len(marks) != 0 {
			t.Errorf("started with %s: %v, markers %q\n%s", flag, err, marks, out)
		}
	}
	// A run whose test made such a start and passed fails after the
	// test, on the marker, and says which start it was.
	stray := exec.Command(os.Args[0], "-test.run=^TestRunIsItsOwn$")
	stray.Env = append(os.Environ(), "LAATMUX_TEST_STRAY=1")
	out, err := stray.CombinedOutput()
	pass, fail := strings.Index(string(out), "PASS\n"), strings.Index(string(out), refusedRun+` ["serve"]`+"\n")
	if !errors.As(err, &exit) || exit.ExitCode() != 1 || pass < 0 || fail < pass {
		t.Errorf("run with a test that made a start the guard ended: %v\n%s", err, out)
	}
}
