// Package palette is the named colours the views draw with, their dark
// and light defaults, and the colour syntax the config takes. The views
// name colours; only the terminal encoding looks them up, so a golden
// test shows a colour by its name and a theme can change it.
package palette

import (
	"fmt"
	"math"
	"slices"
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
func Known(name string) bool { return slices.Contains(Names, name) }

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

// rgb is the colour's red, green and blue: its own, or an indexed
// colour's from 16 to 255 by xterm's formula, the 6×6×6 cube and then
// the greys. ok is false for 0 to 15, which the terminal defines.
func (c Color) rgb() (r, g, b uint8, ok bool) {
	switch {
	case c.RGB:
		return c.R, c.G, c.B, true
	case c.Index < 16 || c.Index > 255:
		return 0, 0, 0, false
	case c.Index < 232:
		level := func(n int) uint8 {
			if n == 0 {
				return 0
			}
			return uint8(55 + 40*n)
		}
		i := c.Index - 16
		return level(i / 36), level(i / 6 % 6), level(i % 6), true
	}
	v := uint8(8 + 10*(c.Index-232))
	return v, v, v, true
}

// luminance is the colour's relative luminance as WCAG defines it, 0
// for black to 1 for white; ok is false for colours 0 to 15, which the
// terminal defines.
func (c Color) luminance() (l float64, ok bool) {
	r, g, b, ok := c.rgb()
	if !ok {
		return 0, false
	}
	linear := func(v uint8) float64 {
		s := float64(v) / 255
		if s <= 0.04045 {
			return s / 12.92
		}
		return math.Pow((s+0.055)/1.055, 2.4)
	}
	return 0.2126*linear(r) + 0.7152*linear(g) + 0.0722*linear(b), true
}

// contrast is WCAG's contrast ratio of two luminances, from 1 for the
// same to 21 for black on white.
func contrast(a, b float64) float64 {
	if a < b {
		a, b = b, a
	}
	return (a + 0.05) / (b + 0.05)
}

// sgr is the escape sequence that sets the colour as the foreground, or
// with bg the background.
func (c Color) sgr(bg bool) string {
	layer := 38
	if bg {
		layer = 48
	}
	if c.RGB {
		return fmt.Sprintf("\x1b[%d;2;%d;%d;%dm", layer, c.R, c.G, c.B)
	}
	return fmt.Sprintf("\x1b[%d;5;%dm", layer, c.Index)
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
	Mono bool
	// Guessed is that the terminal's background is not known: the
	// query had no answer. The colours are the dark defaults, for the
	// accents; the views then keep the terminal's own foreground and
	// background under the selection too, which is right whatever the
	// background.
	Guessed bool
	colors  map[string]Color
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
	c, ok := t.color(name)
	if !ok {
		return ""
	}
	return c.sgr(bg)
}

// color is name as the theme draws it: a palette name's colour, or a
// colour as the config writes it, an agent's own say or a template's.
func (t Theme) color(name string) (Color, bool) {
	if c, ok := t.colors[name]; ok {
		return c, true
	}
	c, err := Parse(name)
	return c, err == nil
}

// DimmedOn is the escape sequence that sets the foreground of dim text
// with no colour of its own on bg, a template's background as a palette
// name or a colour as Parse reads it. The theme's dimmed is chosen to
// read on the terminal's background, not on bg, and is gone on a
// background as dark as itself: of it, a user's own among them, and the
// dark and light defaults' dimmed, the one with the most contrast
// against bg is drawn, the dark default's on a light background, the
// light default's on a dark one, a user's where it reads better than
// both. "" when the theme is monochrome, or bg is none or a colour 0 to
// 15, which the terminal defines: such text stays faint.
func (t Theme) DimmedOn(bg string) string {
	own, ok := t.colors[Dimmed]
	if t.Mono || !ok {
		return ""
	}
	c, ok := t.color(bg)
	if !ok {
		return ""
	}
	lb, ok := c.luminance()
	if !ok {
		return ""
	}
	var best Color
	most := 0.0
	for _, d := range []Color{own, Dark[Dimmed], Light[Dimmed]} {
		l, ok := d.luminance()
		if r := contrast(l, lb); ok && r > most {
			best, most = d, r
		}
	}
	return best.sgr(false)
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
