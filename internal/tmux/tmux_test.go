package tmux

import "testing"

func TestShellJoin(t *testing.T) {
	got := shellJoin([]string{"claude", "--flag", "a b", "it's"})
	want := `claude --flag 'a b' 'it'\''s'`
	if got != want {
		t.Fatalf("got %s want %s", got, want)
	}
}
