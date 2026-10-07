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
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/laat/laatmux/internal/client"
	"github.com/laat/laatmux/internal/config"
	"github.com/laat/laatmux/internal/home"
	"github.com/laat/laatmux/internal/procs"
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
		// The output is read only once Wait has returned: exec's copier
		// writes it until then.
		for deadline := time.Now().Add(10 * time.Second); ; {
			if _, err := home.ReadRuntime(); err == nil && heldBy(d.Process.Pid) {
				break
			}
			if time.Now().After(deadline) {
				d.Process.Kill()
				d.Wait()
				t.Fatalf("%s daemon did not come up: %s", mode, out)
			}
			time.Sleep(20 * time.Millisecond)
		}
		start := time.Now()
		if err := cmdStop(context.Background(), nil); err != nil {
			d.Process.Kill()
			d.Wait()
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
		if heldBy(held.Process.Pid) {
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
		if heldBy(slow.Process.Pid) {
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
	// Reached once it listens: the helper exits with an error when
	// signalled before that.
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
	if err := stopDaemon(context.Background(), nc, home.Runtime{Address: rt.Address, PID: bystander.Process.Pid}, time.Now().Add(stopWait)); !errors.Is(err, errMoved) {
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
	if err == nil || !strings.Contains(err.Error(), "is answered by another daemon, after") {
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
	if err := stopDaemon(context.Background(), nc, rt, time.Now().Add(stopWait)); !errors.Is(err, errMoved) {
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
	// A daemon that holds the lock and is the one the record names, by
	// pid and start time, but cannot be asked, listening on nothing or
	// accepting without a hello, is ended with SIGTERM rather than
	// waited out; the hello wait is shortened so the wedged case does
	// not take 15 s.
	was := client.HelloTimeout
	client.HelloTimeout = 300 * time.Millisecond
	defer func() { client.HelloTimeout = was }()
	for _, mode := range []string{"nolisten", "wedged"} {
		d := exec.Command(os.Args[0], "-test.run=TestStop")
		d.Env = append(os.Environ(), "LAATMUX_TEST_DAEMON="+mode)
		if err := d.Start(); err != nil {
			t.Fatal(err)
		}
		for deadline := time.Now().Add(10 * time.Second); ; {
			if rt, err := home.ReadRuntime(); err == nil && rt.PID == d.Process.Pid && heldBy(d.Process.Pid) {
				break
			}
			if time.Now().After(deadline) {
				d.Process.Kill()
				d.Wait()
				t.Fatalf("%s daemon did not come up", mode)
			}
			time.Sleep(20 * time.Millisecond)
		}
		start := time.Now()
		if err := cmdStop(context.Background(), nil); err != nil {
			d.Process.Kill()
			d.Wait()
			t.Fatalf("stop %s: %v", mode, err)
		}
		if took := time.Since(start); took > 5*time.Second {
			t.Errorf("stop %s took %s", mode, took)
		}
		if err := d.Wait(); err != nil {
			t.Fatalf("%s daemon: %v", mode, err)
		}
	}
	if err := cmdStop(context.Background(), []string{"x"}); err == nil {
		t.Fatal("arguments accepted")
	}
}

// testDaemon is the stand-in daemon of TestStop, and of
// TestReportPrefixOnce's start: serve is the real daemon with the
// shutdown message; legacy is one without it, ended by SIGTERM.
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
	case "nolisten", "wedged", "wedged-old":
		// The lock and the record, and nothing to ask: no listener at
		// the record's address, or one that accepts and never answers;
		// -old writes a record without the start, as an older build.
		err = wedgedServe(ctx, mode != "nolisten", mode == "wedged-old")
	case "absent":
		// Nothing: a start that never comes up.
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
		return errors.New("signalled before listening")
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
	go serveFake(ln, protocol.Message{Type: protocol.TypeHello, Protocol: protocol.Version, EnvironmentID: "legacy", Version: "legacy", Capabilities: []string{protocol.CapStatus}}, nil)
	<-ctx.Done()
	return nil
}

// wedgedServe is a daemon that cannot be asked: the lock and the
// runtime record, then a listener that accepts and says nothing, or
// none at the record's address. SIGTERM ends it.
func wedgedServe(ctx context.Context, listen, old bool) error {
	lock, err := home.TryLock()
	if err != nil {
		return err
	}
	defer lock.Release()
	// Nothing listens at a socket path in the state directory; a port
	// released would be another process's to take.
	addr := "unix:" + filepath.Join(home.Dir(), "wedged.sock")
	if listen {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return err
		}
		defer ln.Close()
		addr = "tcp:" + ln.Addr().String()
		go func() {
			for {
				c, err := ln.Accept()
				if err != nil {
					return
				}
				go func() { <-ctx.Done(); c.Close() }()
			}
		}()
	}
	rt := home.Runtime{Address: addr, PID: os.Getpid(), Version: "wedged", StartedAt: time.Now()}
	if self, ok := procs.Lookup(os.Getpid()); ok && !old {
		rt.ProcessStart = self.StartID
	}
	if err := home.WriteRuntime(rt); err != nil {
		return err
	}
	defer home.RemoveRuntime(os.Getpid())
	<-ctx.Done()
	return nil
}

// crashLeft is what a daemon that died without its defers leaves: a
// lock file and a record naming its pid, with that pid now another
// process's, the bystander, which a stop must never signal.
func crashLeft(t *testing.T) *exec.Cmd {
	t.Helper()
	t.Setenv("LAATMUX_HOME", t.TempDir())
	t.Setenv("LAATMUX_CONFIG", filepath.Join(t.TempDir(), "none.yaml"))
	t.Setenv("TMUX_TMPDIR", t.TempDir())
	bystander := exec.Command("sleep", "30")
	if err := bystander.Start(); err != nil {
		t.Fatal(err)
	}
	// signalled's goroutine is the one that waits; the cleanup only
	// ends it.
	t.Cleanup(func() { bystander.Process.Kill() })
	if err := os.WriteFile(filepath.Join(home.Dir(), "daemon.lock"), []byte(fmt.Sprintf("%d\n", bystander.Process.Pid)), 0o600); err != nil {
		t.Fatal(err)
	}
	// The record carries the dead daemon's start, which is not the
	// bystander's.
	if err := home.WriteRuntime(home.Runtime{Address: "unix:" + filepath.Join(home.Dir(), "none.sock"), PID: bystander.Process.Pid, Version: "crashed", StartedAt: time.Now(), ProcessStart: "1"}); err != nil {
		t.Fatal(err)
	}
	return bystander
}

// signalled reports whether the bystander ended within a moment; it
// is called once per bystander, and its goroutine reaps it.
func signalled(bystander *exec.Cmd) error {
	done := make(chan error, 1)
	go func() { done <- bystander.Wait() }()
	select {
	case err := <-done:
		if err == nil {
			err = errors.New("exited")
		}
		return err
	case <-time.After(300 * time.Millisecond):
		return nil
	}
}

// holdLock takes the flock on the lock file without rewriting it, as a
// daemon between its flock and its truncate holds it.
func holdLock(t *testing.T) *os.File {
	t.Helper()
	f, err := os.OpenFile(filepath.Join(home.Dir(), "daemon.lock"), os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	return f
}

// heldBy reports whether the helper with pid holds the startup lock,
// for a test waiting for one to start. It reads the lock file first,
// which TryLock rewrites with the holder's pid once it has the lock,
// and probes the lock only after, so the wait's probes do not take a
// free lock for an instant while the helper starts: TryLock tries again
// past such a probe (home's TestTryLockOutlastsAProbe), and the waits
// do not lean on that.
func heldBy(pid int) bool {
	b, _ := os.ReadFile(filepath.Join(home.Dir(), "daemon.lock"))
	if strings.TrimSpace(string(b)) != strconv.Itoa(pid) {
		return false
	}
	holder, _ := home.Holder()
	return holder == pid
}

// A crash-left record names a pid since reused, and the files alone say
// it holds the lock: stop never signals it. A new daemon stalled
// between taking the lock and rewriting the file shows the old content
// on every reading; a probe holds the free lock for an instant over it;
// a new daemon holds the lock with its own pid written.
func TestStopNeverSignalsAReusedPid(t *testing.T) {
	was := stopWait
	stopWait = time.Second
	defer func() { stopWait = was }()
	t.Run("stalled winner", func(t *testing.T) {
		bystander := crashLeft(t)
		holdLock(t)
		err := cmdStop(context.Background(), nil)
		if err == nil || !strings.Contains(err.Error(), "holds the lock but answers on no socket") {
			t.Errorf("stop: %v", err)
		}
		if err := signalled(bystander); err != nil {
			t.Fatalf("bystander (pid %d) signalled while another holds the lock: %v", bystander.Process.Pid, err)
		}
	})
	t.Run("short probe", func(t *testing.T) {
		bystander := crashLeft(t)
		f := holdLock(t)
		unlocked := make(chan struct{})
		go func() {
			time.Sleep(50 * time.Millisecond)
			syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
			close(unlocked)
		}()
		cmdStop(context.Background(), nil)
		<-unlocked
		if err := signalled(bystander); err != nil {
			t.Fatalf("bystander signalled after a short probe: %v", err)
		}
	})
	t.Run("identified, another holder", func(t *testing.T) {
		// The record names a laatmux by pid and start, which is this
		// machine's daemon of another state directory; our lock is
		// another daemon's. The holder check alone keeps it unsignalled.
		crashLeft(t)
		twin := exec.Command(os.Args[0], "-test.run=TestStop")
		twin.Env = append(os.Environ(), "LAATMUX_TEST_DAEMON=held", "LAATMUX_HOME="+t.TempDir())
		if err := twin.Start(); err != nil {
			t.Fatal(err)
		}
		var twinErr error
		twinDone := make(chan struct{})
		go func() { twinErr = twin.Wait(); close(twinDone) }()
		defer func() {
			twin.Process.Kill()
			<-twinDone
		}()
		p, ok := procs.Lookup(twin.Process.Pid)
		if !ok {
			t.Fatal("no lookup of the twin")
		}
		if err := home.WriteRuntime(home.Runtime{Address: "unix:" + filepath.Join(home.Dir(), "none.sock"), PID: twin.Process.Pid, Version: "twin", StartedAt: time.Now(), ProcessStart: p.StartID}); err != nil {
			t.Fatal(err)
		}
		held := exec.Command(os.Args[0], "-test.run=TestStop")
		held.Env = append(os.Environ(), "LAATMUX_TEST_DAEMON=held")
		if err := held.Start(); err != nil {
			t.Fatal(err)
		}
		defer func() { held.Process.Signal(syscall.SIGTERM); held.Wait() }()
		for deadline := time.Now().Add(10 * time.Second); ; {
			if heldBy(held.Process.Pid) {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("held did not take the lock")
			}
			time.Sleep(20 * time.Millisecond)
		}
		if err := cmdStop(context.Background(), nil); err == nil || !strings.Contains(err.Error(), "while the runtime record's") {
			t.Errorf("stop: %v", err)
		}
		select {
		case <-twinDone:
			t.Fatalf("the record's process signalled while another holds the lock (exit %v)", twinErr)
		case <-time.After(300 * time.Millisecond):
		}
	})
	t.Run("other holder", func(t *testing.T) {
		bystander := crashLeft(t)
		held := exec.Command(os.Args[0], "-test.run=TestStop")
		held.Env = append(os.Environ(), "LAATMUX_TEST_DAEMON=held")
		if err := held.Start(); err != nil {
			t.Fatal(err)
		}
		defer func() { held.Process.Signal(syscall.SIGTERM); held.Wait() }()
		for deadline := time.Now().Add(10 * time.Second); ; {
			if heldBy(held.Process.Pid) {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("held did not take the lock")
			}
			time.Sleep(20 * time.Millisecond)
		}
		cmdStop(context.Background(), nil)
		if err := signalled(bystander); err != nil {
			t.Fatalf("bystander signalled while another daemon holds the lock: %v", err)
		}
	})
}

// The hello wait is bounded by stop's own deadline: a wedged daemon
// the record does not identify (a build before the start field) is
// reported when the deadline passes, not after the whole hello wait.
func TestStopHelloWaitBounded(t *testing.T) {
	t.Setenv("LAATMUX_HOME", t.TempDir())
	t.Setenv("LAATMUX_CONFIG", filepath.Join(t.TempDir(), "none.yaml"))
	t.Setenv("TMUX_TMPDIR", t.TempDir())
	d := exec.Command(os.Args[0], "-test.run=TestStop")
	d.Env = append(os.Environ(), "LAATMUX_TEST_DAEMON=wedged-old")
	if err := d.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- d.Wait() }()
	defer func() {
		d.Process.Kill()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
		}
	}()
	for deadline := time.Now().Add(10 * time.Second); ; {
		if rt, err := home.ReadRuntime(); err == nil && rt.PID == d.Process.Pid && heldBy(d.Process.Pid) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("did not come up")
		}
		time.Sleep(20 * time.Millisecond)
	}
	was := stopWait
	stopWait = time.Second
	defer func() { stopWait = was }()
	start := time.Now()
	err := cmdStop(context.Background(), nil)
	if took := time.Since(start); err == nil || !strings.Contains(err.Error(), "gives no hello before the wait ran out after") || !strings.Contains(err.Error(), fmt.Sprintf("kill %d ends it", d.Process.Pid)) || took > 3*time.Second {
		t.Fatalf("stop of an unidentified wedged daemon: %v after %s", err, took)
	}
	select {
	case err := <-done:
		t.Fatalf("an unidentified daemon signalled (exit %v)", err)
	case <-time.After(300 * time.Millisecond):
	}
}

// A stop whose context is cancelled signals nothing, against a daemon
// that holds the lock, is the record's and listens on nothing.
func TestStopCancelledSignalsNothing(t *testing.T) {
	t.Setenv("LAATMUX_HOME", t.TempDir())
	t.Setenv("LAATMUX_CONFIG", filepath.Join(t.TempDir(), "none.yaml"))
	t.Setenv("TMUX_TMPDIR", t.TempDir())
	d := exec.Command(os.Args[0], "-test.run=TestStop")
	d.Env = append(os.Environ(), "LAATMUX_TEST_DAEMON=nolisten")
	if err := d.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- d.Wait() }()
	defer func() {
		d.Process.Kill()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
		}
	}()
	for deadline := time.Now().Add(10 * time.Second); ; {
		if rt, err := home.ReadRuntime(); err == nil && rt.PID == d.Process.Pid && heldBy(d.Process.Pid) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("did not come up")
		}
		time.Sleep(20 * time.Millisecond)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := cmdStop(ctx, nil); !errors.Is(err, context.Canceled) {
		t.Errorf("stop with a cancelled context: %v", err)
	}
	// Two seconds: a signalled helper takes about one to exit under
	// the race detector.
	select {
	case err := <-done:
		t.Fatalf("daemon signalled under a cancelled context (exit %v)", err)
	case <-time.After(2 * time.Second):
	}
}

// The record's daemon is told from a pid reused since it died by the
// start the kernel keeps for the process, which the record carries:
// the same pid with another start is another process, and a record
// without the start names no daemon this way.
func TestIsDaemon(t *testing.T) {
	me := os.Getpid()
	self, ok := procs.Lookup(me)
	if !ok || self.StartID == "" {
		t.Fatalf("no lookup of this process: %+v %v", self, ok)
	}
	if !isDaemon(home.Runtime{PID: me, ProcessStart: self.StartID}) {
		t.Error("this process, with its own start: not the daemon")
	}
	if isDaemon(home.Runtime{PID: me, ProcessStart: self.StartID + "1"}) {
		t.Error("this pid with another start taken as the daemon")
	}
	if isDaemon(home.Runtime{PID: me}) {
		t.Error("a record without the start taken as naming the daemon")
	}
	other := exec.Command("sleep", "100")
	if err := other.Start(); err != nil {
		t.Fatal(err)
	}
	defer other.Process.Kill()
	// Linux counts starts in clock ticks: a child started in this
	// process's tick has its start, and the case is not made then.
	started := func(p *os.Process) string {
		lp, ok := procs.Lookup(p.Pid)
		if !ok || lp.StartID == "" {
			t.Fatalf("no lookup of pid %d: %+v %v", p.Pid, lp, ok)
		}
		return lp.StartID
	}
	if started(other.Process) != self.StartID && isDaemon(home.Runtime{PID: other.Process.Pid, ProcessStart: self.StartID}) {
		t.Error("another process with this one's start taken as the daemon")
	}
	other.Process.Kill()
	other.Wait()
	if isDaemon(home.Runtime{PID: 1 << 30, ProcessStart: self.StartID}) {
		t.Error("a pid that is no process taken as the daemon")
	}
	// A laatmux of the same binary whose record names another start:
	// the pid reused by a laatmux, or a crash-left record naming the
	// pid of the daemon that replaced it.
	twin := exec.Command(os.Args[0], "-test.run=TestStop")
	twin.Env = append(os.Environ(), "LAATMUX_TEST_DAEMON=held", "LAATMUX_HOME="+t.TempDir())
	if err := twin.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { twin.Process.Kill(); twin.Wait() }()
	if started(twin.Process) != self.StartID && isDaemon(home.Runtime{PID: twin.Process.Pid, ProcessStart: self.StartID}) {
		t.Error("another laatmux with this one's start taken as the daemon")
	}
	if isDaemon(home.Runtime{PID: twin.Process.Pid, ProcessStart: started(twin.Process) + "1"}) {
		t.Error("a laatmux with another start taken as the daemon")
	}
	if !isDaemon(home.Runtime{PID: twin.Process.Pid, ProcessStart: started(twin.Process)}) {
		t.Error("a laatmux with its own start not the daemon")
	}
}

func TestMain(m *testing.M) {
	// First: a daemon client.StartDaemon starts is this binary with
	// "serve" and no -test.run, which would run the whole suite.
	if mode := os.Getenv("LAATMUX_TEST_DAEMON"); mode != "" {
		testDaemon(mode)
	}
	// A start as "serve" without it, from a test that reached
	// client.StartDaemon with no stand-in asked for, ends at once, as a
	// daemon that never comes up: run on, it would run the whole suite
	// again, detached, and a test that starts a daemon would start
	// another.
	if len(os.Args) > 1 && os.Args[1] == "serve" {
		fmt.Fprintln(os.Stderr, noDaemon)
		os.Exit(1)
	}
	// A child of TestRunStateIsItsOwn that got past the check above ends
	// here, with a status of its own: run on, it would run the suite,
	// and that test would start another child.
	if os.Getenv("LAATMUX_TEST_SERVED") != "" {
		fmt.Fprintln(os.Stderr, "started as a daemon, and not stopped")
		os.Exit(2)
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
	// Nor the user's laatmux, whatever the environment names: the state
	// directory, with last.json and the runtime file that names their
	// daemon, and the config, a file that is not there, are the run's
	// own. A test that wants either sets it itself. After the daemon
	// above, so a daemon a test starts keeps the test's; any other child
	// of this binary is a run of its own, and gets its own.
	runDir = dir
	os.Setenv("LAATMUX_HOME", filepath.Join(dir, "home"))
	os.Setenv("LAATMUX_CONFIG", filepath.Join(dir, "config.yaml"))
	code := m.Run()
	exec.Command("tmux", "-L", "default", "kill-server").Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

// runDir is the directory TestMain gives the run: its tmux sockets,
// its state directory and its config are under it.
var runDir string

// noDaemon is what the binary says, started as a daemon with no
// stand-in named.
const noDaemon = "the cmd/laatmux tests start no daemon without LAATMUX_TEST_DAEMON"

// A test that sets no LAATMUX_HOME or LAATMUX_CONFIG, as this one, has
// the run's. Run again under a state directory whose runtime file names
// a live daemon at a socket that is not there, and a config file, it
// reads neither. The binary started as client.StartDaemon starts it,
// with or without arguments after "serve", runs no test: it runs the
// stand-in LAATMUX_TEST_DAEMON names, or none.
func TestRunStateIsItsOwn(t *testing.T) {
	if runDir == "" {
		t.Fatal("TestMain gave the run no directory")
	}
	for what, p := range map[string]string{"state directory": home.Dir(), "config": config.Path()} {
		if !strings.HasPrefix(p, runDir+"/") {
			t.Errorf("the %s is %s, not under the run's %s", what, p, runDir)
		}
	}
	if os.Getenv("LAATMUX_TEST_USER_STATE") != "" {
		if rt, err := home.ReadRuntime(); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("the user's runtime file read: %+v %v", rt, err)
		}
		if _, err := os.Stat(config.Path()); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("a config at %s: %v", config.Path(), err)
		}
		return
	}
	// As StartDaemon starts it: its own executable, with "serve" and
	// what LAATMUX_SERVE_ARGS adds. With no stand-in named, the guard's
	// status, 1, and its line first; a binary built with -cover may add
	// one of its own at exit. With one, that stand-in: absent exits 0.
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"serve"}, {"serve", "--listen", "tcp:127.0.0.1:0"}} {
		for _, mode := range []string{"", "absent"} {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			serve := exec.CommandContext(ctx, self, args...)
			serve.Env = append(os.Environ(), "LAATMUX_TEST_SERVED=1", "LAATMUX_TEST_DAEMON="+mode)
			out, err := serve.CombinedOutput()
			cancel()
			var exit *exec.ExitError
			if mode == "" && (!errors.As(err, &exit) || exit.ExitCode() != 1 || !strings.HasPrefix(string(out), noDaemon+"\n")) {
				t.Errorf("started with %q: %v\n%s", args, err, out)
			}
			if mode != "" && (err != nil || strings.Contains(string(out), noDaemon)) {
				t.Errorf("started with %q and the %s stand-in: %v\n%s", args, mode, err, out)
			}
		}
	}
	t.Setenv("LAATMUX_HOME", t.TempDir())
	t.Setenv("LAATMUX_CONFIG", filepath.Join(t.TempDir(), "config.yaml"))
	if err := home.WriteRuntime(home.Runtime{Address: "unix:" + filepath.Join(home.Dir(), "laatmux.sock"), PID: os.Getpid()}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(config.Path(), []byte("hosts: [\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	run := exec.Command(os.Args[0], "-test.run=^TestRunStateIsItsOwn$")
	run.Env = append(os.Environ(), "LAATMUX_TEST_USER_STATE=1")
	if out, err := run.CombinedOutput(); err != nil {
		t.Errorf("run under the user's state and config: %v\n%s", err, out)
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
