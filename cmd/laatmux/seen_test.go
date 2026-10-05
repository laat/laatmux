package main

import (
	"testing"

	"github.com/laat/laatmux/internal/daemon"
	"github.com/laat/laatmux/internal/tmux"
)

// A focused sidebar pane stands for the window's live attach pane, else
// its one other pane; a dead attach pane is not live, so a window with
// one and a shell has two other panes, and stands for none.
func TestPickBeside(t *testing.T) {
	line := func(f ...string) string {
		out := f[0]
		for _, s := range f[1:] {
			out += tmux.Sep + s
		}
		return out + "\n"
	}
	side := line("%1", "0", "1", "", "")
	for _, c := range []struct {
		name, out string
		want      daemon.ClientView
	}{
		{"attach", side + line("%2", "0", "", "1", "proj/x"), daemon.ClientView{Pane: "%2", AttachPane: true, Target: "proj/x"}},
		{"shell", side + line("%3", "0", "", "", ""), daemon.ClientView{Pane: "%3"}},
		{"dead attach", side + line("%2", "1", "", "1", "proj/x") + line("%3", "0", "", "", ""), daemon.ClientView{}},
		{"attach among two", side + line("%3", "0", "", "", "") + line("%2", "0", "", "1", "proj/x"), daemon.ClientView{Pane: "%2", AttachPane: true, Target: "proj/x"}},
		{"alone", side, daemon.ClientView{}},
	} {
		if got := pickBeside(c.out); got != c.want {
			t.Errorf("%s: %+v, want %+v", c.name, got, c.want)
		}
	}
}

// The github line trusts only an answer with a viewer: a body with data
// null and errors, which gh returns with exit 0, is the error.
func TestViewerStatus(t *testing.T) {
	for body, want := range map[string]string{
		`{"data":{"viewer":{"login":"laat"}}}`:                           "ok",
		`{"data":null,"errors":[{"message":"API rate limit exceeded"}]}`: "gh api graphql: API rate limit exceeded",
		`{"data":{"viewer":null}}`:                                       "gh api graphql: no viewer",
	} {
		if got := viewerStatus([]byte(body)); got != want {
			t.Errorf("%s: %q, want %q", body, got, want)
		}
	}
}
