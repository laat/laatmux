package daemon

import (
	"go/version"
	"strings"
	"testing"
)

func TestRaceForkBroken(t *testing.T) {
	for v, want := range map[string]bool{
		"go1.26.0": true, "go1.26rc2": true, "go1.26beta1": true, "go1.26.3": true, "go1.26.4": true,
		"go1.26.3-X:nogreenteagc": true, "go1.26.3-custom X:nodwarf5": true, "go1.26.3 (Red Hat 1.26.3-1.el9)": true,
		"go1.26.5": false, "go1.26.8": false, "go1.26.8-X:nogreenteagc": false, "go1.26.10": false,
		"go1.25.9": false, "go1.27.1": false, "devel go1.26-abcdef": false,
	} {
		if got := raceForkBroken(v); got != want {
			t.Errorf("raceForkBroken(%q) = %v, want %v", v, got, want)
		}
	}
}

// raceForkBroken reports whether a toolchain of the version
// runtime.Version gives has the fork bug race_darwin_test.go stops:
// go1.26 before go1.26.5. A devel version does not say whether it has
// the fix, and passes.
func raceForkBroken(v string) bool {
	// After a space comes an experiment or distribution note; a suffix
	// after a hyphen go/version ignores.
	v, _, _ = strings.Cut(v, " ")
	return version.Lang(v) == "go1.26" && version.Compare(v, "go1.26.5") < 0
}
