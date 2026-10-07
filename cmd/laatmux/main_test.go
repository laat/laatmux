package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/laat/laatmux/internal/client"
	"github.com/laat/laatmux/internal/daemon"
	"github.com/laat/laatmux/internal/home"
	"github.com/laat/laatmux/internal/merged"
	"github.com/laat/laatmux/internal/protocol"
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

// The commands' errors that name the state directory, from a
// LAATMUX_HOME with a tab and an ESC in its name, name it as
// tmux.Printable shows it: stop's reads of the runtime file and the
// lock, serve's lock and unix socket, the sidebar's lock and tasks
// show's read of a record. laatmux tasks prints an add that failed at
// the journal with the journal's file quoted once, not the whole error
// quoted again.
func TestStateDirQuoted(t *testing.T) {
	q := strconv.Quote
	check := func(what string, err error, want string) {
		t.Helper()
		if err == nil || !strings.HasPrefix(err.Error(), want) || strings.ContainsAny(err.Error(), "\t\x1b") {
			t.Errorf("%s: %v, want %s...", what, err, want)
		}
	}
	ctx := context.Background()
	base := t.TempDir()
	t.Setenv("LAATMUX_CONFIG", filepath.Join(base, "none.yaml"))
	t.Setenv("TMUX_TMPDIR", base)
	file := filepath.Join(base, "fi\tle\x1b[31m")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LAATMUX_HOME", filepath.Join(file, "state"))
	check("stop", cmdStop(ctx, nil), "open "+q(filepath.Join(file, "state", "runtime.json"))+": ")
	check("serve", cmdServe(ctx, []string{"--listen", "tcp:127.0.0.1:0"}), "mkdir "+q(file)+": ")
	_, err := sidebarLock()
	check("sidebar lock", err, "mkdir "+q(file)+": ")

	dir := filepath.Join(base, "st\tate\x1b[31m")
	t.Setenv("LAATMUX_HOME", dir)
	path := func(name ...string) string { return filepath.Join(append([]string{dir}, name...)...) }
	for _, p := range []string{path("daemon.lock"), path("sidebar.lock"), path("pending", daemon.FileName("t1"))} {
		if err := os.MkdirAll(p, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	check("stop", cmdStop(ctx, nil), "open "+q(path("daemon.lock"))+": ")
	check("serve", cmdServe(ctx, []string{"--listen", "tcp:127.0.0.1:0"}), "open "+q(path("daemon.lock"))+": ")
	_, err = sidebarLock()
	check("sidebar lock", err, "open "+q(path("sidebar.lock"))+": ")
	check("tasks show", showTask("t1"), "read "+q(path("pending", daemon.FileName("t1")))+": ")
	if err := os.Remove(path("daemon.lock")); err != nil {
		t.Fatal(err)
	}
	sock := path("gone", "s.sock")
	check("serve", cmdServe(ctx, []string{"--listen", "unix:" + sock}), "listen unix "+q(sock)+": ")

	// The journal's write as the daemon gives it, with a file where its
	// directory should be: the add's error the relay keeps.
	commands := path("commands")
	if err := os.WriteFile(commands, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	werr := home.WriteAtomic(filepath.Join(commands, "t2.json"), nil)
	if werr == nil {
		t.Fatal("a write under a file succeeded")
	}
	check("journal", werr, "open "+q(filepath.Join(commands, "t2.json.tmp."+strconv.Itoa(os.Getpid())))+": ")
	msg := "failed at resolve: " + werr.Error()
	m := merged.New()
	m.Apply(protocol.Message{Type: protocol.TypeSnapshot,
		Hosts:    []protocol.HostStatus{{Name: "vm", SSH: "vm", EnvironmentID: "venv"}},
		Pendings: []protocol.Pending{{ID: "t2", Host: "vm", Repo: "proj", Branch: "b", Taken: true, Done: true, Stage: protocol.StageResolve, Error: msg}},
	})
	if got := taskReport(m.Status("")); !strings.HasPrefix(got, "t2  proj/b on vm  ") || !strings.HasSuffix(got, "  "+msg+"\n") {
		t.Errorf("tasks:\n%q\nwant the line to end with %q", got, msg)
	}
}
