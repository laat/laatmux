package palette

import (
	"math"
	"strings"
	"testing"
)

func TestParse(t *testing.T) {
	for in, want := range map[string]Color{
		"#d97757":  {R: 0xd9, G: 0x77, B: 0x57, RGB: true},
		"208":      {Index: 208},
		"colour12": {Index: 12},
		"color0":   {Index: 0},
	} {
		if got, err := Parse(in); err != nil || got != want {
			t.Errorf("%q: %+v %v", in, got, err)
		}
	}
	for _, bad := range []string{"", "#fff", "#gggggg", "256", "-1", "red", "colourx"} {
		if _, err := Parse(bad); err == nil {
			t.Errorf("%q parsed", bad)
		}
	}
}

// A theme is the defaults for the background with the custom colours
// over them; an unknown name or a bad colour is refused; a monochrome
// theme and the zero theme encode nothing.
func TestTheme(t *testing.T) {
	th, err := New(true, map[string]string{Accent: "#010203", Info: "33"})
	if err != nil {
		t.Fatal(err)
	}
	if got := th.SGR(Accent, false); got != "\x1b[38;2;1;2;3m" {
		t.Errorf("custom rgb: %q", got)
	}
	if got := th.SGR(Info, true); got != "\x1b[48;5;33m" {
		t.Errorf("custom index as background: %q", got)
	}
	if got := th.SGR(Success, false); !strings.HasPrefix(got, "\x1b[38;2;") {
		t.Errorf("default: %q", got)
	}
	light, _ := New(false, nil)
	if light.SGR(Text, false) == th.SGR(Text, false) {
		t.Error("the light theme's text is the dark one's")
	}
	if _, err := New(true, map[string]string{"purple": "#000000"}); err == nil {
		t.Error("an unknown palette name was taken")
	}
	if _, err := New(true, map[string]string{Accent: "purple"}); err == nil {
		t.Error("a bad colour was taken")
	}
	if Mono().SGR(Info, false) != "" || (Theme{}).SGR(Info, false) != "" {
		t.Error("a theme without colours encoded one")
	}
}

// Luminance is WCAG's, of a colour's own red, green and blue or an
// indexed colour's by xterm's formula: the cube from 16, the greys from
// 232; none for 0 to 15. The contrast ratios are #152's table.
func TestLuminance(t *testing.T) {
	lum := func(s string) float64 {
		t.Helper()
		l, ok := hex(s).Luminance()
		if !ok {
			t.Fatalf("%q: no luminance", s)
		}
		return l
	}
	near := func(a, b float64) bool { return math.Abs(a-b) < 0.005 }
	for s, want := range map[string]float64{"#000000": 0, "#ffffff": 1, "#ff0000": 0.2126, "#565f89": 0.1197, "#8990b3": 0.2852, "#808080": 0.2159} {
		if got := lum(s); !near(got, want) {
			t.Errorf("%s: %.4f, want %.4f", s, got, want)
		}
	}
	// The cube's levels are 0, 95, 135, 175, 215 and 255; 67 is 1, 2, 3
	// in it, red, green, blue; the greys run 8 to 238 by tens.
	for idx, rgb := range map[string]string{"16": "#000000", "67": "#5f87af", "196": "#ff0000", "230": "#ffffd7", "231": "#ffffff", "232": "#080808", "235": "#262626", "240": "#585858", "255": "#eeeeee"} {
		if got, want := lum(idx), lum(rgb); got != want {
			t.Errorf("colour %s: %.4f, want %s's %.4f", idx, got, rgb, want)
		}
	}
	for _, s := range []string{"0", "7", "15"} {
		if _, ok := hex(s).Luminance(); ok {
			t.Errorf("colour %s has a luminance", s)
		}
	}
	for _, c := range []struct {
		fg, bg string
		want   float64
	}{
		{"#565f89", "#ffff00", 5.76}, {"#565f89", "#ffffff", 6.19}, {"#565f89", "#112233", 2.61},
		{"#8990b3", "#112233", 5.16}, {"#8990b3", "235", 4.83}, {"#8990b3", "#283457", 3.90},
		{"#8990b3", "#3b4261", 3.14}, {"#8990b3", "240", 2.27}, {"#8990b3", "#565f89", 1.98},
		{"#000000", "#ffffff", 21}, {"#565f89", "#565f89", 1},
	} {
		if got := Contrast(lum(c.fg), lum(c.bg)); !near(got, c.want) {
			t.Errorf("%s on %s: %.2f, want %.2f", c.fg, c.bg, got, c.want)
		}
		if Contrast(lum(c.bg), lum(c.fg)) != Contrast(lum(c.fg), lum(c.bg)) {
			t.Errorf("%s on %s: the ratio depends on the order", c.fg, c.bg)
		}
	}
}

// Dim text on a template's background: the dark default's dimmed on a
// light background, the light default's on a dark one, whatever the
// theme; a palette name through the theme, an indexed colour by its
// xterm colour; nothing on 0 to 15, none, or in a theme without
// colours. A user's own dimmed where it reads better than both.
func TestDimmedOn(t *testing.T) {
	dark, _ := New(true, nil)
	light, _ := New(false, nil)
	darkDim, lightDim := dark.SGR("#565f89", false), dark.SGR("#8990b3", false)
	mono := dark
	mono.Mono = true
	for _, c := range []struct {
		th       Theme
		bg, want string
	}{
		{dark, "#ffff00", darkDim}, {dark, "#ffffff", darkDim}, {dark, "#112233", lightDim},
		{dark, "colour235", lightDim}, {dark, "240", lightDim}, {dark, "230", darkDim}, {dark, "#808080", darkDim},
		{dark, HighlightRowBg, lightDim}, {dark, Border, lightDim}, {dark, Dimmed, lightDim},
		{light, HighlightRowBg, darkDim}, {light, Border, darkDim}, {light, "#112233", lightDim}, {light, "#ffff00", darkDim},
		{dark, "3", ""}, {dark, "colour15", ""}, {dark, "", ""}, {dark, "mauve", ""},
		{Mono(), "#ffff00", ""}, {mono, "#ffff00", ""}, {Theme{}, "#ffff00", ""},
	} {
		if got := c.th.DimmedOn(c.bg); got != c.want {
			t.Errorf("on %q: %q, want %q", c.bg, got, c.want)
		}
	}
	// A user's dimmed is drawn where it has more contrast than both
	// defaults, not where it has less; one of 0 to 15 is not measured;
	// a user's background colour is the theme's.
	for _, c := range []struct {
		custom   map[string]string
		bg, want string
	}{
		{map[string]string{Dimmed: "#444444"}, "#ffff00", dark.SGR("#444444", false)},
		{map[string]string{Dimmed: "#444444"}, "#112233", lightDim},
		{map[string]string{Dimmed: "#c0c0c0"}, "#112233", dark.SGR("#c0c0c0", false)},
		{map[string]string{Dimmed: "#7f849c"}, "#ffff00", darkDim},
		{map[string]string{Dimmed: "8"}, "#ffff00", darkDim},
		{map[string]string{Dimmed: "8"}, "#112233", lightDim},
		{map[string]string{HighlightRowBg: "#ffffff"}, HighlightRowBg, darkDim},
		{map[string]string{Accent: "5"}, Accent, ""},
	} {
		for _, isDark := range []bool{true, false} {
			th, err := New(isDark, c.custom)
			if err != nil {
				t.Fatal(err)
			}
			if got := th.DimmedOn(c.bg); got != c.want {
				t.Errorf("%v (dark %v) on %q: %q, want %q", c.custom, isDark, c.bg, got, c.want)
			}
		}
	}
}

func TestDarkBackground(t *testing.T) {
	for answer, want := range map[string]bool{
		"\x1b]11;rgb:1a1a/1b1b/2626\x1b\\": true,
		"\x1b]11;rgb:ffff/ffff/ffff\x07":   false,
		"\x1b]11;rgb:fa/fa/f0\x07":         false,
		"\x1b]11;rgb:0/0/0\x07":            true,
	} {
		if dark, ok := DarkBackground(answer); !ok || dark != want {
			t.Errorf("%q: dark %v ok %v", answer, dark, ok)
		}
	}
	for _, bad := range []string{"", "\x1b]11;?\x07", "\x1b]11;rgb:zz/00/00\x07", "\x1b]11;rgb:12345/0/0\x07"} {
		if _, ok := DarkBackground(bad); ok {
			t.Errorf("%q read", bad)
		}
	}
}
