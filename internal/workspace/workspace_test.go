package workspace

import (
	"slices"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"

	"github.com/laat/laatmux/internal/peer"
	"github.com/laat/laatmux/internal/protocol"
	"github.com/laat/laatmux/internal/tmux"
)

func TestSessionName(t *testing.T) {
	if got := SessionName("vm", "proj", "fix/v1.2"); got != "vm/proj/fix/v1%2e2" {
		t.Fatalf("SessionName = %q", got)
	}
}

func TestParseSessions(t *testing.T) {
	sep := tmux.Sep
	out := strings.Join([]string{
		strings.Join([]string{"vm/proj/fix", "env1/root/a", "vm", "", "1", "git@x:o/proj.git", "fix"}, sep),
		strings.Join([]string{"mac/work", "", "mac", "mac/work", "", "", ""}, sep),
		strings.Join([]string{"notes", "", "", "", "", "", ""}, sep),
		"",
	}, "\n")
	locals := parseSessions(out)
	if len(locals) != 3 {
		t.Fatalf("got %d sessions: %+v", len(locals), locals)
	}
	ws := locals[0]
	if !ws.Workspace() || ws.Key != "env1/root/a" || ws.Host != "vm" || !ws.Settled || ws.Source != "git@x:o/proj.git" || ws.Branch != "fix" {
		t.Errorf("workspace session parsed as %+v", ws)
	}
	// Published: the workspace and the plain attachment, not the
	// user's own session.
	if recs := Records(locals); len(recs) != 2 || recs[0].Name != "vm/proj/fix" || recs[1].Name != "mac/work" {
		t.Errorf("Records: %+v", recs)
	}
	// Found by identity tags whatever the name, and not across hosts.
	if l, ok := FindWorktree(locals, "env1", "git@x:o/proj.git", "fix"); !ok || l.Name != "vm/proj/fix" {
		t.Errorf("FindWorktree: %+v %v", l, ok)
	}
	if _, ok := FindWorktree(locals, "env2", "git@x:o/proj.git", "fix"); ok {
		t.Error("FindWorktree matched another environment")
	}
	if _, ok := FindWorktree(locals, "env1", "", ""); ok {
		t.Error("FindWorktree matched an untagged session")
	}
	if at := locals[1]; at.Workspace() || at.Attach != "mac/work" || at.Settled {
		t.Errorf("attach session parsed as %+v", at)
	}
	if plain := locals[2]; plain.Workspace() || plain.Attach != "" || plain.Host != "" {
		t.Errorf("plain session parsed as %+v", plain)
	}
	if l, ok := Find(locals, "env1/root/a", ""); !ok || l.Name != "vm/proj/fix" {
		t.Errorf("Find by key: %+v %v", l, ok)
	}
	if l, ok := Find(locals, "", "mac/work"); !ok || l.Name != "mac/work" {
		t.Errorf("Find by attach: %+v %v", l, ok)
	}
	if _, ok := Find(locals, "", ""); ok {
		t.Error("Find with nothing to match found something")
	}
	if _, ok := ByName(locals, "vm/proj"); ok {
		t.Error("ByName matched a prefix")
	}
}

// A key is stored with its root encoded after a % where the root has a
// byte tmux 3.4 prints escaped, a tab, a newline, a C1 control
// character, a U+2063 or a %, so the value has none of them and reads
// back as the key; any other key is stored as given. A value with a /
// after the environment id is never decoded, so one an earlier build
// wrote for a root with %01 in it still reads as that root's key.
func TestKeyStoredEncoded(t *testing.T) {
	for root, want := range map[string]string{
		"/w/proj/fix/v1.2":                     "env//w/proj/fix/v1.2",
		`/w/a\b$x#y;`:                          `env//w/a\b$x#y;`,
		"/w/bl\u00e5b\u00e6r/\U0001f600\ufffd": "env//w/bl\u00e5b\u00e6r/\U0001f600\ufffd",
		"/w/nb\u00a0x\u2064y\u2028":            "env//w/nb\u00a0x\u2064y\u2028",
		"/w/a\x01b":                            "env%/w/a%01b",
		"/w/tab\tx":                            "env%/w/tab%09x",
		"/w/nl\nx":                             "env%/w/nl%0ax",
		"/w/del\x7f":                           "env%/w/del%7f",
		"/w/a\xffb":                            "env%/w/a%ffb",
		"/w/c1\u0085x":                         "env%/w/c1%c2%85x",
		"/w/sep\u2063\u2063x":                  "env%/w/sep%e2%81%a3%e2%81%a3x",
		"/w/100%":                              "env%/w/100%25",
		"/w/a%01b":                             "env%/w/a%2501b",
		"/w/\xe2\x82":                          "env%/w/%e2%82",
	} {
		key := protocol.SessionKey("env", root)
		v := encodeKey(key)
		if v != want {
			t.Errorf("encodeKey(%q) = %q, want %q", key, v, want)
		}
		if back := DecodeKey(v); back != key {
			t.Errorf("DecodeKey(%q) = %q, want %q", v, back, key)
		}
	}
	// Every byte alone, and every code point, round trips through a
	// value that is UTF-8 with no control character and no U+2063.
	check := func(root string) {
		key := protocol.SessionKey("env", root)
		v := encodeKey(key)
		if back := DecodeKey(v); back != key {
			t.Errorf("encodeKey(%q) = %q, decoded %q", key, v, back)
		}
		if !utf8.ValidString(v) || strings.IndexFunc(v, func(r rune) bool { return unicode.IsControl(r) || r == '\u2063' }) >= 0 {
			t.Errorf("encodeKey(%q) = %q", key, v)
		}
	}
	for c := 0; c < 256; c++ {
		check("/w/" + string([]byte{byte(c)}))
	}
	for r := rune(0); r <= unicode.MaxRune; r++ {
		check("/w/" + string(r))
	}
	// A value with a / is the key as it is, a %01 or a byte in it too;
	// one with a % has the root decoded, a % before no two hex digits
	// kept. parseSessions reads the key so.
	for v, key := range map[string]string{
		"env//w/a%01b":  "env//w/a%01b",
		"env//w/a\x01b": "env//w/a\x01b",
		"env1/root/a":   "env1/root/a",
		"env%/w/a%01b":  "env//w/a\x01b",
		"env%/w/a%1":    "env//w/a%1",
		"env%/w/a%zzb":  "env//w/a%zzb",
		"env%/w/a%FFb":  "env//w/a\xffb",
		"env":           "env",
		"":              "",
	} {
		if got := DecodeKey(v); got != key {
			t.Errorf("DecodeKey(%q) = %q, want %q", v, got, key)
		}
		line := strings.Join([]string{"s", v, "mac", "", "", "", ""}, tmux.Sep)
		if locals := parseSessions(line + "\n"); len(locals) != 1 || locals[0].Key != key {
			t.Errorf("%q parsed as %+v, want key %q", v, locals, key)
		}
	}
}

// The remote shell command passes the root through as one argument
// whatever it contains, and $SHELL is left for the remote side to expand.
func TestShellCommand(t *testing.T) {
	h := peer.Host{Name: "vm", SSH: "vm"}
	got := ShellCommand(h, "/home/u/src/worktrees/proj/it's here")
	want := `ssh -t vm 'cd '\''/home/u/src/worktrees/proj/it'\''\'\'''\''s here'\'' && exec "$SHELL" -l'`
	if got != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
	if got, want := AttachCommand(h, "proj/x"), "ssh -t -o 'ServerAliveInterval=15' -o 'ServerAliveCountMax=3' vm 'tmux -u -L laatmux attach-session -t '\\''=proj/x'\\'''"; got != want {
		t.Fatalf("remote attach:\n got %s\nwant %s", got, want)
	}
	if !strings.Contains(AttachCommand(h, "proj/x"), "ssh -t") || strings.Contains(AttachCommand(peer.Host{Name: "mac"}, "proj/x"), "ssh") {
		t.Error("AttachCommand picked the wrong transport")
	}
}

// A reuse that does not know the source or branch leaves the session's
// tags alone rather than clearing them. The tags are one sequence, its
// commands separated by tmux.Next, so a branch that is ; is a value.
func TestTagArgsPreserveUnknownIdentity(t *testing.T) {
	full := tagArgs("s", Spec{Host: peer.Host{Name: "vm"}, Key: "k", Source: "src", Branch: ";"})
	want := []string{"set-option", "-t", "=s:", "@laatmux_host", "vm",
		tmux.Next, "set-option", "-t", "=s:", "@laatmux_repo", "src",
		tmux.Next, "set-option", "-t", "=s:", "@laatmux_branch", ";"}
	if !slices.Equal(full, want) {
		t.Errorf("full spec: %q, want %q", full, want)
	}
	partial := strings.Join(tagArgs("s", Spec{Host: peer.Host{Name: "vm"}, Key: "k", Branch: "b"}), " ")
	if strings.Contains(partial, "@laatmux_repo") || !strings.Contains(partial, "@laatmux_branch b") || !strings.Contains(partial, "@laatmux_host vm") {
		t.Errorf("partial spec wrote an empty source or dropped the rest: %q", partial)
	}
	if plain := strings.Join(tagArgs("s", Spec{Host: peer.Host{Name: "vm"}, Source: "src"}), " "); strings.Contains(plain, "@laatmux_repo") {
		t.Errorf("plain attachment got identity tags: %q", plain)
	}
}

func TestAttachHintSelectsDefaultServer(t *testing.T) {
	if got := AttachHint("vm/proj/x"); got != `tmux -L default attach-session -t '=vm/proj/x'` {
		t.Fatalf("AttachHint = %s", got)
	}
}
