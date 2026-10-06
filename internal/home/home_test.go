package home

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// Two starters must not both own the lock. A child
// process holds it while the parent tries; flock is per open file
// description, so the test needs a second process.
func TestLockExcludesSecondOwner(t *testing.T) {
	if os.Getenv("LAATMUX_TEST_HOLD_LOCK") == "1" {
		// Child: LAATMUX_HOME comes from the parent.
		l, err := TryLock()
		if err != nil {
			os.Exit(3)
		}
		defer l.Release()
		os.Stdout.WriteString("held\n")
		time.Sleep(2 * time.Second)
		os.Exit(0)
	}
	dir := t.TempDir()
	t.Setenv("LAATMUX_HOME", dir)
	cmd := exec.Command(os.Args[0], "-test.run=TestLockExcludesSecondOwner")
	cmd.Env = append(os.Environ(), "LAATMUX_TEST_HOLD_LOCK=1", "LAATMUX_HOME="+dir)
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer cmd.Wait()
	buf := make([]byte, 5)
	if _, err := out.Read(buf); err != nil || string(buf) != "held\n" {
		t.Fatalf("child did not take lock: %q %v", buf, err)
	}
	// A second serve against a daemon is told who holds the lock, once
	// its tries have run out and not much later.
	start := time.Now()
	if l, err := TryLock(); err == nil {
		l.Release()
		t.Fatal("second owner acquired the lock while the first holds it")
	} else if want := fmt.Sprintf("daemon lock held by pid %d", cmd.Process.Pid); !strings.Contains(err.Error(), want) {
		t.Fatalf("second owner: %v, want %q", err, want)
	}
	if took := time.Since(start); took < lockRetry || took > time.Second {
		t.Errorf("second owner told after %s", took)
	}
	cmd.Wait()
	l, err := TryLock()
	if err != nil {
		t.Fatalf("lock not released after holder exit: %v", err)
	}
	l.Release()
}

// A daemon whose TryLock falls in the instant Holder's probe holds the
// free lock, as one starting while stop's last probe finds the old
// daemon gone, takes the lock once the probe lets go. The probe is kept
// holding until TryLock's first try has been refused, and let go in the
// pause after it. flock is per open file description, so a probe and a
// start in one process exclude each other as in two.
func TestTryLockOutlastsAProbe(t *testing.T) {
	t.Setenv("LAATMUX_HOME", t.TempDir())
	inProbe, letGo := make(chan struct{}), make(chan struct{})
	probeDone := make(chan error, 1)
	probed = func() { close(inProbe); <-letGo }
	refused := false // TryLock found the probe holding the lock
	var probeErr error
	pause = func(d time.Duration) {
		if !refused {
			refused = true
			close(letGo)
			probeErr = <-probeDone
		}
		time.Sleep(d)
	}
	defer func() {
		if !refused {
			close(letGo)
			<-probeDone
		}
		probed, pause = func() {}, time.Sleep
	}()
	go func() {
		_, err := Holder()
		probeDone <- err
	}()
	<-inProbe
	l, err := TryLock()
	if err != nil {
		t.Fatalf("a start during a probe of the free lock: %v", err)
	}
	defer l.Release()
	if !refused {
		t.Fatal("TryLock was never refused: the probe did not hold the lock")
	}
	if probeErr != nil {
		t.Fatalf("probe: %v", probeErr)
	}
	if pid, err := Holder(); err != nil || pid != os.Getpid() {
		t.Errorf("holder after the start: %d %v, want %d", pid, err, os.Getpid())
	}
}
