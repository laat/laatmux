package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/laat/laatmux/internal/home"
)

func TestParsePlatform(t *testing.T) {
	cases := []struct {
		in, goos, goarch string
		bad              bool
	}{
		{"Linux x86_64\n", "linux", "amd64", false},
		{"Linux aarch64", "linux", "arm64", false},
		{"Darwin arm64", "darwin", "arm64", false},
		{"Darwin x86_64", "darwin", "amd64", false},
		{"FreeBSD amd64", "", "", true},
		{"Linux riscv64", "", "", true},
		{"", "", "", true},
	}
	for _, c := range cases {
		goos, goarch, err := parsePlatform(c.in)
		if (err != nil) != c.bad || goos != c.goos || goarch != c.goarch {
			t.Errorf("%q: %s/%s %v", c.in, goos, goarch, err)
		}
	}
}

// The install script writes beside the configured binary and renames
// over it, then stops the daemon with the new binary; a path under ~ is
// the remote home, a bare name is looked up on the remote PATH.
func TestInstallScript(t *testing.T) {
	s := installScript("~/.local/bin/laatmux")
	for _, want := range []string{`bin="$HOME"/.local/bin/laatmux;`, `cat > "$bin.new"`, `mv -f "$bin.new" "$bin"`, `"$bin" stop`, "set -e"} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q in %s", want, s)
		}
	}
	if s := installScript("/opt/lm/laatmux"); !strings.Contains(s, "bin=/opt/lm/laatmux;") {
		t.Errorf("absolute path: %s", s)
	}
	if s := installScript("~/my bin/laatmux"); !strings.Contains(s, `bin="$HOME"/'my bin/laatmux';`) {
		t.Errorf("path with a space: %s", s)
	}
	for _, bin := range []string{"", "laatmux"} {
		if s := installScript(bin); !strings.Contains(s, "bin=$(command -v laatmux) ||") || !strings.Contains(s, "set bin in the host config") {
			t.Errorf("bare name %q: %s", bin, s)
		}
	}
	if s := installScript("laat mux"); !strings.Contains(s, `command -v 'laat mux'`) {
		t.Errorf("bare name with a space: %s", s)
	}
}

// hosts marks the daemons whose build is not this client's and says what
// the mark means; with every build the same there is no mark and no
// line.
func TestHostsReport(t *testing.T) {
	rows := []hostRow{{name: "mac", status: "ok  daemon abc"}, {name: "vm", status: "ok  daemon def", differs: true}, {name: "box", status: "unreachable: x"}}
	out := hostsReport(rows, "abc")
	if !strings.HasPrefix(out, "  mac ") || !strings.Contains(out, "\n* vm ") || !strings.Contains(out, "\n  box ") || !strings.Contains(out, "differs from this client's (abc)") {
		t.Errorf("report:\n%s", out)
	}
	rows[1].differs = false
	if out := hostsReport(rows, "abc"); strings.Contains(out, "*") {
		t.Errorf("mark without a difference:\n%s", out)
	}
}

// stop ends the daemon the runtime file names and waits for it; no
// daemon, or a stale runtime file, is not an error.
func TestStop(t *testing.T) {
	t.Setenv("LAATMUX_HOME", t.TempDir())
	if err := cmdStop(context.Background(), nil); err != nil {
		t.Fatalf("no daemon: %v", err)
	}
	// A process standing in for the daemon: sleep dies on SIGTERM. It
	// is reaped as it exits, as a detached daemon is by init, so the pid
	// is gone rather than a zombie.
	sleep := exec.Command("sleep", "30")
	if err := sleep.Start(); err != nil {
		t.Fatal(err)
	}
	defer sleep.Process.Kill()
	waited := make(chan error, 1)
	go func() { waited <- sleep.Wait() }()
	if err := home.WriteRuntime(home.Runtime{Address: "tcp:127.0.0.1:1", PID: sleep.Process.Pid, Version: "old"}); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if err := cmdStop(context.Background(), nil); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Errorf("stop took %s", time.Since(start))
	}
	select {
	case err := <-waited:
		if err == nil {
			t.Fatal("stand-in exited normally")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("stand-in still running")
	}
	// The runtime file is now stale: nothing to stop.
	if err := cmdStop(context.Background(), nil); err != nil {
		t.Fatalf("stale: %v", err)
	}
	if err := cmdStop(context.Background(), []string{"x"}); err == nil {
		t.Fatal("arguments accepted")
	}
}

// The source is the current directory only when it is the laatmux
// checkout; --src overrides.
func TestBuilderSource(t *testing.T) {
	b := &builder{src: "/elsewhere"}
	if src, err := b.source(); err != nil || src != "/elsewhere" {
		t.Errorf("--src: %q %v", src, err)
	}
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example.com/other\n"), 0o644)
	wd, _ := os.Getwd()
	defer os.Chdir(wd)
	os.Chdir(dir)
	if _, err := (&builder{}).source(); err == nil || !strings.Contains(err.Error(), "not in the laatmux checkout") {
		t.Errorf("other module: %v", err)
	}
	os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module github.com/laat/laatmux\n\ngo 1.26\n"), 0o644)
	if src, err := (&builder{}).source(); err != nil || src == "" {
		t.Errorf("checkout: %q %v", src, err)
	}
}
