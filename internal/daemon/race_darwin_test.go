//go:build race

package daemon

import (
	"fmt"
	"os"
	"runtime"
)

// Go 1.26.0 to 1.26.4 build darwin's raw syscall wrappers as Go code in
// package syscall, which the race detector instruments, and the child
// of a fork calls them before exec with its parent's thread state. The
// child can crash in any of those calls (a ThreadSanitizer CHECK and
// exit 66, or SIGSEGV), which reads as the command failing, or hang.
// One that hangs before it replaces its stdio holds the test binary's
// stdout: once the tests pass, go test waits a minute on it and fails
// the package with "Test I/O incomplete". The toolchain line in go.mod
// takes a release with the fix; this stops a -race run on darwin under
// one without it before any test runs, rather than leave it to flake.
// It sits here because go test ./... runs it; other packages fork git
// too.
func init() {
	if raceForkBroken(runtime.Version()) {
		fmt.Fprintf(os.Stderr, "%s with -race on darwin can crash or hang a forked child before exec (fixed in go1.26.5); "+
			"run with go.mod's toolchain (GOTOOLCHAIN=auto)\n", runtime.Version())
		os.Exit(1)
	}
}
