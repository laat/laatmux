package tmux

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/laat/laatmux/internal/protocol"
)

func TestShellJoin(t *testing.T) {
	got := shellJoin([]string{"claude", "--flag", "a b", "it's"})
	want := `claude --flag 'a b' 'it'\''s'`
	if got := shellJoin([]string{"tmux", "attach-session", "-t", "=lcl"}); got != `tmux attach-session -t '=lcl'` {
		t.Fatalf("leading = not quoted: %s", got)
	}
	if got != want {
		t.Fatalf("got %s want %s", got, want)
	}
}

func TestParseLabelRoundTrip(t *testing.T) {
	for _, v := range []string{"default", "laatmux", "work", "/tmp/tmux-1/sock"} {
		if got := Parse(v).Label(); got != v {
			t.Fatalf("Parse(%q).Label() = %q", v, got)
		}
	}
	if Parse("") != DefaultServer || Parse("default") != DefaultServer {
		t.Fatal("empty and default are not the default server")
	}
	if got := DefaultServer.args("list-panes"); len(got) != 3 || got[0] != "-L" || got[1] != "default" {
		t.Fatalf("default server does not select -L default: %v", got)
	}
	if (Server{}).Label() != "current" {
		t.Fatal("zero server label")
	}
	if !Parse("laatmux").Managed() {
		t.Fatal("laatmux not managed")
	}
	for _, v := range []string{"default", "work", "/tmp/tmux-1/sock"} {
		if Parse(v).Managed() {
			t.Fatalf("%q reported managed", v)
		}
	}
}

// The zero Server follows the inherited TMUX variable, so a daemon
// started from inside a non-default server would poll that server under the
// label "default". DefaultServer must ignore TMUX.
func TestDefaultServerIgnoresInheritedTMUX(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}
	const sock = "/tmp/laatmux-test-nondefault.sock"
	t.Setenv("TMUX", sock+",12345,0")
	ctx := context.Background()
	if _, err := (Server{}).Run(ctx, "list-panes", "-a"); err == nil || !strings.Contains(err.Error(), sock) {
		t.Fatalf("zero server did not follow TMUX: %v", err)
	}
	if _, err := DefaultServer.Run(ctx, "list-panes", "-a"); err != nil && strings.Contains(err.Error(), sock) {
		t.Fatalf("DefaultServer followed TMUX: %v", err)
	}
}

func TestNoServer(t *testing.T) {
	cases := map[string]bool{
		"no server running on /tmp/tmux-501/default":                            true,
		"error connecting to /tmp/tmux-501/default (No such file or directory)": true,
		"error connecting to /tmp/tmux-501/default (Connection refused)":        true,
		"error connecting to /tmp/tmux-501/default (Permission denied)":         false,
		"can't find session: =x":                                                false,
	}
	for msg, want := range cases {
		if got := NoServer(&Error{Msg: msg}); got != want {
			t.Errorf("NoServer(%q) = %v, want %v", msg, got, want)
		}
		if got := NoServer(fmt.Errorf("list: %w", &Error{Msg: msg})); got != want {
			t.Errorf("NoServer(wrapped %q) = %v, want %v", msg, got, want)
		}
	}
	if NoServer(context.Canceled) {
		t.Error("non-tmux error reported as no server")
	}
}

func TestEncodeBranch(t *testing.T) {
	cases := map[string]string{
		"main":       "main",
		"fix/v1.2":   "fix/v1%2e2",
		"a:b":        "a%3ab",
		"100%":       "100%25",
		"a.b":        "a%2eb",
		"a-b":        "a-b",
		"%2e":        "%252e",
		"feat/x.y:z": "feat/x%2ey%3az",
		"fix#12":     "fix%2312",
		"x#{pid}":    "x%23{pid}",
		"semi;":      "semi%3b",
		"a;b":        "a%3bb",
		"%23":        "%2523",
		"%3b":        "%253b",
	}
	for in, want := range cases {
		got := EncodeBranch(in)
		if got != want {
			t.Errorf("EncodeBranch(%q) = %q, want %q", in, got, want)
		}
		if back := decodeBranch(got); back != in {
			t.Errorf("decodeBranch(%q) = %q, want %q", got, back, in)
		}
		if strings.ContainsAny(got, ".:#;") {
			t.Errorf("EncodeBranch(%q) = %q contains a character tmux would not keep as given", in, got)
		}
	}
	if got := SessionName("proj", "fix/v1.2"); got != "proj/fix/v1%2e2" {
		t.Errorf("SessionName = %q", got)
	}
}

// Redact replaces the secret, bare and shell-quoted, in an error's text;
// Submitted tells a launch that may have taken from one that did not.
func TestRedactAndSubmitted(t *testing.T) {
	err := &Error{Args: []string{"new-session", "-d", shellJoin([]string{"claude", "the secret"})}, Msg: "the secret is bad"}
	got := Redact(err, "the secret", "{prompt}").Error()
	if strings.Contains(got, "secret") || strings.Count(got, "{prompt}") != 2 {
		t.Fatalf("redacted %q", got)
	}
	if Redact(nil, "x", "y") != nil || Redact(err, "", "y") != err {
		t.Fatal("nil or empty secret")
	}
	if !Submitted(&SubmittedError{Err: err}) || Submitted(err) {
		t.Fatal("submitted")
	}
	var pe *PasteError
	if e := (&PasteError{Step: "enter", Err: err}); !errors.As(fmt.Errorf("w: %w", e), &pe) || pe.Step != "enter" || !strings.HasPrefix(e.Error(), "paste (enter)") {
		t.Fatalf("paste error %v", e)
	}
}

// An empty server, kept by exit-empty off, lists no panes rather than
// failing; a server that is not running is NoServer.
func TestListPanesEmptyServer(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}
	ctx := context.Background()
	s := Server{Name: fmt.Sprintf("laatmux-test-%d", os.Getpid())}
	if _, err := s.ListPanes(ctx); !NoServer(err) {
		t.Fatalf("not running: %v", err)
	}
	if _, err := s.Run(ctx, "-f", "/dev/null", "start-server", ";", "set-option", "-s", "exit-empty", "off"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Run(ctx, "kill-server") })
	panes, err := s.ListPanes(ctx)
	if err != nil || len(panes) != 0 {
		t.Fatalf("empty server: %v %v", panes, err)
	}
}

// startManaged starts the managed server without the user's config and
// keeps it up with no session; it is killed at the end. A server the
// previous test killed may still be going: a start that reaches it is
// tried again.
func startManaged(t *testing.T) Server {
	t.Helper()
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}
	s := LaatmuxServer
	var err error
	for i := 0; i < 50; i++ {
		if _, err = s.Run(context.Background(), "-f", "/dev/null", "start-server", ";", "set-option", "-s", "exit-empty", "off"); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Run(context.Background(), "kill-server") })
	return s
}

// A hand-started server's sessions lose their overrides of the
// isolation options, each named exactly: the first one is called 0,
// which as a bare target is pane 0 of the most recent session, b here.
func TestEnsureConfiguredClearsEverySession(t *testing.T) {
	s := startManaged(t)
	ctx := context.Background()
	for _, args := range [][]string{
		{"new-session", "-d", "-s", "0", "sleep 600"},
		{"set-option", "-t", "=0:", "status", "on"},
		{"new-session", "-d", "-s", "b", "sleep 600"},
		{"set-option", "-t", "=b:", "status", "on"},
	} {
		if _, err := s.Run(ctx, args...); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.EnsureConfigured(ctx); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"0", "b"} {
		if out, err := s.Run(ctx, "show-options", "-t", "="+name+":", "status"); err != nil || strings.TrimSpace(string(out)) != "" {
			t.Errorf("session %s keeps %q %v", name, out, err)
		}
	}
}

// NewSession counts the panes of the session it made, named exactly. A
// caller whose TMUX_PANE names a pane on the managed server, as a pane
// id of the user's own server can, has that pane's session as current,
// and =proj/z alone is a window of it before it is a session: other's
// two panes were counted and the new session killed.
func TestNewSessionCountsItsOwnPanes(t *testing.T) {
	s := startManaged(t)
	ctx := context.Background()
	out, err := s.Run(ctx, "new-session", "-d", "-s", "other", "-n", "proj/z", "-P", "-F", "#{pane_id}", "sleep 600")
	if err != nil {
		t.Fatal(err)
	}
	pane := strings.TrimSpace(string(out))
	if _, err := s.Run(ctx, "split-window", "-d", "-t", pane, "sleep 600"); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMUX_PANE", pane)
	if _, err := s.NewSession(ctx, NewSessionOpts{Name: "proj/z", Cwd: t.TempDir(), Cmd: []string{"sleep", "600"}}); err != nil {
		t.Fatal(err)
	}
	if !s.HasSession(ctx, "proj/z") {
		t.Fatal("the new session is gone")
	}
}

// A branch with a # or a ; gets a session with the name SessionName
// computed, its pane tagged: new-session expands a # in the name as a
// format, and an argument that ends in ; splits the sequence there.
func TestNewSessionEncodedNames(t *testing.T) {
	s := startManaged(t)
	ctx := context.Background()
	want := map[string]bool{}
	for _, branch := range []string{"fix#12", "x#{session_id}", "y##", "semi;", "a;b"} {
		name := SessionName("proj", branch)
		if _, err := s.NewSession(ctx, NewSessionOpts{Name: name, Cwd: t.TempDir(), Cmd: []string{"sleep", "600"}}); err != nil {
			t.Fatalf("%s: %v", branch, err)
		}
		if out, err := s.Run(ctx, "show-options", "-pqv", "-t", "="+name+":", "@laatmux_managed"); err != nil || strings.TrimSpace(string(out)) != "1" {
			t.Errorf("%s: session %s pane tag %q %v", branch, name, out, err)
		}
		want[name] = true
	}
	out, err := s.Run(ctx, "list-sessions", "-F", "#{session_name}")
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Fields(string(out))
	for _, name := range got {
		if !want[name] {
			t.Errorf("session %q, not a computed name", name)
		}
	}
	if len(got) != len(want) {
		t.Errorf("sessions %q, want %d", got, len(want))
	}
}

// A session starts in its root when the root has a # in it, as a
// branch's root does in the default layout, and its pane's tag is the
// root as given: new-session expands -c as a format, and a directory
// that is not there starts the pane in $HOME with no error. The
// worktrees directory, which is the user's, has a #[ in it, which tmux
// keeps as it is.
func TestNewSessionRootWithHash(t *testing.T) {
	s := startManaged(t)
	ctx := context.Background()
	for _, branch := range []string{"fix#12", "x#{session_id}", "y##"} {
		root := filepath.Join(t.TempDir(), "#[scratch]", "proj", branch)
		if err := os.MkdirAll(root, 0o755); err != nil {
			t.Fatal(err)
		}
		want, err := filepath.EvalSymlinks(root)
		if err != nil {
			t.Fatal(err)
		}
		name := SessionName("proj", branch)
		if _, err := s.NewSession(ctx, NewSessionOpts{Name: name, Cwd: root, Cmd: []string{"sleep", "600"}}); err != nil {
			t.Fatalf("%s: %v", branch, err)
		}
		// The pane's path is read from its process, which may not have
		// changed directory yet.
		var got string
		for i := 0; i < 200 && got != want; i++ {
			out, _ := s.Run(ctx, "display-message", "-p", "-t", "="+name+":", "#{pane_current_path}")
			got = strings.TrimSpace(string(out))
			time.Sleep(10 * time.Millisecond)
		}
		if got != want {
			t.Errorf("%s: the pane is in %q, want %q", branch, got, want)
		}
		if out, err := s.Run(ctx, "show-options", "-pqv", "-t", "="+name+":", "@laatmux_cwd"); err != nil || strings.TrimSpace(string(out)) != root {
			t.Errorf("%s: @laatmux_cwd %q %v, want %q", branch, out, err, root)
		}
	}
}

// formatLiteral doubles every # but a run of them before a [, which
// tmux keeps as it is; the new-session test above checks with tmux.
func TestFormatLiteral(t *testing.T) {
	cases := map[string]string{
		"":                 "",
		"/w/proj/main":     "/w/proj/main",
		"fix#12":           "fix##12",
		"x#{session_id}":   "x##{session_id}",
		"y##":              "y####",
		"#":                "##",
		"a#[b":             "a#[b",
		"a##[b":            "a##[b",
		"a#[b]#{c}":        "a#[b]##{c}",
		"/w/#[s]/proj/x#H": "/w/#[s]/proj/x##H",
	}
	for in, want := range cases {
		if got := formatLiteral(in); got != want {
			t.Errorf("formatLiteral(%q) = %q, want %q", in, got, want)
		}
	}
}

// A laatmux process without a UTF-8 locale, a daemon started without
// LANG or a command run over ssh, reads a session name and tag with
// non-ASCII bytes in them back as tmux keeps them. tmux prints each
// non-ASCII character to a client that is not UTF-8 as _, and Sep,
// whose characters take no column, as nothing: without -u ListPanes
// found no field in any line and listed no pane at all, and a name or
// root read by any other format came back with _ in it.
func TestRunReadsUTF8WithoutLocale(t *testing.T) {
	t.Setenv("LC_ALL", "C")
	for _, k := range []string{"LANG", "LC_CTYPE"} {
		t.Setenv(k, "")
		os.Unsetenv(k)
	}
	s := startManaged(t)
	ctx := context.Background()
	cwd := filepath.Join(t.TempDir(), "fiks-æøå")
	if err := os.Mkdir(cwd, 0o755); err != nil {
		t.Fatal(err)
	}
	name := SessionName("proj", "fiks-æøå")
	if _, err := s.NewSession(ctx, NewSessionOpts{Name: name, Cwd: cwd, Cmd: []string{"sleep", "600"}}); err != nil {
		t.Fatal(err)
	}
	// The managed server by name, and the same server by its socket as
	// a watched one is addressed.
	sock, err := s.Run(ctx, "display-message", "-p", "#{socket_path}")
	if err != nil {
		t.Fatal(err)
	}
	for _, srv := range []Server{s, {Path: strings.TrimSpace(string(sock))}} {
		panes, err := srv.ListPanes(ctx)
		if err != nil || len(panes) != 1 || panes[0].Session != name || panes[0].Cwd != cwd {
			t.Fatalf("%s listed %+v %v, want session %q with cwd %q", srv.Label(), panes, err, name, cwd)
		}
	}
	if out, err := s.Run(ctx, "show-options", "-p", "-t", "="+name+":", "-v", "@laatmux_cwd"); err != nil || strings.TrimSpace(string(out)) != cwd {
		t.Fatalf("show-options: %q %v, want %q", out, err, cwd)
	}
	if _, err := s.Run(ctx, "new-session", "-d", "-s", name); err == nil || !strings.Contains(err.Error(), "duplicate session: "+name) {
		t.Fatalf("a second session of the name: %v", err)
	}
	// The same listing without -u is what the test guards against: in
	// this environment tmux does not take the client for UTF-8. The
	// session name alone, since the temporary directory may have
	// non-ASCII characters of its own.
	out, err := exec.Command("tmux", s.args("list-panes", "-a", "-F", "#{session_name}")...).Output()
	if got := strings.TrimSpace(string(out)); err != nil || got != "proj/fiks-___" {
		t.Fatalf("without -u: %q %v, want %q", got, err, "proj/fiks-___")
	}
}

// decodeBranch reverses EncodeBranch, for the round trip. Sequences
// EncodeBranch never emits are left as they are.
func decodeBranch(name string) string {
	var b strings.Builder
	for i := 0; i < len(name); i++ {
		if name[i] == '%' && i+2 < len(name) {
			switch name[i+1 : i+3] {
			case "25":
				b.WriteByte('%')
				i += 2
				continue
			case "23":
				b.WriteByte('#')
				i += 2
				continue
			case "2e":
				b.WriteByte('.')
				i += 2
				continue
			case "3a":
				b.WriteByte(':')
				i += 2
				continue
			case "3b":
				b.WriteByte(';')
				i += 2
				continue
			}
		}
		b.WriteByte(name[i])
	}
	return b.String()
}

// The two named servers are labelled as the protocol's records say.
func TestServerLabelsAreTheProtocolConstants(t *testing.T) {
	if LaatmuxServer.Label() != protocol.ServerLaatmux || DefaultServer.Label() != protocol.ServerDefault {
		t.Fatalf("labels %q %q, protocol constants %q %q", LaatmuxServer.Label(), DefaultServer.Label(), protocol.ServerLaatmux, protocol.ServerDefault)
	}
	if Parse(protocol.ServerLaatmux) != LaatmuxServer || Parse(protocol.ServerDefault) != DefaultServer {
		t.Fatal("the labels do not parse back to the servers")
	}
}

func TestServersDefaultAndParsed(t *testing.T) {
	got, err := ParseServers(nil)
	if err != nil || len(got) != 1 || got[0] != LaatmuxServer {
		t.Fatalf("default = %v, %v", got, err)
	}
	got, err = ParseServers([]string{"laatmux", "default"})
	if err != nil || len(got) != 2 || got[0] != LaatmuxServer || got[1] != DefaultServer {
		t.Fatalf("parsed = %v, %v", got, err)
	}
	if _, err := ParseServers([]string{"default", ""}); err == nil {
		t.Fatal("duplicate default accepted")
	}
}
