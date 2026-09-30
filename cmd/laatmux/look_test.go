package main

import (
	"strings"
	"testing"

	"github.com/laat/laatmux/internal/config"
	"github.com/laat/laatmux/internal/palette"
)

// The theme: NO_COLOR draws with the attributes alone and asks nothing;
// dark and light are taken as set without asking; auto asks, and takes
// the dark defaults when the terminal does not say. The icons come from
// the config as they are.
func TestLook(t *testing.T) {
	dark, _ := palette.New(true, nil)
	light, _ := palette.New(false, nil)
	for _, c := range []struct {
		name, mode, noColor string
		answer, answered    bool
		want                palette.Theme
		asks                bool
	}{
		{"no colour", "", "1", false, false, palette.Mono(), false},
		{"dark", "dark", "", false, false, dark, false},
		{"light", "light", "", false, false, light, false},
		{"auto, dark answer", "auto", "", true, true, dark, true},
		{"auto, light answer", "", "", false, true, light, true},
		{"auto, no answer", "auto", "", false, false, dark, true},
	} {
		t.Setenv("NO_COLOR", c.noColor)
		t.Setenv("COLORFGBG", "")
		asked := false
		cfg := config.Config{Theme: config.Theme{Mode: c.mode}, Icons: "ascii", StatusIcons: map[string]string{"waiting": "?"}}
		th, icons := lookWith(cfg, func() (bool, bool) { asked = true; return c.answer, c.answered })
		if asked != c.asks {
			t.Errorf("%s: asked %v", c.name, asked)
		}
		if th.SGR(palette.Text, false) != c.want.SGR(palette.Text, false) || th.Mono != c.want.Mono {
			t.Errorf("%s: wrong theme", c.name)
		}
		if guessed := c.asks && !c.answered; th.Guessed != guessed {
			t.Errorf("%s: guessed %v", c.name, th.Guessed)
		}
		if icons.Set != "ascii" || icons.Waiting != "?" {
			t.Errorf("%s: icons %+v", c.name, icons)
		}
	}
}

// A band whose colours the config sets itself is drawn without an
// answer: the colours do not depend on the background.
func TestLookCustomBand(t *testing.T) {
	t.Setenv("NO_COLOR", "")
	t.Setenv("COLORFGBG", "")
	no := func() (bool, bool) { return false, false }
	both := config.Config{Theme: config.Theme{Custom: map[string]string{"highlight_row_bg": "#303030", "text": "#e0e0e0"}}}
	if th, _ := lookWith(both, no); th.Guessed {
		t.Error("both set: guessed")
	}
	one := config.Config{Theme: config.Theme{Custom: map[string]string{"highlight_row_bg": "#303030"}}}
	if th, _ := lookWith(one, no); !th.Guessed {
		t.Error("one set: not guessed")
	}
}

// COLORFGBG says the background when the terminal does not answer: a
// light one is taken, and the theme is not a guess.
func TestLookColorFgBg(t *testing.T) {
	t.Setenv("NO_COLOR", "")
	t.Setenv("COLORFGBG", "0;15")
	light, _ := palette.New(false, nil)
	th, _ := lookWith(config.Config{}, func() (bool, bool) { return false, false })
	if th.Guessed || th.SGR(palette.Text, false) != light.SGR(palette.Text, false) {
		t.Errorf("COLORFGBG light: guessed %v", th.Guessed)
	}
	for v, want := range map[string]bool{"15;0": true, "0;15": false, "7;default;0": true, "0;7": false, "15;8": true} {
		if dark, ok := colorFgBg(v); !ok || dark != want {
			t.Errorf("%q: dark %v ok %v", v, dark, ok)
		}
	}
	for _, bad := range []string{"", "15", "x;y", "0;16"} {
		if _, ok := colorFgBg(bad); ok {
			t.Errorf("%q read", bad)
		}
	}
}

// The templates: the dashboard's defaults carry the git and PR
// columns, the sidebar's do not, and a configured line is the same in
// both.
func TestTemplatesPerHost(t *testing.T) {
	var cfg config.Config
	side, dash := templates(cfg, false), templates(cfg, true)
	if side.Tiles[2].Source() == dash.Tiles[2].Source() || !strings.Contains(dash.Tiles[2].Source(), "{pr_state}") || strings.Contains(side.Tiles[2].Source(), "{pr_state}") {
		t.Errorf("tile 3: sidebar %q, dashboard %q", side.Tiles[2].Source(), dash.Tiles[2].Source())
	}
	if !strings.Contains(dash.Tree.Worktree.Source(), "{git_sync}") || strings.Contains(side.Tree.Worktree.Source(), "{git_sync}") {
		t.Errorf("worktree: sidebar %q, dashboard %q", side.Tree.Worktree.Source(), dash.Tree.Worktree.Source())
	}
	cfg.Sidebar.Templates.Tiles = []string{"{primary}", "{host}", "{pr_number}"}
	cfg.Sidebar.Templates.Tree.Worktree = "{repo}"
	side, dash = templates(cfg, false), templates(cfg, true)
	if side.Tiles[2].Source() != "{pr_number}" || dash.Tiles[2].Source() != "{pr_number}" || dash.Tree.Worktree.Source() != "{repo}" {
		t.Errorf("configured lines: sidebar %q, dashboard %q %q", side.Tiles[2].Source(), dash.Tiles[2].Source(), dash.Tree.Worktree.Source())
	}
}
