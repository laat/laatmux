package home

import (
	"os"
	"os/exec"
	"testing"
	"time"
)

// Review finding 4: two starters must not both own the lock. A child
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
	if l, err := TryLock(); err == nil {
		l.Release()
		t.Fatal("second owner acquired the lock while the first holds it")
	}
	cmd.Wait()
	l, err := TryLock()
	if err != nil {
		t.Fatalf("lock not released after holder exit: %v", err)
	}
	l.Release()
}
