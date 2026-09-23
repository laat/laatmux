package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/laat/laatmux/internal/client"
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

// The install script writes to a fresh temporary file beside the
// configured binary, checks it runs, renames it over, then stops the
// daemon with the new binary; the binary is the word the bridge runs.
func TestInstallScript(t *testing.T) {
	s := installScript("~/.local/bin/laatmux")
	for _, want := range []string{`bin="$HOME"/.local/bin/laatmux;`, `tmp=$(mktemp "$dir/.laatmux.XXXXXX")`, `trap 'rm -f "$tmp"' EXIT`, `cat > "$tmp"`, `"$tmp" version >/dev/null ||`, `mv -f "$tmp" "$bin"`, `"$bin" stop`, "set -e"} {
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
	if s := installScript("/opt/my bin/laatmux"); !strings.Contains(s, `bin='/opt/my bin/laatmux';`) {
		t.Errorf("absolute path with a space: %s", s)
	}
	for _, bin := range []string{"", "laatmux"} {
		if s := installScript(bin); !strings.Contains(s, "bin=$(command -v laatmux) ||") || !strings.Contains(s, "set bin in the host config") {
			t.Errorf("bare name %q: %s", bin, s)
		}
	}
	if s := installScript("laat mux"); !strings.Contains(s, `command -v 'laat mux'`) {
		t.Errorf("bare name with a space: %s", s)
	}
	// The install runs the same word the bridge does.
	for _, bin := range []string{"~/.local/bin/laatmux", "/opt/my bin/laatmux", "laatmux"} {
		if !strings.Contains(installScript(bin), client.RemoteBin(bin)) {
			t.Errorf("%q: install and bridge disagree", bin)
		}
	}
}

// Flags come before, between or after the hosts.
func TestParseInterspersed(t *testing.T) {
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	bin := fs.String("bin", "", "")
	src := fs.String("src", "", "")
	pos, err := parseInterspersed(fs, []string{"vm", "--bin", "/tmp/x", "box", "--src", "/s"})
	if err != nil || strings.Join(pos, ",") != "vm,box" || *bin != "/tmp/x" || *src != "/s" {
		t.Errorf("%v %v bin=%q src=%q", pos, err, *bin, *src)
	}
	fs = flag.NewFlagSet("t", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	if _, err := parseInterspersed(fs, []string{"vm", "--nope"}); err == nil {
		t.Error("unknown flag accepted")
	}
}

// installFile refuses a candidate that does not run and leaves the
// destination as it was; one that runs replaces it.
func TestInstallFile(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "laatmux")
	os.WriteFile(dst, []byte("old"), 0o755)
	bad := filepath.Join(dir, "bad")
	os.WriteFile(bad, []byte("not a binary"), 0o644)
	err := installFile(context.Background(), bad, dst)
	if err == nil || !strings.Contains(err.Error(), "does not run here") {
		t.Fatalf("bad candidate: %v", err)
	}
	if b, _ := os.ReadFile(dst); string(b) != "old" {
		t.Fatal("destination replaced by a candidate that does not run")
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 2 {
		t.Fatalf("temporary left behind: %v", entries)
	}
	good, err := exec.LookPath("true")
	if err != nil {
		t.Skip("no true")
	}
	if err := installFile(context.Background(), good, dst); err != nil {
		t.Fatalf("good candidate: %v", err)
	}
	if fi, err := os.Stat(dst); err != nil || fi.Mode().Perm()&0o100 == 0 || fi.Size() < 100 {
		t.Fatalf("destination after install: %v %v", fi, err)
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

// stop ends the daemon that holds the startup lock and waits for the
// lock to go, so a daemon that exited but was not reaped counts as gone
// and a pid the runtime file remembers is never signalled on its own.
func TestStop(t *testing.T) {
	t.Setenv("LAATMUX_HOME", t.TempDir())
	if err := cmdStop(context.Background(), nil); err != nil {
		t.Fatalf("no daemon: %v", err)
	}
	// A runtime file naming a live process that holds no lock: not the
	// daemon, not signalled.
	bystander := exec.Command("sleep", "30")
	if err := bystander.Start(); err != nil {
		t.Fatal(err)
	}
	defer bystander.Process.Kill()
	home.WriteRuntime(home.Runtime{Address: "tcp:127.0.0.1:1", PID: bystander.Process.Pid, Version: "old"})
	if err := cmdStop(context.Background(), nil); err != nil {
		t.Fatalf("stale runtime with a live pid: %v", err)
	}
	if !home.Alive(bystander.Process.Pid) {
		t.Fatal("a process that holds no lock was signalled")
	}
	// A stand-in daemon: this test binary holding the lock until
	// SIGTERM. It is not reaped until after stop has returned, so the
	// pid is a zombie while stop waits; the lock says it is gone.
	holder := exec.Command(os.Args[0], "-test.run=TestStop")
	holder.Env = append(os.Environ(), "LAATMUX_TEST_HOLD_LOCK=1")
	out := &strings.Builder{}
	holder.Stdout, holder.Stderr = out, out
	if err := holder.Start(); err != nil {
		t.Fatal(err)
	}
	defer holder.Process.Kill()
	for deadline := time.Now().Add(5 * time.Second); ; {
		pid, err := home.Holder()
		if err != nil {
			t.Fatal(err)
		}
		if pid == holder.Process.Pid {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("stand-in did not take the lock: %s", out)
		}
		time.Sleep(20 * time.Millisecond)
	}
	start := time.Now()
	if err := cmdStop(context.Background(), nil); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Errorf("stop took %s", time.Since(start))
	}
	if err := holder.Wait(); err != nil {
		t.Fatalf("stand-in: %v: %s", err, out)
	}
	if err := cmdStop(context.Background(), []string{"x"}); err == nil {
		t.Fatal("arguments accepted")
	}
}

// holdLock is the stand-in daemon of TestStop: it takes the startup
// lock and exits cleanly on SIGTERM.
func holdLock() {
	l, err := home.TryLock()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM)
	<-sig
	l.Release()
	os.Exit(0)
}

func TestMain(m *testing.M) {
	if os.Getenv("LAATMUX_TEST_HOLD_LOCK") != "" {
		holdLock()
	}
	os.Exit(m.Run())
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
