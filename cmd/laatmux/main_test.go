package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/laat/laatmux/internal/client"
	"github.com/laat/laatmux/internal/home"
)

// A command's error is printed with "laatmux: " once: the packages'
// errors leave it to main. A second serve, against a daemon that holds
// the lock, and a client with no daemon to dial printed it twice, and a
// daemon start that never came up put it mid-line in the dashboard's
// "local daemon: " error.
func TestReportPrefixOnce(t *testing.T) {
	base := t.TempDir()
	t.Setenv("LAATMUX_HOME", filepath.Join(base, "home"))
	t.Setenv("LAATMUX_CONFIG", filepath.Join(base, "none.yaml"))
	held, err := home.TryLock()
	if err != nil {
		t.Fatal(err)
	}
	defer held.Release()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	serveErr := cmdServe(ctx, []string{"--listen", "tcp:127.0.0.1:0"})
	_, dialErr := client.DialLocal(ctx, false)
	_, addrErr := client.DialAddress("nowhere")
	for _, c := range []struct {
		err  error
		want string
		code int
		exit bool
	}{
		{serveErr, fmt.Sprintf("laatmux: daemon lock held by pid %d\n", os.Getpid()), 1, true},
		{dialErr, "laatmux: daemon not running\n", 1, true},
		{addrErr, "laatmux: bad runtime address \"nowhere\"\n", 1, true},
		{home.ErrStale, "laatmux: runtime file is stale\n", 1, true},
		{&exitError{code: 130, msg: "cancelled"}, "laatmux: cancelled\n", 130, true},
		{fmt.Errorf("run: %w", &exitError{code: 7, msg: "m"}), "laatmux: m\n", 7, true},
		{&exitError{code: 3}, "", 3, true},
		{&exitError{}, "", 0, true},
		{context.Canceled, "", 0, false},
		{fmt.Errorf("local daemon: %w", context.Canceled), "", 0, false},
		{nil, "", 0, false},
	} {
		var b strings.Builder
		if code, exit := report(&b, c.err); b.String() != c.want || code != c.code || exit != c.exit {
			t.Errorf("%v: printed %q, exit %d %v; want %q, exit %d %v", c.err, b.String(), code, exit, c.want, c.code, c.exit)
		}
	}
	// The start runs this test binary, which exits at once in the
	// absent mode; the dial gives up after its 5 s.
	t.Setenv("LAATMUX_TEST_DAEMON", "absent")
	_, err = dialMergedOrExplain(ctx)
	var b strings.Builder
	report(&b, err)
	if got := b.String(); !strings.HasPrefix(got, "laatmux: local daemon: daemon did not come up; ") || strings.Count(got, "laatmux:") != 1 {
		t.Errorf("a start that does not come up: printed %q", got)
	}
}
