package procs

import (
	"os"
	"slices"
	"syscall"
	"testing"
	"unsafe"

	"golang.org/x/sys/unix"
)

// The buffers are kern.procargs2's layout as macOS gives it for
// `probe a "" x` with PROBE=1 and B=2, cut short: an empty argument is
// a lone NUL in argv and stays there; it shifted the first env string
// into argv once and lost it from env. A buffer that ends before argc
// strings gives the whole ones it has.
func TestParseProcArgs(t *testing.T) {
	for _, c := range []struct {
		name      string
		raw       string
		argv, env []string
	}{
		{"empty argument", "\x04\x00\x00\x00/p/probe\x00\x00\x00\x00\x00/p/probe\x00a\x00\x00x\x00PROBE=1\x00B=2\x00\x00\x00\x00ptr_munge=\x00\x00\x00executable_file=0x1\x00\x00\x00",
			[]string{"/p/probe", "a", "", "x"}, []string{"PROBE=1", "B=2", "ptr_munge=", "executable_file=0x1"}},
		{"short buffer", "\x03\x00\x00\x00/p/x\x00\x00\x00/p/x\x00a\x00", []string{"/p/x", "a"}, nil},
		{"cut in a string", "\x03\x00\x00\x00/p/x\x00\x00\x00/p/x\x00ab", []string{"/p/x"}, nil},
		{"no argv", "\x00\x00\x00\x00/p/x\x00\x00\x00B=2\x00", []string{}, []string{"B=2"}},
		{"no exec path end", "\x01\x00\x00\x00/p/x", nil, nil},
		{"no argc", "\x01\x00", nil, nil},
	} {
		argv, env := parseProcArgs([]byte(c.raw))
		if !slices.Equal(argv, c.argv) || !slices.Equal(env, c.env) {
			t.Errorf("%s: argv %q env %q, want %q and %q", c.name, argv, env, c.argv, c.env)
		}
	}
}

// zombieOn reports whether pid is a zombie whose controlling terminal
// is tty.
func zombieOn(t *testing.T, pid int, tty string) bool {
	t.Helper()
	st, err := os.Stat(tty)
	if err != nil {
		t.Fatal(err)
	}
	kp, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil {
		t.Fatal(err)
	}
	return kp.Proc.P_stat == sZomb && kp.Eproc.Tdev == st.Sys().(*syscall.Stat_t).Rdev
}

// openPTY opens a pty's master and names its slave, as posix_openpt,
// grantpt, unlockpt and ptsname do.
func openPTY(t *testing.T) (*os.File, string) {
	t.Helper()
	m, err := os.OpenFile("/dev/ptmx", os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.Close() })
	fd := int(m.Fd())
	if err := unix.IoctlSetInt(fd, unix.TIOCPTYGRANT, 0); err != nil {
		t.Fatal("grantpt:", err)
	}
	if err := unix.IoctlSetInt(fd, unix.TIOCPTYUNLK, 0); err != nil {
		t.Fatal("unlockpt:", err)
	}
	var name [128]byte
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), unix.TIOCPTYGNAME, uintptr(unsafe.Pointer(&name[0]))); errno != 0 {
		t.Fatal("ptsname:", errno)
	}
	return m, unix.ByteSliceToString(name[:])
}
