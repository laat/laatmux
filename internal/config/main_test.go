package config

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

// TestMain points the state directory at one of the run's own: SetPaused
// takes its lock there, and no test may reach the user's. A child that
// TestSetPausedTwoProcesses starts pauses instead, in the state
// directory its parent gave it.
func TestMain(m *testing.M) {
	if job := os.Getenv("LAATMUX_TEST_PAUSE"); job != "" {
		os.Exit(pauseJob(job))
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

// pauseJob pauses the hosts of a "<config>|<host>,<host>" job, with a
// pause between each write's check and its rename.
func pauseJob(job string) int {
	file, hosts, _ := strings.Cut(job, "|")
	testBeforeRename = func() { time.Sleep(200 * time.Millisecond) }
	for _, h := range strings.Split(hosts, ",") {
		if _, err := SetPaused(file, h, true); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
	}
	return 0
}
