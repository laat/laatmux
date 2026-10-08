package config

import (
	"os"
	"testing"
)

// TestMain points the state directory at one of the run's own: AddRepo
// takes its lock there, and no test may reach the user's.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "lmxc")
	if err != nil {
		panic(err)
	}
	os.Setenv("LAATMUX_HOME", dir)
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}
