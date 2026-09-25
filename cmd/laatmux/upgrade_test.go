package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
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
	"github.com/laat/laatmux/internal/protocol"
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
	for _, want := range []string{`bin="$HOME"/.local/bin/laatmux;`, `tmp=$(mktemp "$dir/.laatmux.XXXXXX")`, `trap 'rm -f "$tmp"' EXIT`, `cat > "$tmp"`, `case $v in "laatmux "*" protocol "*) [ "$(printf %s "$v" | wc -l)" -eq 0 ];; *) false;; esac ||`, `echo "binary was $("$bin" version 2>/dev/null || echo none)"`, `mv -f "$tmp" "$bin"`, `"$bin" stop`, "set -e"} {
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
		if s := installScript(bin); !strings.Contains(s, "bin=$(command -v laatmux) ||") || !strings.Contains(s, "set bin in the host config to a path") {
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

// installFile refuses a candidate that does not answer version as
// laatmux does, an empty file or a program that says nothing included,
// and leaves the destination as it was; one that answers replaces it.
func TestInstallFile(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "laatmux")
	os.WriteFile(dst, []byte("old"), 0o755)
	for name, content := range map[string]string{"garbage": "not a binary", "empty": "", "silent": "#!/bin/sh\nexit 0\n", "failing": "#!/bin/sh\necho laatmux x protocol 1\nexit 7\n", "partial": "#!/bin/sh\necho laatmux fake\n", "chatty": "#!/bin/sh\necho laatmux x protocol 1\necho more\n"} {
		bad := filepath.Join(dir, name)
		os.WriteFile(bad, []byte(content), 0o644)
		err := installFile(context.Background(), bad, dst)
		if err == nil || !strings.Contains(err.Error(), "left as it was") {
			t.Fatalf("%s candidate: %v", name, err)
		}
		if b, _ := os.ReadFile(dst); string(b) != "old" {
			t.Fatalf("destination replaced by the %s candidate", name)
		}
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 7 {
		t.Fatalf("temporary left behind: %v", entries)
	}
	good := filepath.Join(dir, "good")
	os.WriteFile(good, []byte("#!/bin/sh\necho laatmux stand-in protocol 1\n"), 0o755)
	if err := installFile(context.Background(), good, dst); err != nil {
		t.Fatalf("good candidate: %v", err)
	}
	if b, err := os.ReadFile(dst); err != nil || !strings.Contains(string(b), "stand-in") {
		t.Fatalf("destination after install: %q %v", b, err)
	}
}

// The install script, run by sh as it is on a host, replaces the binary
// with a candidate that answers version and stops the daemon with it;
// an empty or wrong candidate leaves the binary and no temporary.
func TestInstallScriptRuns(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "laatmux")
	os.WriteFile(bin, []byte("old"), 0o755)
	standIn := "#!/bin/sh\ncase $1 in version) echo laatmux stand-in protocol 1;; stop) echo stopped by stand-in;; esac\n"
	run := func(input string) (string, error) {
		cmd := exec.Command("sh", "-c", installScript(bin))
		cmd.Stdin = strings.NewReader(input)
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
	for name, input := range map[string]string{"empty": "", "garbage": "not a binary\n", "silent": "#!/bin/sh\nexit 0\n", "failing": "#!/bin/sh\necho laatmux x protocol 1\nexit 7\n", "partial": "#!/bin/sh\necho laatmux fake\n", "chatty": "#!/bin/sh\necho laatmux x protocol 1\necho more\n"} {
		out, err := run(input)
		if err == nil || !strings.Contains(out, "left as it was") {
			t.Fatalf("%s: %v\n%s", name, err, out)
		}
		if b, _ := os.ReadFile(bin); string(b) != "old" {
			t.Fatalf("%s: binary replaced", name)
		}
	}
	out, err := run(standIn)
	if err != nil || !strings.Contains(out, "stopped by stand-in") || !strings.Contains(out, "binary was none") {
		t.Fatalf("stand-in: %v\n%s", err, out)
	}
	if out, err := run(standIn); err != nil || !strings.Contains(out, "binary was laatmux stand-in protocol 1") {
		t.Fatalf("second install: %v\n%s", err, out)
	}
	if b, _ := os.ReadFile(bin); string(b) != standIn {
		t.Fatalf("binary after install: %q", b)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Fatalf("temporary left behind: %v", entries)
	}
}

// stop reaches the daemon over its socket and asks it to shut down, or
// sends SIGTERM to one from before the message, and waits for the lock
// to leave that daemon's hands; a daemon that exited but was not reaped
// counts as gone. A runtime file naming a process that answers on no
// socket is no daemon, and that process is never signalled.
func TestStop(t *testing.T) {
	t.Setenv("LAATMUX_HOME", t.TempDir())
	t.Setenv("LAATMUX_CONFIG", filepath.Join(t.TempDir(), "none.yaml"))
	t.Setenv("TMUX_TMPDIR", t.TempDir())
	if err := cmdStop(context.Background(), nil); err != nil {
		t.Fatalf("no daemon: %v", err)
	}
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
		t.Fatal("a process that answers on no socket was signalled")
	}
	os.Remove(filepath.Join(home.Dir(), "runtime.json"))
	// A real daemon, then one from before the shutdown message: each is
	// this test binary in a helper mode, not reaped until stop has
	// returned, so its pid is a zombie while stop waits.
	for _, mode := range []string{"serve", "legacy"} {
		d := exec.Command(os.Args[0], "-test.run=TestStop")
		d.Env = append(os.Environ(), "LAATMUX_TEST_DAEMON="+mode)
		out := &strings.Builder{}
		d.Stdout, d.Stderr = out, out
		if err := d.Start(); err != nil {
			t.Fatal(err)
		}
		for deadline := time.Now().Add(10 * time.Second); ; {
			pid, err := home.Holder()
			if err != nil {
				t.Fatal(err)
			}
			if _, rerr := home.ReadRuntime(); pid == d.Process.Pid && rerr == nil {
				break
			}
			if time.Now().After(deadline) {
				d.Process.Kill()
				t.Fatalf("%s daemon did not come up: %s", mode, out)
			}
			time.Sleep(20 * time.Millisecond)
		}
		start := time.Now()
		if err := cmdStop(context.Background(), nil); err != nil {
			d.Process.Kill()
			t.Fatalf("stop %s: %v\n%s", mode, err, out)
		}
		if time.Since(start) > 5*time.Second {
			t.Errorf("stop %s took %s", mode, time.Since(start))
		}
		if err := d.Wait(); err != nil {
			t.Fatalf("%s daemon: %v\n%s", mode, err, out)
		}
	}
	// A daemon that holds the lock but answers on no socket is one
	// shutting down, its listener gone before its runs: stop waits for
	// the lock to leave its hands rather than reporting no daemon.
	held := exec.Command(os.Args[0], "-test.run=TestStop")
	held.Env = append(os.Environ(), "LAATMUX_TEST_DAEMON=held")
	if err := held.Start(); err != nil {
		t.Fatal(err)
	}
	defer held.Process.Kill()
	for deadline := time.Now().Add(10 * time.Second); ; {
		if pid, _ := home.Holder(); pid == held.Process.Pid {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("held daemon did not take the lock")
		}
		time.Sleep(20 * time.Millisecond)
	}
	stopped := make(chan error, 1)
	go func() { stopped <- cmdStop(context.Background(), nil) }()
	select {
	case err := <-stopped:
		t.Fatalf("stop returned while the lock was held: %v", err)
	case <-time.After(500 * time.Millisecond):
	}
	held.Process.Signal(syscall.SIGTERM)
	if err := <-stopped; err != nil {
		t.Fatalf("stop after the holder left: %v", err)
	}
	held.Wait()
	// A daemon still starting, the lock taken and the listener not up
	// yet, is reached once it is and stopped, not waited out.
	slow := exec.Command(os.Args[0], "-test.run=TestStop")
	slow.Env = append(os.Environ(), "LAATMUX_TEST_DAEMON=slow")
	if err := slow.Start(); err != nil {
		t.Fatal(err)
	}
	defer slow.Process.Kill()
	for deadline := time.Now().Add(10 * time.Second); ; {
		if pid, _ := home.Holder(); pid == slow.Process.Pid {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("slow daemon did not take the lock")
		}
		time.Sleep(20 * time.Millisecond)
	}
	start := time.Now()
	if err := cmdStop(context.Background(), nil); err != nil {
		t.Fatalf("stop a starting daemon: %v", err)
	}
	if took := time.Since(start); took > 5*time.Second {
		t.Errorf("stop of a starting daemon took %s", took)
	}
	if err := slow.Wait(); err != nil {
		t.Fatalf("slow daemon: %v", err)
	}
	// A daemon reached on a record's address that is not the record's
	// daemon, by its hello's pid, is not stopped: the record is read
	// again. The real daemon says its pid; here the record names the
	// bystander.
	serve := exec.Command(os.Args[0], "-test.run=TestStop")
	serve.Env = append(os.Environ(), "LAATMUX_TEST_DAEMON=serve")
	if err := serve.Start(); err != nil {
		t.Fatal(err)
	}
	defer serve.Process.Kill()
	var rt home.Runtime
	for deadline := time.Now().Add(10 * time.Second); ; {
		var err error
		if rt, err = home.ReadRuntime(); err == nil && rt.PID == serve.Process.Pid {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("daemon did not come up")
		}
		time.Sleep(20 * time.Millisecond)
	}
	nc, err := client.DialAddress(rt.Address)
	if err != nil {
		t.Fatal(err)
	}
	if err := stopDaemon(context.Background(), nc, home.Runtime{Address: rt.Address, PID: bystander.Process.Pid}); !errors.Is(err, errMoved) {
		t.Fatalf("mismatched record: %v", err)
	}
	if pid, _ := home.Holder(); pid != serve.Process.Pid {
		t.Fatal("daemon stopped on a mismatched record")
	}
	// A record that keeps naming another pid at the daemon's address
	// is retried within the bound, then reported, never spun on.
	home.WriteRuntime(home.Runtime{Address: rt.Address, PID: bystander.Process.Pid, Version: "wrong"})
	stopWait = time.Second
	start = time.Now()
	err = cmdStop(context.Background(), nil)
	stopWait = 20 * time.Second
	if err == nil || !strings.Contains(err.Error(), "not the daemon the runtime record names") {
		t.Fatalf("persistent mismatch: %v", err)
	}
	if took := time.Since(start); took < time.Second || took > 5*time.Second {
		t.Errorf("persistent mismatch took %s", took)
	}
	if pid, _ := home.Holder(); pid != serve.Process.Pid || !home.Alive(bystander.Process.Pid) {
		t.Fatal("something was stopped on a persistently mismatched record")
	}
	home.WriteRuntime(rt)
	if err := cmdStop(context.Background(), nil); err != nil {
		t.Fatalf("stop: %v", err)
	}
	serve.Wait()
	// The legacy fallback signals the pid of the record that was
	// dialled, not whatever the runtime file says by then.
	legacy := exec.Command(os.Args[0], "-test.run=TestStop")
	legacy.Env = append(os.Environ(), "LAATMUX_TEST_DAEMON=legacy")
	if err := legacy.Start(); err != nil {
		t.Fatal(err)
	}
	defer legacy.Process.Kill()
	for deadline := time.Now().Add(10 * time.Second); ; {
		var err error
		if rt, err = home.ReadRuntime(); err == nil && rt.PID == legacy.Process.Pid {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("legacy daemon did not come up")
		}
		time.Sleep(20 * time.Millisecond)
	}
	// A legacy daemon has no pid in its hello: the record must still
	// stand when it answers, else it is read again and nothing is
	// signalled.
	nc, err = client.DialAddress(rt.Address)
	if err != nil {
		t.Fatal(err)
	}
	home.WriteRuntime(home.Runtime{Address: rt.Address, PID: bystander.Process.Pid, Version: "replacement"})
	if err := stopDaemon(context.Background(), nc, rt); !errors.Is(err, errMoved) {
		t.Fatalf("legacy with a replaced record: %v", err)
	}
	if !home.Alive(bystander.Process.Pid) {
		t.Fatal("the replacement's pid was signalled")
	}
	home.WriteRuntime(rt)
	if err := cmdStop(context.Background(), nil); err != nil {
		t.Fatalf("stop legacy: %v", err)
	}
	legacy.Wait()
	os.Remove(filepath.Join(home.Dir(), "runtime.json"))
	if err := cmdStop(context.Background(), []string{"x"}); err == nil {
		t.Fatal("arguments accepted")
	}
}

// testDaemon is the stand-in daemon of TestStop: serve is the real
// daemon with the shutdown message; legacy is one without it, ended by
// SIGTERM.
func testDaemon(mode string) {
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM)
	defer cancel()
	var err error
	switch mode {
	case "serve":
		err = cmdServe(ctx, []string{"--listen", "tcp:127.0.0.1:0"})
	case "legacy":
		err = legacyServe(ctx)
	case "held":
		// The lock alone: a daemon whose listener is gone already.
		var l *home.Lock
		if l, err = home.TryLock(); err == nil {
			<-ctx.Done()
			l.Release()
		}
	case "slow":
		// The lock first, the listener a while later, as serve does.
		err = legacyServeAfter(ctx, 800*time.Millisecond)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Exit(0)
}

// legacyServe is a daemon from before the shutdown message: the lock,
// the runtime file, a listener, no Shutdown in its config.
func legacyServe(ctx context.Context) error { return legacyServeAfter(ctx, 0) }

// legacyServeAfter is legacyServe with a pause between the lock and the
// listener.
func legacyServeAfter(ctx context.Context, pause time.Duration) error {
	lock, err := home.TryLock()
	if err != nil {
		return err
	}
	defer lock.Release()
	select {
	case <-time.After(pause):
	case <-ctx.Done():
		return nil
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	defer ln.Close()
	if err := home.WriteRuntime(home.Runtime{Address: "tcp:" + ln.Addr().String(), PID: os.Getpid(), Version: "legacy"}); err != nil {
		return err
	}
	defer home.RemoveRuntime(os.Getpid())
	// The hello of a daemon before the pid field and the shutdown
	// message, answered by hand so this branch's daemon package does
	// not make it current.
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				pc := protocol.NewConn(c)
				pc.Write(protocol.Message{Type: protocol.TypeHello, Protocol: protocol.Version, EnvironmentID: "legacy", Version: "legacy", Capabilities: []string{protocol.CapStatus}})
				for {
					if _, err := pc.Read(); err != nil {
						return
					}
				}
			}()
		}
	}()
	<-ctx.Done()
	return nil
}

func TestMain(m *testing.M) {
	if mode := os.Getenv("LAATMUX_TEST_DAEMON"); mode != "" {
		testDaemon(mode)
	}
	// No test reaches the user's tmux: the default server's socket, and
	// every other, is under a directory of the run's own, and the
	// variables that name the user's session are cleared. A jump or an
	// add that gets as far as tmux finds no server. A test that wants a
	// server of its own sets TMUX_TMPDIR itself.
	dir, err := os.MkdirTemp("/tmp", "lmxc")
	if err != nil {
		panic(err)
	}
	os.Setenv("TMUX_TMPDIR", dir)
	os.Unsetenv("TMUX")
	os.Unsetenv("TMUX_PANE")
	code := m.Run()
	exec.Command("tmux", "-L", "default", "kill-server").Run()
	os.RemoveAll(dir)
	os.Exit(code)
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
