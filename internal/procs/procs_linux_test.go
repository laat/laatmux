package procs

import (
	"bytes"
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"
)

// /proc/<pid>/stat as Linux writes it, a comm with a paren in it: a
// zombie with no thread left and a process being reaped have exited; a
// leader that ended with pthread_exit while another thread runs shows Z
// as well, in a live process, which is still listed.
func TestParseStatExited(t *testing.T) {
	for _, c := range []struct {
		state   string
		threads int
		exited  bool
	}{
		{"S", 1, false}, {"R", 3, false}, {"Z", 1, true}, {"Z", 2, false}, {"X", 1, true},
	} {
		b := fmt.Appendf(nil, "42 (a) b) %s 1 42 42 34816 42 4194304 0 0 0 0 0 0 0 0 20 0 %d 0 12345 0 0\n", c.state, c.threads)
		p, tty, exited, ok := parseStat(42, b)
		if !ok || p.Comm != "a) b" || tty != 34816 || exited != c.exited {
			t.Errorf("state %s with %d threads: comm %q tty %d exited %v ok %v, want exited %v", c.state, c.threads, p.Comm, tty, exited, ok, c.exited)
		}
	}
}

// zombieOn reports whether pid is a zombie whose controlling terminal
// is tty, read from /proc apart from the code under test.
func zombieOn(t *testing.T, pid int, tty string) bool {
	t.Helper()
	st, err := os.Stat(tty)
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		t.Fatal(err)
	}
	fields := strings.Fields(string(b[bytes.LastIndexByte(b, ')')+2:]))
	return fields[0] == "Z" && fields[4] == strconv.FormatUint(uint64(st.Sys().(*syscall.Stat_t).Rdev), 10)
}

// openPTY opens a pty's master and names its slave, as posix_openpt,
// unlockpt and ptsname do.
func openPTY(t *testing.T) (*os.File, string) {
	t.Helper()
	m, err := os.OpenFile("/dev/ptmx", os.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.Close() })
	fd := int(m.Fd())
	if err := unix.IoctlSetPointerInt(fd, unix.TIOCSPTLCK, 0); err != nil {
		t.Fatal("unlockpt:", err)
	}
	n, err := unix.IoctlGetUint32(fd, unix.TIOCGPTN)
	if err != nil {
		t.Fatal("ptsname:", err)
	}
	return m, "/dev/pts/" + strconv.FormatUint(uint64(n), 10)
}
