package main

import (
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/laat/laatmux/internal/config"
	"github.com/laat/laatmux/internal/palette"
	"github.com/laat/laatmux/internal/view"
)

// backgroundWait is how long a view waits for the terminal's answer to
// the background query before it takes the dark theme.
const backgroundWait = 150 * time.Millisecond

// look is the theme and the icons a view draws with, from the config.
// NO_COLOR, set to anything, draws with the attributes alone; a mode of
// auto asks the terminal for its background, dark when it does not say.
// The config was validated when it was read, so the custom colours
// parse.
func look(cfg config.Config, t *view.Term) (palette.Theme, view.Icons) {
	return lookWith(cfg, func() (bool, bool) { return t.Background(backgroundWait) })
}

// templates compiles the config's line templates, the defaults for what
// it leaves out; a template that does not parse draws its error in the
// view rather than failing the pane.
func templates(cfg config.Config) view.Templates {
	t := cfg.Sidebar.Templates
	return view.CompileTemplates(t.Tiles, t.Compact, t.Top, t.Tree.Repo, t.Tree.Worktree, t.Tree.Agent, t.Tree.Pane, t.Tree.Run)
}

// agentIcons is the config's agent icons as the view keeps them.
func agentIcons(cfg config.Config) map[string]view.AgentIcon {
	if len(cfg.AgentIcons) == 0 {
		return nil
	}
	out := make(map[string]view.AgentIcon, len(cfg.AgentIcons))
	for name, a := range cfg.AgentIcons {
		out[name] = view.AgentIcon{Icon: a.Icon, Color: a.Color}
	}
	return out
}

// lookWith is look with the terminal's background asked through
// background, which only a mode of auto calls.
func lookWith(cfg config.Config, background func() (dark, ok bool)) (palette.Theme, view.Icons) {
	icons := view.Icons{Set: cfg.Icons, Working: cfg.StatusIcons["working"], Waiting: cfg.StatusIcons["waiting"],
		Done: cfg.StatusIcons["done"], Stale: cfg.StatusIcons["stale"]}
	if os.Getenv("NO_COLOR") != "" {
		return palette.Mono(), icons
	}
	dark, known := true, true
	switch cfg.Theme.Mode {
	case palette.ModeLight:
		dark = false
	case palette.ModeDark:
	default:
		// The terminal's answer, else COLORFGBG as some terminals and
		// shells set it; a tmux popup gets no answer to the query.
		if d, ok := background(); ok {
			dark = d
		} else if d, ok := colorFgBg(os.Getenv("COLORFGBG")); ok {
			dark = d
		} else {
			known = false
		}
	}
	th, err := palette.New(dark, cfg.Theme.Custom)
	if err != nil {
		return palette.Mono(), icons
	}
	// Band colours the config sets itself do not depend on the
	// background: they are used whatever it is.
	_, bg := cfg.Theme.Custom[palette.HighlightRowBg]
	_, fg := cfg.Theme.Custom[palette.Text]
	th.Guessed = !known && !(bg && fg)
	return th, icons
}

// colorFgBg reads COLORFGBG, `fg;bg` or `fg;default;bg` with ANSI colour
// numbers: a background of 0 to 6 or 8 is dark, 7 and 9 to 15 light.
func colorFgBg(v string) (dark, ok bool) {
	parts := strings.Split(v, ";")
	if len(parts) < 2 {
		return false, false
	}
	bg, err := strconv.Atoi(parts[len(parts)-1])
	if err != nil || bg < 0 || bg > 15 {
		return false, false
	}
	return bg <= 6 || bg == 8, true
}
