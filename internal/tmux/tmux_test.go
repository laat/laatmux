package tmux

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
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

// Review: the zero Server follows the inherited TMUX variable, so a daemon
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
	}
	for in, want := range cases {
		got := EncodeBranch(in)
		if got != want {
			t.Errorf("EncodeBranch(%q) = %q, want %q", in, got, want)
		}
		if back := DecodeBranch(got); back != in {
			t.Errorf("DecodeBranch(%q) = %q, want %q", got, back, in)
		}
		if strings.ContainsAny(got, ".:") {
			t.Errorf("EncodeBranch(%q) = %q contains a character tmux rejects", in, got)
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
