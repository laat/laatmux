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
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/laat/laatmux/internal/protocol"
)

// shellWords are words shellJoin must quote, one per character that
// forces it, with the character inside the word, and words it must
// leave bare: what git allows in a branch besides those characters,
// non-ASCII letters included.
// The set is spelled out here rather than read from the code, so a
// character dropped from the code fails the test.
func shellWords() (quoted, bare []string) {
	for _, c := range " \t\n'\"\\$`!*?[]{}()<>|&;#~=^" {
		quoted = append(quoted, "a"+string(c)+"b")
	}
	// Under zsh's magic_equal_subst an unquoted a==ls is a=/bin/ls, a
	// branch add would then make; =lcl is zsh's equals expansion. Under
	// extended_glob ^missing is every file but missing, and HEAD^ is an
	// agent's argument.
	quoted = append(quoted, "a==ls", "a/==ls", "=lcl", "^missing", "HEAD^", "")
	bare = []string{"a%b", "a+b", "a,b", "a@b", "feature/x-1_2.3", "ABCXYZabcxyz0123456789", "blåbær"}
	return quoted, bare
}

func TestShellJoin(t *testing.T) {
	quoted, bare := shellWords()
	for _, w := range quoted {
		want := "'" + strings.ReplaceAll(w, "'", `'\''`) + "'"
		if got := shellJoin([]string{w}); got != want {
			t.Errorf("%q: got %s want %s", w, got, want)
		}
	}
	for _, w := range bare {
		if got := shellJoin([]string{w}); got != w {
			t.Errorf("%q quoted: %s", w, got)
		}
	}
	if got, want := shellJoin([]string{"claude", "--flag", "a b", "it's"}), `claude --flag 'a b' 'it'\''s'`; got != want {
		t.Fatalf("got %s want %s", got, want)
	}
}

// The joined words reach a command as they were, through sh and through
// zsh with magic_equal_subst, which expands the value of any unquoted
// word with an =, and with extended_glob, which makes a word with a ^ a
// pattern that matches files or aborts the line.
func TestShellJoinRoundTrip(t *testing.T) {
	quoted, bare := shellWords()
	words := append(quoted, bare...)
	line := `printf '%s\0' ` + shellJoin(words)
	shells := [][]string{{"sh", "-c", line}}
	if _, err := exec.LookPath("zsh"); err == nil {
		// -f reads none of the user's startup files, so their options
		// stay out; only the system's zshenv is read.
		shells = append(shells,
			[]string{"zsh", "-f", "-o", "magic_equal_subst", "-c", line},
			[]string{"zsh", "-f", "-o", "extended_glob", "-c", line})
	} else {
		t.Log("no zsh on PATH; the round trip runs through sh only")
	}
	for _, sh := range shells {
		name := strings.Join(sh[:len(sh)-2], " ")
		cmd := exec.Command(sh[0], sh[1:]...)
		var stderr strings.Builder
		cmd.Stderr = &stderr
		out, err := cmd.Output()
		if err != nil {
			t.Errorf("%s: %v: %s", name, err, stderr.String())
			continue
		}
		got := strings.Split(strings.TrimSuffix(string(out), "\x00"), "\x00")
		if !slices.Equal(got, words) {
			t.Errorf("%s printed %q, want %q", name, got, words)
		}
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

// A run with no tmux on PATH is NotInstalled, wrapped or not, and not
// NoServer. A tmux that is there is not NotInstalled: one that runs and
// fails, whatever it says, one that cannot start, or one found only
// relative to the current directory, which exec refuses. Nor is an
// error tmux did not give. The cause is not unwrapped, so no other
// caller's errors.Is or errors.As changes.
func TestNotInstalled(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	t.Setenv("PATH", dir)
	_, err := DefaultServer.Run(ctx, "list-sessions")
	if !NotInstalled(err) || !NotInstalled(fmt.Errorf("list: %w", err)) || NoServer(err) {
		t.Errorf("no tmux on PATH: %v, NotInstalled %v, NoServer %v", err, NotInstalled(err), NoServer(err))
	}
	if errors.Is(err, exec.ErrNotFound) {
		t.Errorf("the cause is unwrapped: %v", err)
	}
	fake := func(script string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, "tmux"), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	fake("#!/bin/sh\necho 'no server running on /x' >&2\nexit 1\n")
	if _, err := DefaultServer.Run(ctx, "list-sessions"); NotInstalled(err) || !NoServer(err) {
		t.Errorf("tmux that ran: %v, NotInstalled %v, NoServer %v", err, NotInstalled(err), NoServer(err))
	}
	fake("#!/bin/sh\necho 'exec: \"tmux\": executable file not found in $PATH' >&2\nexit 1\n")
	if _, err := DefaultServer.Run(ctx, "list-sessions"); err == nil || NotInstalled(err) {
		t.Errorf("tmux that says not found: %v, NotInstalled %v", err, NotInstalled(err))
	}
	fake("#!/nonexistent/sh\n")
	if _, err := DefaultServer.Run(ctx, "list-sessions"); err == nil || NotInstalled(err) {
		t.Errorf("tmux that cannot start: %v, NotInstalled %v", err, NotInstalled(err))
	}
	fake("#!/bin/sh\nexit 0\n")
	t.Chdir(dir)
	t.Setenv("PATH", ".")
	if _, err := DefaultServer.Run(ctx, "list-sessions"); err == nil || !strings.Contains(err.Error(), exec.ErrDot.Error()) || NotInstalled(err) {
		t.Errorf("tmux relative to the current directory: %v, NotInstalled %v", err, NotInstalled(err))
	}
	if NotInstalled(&Error{Msg: "m"}) || NotInstalled(&exec.Error{Name: "tmux", Err: exec.ErrNotFound}) {
		t.Error("an error not from a tmux run reported as not installed")
	}
}

// The socket is found as tmux finds it, and NoSocket is nothing there:
// anything there, a path that cannot be checked, or a TMUX_TMPDIR that
// does not resolve, may be a server.
func TestNoSocket(t *testing.T) {
	dir := t.TempDir()
	real, err := filepath.EvalSymlinks(dir) // macOS's /var is a link
	if err != nil {
		t.Fatal(err)
	}
	uid := "tmux-" + strconv.Itoa(os.Getuid())
	t.Setenv("TMUX_TMPDIR", dir)
	t.Setenv("TMUX", "")
	socket := func(s Server, want string) {
		t.Helper()
		if got, ok := s.socket(); !ok || got != want {
			t.Errorf("%+v with TMUX %q, TMUX_TMPDIR %q: socket %q %v, want %q", s, os.Getenv("TMUX"), os.Getenv("TMUX_TMPDIR"), got, ok, want)
		}
	}
	for _, c := range []struct {
		s    Server
		tmux string
		want string
	}{
		{DefaultServer, "", filepath.Join(real, uid, "default")},
		{LaatmuxServer, "/s/other,1,0", filepath.Join(real, uid, "laatmux")},
		{Server{Path: "/s/p", Name: "n"}, "", "/s/p"},
		{Server{}, "/s/current,1,0", "/s/current"},
		{Server{}, "", filepath.Join(real, uid, "default")},
	} {
		t.Setenv("TMUX", c.tmux)
		socket(c.s, c.want)
	}
	t.Setenv("TMUX", "")
	// Resolved before tmux-<uid> is added, as tmux's realpath does: a ..
	// after a link goes up from the link's target.
	if err := os.MkdirAll(filepath.Join(dir, "target", "child"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "case"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(dir, "target", "child"), filepath.Join(dir, "case", "link")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMUX_TMPDIR", dir+"/case/link/..")
	socket(DefaultServer, filepath.Join(real, "target", uid, "default"))
	t.Setenv("TMUX_TMPDIR", "")
	socket(DefaultServer, filepath.Join("/tmp", uid, "default"))
	t.Setenv("TMUX_TMPDIR", filepath.Join(dir, "missing"))
	if got, ok := DefaultServer.socket(); ok || DefaultServer.NoSocket() {
		t.Errorf("TMUX_TMPDIR that does not resolve: socket %q %v, NoSocket %v", got, ok, DefaultServer.NoSocket())
	}
	// One that is a file has no socket under it that tmux could make,
	// and is not told for no socket.
	file := filepath.Join(dir, "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMUX_TMPDIR", file)
	if DefaultServer.NoSocket() {
		t.Error("TMUX_TMPDIR that is a file has no socket")
	}
	t.Setenv("TMUX_TMPDIR", dir)
	if !DefaultServer.NoSocket() {
		t.Error("nothing there is a socket")
	}
	sock := filepath.Join(dir, uid, "default")
	if err := os.MkdirAll(filepath.Dir(sock), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(dir, "nowhere"), sock); err != nil {
		t.Fatal(err)
	}
	if DefaultServer.NoSocket() {
		t.Error("a dangling link at the socket is no socket")
	}
	if err := os.Remove(sock); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sock, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if DefaultServer.NoSocket() {
		t.Error("a file at the socket is no socket")
	}
	if os.Getuid() != 0 {
		if err := os.Chmod(filepath.Join(dir, uid), 0); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { os.Chmod(filepath.Join(dir, uid), 0o700) })
		if (Server{Name: "other"}).NoSocket() {
			t.Error("a socket directory that cannot be read is no socket")
		}
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

		// A "$" tmux 3.2 to 3.4 store as "\$", before a letter, "_" or
		// "{", and one they keep, encoded alike.
		"fix$HOME": "fix%24HOME",
		"a$_b":     "a%24_b",
		"a${b}":    "a%24{b}",
		"a$1b":     "a%241b",
		"end$":     "end%24",
		"%24":      "%2524",

		// Bytes tmux escapes by vis(3), and UTF-8 it keeps as given.
		"tab\tx":       "tab%09x",
		"nl\n":         "nl%0a",
		"a\x01b":       "a%01b",
		"del\x7f":      "del%7f",
		"a\xffb":       "a%ffb",
		"%09":          "%2509",
		"blåbær/ø":     "blåbær/ø",
		"feat/😀":       "feat/😀",
		"日本\x80":       "日本%80",
		"\xe2\x82":     "%e2%82",
		"\xed\xa0\x80": "%ed%a0%80",
		"\xef\xbf\xbd": "\xef\xbf\xbd",

		// A C1 control character (U+0085, U+0080, U+009F), which tmux
		// 3.3 stores escaped, and U+2063, of which Sep is made, encoded
		// byte by byte; U+00A0 and U+2064 next to them kept.
		"c1\xc2\x85x":                 "c1%c2%85x",
		"\xc2\x80":                    "%c2%80",
		"\xc2\x9f":                    "%c2%9f",
		"nb\xc2\xa0x":                 "nb\xc2\xa0x",
		"a\xe2\x81\xa3b":              "a%e2%81%a3b",
		"sep\xe2\x81\xa3\xe2\x81\xa3": "sep%e2%81%a3%e2%81%a3",
		"is\xe2\x81\xa4x":             "is\xe2\x81\xa4x",

		// A line and a paragraph separator and a noncharacter, which a
		// tmux 3.3 built without utf8proc stores escaped, encoded byte
		// by byte; U+2027, U+FDCF, U+FDF0, U+FFFD and U+FFDD0, a private
		// use code point at U+FDD0 of plane 15, next to them kept.
		"ls\u2028x":    "ls%e2%80%a8x",
		"\u2029":       "%e2%80%a9",
		"\u2027":       "\u2027",
		"nc\ufdd0x":    "nc%ef%b7%90x",
		"\ufdef":       "%ef%b7%af",
		"\ufdcf\ufdf0": "\ufdcf\ufdf0",
		"nc\ufffex":    "nc%ef%bf%bex",
		"\uffff":       "%ef%bf%bf",
		"\U0001fffe":   "%f0%9f%bf%be",
		"\U0010ffff":   "%f4%8f%bf%bf",
		"\U000ffdd0":   "\U000ffdd0",
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
	// byte that cannot stand alone in UTF-8, and every code point.
	for c := 0; c < 256; c++ {
		in := string([]byte{byte(c)})
		got := EncodeBranch(in)
		if back := decodeBranch(got); back != in || !keptByTmux(got) {
			t.Errorf("EncodeBranch(%q) = %q, decoded %q", in, got, back)
		}
	}
	for r, bad := rune(0), 0; r <= unicode.MaxRune && bad < 20; r++ {
		in := string(r)
		got := EncodeBranch(in)
		if back := decodeBranch(got); back != in || !keptByTmux(got) {
			t.Errorf("EncodeBranch(%q) = %q, decoded %q", in, got, back)
			bad++
		}
	}
	if got := SessionName("proj", "fix/v1.2"); got != "proj/fix/v1%2e2" {
		t.Errorf("SessionName = %q", got)
	}
}

// keptByTmux is a name tmux stores as given and laatmux reads back
// whole: valid UTF-8 with no control character, C1 included, none of
// the characters tmux changes or reads, and no U+2063, of which Sep is
// made. A "$" counts as changed, as tmux 3.2 to 3.4 change one before a
// letter, "_" or "{", and so do U+2028, U+2029 and a noncharacter,
// which a tmux 3.3 built without utf8proc escapes. It does not know
// what code points such a tmux's C library has no tables for, an
// unassigned one say, which that tmux escapes too.
func keptByTmux(name string) bool {
	return utf8.ValidString(name) && !strings.ContainsAny(name, ".:#;$\\\u2028\u2029") &&
		!strings.ContainsFunc(name, func(r rune) bool {
			return unicode.IsControl(r) || strings.ContainsRune(Sep, r) || r >= 0xfdd0 && r <= 0xfdef || r&0xffff >= 0xfffe
		})
}

// A session name as tmux lists it, escaped by vis(3), is decoded and
// the bytes EncodeBranch encodes, but for %, #, ; and :, written as it
// writes them; a name that is not one tmux could have stored is
// encoded as it is; a name with nothing to encode, a managed session's
// encoded one say, is kept. Every result without a :, under a host's
// name, is one CheckSessionName lets a plain attachment be made under:
// a . that tmux 3.7 lists as given among them, which a tmux before 3.7
// would store as _.
func TestEncodeListed(t *testing.T) {
	cases := map[string]string{
		"notes":          "notes",
		"proj/fix%2ebar": "proj/fix%2ebar",
		"proj/a%5cb":     "proj/a%5cb",
		"blåbær/ø":       "blåbær/ø",
		"a b":            "a b",

		// As tmux lists a\b, \\, a tab, a newline, the C-style controls,
		// another control byte, DEL, bytes that are not UTF-8, and an
		// octal escape before a digit.
		`a\\b`:           "a%5cb",
		`\\\\`:           "%5c%5c",
		`end\\`:          "end%5c",
		`tab\tx`:         "tab%09x",
		`nl\n`:           "nl%0a",
		`\a\b\v\f\r`:     "%07%08%0b%0c%0d",
		`a\001b`:         "a%01b",
		`del\177`:        "del%7f",
		`a\377b`:         "a%ffb",
		`日本\200`:         "日本%80",
		`\342\202x`:      "%e2%82x",
		`n\0011`:         "n%011",
		`a\\001`:         "a%5c001",
		`c1` + "\u0085x": "c1%c2%85x",
		"sep\u2063x":     "sep%e2%81%a3x",

		// A line separator and a noncharacter as a host whose tmux keeps
		// them lists them, encoded byte by byte, as EncodeBranch encodes
		// them, since a tmux 3.3 built without utf8proc stores them
		// escaped; a private use code point at U+FDD0's place in plane 15
		// kept.
		"ls\u2028x":     "ls%e2%80%a8x",
		"nc\ufffex":     "nc%ef%bf%bex",
		"pu\U000ffdd0x": "pu\U000ffdd0x",

		// A # and a ; are kept. A . and a :, which tmux 3.7 lists as
		// given: the . encoded, as a tmux before 3.7 would store it as
		// _, and the : kept, as no target reaches the session.
		"a#{b};": "a#{b};",
		"a.b":    "a%2eb",
		"x.":     "x%2e",
		"a.b:c":  "a%2eb:c",

		// A $ tmux 3.2 to 3.4 store escaped, one they keep, which is
		// encoded all the same, and c$xd as tmux 3.2 and 3.4 list it.
		"c$xd":   "c%24xd",
		"a$_b":   "a%24_b",
		"a${b}":  "a%24{b}",
		"a$1b":   "a%241b",
		"$(x)":   "%24(x)",
		"end$":   "end%24",
		"a$é":    "a%24é",
		`c\$xd`:  "c%5c%24xd",
		`c\\$xd`: "c%5c%24xd",

		// A valid U+FFFD is kept whole, as tmux keeps it, so a name with
		// one decodes.
		"v\\\\\ufffdx": "v%5c\ufffdx",

		// Not as tmux lists a name: an escape vis does not write, a
		// lone \ at the end, an octal escape past 0377, a valid UTF-8
		// character written in octal, and a raw tab; each is encoded
		// whole as it is, the escapes that would decode with it too.
		`a\q`:      "a%5cq",
		`end\`:     "end%5c",
		`\400`:     "%5c400",
		`\303\251`: "%5c303%5c251",
		"raw\ttab": "raw%09tab",
		`a\\b\`:    "a%5c%5cb%5c",
		`a\\$x\q`:  "a%5c%5c%24x%5cq",
		`x\0`:      "x%5c0",
		`\0012\9`:  "%5c0012%5c9",
	}
	for in, want := range cases {
		got := EncodeListed(in)
		if got != want {
			t.Errorf("EncodeListed(%q) = %q, want %q", in, got, want)
		}
		if err := CheckSessionName("mac/" + got); err != nil && !strings.Contains(got, ":") {
			t.Errorf("EncodeListed(%q) = %q: %v", in, got, err)
		}
	}
	// Every byte alone, and between two letters, as tmux would list a
	// name with it: decoded, it is encoded as EncodeBranch encodes it,
	// but for the characters only EncodeBranch writes.
	for c := 1; c < 256; c++ {
		for _, name := range []string{string([]byte{byte(c)}), "a" + string([]byte{byte(c)}) + "z"} {
			if strings.ContainsAny(name, "%#;:") {
				continue
			}
			if got, want := EncodeListed(visName(name)), EncodeBranch(name); got != want {
				t.Errorf("EncodeListed(%q), listed from %q, = %q, want %q", visName(name), name, got, want)
			}
		}
	}
}

// dollars is the name with each "$" before an ASCII letter, "_" or "{"
// given the prefix before it: a "\" for the name tmux 3.2 to 3.4 store
// for a session tmux 3.5 stores as this, which tmux 3.2 lists as
// stored, and two for the name tmux 3.4 lists, with one more.
func dollars(name, prefix string) string {
	var b strings.Builder
	for i := 0; i < len(name); i++ {
		if name[i] == '$' && i+1 < len(name) {
			if c := name[i+1]; 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || c == '_' || c == '{' {
				b.WriteString(prefix)
			}
		}
		b.WriteByte(name[i])
	}
	return b.String()
}

// The model of tmux's vis against tmux itself: a session renamed to a
// name with each byte in turn is listed as visName writes the name, or
// on tmux 3.2 to 3.4 with its $ before a letter changed, or before 3.7
// with a . or : as _, and a session
// renamed to EncodeListed of what was listed, under a host's name, is
// listed with exactly that name. rename-session stores a name as
// new-session does; one session renamed over and over keeps the run to
// one process and a dozen tmux commands.
func TestEncodeListedKeptByTmux(t *testing.T) {
	s := startManaged(t)
	ctx := context.Background()
	out, err := s.Run(ctx, "new-session", "-d", "-P", "-F", "#{session_id}", "sleep 600")
	if err != nil {
		t.Fatal(err)
	}
	id := strings.TrimSpace(string(out))
	var names []string
	for c := 1; c < 256; c++ {
		// new-session and rename-session expand a # as a format.
		if c != '#' {
			names = append(names, "a"+string([]byte{byte(c)})+"z")
		}
	}
	names = append(names, "blåbær", "e😀", "u\ufffdx", "t\xe2\x82x", "s\xed\xa0\x80", `a\$xb`, "a$_b", "a${b", "a$1b", "end$")
	// tmux 3.7 refuses a name with a control byte, DEL or a byte that is
	// not UTF-8, so it lists none with one.
	if _, err := s.Run(ctx, "rename-session", "-t", id, "a\x01z"); err != nil {
		if !strings.Contains(err.Error(), "invalid session name") {
			t.Fatal(err)
		}
		names = slices.DeleteFunc(names, func(n string) bool {
			return !utf8.ValidString(n) || strings.ContainsFunc(n, func(r rune) bool { return r < 0x20 || r == 0x7f })
		})
	}
	// Fifty names to a tmux command, which takes a command line of so
	// many bytes only.
	rename := func(names []string) []string {
		var got []string
		for chunk := range slices.Chunk(names, 50) {
			var args []string
			for _, n := range chunk {
				args = append(args, "rename-session", "-t", id, n, Next, "display-message", "-p", "-t", id, "#{session_name}", Next)
			}
			out, err := s.Run(ctx, args[:len(args)-1]...)
			if err != nil {
				t.Fatalf("%q: %v", chunk, err)
			}
			got = append(got, strings.Split(strings.TrimSuffix(string(out), "\n"), "\n")...)
		}
		if len(got) != len(names) {
			t.Fatalf("%d names listed for %d: %q", len(got), len(names), got)
		}
		return got
	}
	listed := rename(names)
	var local []string
	for i, l := range listed {
		// tmux before 3.7 stores a . or a : as _.
		want := visName(names[i])
		if !slices.Contains([]string{want, dollars(want, `\`), dollars(want, `\\`), strings.NewReplacer(".", "_", ":", "_").Replace(want)}, l) {
			t.Errorf("%q listed as %q, visName %q", names[i], l, want)
		}
		local = append(local, "mac/"+EncodeListed(l))
	}
	for i, got := range rename(local) {
		if got != local[i] {
			t.Errorf("%q, listed as %q: local name %q listed as %q", names[i], listed[i], local[i], got)
		}
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

// A prompt with a newline makes the argument it is in, and tmux's
// message that repeats it, quoted, and the prompt is in them escaped:
// Redact replaces it there too. So it does for a prompt with a " in an
// argument another word's tab makes quoted, and for a prompt with a
// ', which is in the argument as shellJoin quotes it, escaped once
// more where the argument is quoted. A prompt such as p, which the
// placeholder has in it, is not replaced again in the placeholder; and
// forms that overlap leave nothing of either: two apostrophes in their
// own quoting, and claude 'claude and e 'e, whose bare forms start at
// the command's name or in it, before their shell-quoted forms.
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
		"p":              "tmux new-session -d claude {prompt}: failed: {prompt}",
		"{p":             "tmux new-session -d claude {prompt}: failed: {prompt}",
		"rom":            "tmux new-session -d claude {prompt}: failed: {prompt}",
		"prompt":         "tmux new-session -d claude {prompt}: failed: {prompt}",
		"''":             "tmux new-session -d claude {prompt}: failed: {prompt}",
		"claude 'claude": "tmux new-session -d {prompt}: failed: {prompt}",
		"e 'e":           "tmux new-session -d claud{prompt}: failed: {prompt}",
		"p\nq":           `tmux new-session -d "claude {prompt}": "failed: {prompt}"`,
	} {
		err := &Error{Args: []string{"new-session", "-d", shellJoin([]string{"claude", secret})}, Msg: "failed: " + secret}
		if got := Redact(err, secret, "{prompt}").Error(); got != want {
			t.Errorf("%q: redacted %q, want %q", secret, got, want)
		}
	}
	// Occurrences of one form that overlap are all hidden, as one run;
	// so is a prompt of backslashes in an argument another word's tab
	// makes quoted, where the bare prompt is found at every place in
	// the doubled run.
	if got, want := Redact(&Error{Args: []string{"x", "ababa"}, Msg: "ababa"}, "aba", "{prompt}").Error(), "tmux x {prompt}: {prompt}"; got != want {
		t.Errorf("overlapping: %q, want %q", got, want)
	}
	slashes := strings.Repeat(`\`, 4096)
	err := &Error{Args: []string{"new-session", "-d", shellJoin([]string{"claude", "--dir", "/w/a\tb", slashes})}, Msg: "failed"}
	if got, want := Redact(err, slashes, "{prompt}").Error(), `tmux new-session -d "claude --dir '/w/a\tb' {prompt}": failed`; got != want {
		t.Errorf("backslashes: %q, want %q", got, want)
	}
}

// cover marks every occurrence, overlapping ones too, and nothing
// else, as a search from every byte does.
func TestCover(t *testing.T) {
	for _, c := range []struct{ msg, f string }{
		{"ababa", "aba"}, {"aaaa", "aa"}, {"abcabcab", "abcab"}, {"xabx", "ab"},
		{"aabaabaaab", "aabaa"}, {"abc", "abcd"}, {"abc", "c"}, {"", "a"},
		{strings.Repeat("ab", 50) + "a", "abababa"}, {"aaabaaaabaaaaab", "aaab"}, {"aabaaabaaa", "aabaaa"},
	} {
		got := make([]bool, len(c.msg))
		cover(got, c.msg, c.f)
		want := make([]bool, len(c.msg))
		for i := 0; i+len(c.f) <= len(c.msg); i++ {
			if c.msg[i:i+len(c.f)] == c.f {
				for j := i; j < i+len(c.f); j++ {
					want[j] = true
				}
			}
		}
		if !slices.Equal(got, want) {
			t.Errorf("cover(%q, %q) = %v, want %v", c.msg, c.f, got, want)
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

// PrintablePath quotes the path of a *fs.PathError and both of an
// *os.LinkError, and keeps the op and the cause; any other error, one
// that wraps either among them, is returned as it is, since rebuilding
// it would drop what wraps it.
func TestPrintablePath(t *testing.T) {
	pe := &fs.PathError{Op: "open", Path: "/w/a\tb\x1b[31m/.git", Err: fs.ErrPermission}
	got := PrintablePath(pe)
	if want := "open " + strconv.Quote(pe.Path) + ": permission denied"; got.Error() != want || !errors.Is(got, fs.ErrPermission) {
		t.Errorf("PrintablePath = %v, want %s", got, want)
	}
	if pe.Path != "/w/a\tb\x1b[31m/.git" {
		t.Errorf("the original's path changed: %q", pe.Path)
	}
	le := &os.LinkError{Op: "renameat", Old: "d\tir/.tmp", New: "d\tir/f\x1b[1m", Err: fs.ErrExist}
	got = PrintablePath(le)
	if want := "renameat " + strconv.Quote(le.Old) + " " + strconv.Quote(le.New) + ": file already exists"; got.Error() != want || !errors.Is(got, fs.ErrExist) {
		t.Errorf("PrintablePath = %v, want %s", got, want)
	}
	if le.Old != "d\tir/.tmp" || le.New != "d\tir/f\x1b[1m" {
		t.Errorf("the original's paths changed: %q %q", le.Old, le.New)
	}
	if wrapped := fmt.Errorf("x: %w", le); PrintablePath(wrapped) != wrapped {
		t.Errorf("a wrapping error rebuilt")
	}
	// A cause that is itself a *fs.PathError, as os.Root's MkdirAll
	// gives, is quoted too.
	nested := &fs.PathError{Op: "mkdirat", Path: "d\tir", Err: &fs.PathError{Op: "statat", Path: "d\tir\x1b[1m", Err: syscall.ELOOP}}
	got = PrintablePath(nested)
	if want := "mkdirat " + strconv.Quote("d\tir") + ": statat " + strconv.Quote("d\tir\x1b[1m") + ": " + syscall.ELOOP.Error(); got.Error() != want || !errors.Is(got, syscall.ELOOP) {
		t.Errorf("PrintablePath = %v, want %s", got, want)
	}
	got = PrintablePath(&os.LinkError{Op: "renameat", Old: "a", New: "b", Err: nested.Err})
	if want := "renameat a b: statat " + strconv.Quote("d\tir\x1b[1m") + ": " + syscall.ELOOP.Error(); got.Error() != want {
		t.Errorf("PrintablePath = %v, want %s", got, want)
	}
	wrapped := fmt.Errorf("x: %w", pe)
	if got := PrintablePath(wrapped); got != wrapped {
		t.Errorf("a wrapping error rebuilt: %v", got)
	}
	if got := PrintablePath(nil); got != nil {
		t.Errorf("PrintablePath(nil) = %v", got)
	}
	// A listen on a unix socket whose directory is gone: net's error
	// names the socket's path in its address, rebuilt quoted, with the
	// op and the cause kept and the original untouched.
	sock := filepath.Join(t.TempDir(), "go\tne\x1b[31m", "s.sock")
	_, err := net.Listen("unix", sock)
	var oe *net.OpError
	if !errors.As(err, &oe) {
		t.Fatalf("listen: %#v", err)
	}
	got = PrintablePath(err)
	var goe *net.OpError
	if !errors.As(got, &goe) || !strings.HasPrefix(got.Error(), "listen unix "+strconv.Quote(sock)+": ") || goe.Err != oe.Err {
		t.Errorf("PrintablePath = %v", got)
	}
	if oe.Addr.String() != sock {
		t.Errorf("the original's address changed: %q", oe.Addr)
	}
	tcp := &net.OpError{Op: "listen", Net: "tcp", Addr: &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)}, Err: syscall.EADDRINUSE}
	if got := PrintablePath(tcp); got != tcp {
		t.Errorf("a tcp error rebuilt: %v", got)
	}
}

// Each line is as Printable shows it, and the newlines between the lines
// stay, empty lines and a last newline included.
func TestPrintableLines(t *testing.T) {
	for _, c := range [][2]string{
		{"", ""},
		{"plain\nlines", "plain\nlines"},
		{"fatal: '/w/a\tb' is dirty\nhint: use --force\n\nlast\n", strconv.Quote("fatal: '/w/a\tb' is dirty") + "\nhint: use --force\n\nlast\n"},
		{"one\n\x1b[31mtwo\u009b", "one\n" + strconv.Quote("\x1b[31mtwo\u009b")},
		{"bad \xff\r\nok", strconv.Quote("bad \xff\r") + "\nok"},
	} {
		if got := PrintableLines(c[0]); got != c[1] {
			t.Errorf("PrintableLines(%q) = %q, want %q", c[0], got, c[1])
		}
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

// Fields reads back values with Sep, a newline or nothing in them, at
// either end too, a record for each line tmux printed, which it ends
// with a newline after the last separator; what a user's after hook
// prints after the last one is not read. Each Fields draws its own
// separator: a | and base32, with no % for display-message to expand
// as strftime does and no # for a format to expand.
func TestFieldsParse(t *testing.T) {
	f := NewFields("#{a}", "#{b}", "#{c}")
	if want := "#{a}" + f.sep + "#{b}" + f.sep + "#{c}" + f.sep; f.format != want {
		t.Errorf("format = %q, want %q", f.format, want)
	}
	random := strings.TrimPrefix(f.sep, "|")
	if !strings.HasPrefix(f.sep, "|") || len(random) < 16 || strings.Trim(random, "ABCDEFGHIJKLMNOPQRSTUVWXYZ234567") != "" {
		t.Errorf("separator %q", f.sep)
	}
	if g := NewFields("#{a}", "#{b}", "#{c}"); g.sep == f.sep {
		t.Errorf("two Fields share the separator %q", f.sep)
	}
	recs := [][]string{
		{"a" + Sep + "b", "\nx\n", ""},
		{Sep, "\n", "c" + Sep + Sep + "\n" + Sep},
		{"", "", ""},
		{"\n\n", Sep + "\n" + Sep, "end"},
	}
	var out string
	for _, r := range recs {
		out += strings.Join(r, f.sep) + f.sep + "\n"
	}
	out += "hook\n"
	if got := f.Parse([]byte(out)); !slices.EqualFunc(got, recs, slices.Equal) {
		t.Errorf("Parse(%q) = %q, want %q", out, got, recs)
	}
	if got := f.Parse(nil); len(got) != 0 {
		t.Errorf("Parse of nothing = %q", got)
	}
}

// A failed Records names the command with Sep where each separator
// was, so the daemon's log line for a failed poll reads the same at
// every poll, and is still the tmux error it was: a server that is not
// running is NoServer, and no tmux on PATH NotInstalled.
func TestRecordsError(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}
	ctx := context.Background()
	s := Server{Name: fmt.Sprintf("laatmux-test-%d", os.Getpid())}
	f := NewFields("#{pane_id}", "#{pane_tty}")
	recs, err := s.Records(ctx, f, "list-panes", "-a")
	if want := "tmux list-panes -a -F #{pane_id}" + Sep + "#{pane_tty}" + Sep + ": "; !NoServer(err) || !strings.HasPrefix(err.Error(), want) || len(recs) != 0 {
		t.Errorf("Records on no server: %q %v, want an error starting %q", recs, err, want)
	}
	t.Setenv("PATH", t.TempDir())
	if _, err := s.Records(ctx, f, "list-panes", "-a"); !NotInstalled(err) {
		t.Errorf("Records with no tmux on PATH: %v, not NotInstalled", err)
	}
}

// A pane whose directory and @laatmux_cwd have Sep and a newline in
// them, and whose title and window name have Sep, is listed with each
// value in its own field, and so is the pane listed after it. Split at
// Sep, the values after the directory were read from the fields after
// theirs; split at the newline, the pane was not listed at all. Each
// value ends in a $ and comes back as written: tmux 3.4 on macOS puts
// a backslash before a $ that comes before a U+2063, which the | a
// separator starts with keeps away from a value, and Query's decoding
// would undo. The directory has a $ before a letter, which tmux 3.4 prints
// as \$ everywhere, before its newline, where a Query that decoded the
// line with the probe only left it; a title or a window name with one
// is stored with the \$ by tmux 3.4 itself. Before the newline it has
// Sep and the probe without its tail, which such a Query dropped as its
// own. The flags listed last are set, the first pane in copy mode and
// tagged as an attach pane and the second as a sidebar, so a value
// read from another field shows there too.
func TestListPanesValuesWithSep(t *testing.T) {
	s := startManaged(t)
	ctx := context.Background()
	root := filepath.Join(t.TempDir(), "proj", Sep+"a$b"+Sep+probe+"\nc"+Sep+"$")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	real, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	made, err := s.NewSession(ctx, NewSessionOpts{Name: "proj/a", Cwd: root, Cmd: []string{"sleep", "600"}, Host: "vm"})
	if err != nil {
		t.Fatal(err)
	}
	plain := t.TempDir()
	next, err := s.NewSession(ctx, NewSessionOpts{Name: "proj/b", Cwd: plain, Cmd: []string{"sleep", "600"}, Host: "mac"})
	if err != nil {
		t.Fatal(err)
	}
	// The fields after the values are set as well, each half of Own on
	// a pane of its own.
	if _, err := s.Run(ctx, "select-pane", "-t", made.PaneID, "-T", "t"+Sep+"1$", Next, "rename-window", "-t", made.PaneID, "w"+Sep+"1$",
		Next, "set-option", "-p", "-t", made.PaneID, "@laatmux_attach_pane", "1", Next, "copy-mode", "-t", made.PaneID,
		Next, "set-option", "-p", "-t", next.PaneID, "@laatmux_sidebar", "1"); err != nil {
		t.Fatal(err)
	}
	// The pane's path is read from its process, which may not have
	// changed directory yet.
	panes := map[string]Pane{}
	for i := 0; i < 200 && panes[made.PaneID].CurrentPath != real; i++ {
		list, err := s.ListPanes(ctx)
		if err != nil {
			t.Fatal(err)
		}
		clear(panes)
		for _, p := range list {
			panes[p.ID] = p
		}
		time.Sleep(10 * time.Millisecond)
	}
	p := panes[made.PaneID]
	if p.Session != "proj/a" || p.WindowIndex != 0 || p.WindowName != "w"+Sep+"1$" || !strings.HasPrefix(p.TTY, "/dev/") || p.PID <= 0 ||
		p.CurrentPath != real || p.Title != "t"+Sep+"1$" || p.Dead || p.WindowActivity <= 0 || p.Host != "vm" || p.Cwd != root ||
		!p.Managed || p.ServerPID != made.ServerPID || !p.InMode || !p.Own {
		t.Errorf("pane in %q listed as %+v", root, p)
	}
	if p := panes[next.PaneID]; p.Session != "proj/b" || p.Host != "mac" || p.Cwd != plain || !p.Managed || p.ServerPID != next.ServerPID || p.InMode || !p.Own {
		t.Errorf("pane in %q listed as %+v", plain, p)
	}
	if len(panes) != 2 {
		t.Errorf("listed %d panes: %+v", len(panes), panes)
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
// isolation options, each reached by its id whatever its name: the
// first one is called 0, which as a bare target is pane 0 of the most
// recent session; tmux 3.7 keeps c:d, which =c:d: takes for a window of
// a session c; =$1: is the session with the id $1, b here; and n$m is
// stored as n\$m by tmux 3.2 to 3.4, and listed as n\\$m by 3.4. The
// test sets and reads each by its id too.
func TestEnsureConfiguredClearsEverySession(t *testing.T) {
	s := startManaged(t)
	ctx := context.Background()
	names := []string{"0", "b", "c:d", "$1", "n$m"}
	var ids []string
	for _, name := range names {
		out, err := s.Run(ctx, "new-session", "-d", "-s", name, "-P", "-F", "#{session_id}", "sleep 600")
		if err != nil {
			t.Fatal(err)
		}
		id := strings.TrimSpace(string(out))
		if _, err := s.Run(ctx, "set-option", "-t", id, "status", "on"); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	if err := s.EnsureConfigured(ctx); err != nil {
		t.Fatal(err)
	}
	for i, id := range ids {
		if out, err := s.Run(ctx, "show-options", "-t", id, "status"); err != nil || strings.TrimSpace(string(out)) != "" {
			t.Errorf("session %s (%s) keeps %q %v", names[i], id, out, err)
		}
	}
}

// A session is reached by its exact name as SessionTarget writes it
// when the name has a ., which tmux 3.7 keeps: =a.b alone is pane b of
// window a, and has-session, the attach and kill-session found no
// session a.b. A name with a : no target reaches: =c:d: is a window of
// session c, which c's window d:x is, so HasSession does not find c:d;
// nor $1, which as =$1: is the session with the id $1, c:d here, nor no
// name, which as =: is the most recent session. That part runs on every
// version, where tmux before 3.7 stores c:d as c_d; the rest is skipped
// there, a.b being stored as a_b.
func TestSessionTargets(t *testing.T) {
	s := startManaged(t)
	ctx := context.Background()
	for _, args := range [][]string{
		{"new-session", "-d", "-s", "c", "sleep 600"},
		{"new-window", "-d", "-t", "=c:", "-n", "d:x", "sleep 600"},
		{"new-session", "-d", "-s", "c:d", "sleep 600"},
		{"new-session", "-d", "-s", "a.b", "sleep 600"},
		{"new-session", "-d", "-s", "$1", "sleep 600"},
	} {
		if _, err := s.Run(ctx, args...); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"c:d", "$1", ""} {
		if s.HasSession(ctx, name) {
			t.Errorf("HasSession found %q", name)
		}
	}
	out, err := s.Run(ctx, "list-sessions", "-F", "#{session_name}")
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(out), "\n"); n != 4 {
		t.Fatalf("%d sessions left of 4: %q", n, out)
	}
	if !slices.Contains(strings.Split(strings.TrimSpace(string(out)), "\n"), "a.b") {
		t.Skipf("tmux before 3.7 stores a . in a session name as _: %q", out)
	}
	if !s.HasSession(ctx, "a.b") {
		t.Error("HasSession did not find a.b")
	}
	// The attach line as the attach window runs it: a control-mode
	// client with nothing to read attaches, says to which session, and
	// exits.
	out, err = exec.Command("tmux", append([]string{"-u", "-C"}, s.AttachArgsBare("a.b")...)...).CombinedOutput()
	if err != nil || !regexp.MustCompile(`(?m)^%session-changed \$\d+ a\.b$`).Match(out) {
		t.Errorf("attach to a.b: %v\n%s", err, out)
	}
	if _, err := s.Run(ctx, "kill-session", "-t", SessionTarget("a.b")); err != nil || s.HasSession(ctx, "a.b") {
		t.Errorf("kill-session a.b: %v", err)
	}
	if !s.HasSession(ctx, "c") {
		t.Error("session c is gone")
	}
}

// Every pane is listed with its session's id, and KillSessionID kills
// a session by it whatever the name: c:d, which tmux 3.7 keeps and
// =c:d: takes for window d:x of session c, and $1, which as a target is
// the session with the id $1, c:d here (c_d before tmux 3.7). Anything
// but a session id is refused: the empty target is the most recent
// session, a window's id and a pane's the session they are in; so is a
// pid that is none. A pid not the server's, which a server started
// since the listing has, kills nothing. A session gone is no error and
// kills nothing else, so session c and its two windows are left; nor
// is one gone from a server with no session left, or with no server.
// A server that cannot be reached is an error.
func TestKillSessionID(t *testing.T) {
	s := startManaged(t)
	ctx := context.Background()
	var made [][2]string // the pane id and session id each command printed
	for _, args := range [][]string{
		{"new-session", "-d", "-s", "c"},
		{"new-window", "-d", "-t", "=c:", "-n", "d:x"},
		{"new-session", "-d", "-s", "c:d"},
		{"new-session", "-d", "-s", "$1"},
	} {
		out, err := s.Run(ctx, append(args, "-P", "-F", "#{pane_id} #{session_id}", "sleep 600")...)
		if err != nil {
			t.Fatal(err)
		}
		pane, id, _ := strings.Cut(strings.TrimSpace(string(out)), " ")
		made = append(made, [2]string{pane, id})
	}
	panes, err := s.ListPanes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(panes) != len(made) || panes[0].ServerPID <= 0 {
		t.Fatalf("listed %d panes of %d: %+v", len(panes), len(made), panes)
	}
	pid := panes[0].ServerPID
	for _, p := range panes {
		if i := slices.IndexFunc(made, func(m [2]string) bool { return m[0] == p.ID }); i < 0 || p.SessionID != made[i][1] || p.ServerPID != pid {
			t.Errorf("pane %s of %q listed with the session id %q on server %d, made %q on %d", p.ID, p.Session, p.SessionID, p.ServerPID, made, pid)
		}
	}
	for _, id := range []string{"", "c", "=c:", "$", "$1x", "@1", "%1"} {
		if err := s.KillSessionID(ctx, id, pid); err == nil || !strings.Contains(err.Error(), "not a session id") {
			t.Errorf("KillSessionID %q: %v, want a refusal", id, err)
		}
	}
	dollar1 := made[3][1]
	if err := s.KillSessionID(ctx, dollar1, 0); err == nil || !strings.Contains(err.Error(), "not a server pid") {
		t.Errorf("KillSessionID %s on server 0: %v, want a refusal", dollar1, err)
	}
	if err := s.KillSessionID(ctx, dollar1, pid+1); err != nil {
		t.Errorf("KillSessionID %s on another server: %v", dollar1, err)
	}
	if _, err := s.Run(ctx, "has-session", "-t", dollar1); err != nil {
		t.Errorf("KillSessionID %s on another server killed it: %v", dollar1, err)
	}
	for _, m := range [][2]string{made[3], made[2]} {
		if err := s.KillSessionID(ctx, m[1], pid); err != nil {
			t.Errorf("KillSessionID %s: %v", m[1], err)
		}
		if err := s.KillSessionID(ctx, m[1], pid); err != nil {
			t.Errorf("KillSessionID %s gone: %v", m[1], err)
		}
	}
	out, err := s.Run(ctx, "list-sessions", "-F", "#{session_name} #{session_windows}")
	if err != nil || string(out) != "c 2\n" {
		t.Fatalf("sessions left %q %v, want c with 2 windows", out, err)
	}
	c := made[0][1]
	if err := s.KillSessionID(ctx, c, pid); err != nil {
		t.Errorf("KillSessionID %s: %v", c, err)
	}
	if err := s.KillSessionID(ctx, c, pid); err != nil {
		t.Errorf("KillSessionID %s with no session left: %v", c, err)
	}
	// The server may still be going after kill-server returns.
	if _, err := s.Run(ctx, "kill-server"); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 50; i++ {
		if _, err = s.Run(ctx, "list-sessions"); NoServer(err) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !NoServer(err) {
		t.Fatalf("server still up: %v", err)
	}
	if err := s.KillSessionID(ctx, c, pid); err != nil {
		t.Errorf("KillSessionID %s with no server: %v", c, err)
	}
	if err := (Server{Path: "/dev/null/sock"}).KillSessionID(ctx, c, pid); err == nil {
		t.Error("KillSessionID through a socket path under a file: no error")
	}
}

// CheckTarget refuses no name, a name with a : wherever it is, and one
// that starts with a $; a . and a $ or % elsewhere are reached.
func TestCheckTarget(t *testing.T) {
	for name, want := range map[string]string{
		"": "session name required", "a.b": "", "proj/v1%2e2": "", "a$b": "", "%pct": "", "=eq": "", "semi;": "", "x.": "",
		"c:d": "has a :", ":x": "has a :", "x:": "has a :", "a.b:c": "has a :",
		"$0": "starts with a $", "$x": "starts with a $",
	} {
		err := CheckTarget(name)
		if want == "" && err != nil || want != "" && (err == nil || !strings.Contains(err.Error(), want)) {
			t.Errorf("%q: %v, want %q", name, err, want)
		}
	}
	if got := SessionTarget("a.b"); got != "=a.b:" {
		t.Errorf("SessionTarget(a.b) = %q", got)
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

// A branch with a #, a ;, a \, a $, a C1 control character, a line
// separator or a noncharacter gets a session with the name SessionName
// computed, its pane tagged: new-session expands a # in the name as a
// format, an argument that ends in ; splits the sequence there, tmux
// stores a \ in a session name doubled, tmux 3.2 to 3.4 store a $ before
// a letter, _ or { as \$, tmux 3.3 stores U+0085 as \302\205, and one
// built without utf8proc U+2028 as \342\200\250 and U+FFFE as
// \357\277\276. git takes no \ in a branch, but a detached worktree's
// directory name, encoded the same way, may have one.
func TestNewSessionEncodedNames(t *testing.T) {
	s := startManaged(t)
	ctx := context.Background()
	want := map[string]bool{}
	for _, branch := range []string{"fix#12", "x#{session_id}", "y##", "semi;", "a;b", `back\slash`, `end\`, "fix$HOME", "a${b}", "c1\xc2\x85x", "ls\u2028x", "nc\ufffex"} {
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
	// By line: strings.Fields would split a name at U+0085, which it
	// takes for a space.
	got := strings.Split(strings.TrimSuffix(string(out), "\n"), "\n")
	for _, name := range got {
		if !want[name] {
			t.Errorf("session %q, not a computed name", name)
		}
	}
	if len(got) != len(want) {
		t.Errorf("sessions %q, want %d", got, len(want))
	}
}

// CheckSessionName names the character tmux would not store as given,
// or Sep is made of, and takes every other one, a # and a ; at the end
// included. The name SessionName computes passes it for a branch with
// any byte or code point in it, a $, a C1 control character, a U+2063,
// a line or paragraph separator and a noncharacter among them, which
// EncodeBranch encodes.
func TestCheckSessionName(t *testing.T) {
	for name, want := range map[string]string{
		"":             "session name required",
		"a.b":          `session name "a.b" has a ., which tmux before 3.7 stores as _`,
		"a:b":          `has a :, which tmux before 3.7 stores as _ and a target splits at`,
		`a\b`:          `has a \, which tmux stores doubled`,
		"nul\x00":      "has the control character U+0000",
		"tab\tx":       "has the control character U+0009",
		"nl\nx":        "has the control character U+000A",
		"esc\x1bx":     "has the control character U+001B",
		"us\x1fx":      "has the control character U+001F",
		"del\x7fx":     "has the control character U+007F",
		"c1\u0085x":    "has the control character U+0085",
		"c1\u009fx":    "has the control character U+009F",
		"bad\xffx":     "has the byte 0xff, which is not UTF-8",
		"$x":           `"$x" has a $`,
		"a$b":          "has a $",
		"a$1":          "has a $",
		"a\u2063b":     "has the character U+2063",
		"a\u2028b":     "has the character U+2028, which tmux 3.3",
		"a\u2029b":     "has the character U+2029",
		"a\ufdd0b":     "has the character U+FDD0",
		"a\ufdefb":     "has the character U+FDEF",
		"a\ufffeb":     "has the character U+FFFE",
		"a\U0010ffffb": "has the character U+10FFFF",
	} {
		if err := CheckSessionName(name); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("CheckSessionName(%q) = %v, want %q", name, err, want)
		}
	}
	for _, name := range []string{"work", "notes draft", "notes##draft", "x#{session_id}y", "a#b", "#[fg=red]x", "semi;", ";", "a;b", "=eq", "%pct", "@at", "~tilde", "it's", `say "hi"`, "ø-norsk", "proj/x", "100%", "a\uFFFDb", "a\u00a0b", "a\u200bb", "a\ue000b", "a\U0001F600b", "cafe\u0301", "\u2764\ufe0f", "fix-\U0001FAE9", "a\u0378b", "a\U000ffdd0b"} {
		if err := CheckSessionName(name); err != nil {
			t.Errorf("CheckSessionName(%q) = %v", name, err)
		}
	}
	branches := []string{"fix#12", "x#{session_id}", "#[x]", "v1.2:rc", `a\b`, "semi;", "$x", "a$b", "c1\u0085x", "c1\u009f", "a\u2063b", "\u2063\u2063", "a\u2028b", "\u2029", "a\ufdd0b", "\ufdef", "a\ufffeb", "\U0010ffff", "ø-norsk", "a\U0001F600b"}
	for b := 0; b < 256; b++ {
		branches = append(branches, string([]byte{byte(b)}), "a"+string([]byte{byte(b)})+"b")
	}
	for _, branch := range branches {
		if err := CheckSessionName(SessionName("proj", branch)); err != nil {
			t.Errorf("branch %q: %v", branch, err)
		}
	}
	// And every code point, so CheckSessionName takes whatever
	// EncodeBranch keeps.
	for r, bad := rune(0), 0; r <= unicode.MaxRune && bad < 20; r++ {
		if err := CheckSessionName(SessionName("proj", string(r))); err != nil {
			t.Errorf("branch %q: %v", string(r), err)
			bad++
		}
	}
}

// A name laatmux new takes is the name of the session made, its pane
// tagged, and the name the commands after it find and kill it by: a #
// anywhere, which new-session expands as a format, so notes##draft was
// made as notes#draft and x#{session_id}y as xy, their tags finding no
// session; a space, a ; inside or at the end, a leading =, % or @,
// quotes and a letter that is not ASCII.
func TestNewSessionNamesAsGiven(t *testing.T) {
	s := startManaged(t)
	ctx := context.Background()
	names := []string{"notes draft", "notes##draft", "x#{session_id}y", "a#hb", "#[fg=red]x", "y##", "semi;", "a;b", "=eq", "%pct", "@at", "~tilde", "it's", `say "hi"`, "ø-norsk", "proj/x"}
	for _, name := range names {
		if err := CheckSessionName(name); err != nil {
			t.Fatal(err)
		}
		if _, err := s.NewSession(ctx, NewSessionOpts{Name: name, Cwd: t.TempDir(), Cmd: []string{"sleep", "600"}}); err != nil {
			t.Errorf("%q: %v", name, err)
			continue
		}
		if out, err := s.Run(ctx, "show-options", "-pqv", "-t", "="+name+":", "@laatmux_managed"); err != nil || strings.TrimSpace(string(out)) != "1" {
			t.Errorf("%q: pane tag %q %v", name, out, err)
		}
	}
	out, err := s.Run(ctx, "list-sessions", "-F", "#{session_name}")
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Split(strings.TrimRight(string(out), "\n"), "\n")
	slices.Sort(got)
	if want := slices.Sorted(slices.Values(names)); !slices.Equal(got, want) {
		t.Errorf("sessions %q, want %q", got, want)
	}
	for _, name := range names {
		if !s.HasSession(ctx, name) {
			t.Errorf("%q: not found by its name", name)
		}
		if _, err := s.Run(ctx, "kill-session", "-t", SessionTarget(name)); err != nil || s.HasSession(ctx, name) {
			t.Errorf("%q: kill %v, or still there", name, err)
		}
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

// An agent's arguments reach it as they were when the server's default
// shell is a zsh with extended_glob, as a ~/.zshenv may set: HEAD^ is
// an argument an agent may take, a bare a^b is every file in the root
// that starts with a and is not ab and aborts the line when there is
// none, and a bare ^missing is every file in the root but missing.
func TestNewSessionArgvThroughExtendedGlob(t *testing.T) {
	zsh, err := exec.LookPath("zsh")
	if err != nil {
		t.Skip("no zsh on PATH")
	}
	s := startManaged(t)
	ctx := context.Background()
	dir := t.TempDir()
	// The pane runs its command as the default shell's -c; -f keeps
	// the user's startup files out. The wrapper leaves a mark, so the
	// test fails rather than passes when another shell read the line:
	// tmux runs /bin/sh in place of a default shell it cannot execute,
	// and a server started anew has the shell SHELL names. zsh's errors
	// go to a file: the pane of a line zsh refused is gone with them.
	shell := filepath.Join(dir, "zsh-extended-glob")
	if err := os.WriteFile(shell, []byte("#!/bin/sh\n: > \"$0.ran\"\nexec "+shellJoin([]string{zsh})+" -f -o extended_glob \"$@\" 2>> \"$0.err\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Run(ctx, "set-option", "-g", "default-shell", shell); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "argv")
	words := []string{"HEAD^", "a^b", "^missing"}
	cmd := append([]string{"sh", "-c", `printf '%s\0' "$@" > "$0.tmp" && mv "$0.tmp" "$0"; exec sleep 600`, file}, words...)
	if _, err := s.NewSession(ctx, NewSessionOpts{Name: "proj/caret", Cwd: dir, Cmd: cmd}); err != nil {
		t.Fatal(err)
	}
	var out []byte
	for i := 0; i < 500 && out == nil; i++ {
		out, _ = os.ReadFile(file)
		time.Sleep(10 * time.Millisecond)
	}
	if out == nil {
		msg, _ := os.ReadFile(shell + ".err")
		t.Fatalf("the pane wrote no arguments: the shell refused the line: %s", msg)
	}
	if _, err := os.Stat(shell + ".ran"); err != nil {
		t.Fatalf("the default shell was not the wrapper: %v", err)
	}
	if got := strings.Split(strings.TrimSuffix(string(out), "\x00"), "\x00"); !slices.Equal(got, words) {
		t.Errorf("the agent got %q, want %q", got, words)
	}
}

// A root with a $ in it reads back as given, as its pane's path and
// its @laatmux_cwd, through ListPanes and Query: tmux 3.4 prints a $
// before a letter, _ or { back as \$ and a backslash of the value as it
// is, so `a\$b` prints as `a\\$b` and `c\$1` as given, and tmux 3.4 on
// macOS takes é, and the first byte of the Sep after a last value that
// ends in $, for a letter there too; tmux 3.5 and later print every
// value as given, `a\$b` included. A failed Query names the command as
// the caller gave it.
func TestListPanesRootWithDollar(t *testing.T) {
	s := startManaged(t)
	ctx := context.Background()
	for _, leaf := range []string{"fix$foo", `a\$b`, `c\$1`, "d${e}$_f", "e$é", "g$", `h\$`} {
		root := filepath.Join(t.TempDir(), "proj", leaf)
		if err := os.MkdirAll(root, 0o755); err != nil {
			t.Fatal(err)
		}
		real, err := filepath.EvalSymlinks(root)
		if err != nil {
			t.Fatal(err)
		}
		name := SessionName("proj", leaf)
		if _, err := s.NewSession(ctx, NewSessionOpts{Name: name, Cwd: root, Cmd: []string{"sleep", "600"}}); err != nil {
			t.Fatalf("%s: %v", leaf, err)
		}
		// The pane's path is read from its process, which may not have
		// changed directory yet.
		var got Pane
		for i := 0; i < 200 && got.CurrentPath != real; i++ {
			panes, err := s.ListPanes(ctx)
			if err != nil {
				t.Fatal(err)
			}
			for _, p := range panes {
				if p.Session == name {
					got = p
				}
			}
			time.Sleep(10 * time.Millisecond)
		}
		if got.Cwd != root || got.CurrentPath != real {
			t.Errorf("%s: pane @laatmux_cwd %q, path %q; want %q, %q", leaf, got.Cwd, got.CurrentPath, root, real)
		}
		if out, err := s.Query(ctx, "#{@laatmux_cwd}", "display-message", "-p", "-t", "="+name+":"); err != nil || string(out) != root+"\n" {
			t.Errorf("%s: display-message @laatmux_cwd %q %v, want %q", leaf, out, err, root)
		}
	}
	_, err := s.Query(ctx, "#{@laatmux_cwd}", "list-panes", "-t", "=nosuch:")
	if err == nil || !strings.HasPrefix(err.Error(), "tmux list-panes -t =nosuch: -F #{@laatmux_cwd}: ") {
		t.Errorf("failed query: %v", err)
	}
}

// unescapeDollar undoes the backslash tmux 3.4 prints before a $ that
// comes before a letter, _ or {, and keeps every other backslash: tmux
// prints a backslash of the value as it is, so `\\$a` is the value
// `\$a`, and puts none before a $ at the end or before a digit, a
// newline or another $. A \$ before an escaped byte, \351, is kept:
// tmux 3.4 on macOS prints the value $ and a lone 0xe9 so, and so the
// value `\$\351`, which is #214's. Which bytes above 0x7f are letters
// there is the C library's: on macOS the Latin-1 letters, the first
// byte of é and of U+2063 among them but not that of U+05D0; on Linux
// none. Beside the table, every string of up to four pieces from a set
// of them is printed as tmux 3.4 prints it and read back.
func TestUnescapeDollar(t *testing.T) {
	mac := runtime.GOOS == "darwin"
	for _, c := range []struct {
		printed, value string
		macOnly        bool
	}{
		{`fix\$foo`, `fix$foo`, false},
		{`\$_x\${y}\$Z`, `$_x${y}$Z`, false},
		{`a\\$b`, `a\$b`, false},
		{`c\$1`, `c\$1`, false},
		{`end\$`, `end\$`, false},
		{"\\$\n\\$a", "\\$\n$a", false},
		{`$\$a`, `$$a`, false},
		{`\\\$a`, `\\$a`, false},
		{`\377\$a`, `\377$a`, false},
		{`\$\351`, `\$\351`, false},
		{`\$א`, `\$א`, false},
		{`\$é`, `$é`, true},
		{`a\$` + Sep + "b", "a$" + Sep + "b", true},
	} {
		want := c.value
		if c.macOnly && !mac {
			want = c.printed
		}
		if got := unescapeDollar(c.printed); got != want {
			t.Errorf("%q: %q, want %q", c.printed, got, want)
		}
	}
	dollarValues(func(v string) {
		if got := unescapeDollar(print34(v)); got != v {
			t.Errorf("%q printed %q reads back %q", v, print34(v), got)
		}
	})
}

// print34 is what tmux 3.4 prints for a line of whole UTF-8 characters
// with no byte it escapes otherwise: a backslash before every $ that
// comes before a byte dollarLetter takes.
func print34(v string) string {
	var b strings.Builder
	for i := 0; i < len(v); i++ {
		if v[i] == '$' && i+1 < len(v) && dollarLetter(v[i+1]) {
			b.WriteByte('\\')
		}
		b.WriteByte(v[i])
	}
	return b.String()
}

// dollarValues calls f with every string of up to four pieces: a $, a
// backslash, a letter, a digit, an _, a {, é, whose first byte macOS
// takes for a letter, א, whose first byte it does not, and a newline.
func dollarValues(f func(v string)) {
	pieces := []string{"$", `\`, "a", "1", "_", "{", "é", "א", "\n"}
	var walk func(v string, n int)
	walk = func(v string, n int) {
		f(v)
		if n > 0 {
			for _, p := range pieces {
				walk(v+p, n-1)
			}
		}
	}
	walk("", 4)
}

// unframe gives a Query's output as the server holds the values: each
// record's Sep and end dropped, and a record's backslashes undone when
// its own probe was printed escaped. A record ends at its end, not at
// a newline: a value with a newline, or with Sep and the probe without
// its tail, is read whole and decoded whole. What comes after the last
// record is left as printed: what a user's after-hook printed, as
// display-message -p in an after-list-panes hook does. A record is
// decoded before its end is dropped: a value that ends in $ is
// followed by Sep, whose first byte macOS takes for a letter, so tmux
// 3.4 there escapes that $. Beside the table, every value from
// dollarValues as the last field reads back as given from a server
// that prints it as given, and from one that prints it as tmux 3.4
// does, a newline in it or not.
func TestUnframe(t *testing.T) {
	end := probe + "TAIL"
	esc := Sep + `\` + end + "\n"
	for _, c := range []struct{ out, want string }{
		{"", ""},
		{`a\$b` + Sep + end + "\nc" + Sep + end + "\n", "a\\$b\nc\n"},
		{`a\$b` + esc + `c\\$d` + esc, "a$b\n" + `c\$d` + "\n"},
		{`a\$b` + esc + "hook \\$x\n", "a$b\nhook \\$x\n"},
		{"x" + esc + `y\$a` + Sep + end + "\n", "x\n" + `y\$a` + "\n"},
		{`a\$b` + "\n" + `c\$d` + esc, "a$b\nc$d\n"},
		{"p" + Sep + probe + "\nq" + Sep + end + "\n", "p" + Sep + probe + "\nq\n"},
		{"p" + Sep + `\` + probe + "\nq" + Sep + end + "\n", "p" + Sep + `\` + probe + "\nq\n"},
		{"p" + Sep + `\$_` + "\nq" + esc, "p" + Sep + probe + "\nq\n"},
	} {
		if got := unframe(c.out, end); got != c.want {
			t.Errorf("%q: %q, want %q", c.out, got, c.want)
		}
	}
	dollarValues(func(v string) {
		if got := unframe(print34(v+Sep+end)+"\n", end); got != v+"\n" {
			t.Errorf("%q printed by tmux 3.4 as %q reads back %q", v, print34(v+Sep+end), got)
		}
		if got := unframe(v+Sep+end+"\n", end); got != v+"\n" {
			t.Errorf("%q printed as given reads back %q", v, got)
		}
	})
}

// dollarLetter takes the bytes tmux 3.4's isalpha takes beside _ and {:
// the ASCII letters everywhere, and on macOS the Latin-1 letters, which
// is what its C library takes in the UTF-8 locale tmux sets; glibc and
// musl take no byte above 0x7f.
func TestDollarLetter(t *testing.T) {
	for b := 0; b < 256; b++ {
		want := b == '_' || b == '{' || 'a' <= b && b <= 'z' || 'A' <= b && b <= 'Z'
		if runtime.GOOS == "darwin" && (b == 0xaa || b == 0xb5 || b == 0xba || b >= 0xc0 && b != 0xd7 && b != 0xf7) {
			want = true
		}
		if got := dollarLetter(byte(b)); got != want {
			t.Errorf("%#x: %v, want %v", b, got, want)
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
// last ;, the bare line's arguments as well; the attach line's target
// ends in : whatever the name, and the selector gets none either, and
// the caller's slice is left as it is. An error names a Next as the ;
// it stands for. The new-session test below checks with tmux.
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
	if got := LaatmuxServer.AttachArgsBare("x;"); !slices.Equal(got, []string{"-L", "laatmux", "attach-session", "-t", `=x;:`}) {
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
