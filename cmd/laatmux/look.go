package main

import (
	"os"
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
	icons := view.Icons{Set: cfg.Icons, Working: cfg.StatusIcons["working"], Waiting: cfg.StatusIcons["waiting"],
		Done: cfg.StatusIcons["done"], Stale: cfg.StatusIcons["stale"]}
	if os.Getenv("NO_COLOR") != "" {
		return palette.Mono(), icons
	}
	dark := true
	switch cfg.Theme.Mode {
	case palette.ModeLight:
		dark = false
	case palette.ModeDark:
	default:
		if d, ok := t.Background(backgroundWait); ok {
			dark = d
		}
	}
	th, err := palette.New(dark, cfg.Theme.Custom)
	if err != nil {
		return palette.Mono(), icons
	}
	return th, icons
}
