package palette

import (
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
