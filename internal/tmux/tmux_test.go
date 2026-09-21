package tmux

import "testing"

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
