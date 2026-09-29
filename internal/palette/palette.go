// Package palette is the named colours the views draw with, their dark
// and light defaults, and the colour syntax the config takes. The views
// name colours; only the terminal encoding looks them up, so a golden
// test shows a colour by its name and a theme can change it.
package palette

import (
	"fmt"
	"strconv"
	"strings"
)

// The palette, as milestone five's note names it.
const (
	Info              = "info"
	Accent            = "accent"
	Success           = "success"
	Warning           = "warning"
	Danger            = "danger"
	Dimmed            = "dimmed"
	Text              = "text"
	Border            = "border"
	Header            = "header"
	HighlightRowBg    = "highlight_row_bg"
	CurrentWorktreeFg = "current_worktree_fg"
)

// Names is every palette name, in the order the note lists them.
var Names = []string{Info, Accent, Success, Warning, Danger, Dimmed, Text, Border, Header, HighlightRowBg, CurrentWorktreeFg}

// Known reports whether name is a palette name.
func Known(name string) bool {
	for _, n := range Names {
		if n == name {
			return true
		}
	}
	return false
}

// Color is a colour a terminal can show: 24-bit, or one of the 256
// indexed colours.
type Color struct {
	R, G, B uint8
	Index   int  // 0..255 when !RGB
	RGB     bool // R, G and B hold the colour
}

// Parse reads a colour as the config writes it: `#rrggbb`, a number
// from 0 to 255, or tmux's `colour123` and `color123`.
func Parse(s string) (Color, error) {
	t := strings.TrimSpace(s)
	if strings.HasPrefix(t, "#") {
		if len(t) != 7 {
			return Color{}, fmt.Errorf("colour %q is not #rrggbb", s)
		}
		v, err := strconv.ParseUint(t[1:], 16, 32)
		if err != nil {
			return Color{}, fmt.Errorf("colour %q is not #rrggbb", s)
		}
		return Color{R: uint8(v >> 16), G: uint8(v >> 8), B: uint8(v), RGB: true}, nil
	}
	n := strings.TrimPrefix(strings.TrimPrefix(t, "colour"), "color")
	i, err := strconv.Atoi(n)
	if err != nil || i < 0 || i > 255 {
		return Color{}, fmt.Errorf("colour %q is not #rrggbb, 0 to 255, or colourN", s)
	}
	return Color{Index: i}, nil
}

func hex(s string) Color {
	c, err := Parse(s)
	if err != nil {
		panic(err)
	}
	return c
}

// Dark and Light are the defaults, for a dark and a light terminal
// background.
var (
	Dark = map[string]Color{
		Info:              hex("#7dcfff"),
		Accent:            hex("#bb9af7"),
		Success:           hex("#9ece6a"),
		Warning:           hex("#e0af68"),
		Danger:            hex("#f7768e"),
		Dimmed:            hex("#565f89"),
		Text:              hex("#c0caf5"),
		Border:            hex("#3b4261"),
		Header:            hex("#7aa2f7"),
		HighlightRowBg:    hex("#283457"),
		CurrentWorktreeFg: hex("#e0af68"),
	}
	Light = map[string]Color{
		Info:              hex("#007197"),
		Accent:            hex("#7847bd"),
		Success:           hex("#587539"),
		Warning:           hex("#8c6c3e"),
		Danger:            hex("#c64343"),
		Dimmed:            hex("#8990b3"),
		Text:              hex("#3760bf"),
		Border:            hex("#c4c8da"),
		Header:            hex("#2e7de9"),
		HighlightRowBg:    hex("#d0d5e3"),
		CurrentWorktreeFg: hex("#b15c00"),
	}
)

// Modes a theme is chosen by: auto asks the terminal for its background.
const (
	ModeAuto  = "auto"
	ModeDark  = "dark"
	ModeLight = "light"
)

// ValidMode reports whether mode is a theme mode; "" is auto.
func ValidMode(mode string) bool {
	switch mode {
	case "", ModeAuto, ModeDark, ModeLight:
		return true
	}
	return false
}

// Theme is the colours in use, or none: a monochrome theme draws with
// the attributes alone, as the views did before colour, for NO_COLOR.
type Theme struct {
	Mono   bool
	colors map[string]Color
}

// New is the theme for a terminal background, dark or light, with the
// custom colours over the defaults. The custom colours are parsed as
// Parse does; the names must be palette names.
func New(dark bool, custom map[string]string) (Theme, error) {
	base := Light
	if dark {
		base = Dark
	}
	colors := make(map[string]Color, len(base))
	for k, v := range base {
		colors[k] = v
	}
	for k, v := range custom {
		if !Known(k) {
			return Theme{}, fmt.Errorf("theme: %q is not a palette colour", k)
		}
		c, err := Parse(v)
		if err != nil {
			return Theme{}, fmt.Errorf("theme: %s: %w", k, err)
		}
		colors[k] = c
	}
	return Theme{colors: colors}, nil
}

// Mono is the theme with no colours.
func Mono() Theme { return Theme{Mono: true} }

// SGR is the escape sequence that sets name as the foreground, or with
// bg the background; name is a palette name, or a colour as Parse reads
// it. "" when the theme is monochrome or the name is neither.
func (t Theme) SGR(name string, bg bool) string {
	if t.Mono || name == "" {
		return ""
	}
	c, ok := t.colors[name]
	if !ok {
		// A colour as the config writes it, an agent's own say.
		pc, err := Parse(name)
		if err != nil {
			return ""
		}
		c = pc
	}
	layer := 38
	if bg {
		layer = 48
	}
	if c.RGB {
		return fmt.Sprintf("\x1b[%d;2;%d;%d;%dm", layer, c.R, c.G, c.B)
	}
	return fmt.Sprintf("\x1b[%d;5;%dm", layer, c.Index)
}

// DarkBackground reads a terminal's answer to OSC 11, `rgb:RRRR/GGGG/BBBB`
// with one to four hex digits a channel, and reports whether the
// background is dark; ok is false for an answer it cannot read.
func DarkBackground(answer string) (dark, ok bool) {
	i := strings.Index(answer, "rgb:")
	if i < 0 {
		return false, false
	}
	parts := strings.SplitN(answer[i+4:], "/", 3)
	if len(parts) != 3 {
		return false, false
	}
	var ch [3]float64
	for k, p := range parts {
		p = strings.TrimRight(p, "\x07\x1b\\")
		if p == "" || len(p) > 4 {
			return false, false
		}
		v, err := strconv.ParseUint(p, 16, 32)
		if err != nil {
			return false, false
		}
		ch[k] = float64(v) / float64(uint64(1)<<(4*len(p))-1)
	}
	// Relative luminance, without the gamma: good enough to tell a dark
	// terminal from a light one.
	l := 0.2126*ch[0] + 0.7152*ch[1] + 0.0722*ch[2]
	return l < 0.5, true
}
