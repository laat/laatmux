package tmux

import (
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode"
	"unicode/utf8"

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
		`a\b`:        "a%5cb",
		`\\`:         "%5c%5c",
		"%5c":        "%255c",

		// Bytes tmux escapes by vis(3), and UTF-8 it keeps as given.
		"tab\tx":       "tab%09x",
		"nl\n":         "nl%0a",
		"a\x01b":       "a%01b",
		"del\x7f":      "del%7f",
		"a\xffb":       "a%ffb",
		"%09":          "%2509",
		"blåbær/ø":     "blåbær/ø",
		"feat/😀":       "feat/😀",
		"c1\u0085x":    "c1\u0085x",
		"nc\ufffex":    "nc\ufffex",
		"日本\x80":       "日本%80",
		"\xe2\x82":     "%e2%82",
		"\xed\xa0\x80": "%ed%a0%80",
		"\xef\xbf\xbd": "\xef\xbf\xbd",
	}
	for in, want := range cases {
		got := EncodeBranch(in)
		if got != want {
			t.Errorf("EncodeBranch(%q) = %q, want %q", in, got, want)
		}
		if back := decodeBranch(got); back != in {
			t.Errorf("decodeBranch(%q) = %q, want %q", got, back, in)
		}
		if !keptByTmux(got) {
			t.Errorf("EncodeBranch(%q) = %q contains a character tmux would not keep as given", in, got)
		}
	}
	// Every byte alone, which covers each control byte, DEL and each
	// byte that cannot stand alone in UTF-8.
	for c := 0; c < 256; c++ {
		in := string([]byte{byte(c)})
		got := EncodeBranch(in)
		if back := decodeBranch(got); back != in || !keptByTmux(got) {
			t.Errorf("EncodeBranch(%q) = %q, decoded %q", in, got, back)
		}
	}
	if got := SessionName("proj", "fix/v1.2"); got != "proj/fix/v1%2e2" {
		t.Errorf("SessionName = %q", got)
	}
}

// keptByTmux is a name tmux stores as given: valid UTF-8 with no control
// byte, no DEL, and none of the characters it changes or reads, but for
// the "$" tmux 3.4 changes (#220).
func keptByTmux(name string) bool {
	return utf8.ValidString(name) && !strings.ContainsAny(name, ".:#;\\\x7f") &&
		!strings.ContainsFunc(name, func(r rune) bool { return r < 0x20 })
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

// A prompt with a newline makes the argument it is in, and tmux's
// message that repeats it, quoted, and the prompt is in them escaped:
// Redact replaces it there too. So it does for a prompt with a " in an
// argument another word's tab makes quoted, and for a prompt with a
// ', which is in the argument as shellJoin quotes it, escaped once
// more where the argument is quoted. The replacement is one pass: a
// prompt such as p, which the placeholder has in it, is not replaced
// again in the placeholder; and the shell-quoted form wins where the
// bare one starts at the same place, as ” does in its own quoting.
func TestRedactQuoted(t *testing.T) {
	for _, c := range []struct {
		secret string
		argv   []string
	}{
		{"fix it\n\"now\"\tplease", []string{"claude", "fix it\n\"now\"\tplease"}},
		{`fix it "now" please`, []string{"claude", "--dir", "/w/a\tb", `fix it "now" please`}},
		{"don't fix it\nplease", []string{"claude", "don't fix it\nplease"}},
		{"don't fix it, please", []string{"claude", "--dir", "/w/a\tb", "don't fix it, please"}},
	} {
		err := &Error{Args: []string{"new-session", "-d", "-s", "proj/x", shellJoin(c.argv), Next, "set-option", "-p", "@laatmux_cwd", "/w/a\tb"}, Msg: "failed: " + c.secret}
		got := Redact(&SubmittedError{Err: err}, c.secret, "{prompt}").Error()
		if strings.Contains(got, "fix it") || strings.Contains(got, "please") || strings.Count(got, "{prompt}") != 2 {
			t.Errorf("%q: redacted %q", c.secret, got)
		}
		if strings.ContainsFunc(got, unicode.IsControl) {
			t.Errorf("%q: a control byte in %q", c.secret, got)
		}
	}
	for secret, want := range map[string]string{
		"p":      "tmux new-session -d claude {prompt}: failed: {prompt}",
		"{p":     "tmux new-session -d claude {prompt}: failed: {prompt}",
		"rom":    "tmux new-session -d claude {prompt}: failed: {prompt}",
		"prompt": "tmux new-session -d claude {prompt}: failed: {prompt}",
		"''":     "tmux new-session -d claude {prompt}: failed: {prompt}",
		"p\nq":   `tmux new-session -d "claude {prompt}": "failed: {prompt}"`,
	} {
		err := &Error{Args: []string{"new-session", "-d", shellJoin([]string{"claude", secret})}, Msg: "failed: " + secret}
		if got := Redact(err, secret, "{prompt}").Error(); got != want {
			t.Errorf("%q: redacted %q, want %q", secret, got, want)
		}
	}
}

// An argument or tmux's message with a control character, C0, DEL or
// C1, or a byte that is not UTF-8 is printed quoted: raw, a tab or a
// newline breaks the line and an ESC starts an escape sequence, here
// one that sets the terminal's title. A plain one, with a non-ASCII
// letter, a " or a \ in it, is printed as it is, and Next as the ; it
// stands for. A cwd that is not there is named quoted the same way, and
// is still not there to errors.Is.
func TestErrorPrintable(t *testing.T) {
	for in, want := range map[string]string{
		"/w/proj/plain":     "/w/proj/plain",
		"/w/blåbær":         "/w/blåbær",
		`/w/a "b" \c`:       `/w/a "b" \c`,
		"":                  "",
		"a\tb":              `"a\tb"`,
		"a\nb":              `"a\nb"`,
		"/w/a\x1b]0;x\x07b": `"/w/a\x1b]0;x\ab"`,
		"\x01":              `"\x01"`,
		"\x1f":              `"\x1f"`,
		"del\x7f":           `"del\x7f"`,
		"a\xffb":            `"a\xffb"`,
		"truncated\xe2\x82": `"truncated\xe2\x82"`,
		"c1 \u009b31m":      `"c1 \u009b31m"`,
		"blåbær\t\"q\"":     `"blåbær\t\"q\""`,
		"sep" + Sep:         "sep" + Sep,
	} {
		if got := Printable(in); got != want {
			t.Errorf("Printable(%q) = %s, want %s", in, got, want)
		}
	}
	err := &Error{Args: []string{"set-option", "-t", "=mac/proj/x:", "@laatmux_workspace", "env//w/a\x1b]0;x\x07b", Next, "set-option", "@laatmux_branch", "plain", Next, "set-option", "@c", ";"}, Msg: "no such session: =a\tb:"}
	want := `tmux set-option -t =mac/proj/x: @laatmux_workspace "env//w/a\x1b]0;x\ab" ; set-option @laatmux_branch plain ; set-option @c ;: "no such session: =a\tb:"`
	if got := err.Error(); got != want {
		t.Errorf("Error = %s, want %s", got, want)
	}
	gone := "/nonexistent/a\x1b]0;x\x07b"
	_, cerr := LaatmuxServer.NewSession(context.Background(), NewSessionOpts{Name: "proj/x", Cwd: gone})
	if want := "tmux: cwd: stat " + strconv.Quote(gone) + ": no such file or directory"; cerr == nil || cerr.Error() != want || !errors.Is(cerr, fs.ErrNotExist) {
		t.Errorf("NewSession in a cwd not there: %v, want %s", cerr, want)
	}
}

// A failure on a real server for a target with an ESC and a tab: tmux
// repeats the target in its message, and the error prints both quoted,
// with no control byte in it, while the fields keep them as they are.
func TestErrorPrintableOnServer(t *testing.T) {
	s := startManaged(t)
	target := "=no\x1b]0;x\x07such\tsession:"
	_, err := s.Run(context.Background(), "set-option", "-t", target, "@laatmux_k", "v")
	var te *Error
	if !errors.As(err, &te) || te.Args[2] != target {
		t.Fatalf("set-option on no session: %#v", err)
	}
	if got := err.Error(); !strings.Contains(got, strconv.Quote(target)) || strings.ContainsFunc(got, unicode.IsControl) {
		t.Fatalf("Error = %q", got)
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
	if _, err := s.Run(ctx, "-f", "/dev/null", "start-server", Next, "set-option", "-s", "exit-empty", "off"); err != nil {
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
		if _, err = s.Run(context.Background(), "-f", "/dev/null", "start-server", Next, "set-option", "-s", "exit-empty", "off"); err == nil {
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

// A branch with a #, a ; or a \ gets a session with the name
// SessionName computed, its pane tagged: new-session expands a # in the
// name as a format, an argument that ends in ; splits the sequence
// there, and tmux stores a \ in a session name doubled. git takes no \
// in a branch, but a detached worktree's directory name, encoded the
// same way, may have one.
func TestNewSessionEncodedNames(t *testing.T) {
	s := startManaged(t)
	ctx := context.Background()
	want := map[string]bool{}
	for _, branch := range []string{"fix#12", "x#{session_id}", "y##", "semi;", "a;b", `back\slash`, `end\`} {
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

// The panes of a managed server's sessions have a UTF-8 locale whatever
// the locale of the process that started the server. A daemon with
// LC_ALL=C and no LANG, as one run over ssh without a locale forwarded
// or as a service may be, started the server with it in the global
// environment, and every pane ran in the C locale: LANG and LC_* are
// not variables a client updates a session with, unless a hand-started
// server's update-environment lists them, and then they came from the
// daemon. A daemon with a UTF-8 locale gives its own, in place of the
// fallback or with the LC_ALL=C of the server's start removed, less a
// category the host does not have; a daemon without one keeps the
// server's.
func TestNewSessionUTF8Locale(t *testing.T) {
	fallback := fallbackLocale(nil)
	if fallback == "" {
		t.Skip("the host has no UTF-8 locale")
	}
	setLocale := func(kv ...string) {
		for _, k := range localeNames {
			t.Setenv(k, "")
			os.Unsetenv(k)
		}
		for i := 0; i+1 < len(kv); i += 2 {
			if kv[i+1] != "" {
				t.Setenv(kv[i], kv[i+1])
			}
		}
	}
	// The pane runs its command with the server's default shell; a
	// user's zsh would read a ~/.zshenv that may set a locale.
	t.Setenv("SHELL", "/bin/sh")
	ctx := context.Background()
	dir := t.TempDir()
	// paneLocale makes a session that writes its environment to a file
	// and returns the locale variables in it.
	paneLocale := func(s Server, name string) map[string]string {
		t.Helper()
		file := filepath.Join(dir, name)
		if _, err := s.NewSession(ctx, NewSessionOpts{Name: name, Cwd: dir, Cmd: []string{"sh", "-c", `env > "$1.tmp" && mv "$1.tmp" "$1"; exec sleep 600`, "sh", file}}); err != nil {
			t.Fatal(err)
		}
		var out []byte
		for i := 0; i < 500; i++ {
			if b, err := os.ReadFile(file); err == nil {
				out = b
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		if out == nil {
			t.Fatalf("%s wrote no environment", name)
		}
		got := map[string]string{}
		for _, line := range strings.Split(string(out), "\n") {
			if k, v, ok := strings.Cut(line, "="); ok && slices.Contains(localeNames, k) {
				got[k] = v
			}
		}
		return got
	}
	// No locale the host lacks: libc would drop the whole locale for
	// one of them. A host that takes any name, musl, has no such
	// locale, and the panes are checked without one.
	missing := "xx_XX.UTF-8"
	if utf8Locale(nil, "LC_TIME", missing) {
		missing = ""
	}
	// startC starts the server under LC_ALL=C, LC_CTYPE=C and the
	// missing LC_TIME, and makes sure the server that runs is the one
	// started so: one a kill has not ended yet would leave NewSession to
	// start one with the environment of the session it makes.
	startC := func() Server {
		t.Helper()
		setLocale("LC_ALL", "C", "LC_CTYPE", "C", "LC_TIME", missing)
		s := startManaged(t)
		if out, err := s.Run(ctx, "show-environment", "-g", "LC_ALL"); err != nil || strings.TrimSpace(string(out)) != "LC_ALL=C" {
			t.Fatalf("the server started with LC_ALL=C has %q %v", out, err)
		}
		return s
	}
	s := startC()
	if _, err := s.Run(ctx, "set-option", "-g", "update-environment", "LANG LC_ALL"); err != nil {
		t.Fatal(err)
	}
	if got, want := paneLocale(s, "a"), map[string]string{"LANG": fallback}; !maps.Equal(got, want) {
		t.Errorf("a daemon with LC_ALL=C: the pane's locale is %v, want %v", got, want)
	}
	// The user's locale: en_US.UTF-8, or another UTF-8 locale the host
	// has when that one is missing or the fallback.
	same := func(a, b string) bool {
		norm := strings.NewReplacer("-", "", "_", "")
		return strings.EqualFold(norm.Replace(a), norm.Replace(b))
	}
	user := "en_US.UTF-8"
	if !utf8Locale(nil, "LANG", user) || same(user, fallback) {
		user = ""
		for _, name := range hostLocales(nil) {
			if utf8Name(name) && utf8Locale(nil, "LANG", name) && !same(name, fallback) {
				user = name
				break
			}
		}
	}
	if user == "" {
		t.Logf("the host has no UTF-8 locale but %s; the user's locale is not checked", fallback)
		return
	}
	// A daemon restarted with a locale replaces the fallback, less the
	// category the host lacks.
	setLocale("LANG", user, "LC_TIME", missing)
	if got, want := paneLocale(s, "b"), map[string]string{"LANG": user}; !maps.Equal(got, want) {
		t.Errorf("a daemon with LANG=%s LC_TIME=%s on a server with LANG=%s: the pane's locale is %v, want %v", user, missing, fallback, got, want)
	}
	if _, err := s.Run(ctx, "kill-server"); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 200; i++ {
		if _, err := s.Run(ctx, "list-sessions"); NoServer(err) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	s = startC()
	setLocale("LANG", user)
	if got, want := paneLocale(s, "c"), map[string]string{"LANG": user}; !maps.Equal(got, want) {
		t.Errorf("a daemon with LANG=%s on a server started with LC_ALL=C: the pane's locale is %v, want %v", user, got, want)
	}
	setLocale("LC_ALL", "C")
	if got, want := paneLocale(s, "d"), map[string]string{"LANG": user}; !maps.Equal(got, want) {
		t.Errorf("a daemon with LC_ALL=C on a server with LANG=%s: the pane's locale is %v, want %v", user, got, want)
	}
}

// wantLocale drops the variables the host does not take, then takes the
// daemon's locale when its character set is UTF-8 on the host, keeps the
// server's when only that one is, and otherwise sets LANG alone to the
// fallback. Which names are UTF-8 is the host's to say.
func TestWantLocale(t *testing.T) {
	env := func(kv ...string) func(string) string {
		m := map[string]string{}
		for i := 0; i+1 < len(kv); i += 2 {
			m[kv[i]] = kv[i+1]
		}
		return func(k string) string { return m[k] }
	}
	// A glibc host takes either spelling of the codeset and no bare
	// UTF-8. A macOS host takes the codeset as macOS spells it, and a
	// bare UTF-8 for LC_CTYPE alone. musl takes any name.
	glibc := func(_, v string) bool {
		return slices.Contains([]string{"C.UTF-8", "C.utf8", "en_US.UTF-8", "en_US.utf8", "nb_NO.UTF-8", "nb_NO.utf8", "de_DE.UTF-8@euro"}, v)
	}
	macos := func(k, v string) bool {
		return slices.Contains([]string{"C.UTF-8", "en_US.UTF-8", "nb_NO.UTF-8"}, v) || k == "LC_CTYPE" && v == "UTF-8"
	}
	musl := func(_, v string) bool { return v != "C" && v != "POSIX" }
	every := map[string]string{
		"LANG": "nb_NO.UTF-8", "LC_CTYPE": "nb_NO.utf8", "LC_COLLATE": "C", "LC_MESSAGES": "POSIX",
		"LC_MONETARY": "en_US.UTF-8", "LC_NUMERIC": "en_US.utf8", "LC_TIME": "en_US.UTF-8",
		"LC_ADDRESS": "nb_NO.UTF-8", "LC_IDENTIFICATION": "C", "LC_MEASUREMENT": "nb_NO.UTF-8",
		"LC_NAME": "nb_NO.UTF-8", "LC_PAPER": "nb_NO.UTF-8", "LC_TELEPHONE": "nb_NO.UTF-8",
	}
	everyEnv := func(k string) string {
		if k == "LC_TERMINAL" {
			return "iTerm2"
		}
		return every[k]
	}
	cases := []struct {
		name     string
		daemon   func(string) string
		global   func(string) string
		utf8     func(k, v string) bool
		fallback string
		want     map[string]string
	}{
		{"the daemon's UTF-8 locale, every category", everyEnv, env("LC_ALL", "C"), glibc, "C.UTF-8", every},
		{"by LC_ALL", env("LC_ALL", "en_US.UTF-8", "LANG", "nb_NO.UTF-8"), env(), glibc, "C.UTF-8", map[string]string{"LC_ALL": "en_US.UTF-8", "LANG": "nb_NO.UTF-8"}},
		{"by LC_CTYPE over a LANG of C", env("LANG", "C", "LC_CTYPE", "nb_NO.utf8"), env(), glibc, "C.UTF-8", map[string]string{"LANG": "C", "LC_CTYPE": "nb_NO.utf8"}},
		{"with a modifier", env("LANG", "de_DE.UTF-8@euro"), env(), glibc, "C.UTF-8", map[string]string{"LANG": "de_DE.UTF-8@euro"}},
		{"a category the host does not have, dropped", env("LANG", "en_US.UTF-8", "LC_TIME", "sv_SE.UTF-8"), env(), glibc, "C.UTF-8", map[string]string{"LANG": "en_US.UTF-8"}},
		{"a category of another character set, dropped", env("LANG", "en_US.UTF-8", "LC_COLLATE", "en_US.ISO8859-1"), env(), glibc, "C.UTF-8", map[string]string{"LANG": "en_US.UTF-8"}},
		{"macOS's bare UTF-8 on macOS", env("LC_CTYPE", "UTF-8", "LANG", "nb_NO.UTF-8"), env(), macos, "C.UTF-8", map[string]string{"LC_CTYPE": "UTF-8", "LANG": "nb_NO.UTF-8"}},
		{"macOS's bare UTF-8 on glibc, dropped", env("LC_CTYPE", "UTF-8", "LANG", "nb_NO.UTF-8"), env(), glibc, "C.UTF-8", map[string]string{"LANG": "nb_NO.UTF-8"}},
		{"a bare UTF-8 as LANG, which macOS has for LC_CTYPE alone", env("LANG", "UTF-8"), env(), macos, "C.UTF-8", map[string]string{"LANG": "C.UTF-8"}},
		{"a name without a codeset on musl", env("LANG", "de_DE"), env(), musl, "C.UTF-8", map[string]string{"LANG": "de_DE"}},
		{"no locale", env(), env(), glibc, "C.UTF-8", map[string]string{"LANG": "C.UTF-8"}},
		{"LC_ALL=C over a UTF-8 LANG", env("LC_ALL", "C", "LANG", "nb_NO.UTF-8"), env(), glibc, "C.UTF-8", map[string]string{"LANG": "C.UTF-8"}},
		{"LC_ALL=POSIX over a UTF-8 LC_CTYPE", env("LC_ALL", "POSIX", "LC_CTYPE", "en_US.UTF-8"), env(), glibc, "C.UTF-8", map[string]string{"LANG": "C.UTF-8"}},
		{"a UTF-8 locale the host does not have", env("LANG", "sv_SE.UTF-8"), env(), glibc, "C.UTF-8", map[string]string{"LANG": "C.UTF-8"}},
		{"glibc's spelling on macOS", env("LANG", "nb_NO.utf8"), env(), macos, "C.UTF-8", map[string]string{"LANG": "C.UTF-8"}},
		{"a non-UTF-8 locale", env("LANG", "en_US.ISO8859-1"), env(), glibc, "C.UTF-8", map[string]string{"LANG": "C.UTF-8"}},
		{"the server's UTF-8 locale kept", env("LC_ALL", "C"), env("LANG", "nb_NO.UTF-8", "LC_TIME", "C"), glibc, "C.UTF-8", map[string]string{"LANG": "nb_NO.UTF-8", "LC_TIME": "C"}},
		{"the server's UTF-8 locale kept, a category the host does not have dropped", env(), env("LANG", "nb_NO.UTF-8", "LC_TIME", "sv_SE.UTF-8"), glibc, "C.UTF-8", map[string]string{"LANG": "nb_NO.UTF-8"}},
		{"the server's UTF-8 locale the host does not have", env(), env("LANG", "sv_SE.UTF-8"), glibc, "C.UTF-8", map[string]string{"LANG": "C.UTF-8"}},
		{"the server's LC_ALL=C over its UTF-8 LANG", env(), env("LC_ALL", "C", "LANG", "nb_NO.UTF-8"), glibc, "C.UTF-8", map[string]string{"LANG": "C.UTF-8"}},
		{"nothing to fall back on", env("LC_ALL", "C"), env("LC_ALL", "C"), glibc, "", nil},
	}
	for _, c := range cases {
		got := wantLocale(c.daemon, c.global, c.utf8, func() string { return c.fallback })
		if !maps.Equal(got, c.want) || (got == nil) != (c.want == nil) {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
}

// The fallback is C.UTF-8 where the host has it, else en_US.UTF-8, else
// the first locale the host lists that is UTF-8 by its name and on the
// host; the host's list is read only when it comes to that.
func TestPickFallback(t *testing.T) {
	cases := []struct {
		name   string
		has    []string
		listed []string
		want   string
	}{
		{"C.UTF-8", []string{"en_US.UTF-8", "C.UTF-8"}, nil, "C.UTF-8"},
		{"en_US.UTF-8", []string{"en_US.UTF-8", "de_DE.utf8"}, nil, "en_US.UTF-8"},
		{"the first listed", []string{"de_DE.utf8", "fr_FR.utf8"}, []string{"C", "POSIX", "aa_DJ.utf8", "de_DE.iso88591", "de_DE.utf8", "fr_FR.utf8"}, "de_DE.utf8"},
		{"none", []string{"de_DE.iso88591"}, []string{"C", "POSIX", "de_DE.iso88591"}, ""},
	}
	for _, c := range cases {
		read := false
		got := pickFallback(func(name string) bool { return slices.Contains(c.has, name) }, func() []string { read = true; return c.listed })
		if got != c.want || read != (c.listed != nil) {
			t.Errorf("%s: %q, list read %v, want %q", c.name, got, read, c.want)
		}
	}
}

// resetCharmaps forgets every locale(1) answer, now and when the test
// ends, so a test asks with the environment it sets.
func resetCharmaps(t *testing.T) {
	reset := func() {
		charmaps.Lock()
		charmaps.m = map[string]charmapResult{}
		charmaps.Unlock()
	}
	reset()
	t.Cleanup(reset)
}

// utf8Locale asks the host's own locale(1), with the daemon's locale
// variables out of the way: macOS takes a bare UTF-8 for LC_CTYPE alone
// and the codeset only as it spells it, glibc takes either spelling and
// no bare UTF-8, and neither takes a locale it does not have.
func TestUTF8LocaleOnHost(t *testing.T) {
	if _, err := exec.LookPath("locale"); err != nil {
		t.Skip("no locale(1)")
	}
	resetCharmaps(t)
	t.Setenv("LC_ALL", "C")
	for _, name := range []string{"", "C", "POSIX"} {
		if utf8Locale(nil, "LANG", name) || utf8Locale(nil, "LC_CTYPE", name) {
			t.Errorf("%q taken for UTF-8", name)
		}
	}
	if slices.ContainsFunc(hostLocales(nil), utf8Name) && !utf8Locale(nil, "LANG", fallbackLocale(nil)) {
		t.Errorf("the host lists a UTF-8 locale and the fallback %q is not one", fallbackLocale(nil))
	}
	var want map[[2]string]bool
	switch {
	case runtime.GOOS == "darwin":
		want = map[[2]string]bool{
			{"LC_CTYPE", "UTF-8"}: true, {"LANG", "UTF-8"}: false, {"LC_ALL", "UTF-8"}: false,
			{"LANG", "en_US.UTF-8"}: true, {"LANG", "en_US.utf8"}: false, {"LANG", "C.UTF-8"}: true, {"LANG", "C.utf8"}: false,
			{"LANG", "xx_XX.UTF-8"}: false, {"LC_CTYPE", "xx_XX.UTF-8"}: false,
		}
	case exec.Command("getconf", "GNU_LIBC_VERSION").Run() == nil:
		want = map[[2]string]bool{
			{"LC_CTYPE", "UTF-8"}: false, {"LANG", "UTF-8"}: false,
			{"LANG", "C.UTF-8"}:     utf8Locale(nil, "LANG", "C.utf8"),
			{"LANG", "xx_XX.UTF-8"}: false, {"LC_CTYPE", "xx_XX.UTF-8"}: false,
		}
	}
	for kv, w := range want {
		if got := utf8Locale(nil, kv[0], kv[1]); got != w {
			t.Errorf("%s=%s taken for UTF-8: %v, want %v", kv[0], kv[1], got, w)
		}
	}
}

// Where locale(1) cannot be run, as on musl without musl-locales, the
// name's spelling decides, and the fallback is C.UTF-8, which musl
// always takes.
func TestUTF8LocaleWithoutLocale(t *testing.T) {
	resetCharmaps(t)
	t.Setenv("PATH", t.TempDir())
	for name, want := range map[string]bool{"C": false, "UTF-8": false, "de_DE": false, "xx_XX.UTF-8": true, "de_DE.utf8@euro": true} {
		if got := utf8Locale(nil, "LANG", name); got != want {
			t.Errorf("%q taken for UTF-8: %v, want %v", name, got, want)
		}
	}
	if got := fallbackLocale(nil); got != "C.UTF-8" {
		t.Errorf("fallback %q, want C.UTF-8", got)
	}
}

// fakeLocale puts a locale(1) that runs script on PATH, ahead of the
// host's.
func fakeLocale(t *testing.T, script string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "locale"), []byte("#!/bin/sh\n"+script+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// A locale(1) that fails, by a timeout or an exit status, gives no
// answer: the name does not count, as its spelling would say it does,
// and it is asked again next time, so a host slow at boot does not keep
// an LC_TIME it lacks for the daemon's life.
func TestUTF8LocaleProbeFails(t *testing.T) {
	resetCharmaps(t)
	fakeLocale(t, "exit 1")
	if utf8Locale(nil, "LANG", "en_US.UTF-8") {
		t.Error("a failed probe taken for UTF-8")
	}
	fakeLocale(t, "echo UTF-8")
	if !utf8Locale(nil, "LANG", "en_US.UTF-8") {
		t.Error("the failure kept, the name not asked again")
	}
}

// glibc's locale(1) sets LC_CTYPE before every category, and says only
// on stderr that a category failed, with UTF-8 printed and a zero exit:
// such a locale does not count.
func TestUTF8LocaleProbeWarns(t *testing.T) {
	resetCharmaps(t)
	fakeLocale(t, `echo "locale: Cannot set LC_ALL to default locale: No such file or directory" >&2; echo UTF-8`)
	if utf8Locale(nil, "LANG", "en_US.UTF-8") {
		t.Error("a locale with a category missing taken for UTF-8")
	}
}

// The answers are kept by lookup as well: a name the server's LOCPATH
// has and another lookup lacks has an answer for each.
func TestUTF8LocaleCachedByLookup(t *testing.T) {
	resetCharmaps(t)
	fakeLocale(t, `if [ "$LOCPATH" = /a ]; then echo UTF-8; else echo ANSI_X3.4-1968; fi`)
	a, b := []string{"LOCPATH=/a"}, []string{"LOCPATH=/b"}
	if !utf8Locale(a, "LANG", "xx_XX.UTF-8") || utf8Locale(b, "LANG", "xx_XX.UTF-8") || !utf8Locale(a, "LANG", "xx_XX.UTF-8") {
		t.Error("one lookup's answer given for another")
	}
}

// ensureLocale runs one call at a time, from its read to its write: a
// call that reads while another is between its read and its write
// works out its changes against an environment about to change.
func TestEnsureLocaleOneAtATime(t *testing.T) {
	for _, k := range localeNames {
		t.Setenv(k, "")
		os.Unsetenv(k)
	}
	t.Setenv("LC_ALL", "C")
	s := startManaged(t)
	os.Unsetenv("LC_ALL")
	if fallbackLocale(nil) == "" {
		t.Skip("the host has no UTF-8 locale")
	}
	ctx := context.Background()
	localeMu.Lock()
	done := make(chan error, 1)
	go func() { done <- s.EnsureConfigured(ctx) }()
	// The rest of EnsureConfigured takes a fraction of this; the
	// server's LC_ALL=C stays while another call holds the lock.
	for deadline := time.Now().Add(time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		if out, err := s.Run(ctx, "show-environment", "-g", "LC_ALL"); err != nil || strings.TrimSpace(string(out)) != "LC_ALL=C" {
			localeMu.Unlock()
			t.Fatalf("LC_ALL changed while another call held the lock: %q %v", out, err)
		}
	}
	localeMu.Unlock()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if _, err := s.Run(ctx, "show-environment", "-g", "LC_ALL"); err == nil {
		t.Error("LC_ALL=C kept after the lock was let go")
	}
}

// The host is asked with the LOCALE_ARCHIVE and LOCPATH of the server's
// environment, which the panes look their locale up with, not the
// daemon's: a locale that a Nix profile's archive has counts on a server
// started with that archive.
func TestEnsureLocaleAsksWithTheServersLookup(t *testing.T) {
	resetCharmaps(t)
	// The archive and path have xx_XX.UTF-8 and yy_YY.UTF-8, and list
	// yy_YY.UTF-8; neither the host's C.UTF-8 nor en_US.UTF-8 is there.
	fakeLocale(t, `case "$1:$LOCALE_ARCHIVE:$LOCPATH:$LC_ALL$LC_CTYPE" in
charmap:/lmx/archive:/lmx/path:xx_XX.UTF-8 | charmap:/lmx/archive:/lmx/path:yy_YY.UTF-8) echo UTF-8 ;;
charmap:*) echo ANSI_X3.4-1968 ;;
-a:/lmx/archive:/lmx/path:) echo C; echo POSIX; echo yy_YY.UTF-8 ;;
*) echo C; echo POSIX ;;
esac`)
	for _, k := range localeNames {
		t.Setenv(k, "")
		os.Unsetenv(k)
	}
	// Set on the server after its start: a tmux client that ran with a
	// LOCPATH of nothing would not find its own UTF-8 locale on glibc.
	s := startManaged(t)
	ctx := context.Background()
	for _, kv := range [][2]string{{"LOCALE_ARCHIVE", "/lmx/archive"}, {"LOCPATH", "/lmx/path"}} {
		if _, err := s.Run(ctx, "set-environment", "-g", kv[0], kv[1]); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("LOCALE_ARCHIVE", "/daemon/archive")
	t.Setenv("LANG", "xx_XX.UTF-8")
	if err := s.EnsureConfigured(ctx); err != nil {
		t.Fatal(err)
	}
	if out, err := s.Run(ctx, "show-environment", "-g", "LANG"); err != nil || strings.TrimSpace(string(out)) != "LANG=xx_XX.UTF-8" {
		t.Errorf("the server's LANG is %q %v, want the daemon's xx_XX.UTF-8, which the server's archive has", out, err)
	}
	// The fallback, for a daemon and a server without a UTF-8 locale, is
	// picked under the same lookup.
	if _, err := s.Run(ctx, "set-environment", "-g", "LANG", "C"); err != nil {
		t.Fatal(err)
	}
	os.Unsetenv("LANG")
	if err := s.EnsureConfigured(ctx); err != nil {
		t.Fatal(err)
	}
	if out, err := s.Run(ctx, "show-environment", "-g", "LANG"); err != nil || strings.TrimSpace(string(out)) != "LANG=yy_YY.UTF-8" {
		t.Errorf("the server's LANG is %q %v, want the fallback yy_YY.UTF-8, which the server's archive lists", out, err)
	}
}

// FormatLiteral doubles every # but a run of them before a [, which
// tmux keeps as it is; the new-session test above checks with tmux, as
// the split and shell test in cmd/laatmux does for split-window and
// new-window.
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
		if got := FormatLiteral(in); got != want {
			t.Errorf("FormatLiteral(%q) = %q, want %q", in, got, want)
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

// Next is written as a bare ;, the separator, and every other argument
// that ends in ;, a bare ; among them, gets a backslash before that
// last ;, the attach and bare lines' arguments as well; the selector
// does not, and the caller's slice is left as it is. An error names a
// Next as the ; it stands for. The new-session test below checks with
// tmux.
func TestArgsEscapeTrailingSemicolon(t *testing.T) {
	in := []string{"set-option", "@a", "x;", Next, "set-option", "@b", `x\;`, Next, "set-option", "@c", ";", Next, "a;b", "x;;", `\;`, ";x", "x"}
	keep := append([]string(nil), in...)
	want := []string{"-S", "/s;", "set-option", "@a", `x\;`, ";", "set-option", "@b", `x\\;`, ";", "set-option", "@c", `\;`, ";", "a;b", `x;\;`, `\\;`, ";x", "x"}
	if got := (Server{Path: "/s;"}).args(in...); !slices.Equal(got, want) {
		t.Errorf("args = %q, want %q", got, want)
	}
	if !slices.Equal(in, keep) {
		t.Errorf("args changed its argument: %q", in)
	}
	if got := LaatmuxServer.AttachArgsBare("x;"); !slices.Equal(got, []string{"-L", "laatmux", "attach-session", "-t", `=x\;`}) {
		t.Errorf("AttachArgsBare = %q", got)
	}
	if got := (Server{}).ArgsBare("has-session", "-t", "=x;", Next, "set-option", "@b", ";"); !slices.Equal(got, []string{"has-session", "-t", `=x\;`, ";", "set-option", "@b", `\;`}) {
		t.Errorf("ArgsBare = %q", got)
	}
	err := &Error{Args: []string{"set-option", "@a", ";", Next, "set-option", "@b", "x"}, Msg: "m"}
	if got, want := err.Error(), "tmux set-option @a ; ; set-option @b x: m"; got != want {
		t.Errorf("Error = %q, want %q", got, want)
	}
}

// No Go file of the module passes a bare ";" where tmux arguments are
// built: args writes it as the value ";", so a caller that separated
// two commands with it would give the first an argument too many, which
// tmux refuses and only a test on a real server running that very
// sequence notices. A ";" literal, in parentheses or not, is caught as
// an argument of Run, RunInput, ArgsBare or append, as an element of a
// slice or array literal, keyed or not, the inner []string of a
// [][]string or a map of them included, and as the value of a const or
// var, which could then be passed as the separator. The strings
// package's Split and the like, append(b, ";"...) on bytes, map values
// and struct literals are not matched; a ";" that is a value, or not
// tmux's, is written string(';'). The walk skips what the go tool
// skips by name, so it reads this module's code and no other's, and it
// must see a Next among what it checks, so a walk that read nothing
// cannot pass. The module's test files are left out, since the tests
// here pass ";" as a value on purpose.
func TestNoBareSeparator(t *testing.T) {
	root := filepath.Join("..", "..")
	mod, err := os.ReadFile(filepath.Join(root, "go.mod"))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(strings.Split(string(mod), "\n"), func(l string) bool {
		f := strings.Fields(l)
		return len(f) >= 2 && f[0] == "module" && f[1] == "github.com/laat/laatmux"
	}) {
		t.Fatalf("%s is not this module's root", root)
	}
	fset := token.NewFileSet()
	seen := 0
	check := func(e ast.Expr) {
		switch x := ast.Unparen(e).(type) {
		case *ast.BasicLit:
			if v, err := strconv.Unquote(x.Value); x.Kind == token.STRING && err == nil && v == ";" {
				t.Errorf("%s: a bare \";\" goes to tmux as the value ;: separate commands with tmux.Next, and write a ; that is a value, or not tmux's, as string(';')", fset.Position(e.Pos()))
			}
		case *ast.Ident:
			if x.Name == "Next" {
				seen++
			}
		case *ast.SelectorExpr:
			if x.Sel.Name == "Next" {
				seen++
			}
		}
	}
	// elems checks the elements of a literal of type typ: a slice's or
	// an array's, and those of an inner literal whose type is elided
	// when the element or map value type is a slice or array too.
	var elems func(lit *ast.CompositeLit, typ ast.Expr)
	elems = func(lit *ast.CompositeLit, typ ast.Expr) {
		var elt ast.Expr
		switch t := typ.(type) {
		case *ast.ArrayType:
			elt = t.Elt
		case *ast.MapType:
			elt = t.Value
		default:
			return
		}
		_, slice := typ.(*ast.ArrayType)
		for _, e := range lit.Elts {
			if kv, ok := e.(*ast.KeyValueExpr); ok {
				e = kv.Value
			}
			if inner, ok := e.(*ast.CompositeLit); ok && inner.Type == nil {
				elems(inner, elt)
			} else if slice {
				check(e)
			}
		}
	}
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path != root && (strings.HasPrefix(d.Name(), ".") || strings.HasPrefix(d.Name(), "_")) {
			// The go tool ignores these: .git, the agents'
			// worktrees, an editor's lock or an AppleDouble file.
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			if path != root {
				if d.Name() == "testdata" || d.Name() == "vendor" {
					return filepath.SkipDir
				}
				if _, err := os.Stat(filepath.Join(path, "go.mod")); err == nil {
					return filepath.SkipDir
				}
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return err
		}
		ast.Inspect(f, func(n ast.Node) bool {
			var list []ast.Expr
			switch n := n.(type) {
			case *ast.CallExpr:
				switch fn := n.Fun.(type) {
				case *ast.Ident:
					if fn.Name == "append" && !n.Ellipsis.IsValid() {
						list = n.Args
					}
				case *ast.SelectorExpr:
					if fn.Sel.Name == "Run" || fn.Sel.Name == "RunInput" || fn.Sel.Name == "ArgsBare" {
						list = n.Args
					}
				}
			case *ast.CompositeLit:
				// An elided literal is checked from the one it is in,
				// which knows its type.
				if n.Type != nil {
					elems(n, n.Type)
				}
			case *ast.ValueSpec:
				list = n.Values
			}
			for _, e := range list {
				check(e)
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if seen == 0 {
		t.Fatal("the walk saw no Next: it read none of the module's callers")
	}
}

// A root, a host and option values that end in ; reach tmux whole, and
// the sequence goes on past them: tmux takes an argument that ends in ;
// as the text before it followed by a separator, so new-session -c with
// such a root failed on the next flag, and an option value was cut with
// no error. One that ends in \; keeps its backslash, a root that ends
// in #; is written ##\; for -c, which tmux reads back as ##; and
// expands to #;, and a bare ;, a branch named ;, is a value: passed as
// the separator, it left the branch tag with no value, which tmux
// refused.
func TestSemicolonArgumentsReachTmux(t *testing.T) {
	s := startManaged(t)
	ctx := context.Background()
	for i, leaf := range []string{"semi;", `bs\;`, "x;;", "x#;", ";"} {
		root := filepath.Join(t.TempDir(), "proj", leaf)
		if err := os.MkdirAll(root, 0o755); err != nil {
			t.Fatal(err)
		}
		dir, err := filepath.EvalSymlinks(root)
		if err != nil {
			t.Fatal(err)
		}
		name := fmt.Sprintf("s%d", i)
		if _, err := s.NewSession(ctx, NewSessionOpts{Name: name, Cwd: root, Host: "h" + leaf, Cmd: []string{"sleep", "600"}}); err != nil {
			t.Fatalf("%s: %v", leaf, err)
		}
		// The pane's path is read from its process, which may not have
		// changed directory yet.
		var got string
		for j := 0; j < 200 && got != dir; j++ {
			out, _ := s.Run(ctx, "display-message", "-p", "-t", "="+name+":", "#{pane_current_path}")
			got = strings.TrimSpace(string(out))
			time.Sleep(10 * time.Millisecond)
		}
		if got != dir {
			t.Errorf("%s: the pane is in %q, want %q", leaf, got, dir)
		}
		for opt, want := range map[string]string{"@laatmux_cwd": root, "@laatmux_host": "h" + leaf} {
			if out, err := s.Run(ctx, "show-options", "-pqv", "-t", "="+name+":", opt); err != nil || strings.TrimSuffix(string(out), "\n") != want {
				t.Errorf("%s: %s %q %v, want %q", leaf, opt, out, err, want)
			}
		}
		// A key and a branch tag that end in ;, set in one sequence
		// whose last command runs only if the separators still work.
		target := "=" + name + ":"
		if _, err := s.Run(ctx, "set-option", "-t", target, "@laatmux_workspace", "env/"+root,
			Next, "set-option", "-t", target, "@laatmux_branch", leaf,
			Next, "set-option", "-t", target, "@laatmux_repo", "after"); err != nil {
			t.Fatalf("%s: %v", leaf, err)
		}
		for opt, want := range map[string]string{"@laatmux_workspace": "env/" + root, "@laatmux_branch": leaf, "@laatmux_repo": "after"} {
			if out, err := s.Run(ctx, "show-options", "-qv", "-t", target, opt); err != nil || strings.TrimSuffix(string(out), "\n") != want {
				t.Errorf("%s: %s %q %v, want %q", leaf, opt, out, err, want)
			}
		}
	}
}

// decodeBranch reverses EncodeBranch, for the round trip: "%" and two
// lowercase hex digits is the byte they spell. Sequences EncodeBranch
// never emits, uppercase digits say, are left as they are.
func decodeBranch(name string) string {
	var b strings.Builder
	for i := 0; i < len(name); i++ {
		if name[i] == '%' && i+2 < len(name) {
			hex := name[i+1 : i+3]
			if v, err := strconv.ParseUint(hex, 16, 8); err == nil && hex == strings.ToLower(hex) {
				b.WriteByte(byte(v))
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
