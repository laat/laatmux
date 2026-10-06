package daemon

import (
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"testing"
)

// Go 1.26.0 to 1.26.4 build darwin's raw syscall wrapper as Go code
// that the race detector instruments, and the child of a fork calls it
// before exec with its parent's thread state. The child then crashes
// (a ThreadSanitizer CHECK and exit 66, or SIGSEGV), which reads as the
// command failing, or spins for good holding every descriptor of the
// test binary: the command never starts, and once the tests pass go
// test waits a minute on the binary's stdout and fails the package with
// "Test I/O incomplete". This package starts git and shells throughout,
// some of them from daemon goroutines no test waits for. The toolchain
// line in go.mod takes a fixed release; this fails a -race run on
// darwin under any other, rather than leave it to flake.
func TestRaceForkToolchain(t *testing.T) {
	if runtime.GOOS != "darwin" || !raceBuild() {
		t.Skip("only a -race build on darwin is affected")
	}
	if raceForkBroken(runtime.Version()) {
		t.Fatalf("%s with -race on darwin can crash or hang a forked child before exec (fixed in go1.26.5); "+
			"run with go.mod's toolchain (GOTOOLCHAIN=auto)", runtime.Version())
	}
}

func TestRaceForkBroken(t *testing.T) {
	for v, want := range map[string]bool{
		"go1.26": true, "go1.26rc2": true, "go1.26.3": true, "go1.26.4": true, "go1.26.3 X:nodwarf5": true,
		"go1.26.5": false, "go1.26.8": false, "go1.26.10": false, "go1.25.9": false, "go1.27.1": false,
		"devel go1.27-abcdef": false,
	} {
		if got := raceForkBroken(v); got != want {
			t.Errorf("raceForkBroken(%q) = %v, want %v", v, got, want)
		}
	}
}

// raceBuild reports whether the test binary was built with -race.
func raceBuild() bool {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return false
	}
	for _, s := range info.Settings {
		if s.Key == "-race" {
			return s.Value == "true"
		}
	}
	return false
}

// raceForkBroken reports whether a toolchain of the version
// runtime.Version gives has the fork bug: go1.26 before go1.26.5.
func raceForkBroken(version string) bool {
	v, ok := strings.CutPrefix(version, "go1.26")
	if !ok {
		return false
	}
	v, _, _ = strings.Cut(v, " ")
	if v == "" || strings.HasPrefix(v, "rc") {
		return true
	}
	n, err := strconv.Atoi(strings.TrimPrefix(v, "."))
	return strings.HasPrefix(v, ".") && err == nil && n < 5
}
