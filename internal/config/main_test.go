package config

import (
	"fmt"
	"os"
	"path"
	"strings"
	"testing"
	"time"
)

// TestMain points the state directory at one of the run's own: AddRepo
// takes its lock there, and no test may reach the user's. A child that
// TestAddRepoTwoProcesses starts appends instead, in the state
// directory its parent gave it.
func TestMain(m *testing.M) {
	if job := os.Getenv("LAATMUX_TEST_APPEND"); job != "" {
		os.Exit(appendJob(job))
	}
	dir, err := os.MkdirTemp("", "lmxc")
	if err != nil {
		panic(err)
	}
	os.Setenv("LAATMUX_HOME", dir)
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

// appendJob appends the sources of a "<config>|<source>,<source>" job,
// each named for its last element, with a pause between each write's
// check and its rename.
func appendJob(job string) int {
	file, srcs, _ := strings.Cut(job, "|")
	testBeforeRename = func() { time.Sleep(200 * time.Millisecond) }
	for _, src := range strings.Split(srcs, ",") {
		if _, err := AddRepo(file, src, strings.TrimSuffix(path.Base(src), ".git")); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
	}
	return 0
}
