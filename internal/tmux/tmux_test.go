package tmux

import (
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
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
// byte, no DEL, and none of the characters it changes or reads.
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
