package main

import (
	"slices"
	"strings"
	"testing"
)

// The new command line: flags before or after the name, the command
// after it; a name tmux would not keep as given is refused before a
// host is dialled, its character named.
func TestParseNewArgs(t *testing.T) {
	for _, c := range []struct {
		args []string
		want newArgs
	}{
		{[]string{"work", "--cwd", "/tmp", "--", "claude", "--flag"}, newArgs{name: "work", cwd: "/tmp", cmd: []string{"claude", "--flag"}}},
		{[]string{"--host", "vm", "notes draft;", "--cwd", "/tmp"}, newArgs{name: "notes draft;", host: "vm", cwd: "/tmp"}},
		{[]string{"--cwd", "/tmp", "ø-norsk", "sleep", "600"}, newArgs{name: "ø-norsk", cwd: "/tmp", cmd: []string{"sleep", "600"}}},
		{[]string{"notes##draft", "--cwd", "/tmp"}, newArgs{name: "notes##draft", cwd: "/tmp"}},
	} {
		got, err := parseNewArgs(c.args)
		if err != nil || got.name != c.want.name || got.host != c.want.host || got.cwd != c.want.cwd || !slices.Equal(got.cmd, c.want.cmd) {
			t.Errorf("%q: got %+v %v, want %+v", c.args, got, err, c.want)
		}
	}
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"a.b", "--cwd", "/tmp"}, `session name "a.b" has a .`},
		{[]string{"--cwd", "/tmp", "a:b"}, "has a :"},
		{[]string{`a\b`, "--cwd", "/tmp"}, `has a \`},
		{[]string{"tab\tx", "--cwd", "/tmp"}, "has the control character U+0009"},
		{[]string{"bad\xffx", "--cwd", "/tmp"}, "has the byte 0xff"},
		{[]string{"a$b", "--cwd", "/tmp"}, "has a $"},
		{[]string{"", "--cwd", "/tmp"}, "session name required"},
		{[]string{"work"}, "--cwd is required"},
		{nil, "usage: laatmux new"},
	} {
		if got, err := parseNewArgs(c.args); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%q: got %+v %v, want an error with %q", c.args, got, err, c.want)
		}
	}
}
