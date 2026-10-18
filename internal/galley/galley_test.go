package galley

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// The listing read into records: staged ones alone, open and marked,
// the web UI's page as the URL, the first of a branch's kept; a
// published or closed one left to GitHub; the host named to galley
// off github.com.
func TestStaged(t *testing.T) {
	t.Setenv("GALLEY_URL", "http://galley.local:1234/")
	var got []string
	run := func(ctx context.Context, args ...string) ([]byte, error) {
		got = args
		return []byte(`[{"number":4,"state":"STAGED","headRefName":"fix-ls","baseRefName":"main","isDraft":false},
			{"number":5,"state":"staged","headRefName":"draft","baseRefName":"main","isDraft":true},
			{"number":3,"state":"staged","headRefName":"fix-ls","baseRefName":"main","isDraft":false},
			{"number":2,"state":"PUBLISHED","headRefName":"done","baseRefName":"main","isDraft":false},
			{"number":1,"state":"closed","headRefName":"gone","baseRefName":"main","isDraft":false}]`), nil
	}
	prs, err := Staged(context.Background(), run, "github.com", "laat", "laatmux")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) < 4 || got[3] != "laat/laatmux" {
		t.Errorf("args %q", got)
	}
	if len(prs) != 2 {
		t.Fatalf("prs %+v", prs)
	}
	if p := prs["fix-ls"]; p == nil || p.Number != 4 || p.State != "open" || !p.Staged || p.Draft || p.Base != "main" || p.URL != "http://galley.local:1234/laat/laatmux/pull/4" {
		t.Errorf("fix-ls: %+v", p)
	}
	if p := prs["draft"]; p == nil || p.Number != 5 || !p.Draft || !p.Staged {
		t.Errorf("draft: %+v", p)
	}
	if _, err := Staged(context.Background(), run, "ghe.example.com", "o", "r"); err != nil || got[3] != "ghe.example.com/o/r" {
		t.Errorf("an enterprise host: %q %v", got, err)
	}
	t.Setenv("GALLEY_URL", "")
	t.Setenv("GALLEY_PORT", "")
	if u := URL(); u != "http://localhost:24823" {
		t.Errorf("default url %q", u)
	}
	t.Setenv("GALLEY_PORT", "9000")
	if u := URL(); u != "http://localhost:9000" {
		t.Errorf("port url %q", u)
	}
	failing := func(context.Context, ...string) ([]byte, error) { return nil, ErrNotRunning }
	if _, err := Staged(context.Background(), failing, "github.com", "o", "r"); !errors.Is(err, ErrNotRunning) {
		t.Errorf("runner error not passed through: %v", err)
	}
	bad := func(context.Context, ...string) ([]byte, error) { return []byte("not json"), nil }
	if _, err := Staged(context.Background(), bad, "github.com", "o", "r"); err == nil {
		t.Error("bad json read as a listing")
	}
}

// The CLI runner: galley missing is its own error; one beside this
// program is found off PATH; the server not running, as the CLI says
// it without starting one, is its own.
func TestCLI(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PATH", dir)
	if _, err := CLI(context.Background(), "pr", "list"); !errors.Is(err, ErrNoGalley) {
		t.Errorf("no galley: %v", err)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	beside := filepath.Join(filepath.Dir(exe), "galley")
	if err := writeExec(beside, "#!/bin/sh\necho beside\n"); err != nil {
		t.Skipf("cannot write beside the test binary: %v", err)
	}
	defer os.Remove(beside)
	if out, err := CLI(context.Background(), "pr", "list"); err != nil || string(out) != "beside\n" {
		t.Errorf("galley beside the program: %q %v", out, err)
	}
	os.Remove(beside)
	script := "#!/bin/sh\n[ \"$GALLEY_NO_AUTOSTART\" = 1 ] || { echo 'autostart allowed' >&2; exit 2; }\necho 'the galley server is not running at http://localhost:24823, and it could not be started. Run `galley serve` to see why.' >&2\nexit 1\n"
	if err := writeExec(dir+"/galley", script); err != nil {
		t.Fatal(err)
	}
	if _, err := CLI(context.Background(), "pr", "list"); !errors.Is(err, ErrNotRunning) {
		t.Errorf("server down: %v", err)
	}
	if err := writeExec(dir+"/galley", "#!/bin/sh\necho '[]'\n"); err != nil {
		t.Fatal(err)
	}
	if out, err := CLI(context.Background(), "pr", "list"); err != nil || string(out) != "[]\n" {
		t.Errorf("a listing: %q %v", out, err)
	}
	// The CLI runs on the login shell's PATH first, this process's
	// after it: a node the daemon's own PATH lacks is found.
	if err := writeExec(dir+"/sh-login", "#!/bin/sh\ncase \"$1\" in -lc) printf %s /login/bin:/usr/bin;; esac\n"); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SHELL", dir+"/sh-login")
	loginPath = sync.OnceValue(readLoginPath)
	defer func() { loginPath = sync.OnceValue(readLoginPath) }()
	if err := writeExec(dir+"/galley", "#!/bin/sh\nprintf %s \"$PATH\"\n"); err != nil {
		t.Fatal(err)
	}
	if out, err := CLI(context.Background(), "pr", "list"); err != nil || !strings.HasPrefix(string(out), "/login/bin:/usr/bin:"+dir) {
		t.Errorf("the CLI's PATH: %q %v", out, err)
	}
}

func writeExec(path, body string) error {
	return os.WriteFile(path, []byte(body), 0o755)
}

// Status: nothing without galley on PATH; with it, the server's answer
// at /api/meta tells ok and the version, or ok alone without one,
// starting on galley's body with another status, another program,
// nothing running, or nothing answering in time, before the headers
// or after them, which is not "not running".
func TestStatus(t *testing.T) {
	defer func(d time.Duration) { statusTimeout = d }(statusTimeout)
	statusTimeout = 300 * time.Millisecond
	dir := t.TempDir()
	t.Setenv("PATH", dir)
	if s := Status(context.Background()); s != "" {
		t.Errorf("not installed: %q", s)
	}
	if err := writeExec(dir+"/galley", "#!/bin/sh\nexit 0\n"); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/meta" {
			http.NotFound(w, r)
			return
		}
		fmt.Fprint(w, `{"name":"galley","version":"0.1.0"}`)
	}))
	defer srv.Close()
	t.Setenv("GALLEY_URL", srv.URL)
	if s := Status(context.Background()); s != "ok 0.1.0" {
		t.Errorf("running: %q", s)
	}
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "<html>") }))
	defer other.Close()
	t.Setenv("GALLEY_URL", other.URL)
	if s := Status(context.Background()); !strings.HasPrefix(s, "another program answers at "+other.URL) {
		t.Errorf("another program: %q", s)
	}
	t.Setenv("GALLEY_URL", "http://127.0.0.1:1")
	if s := Status(context.Background()); !strings.HasPrefix(s, "not running at http://127.0.0.1:1") {
		t.Errorf("down: %q", s)
	}
	starting := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		fmt.Fprint(w, `{"name":"galley","version":"0.1.0"}`)
	}))
	defer starting.Close()
	t.Setenv("GALLEY_URL", starting.URL)
	if s := Status(context.Background()); s != "starting at "+starting.URL {
		t.Errorf("starting: %q", s)
	}
	unversioned := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, `{"name":"galley"}`) }))
	defer unversioned.Close()
	t.Setenv("GALLEY_URL", unversioned.URL)
	if s := Status(context.Background()); s != "ok" {
		t.Errorf("no version: %q", s)
	}
	// A service that is not HTTP, an ssh banner say: another program
	// on the port, not a server to start another beside.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			fmt.Fprint(c, "SSH-2.0-OpenSSH_9.9\r\n")
			c.Close()
		}
	}()
	t.Setenv("GALLEY_URL", "http://"+ln.Addr().String())
	if s := Status(context.Background()); !strings.HasPrefix(s, "another program answers at http://"+ln.Addr().String()) {
		t.Errorf("an ssh banner: %q", s)
	}
	// Stalled before the headers, and after them with galley's own
	// body cut short: neither is a server to start another beside.
	stall := func(headers bool) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if headers {
				fmt.Fprint(w, `{"name":"gal`)
				w.(http.Flusher).Flush()
			}
			select {
			case <-r.Context().Done():
			case <-time.After(5 * time.Second):
			}
		}))
	}
	for _, headers := range []bool{false, true} {
		srv := stall(headers)
		t.Setenv("GALLEY_URL", srv.URL)
		if s := Status(context.Background()); !strings.HasPrefix(s, "nothing answered at "+srv.URL+" within 300ms") {
			t.Errorf("stalled (headers %v): %q", headers, s)
		}
		srv.Close()
	}
}
