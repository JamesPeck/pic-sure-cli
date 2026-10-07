package styles

import (
	"image/color"
	"testing"

	"charm.land/lipgloss/v2"
)

// TestPaletteValues pins the palette's color values so an accidental edit to a
// hex/ANSI code is caught. A render test asserting a title "carries the brand
// color" is brittle (the output depends on the terminal's background and
// color profile); asserting the declared values is the stable check.
func TestPaletteValues(t *testing.T) {
	if Brand.Light != lipgloss.Color("#224D96") || Brand.Dark != lipgloss.Color("#6F9EEF") {
		t.Errorf("Brand drifted from the logo hue: %+v", Brand)
	}
	// Status colors must stay on plain ANSI 1/2/3 so terminal themes remap them.
	for _, tc := range []struct {
		name string
		got  color.Color
		want color.Color
	}{
		{"StatusOK", StatusOK, lipgloss.Color("2")},
		{"StatusWarn", StatusWarn, lipgloss.Color("3")},
		{"StatusBad", StatusBad, lipgloss.Color("1")},
	} {
		if tc.got != tc.want {
			t.Errorf("%s = %#v, want ANSI %#v", tc.name, tc.got, tc.want)
		}
	}
}

// TestBrandFollowsBackground: Brand resolves to its dark variant until the
// TUI reports a light background, and back again.
func TestBrandFollowsBackground(t *testing.T) {
	t.Cleanup(func() { SetDarkBackground(true) })
	if !HasDarkBackground() {
		t.Fatal("background should default to dark")
	}
	if !sameRGBA(Brand, Brand.Dark) {
		t.Error("Brand should render its dark variant by default")
	}
	SetDarkBackground(false)
	if !sameRGBA(Brand, Brand.Light) {
		t.Error("Brand should render its light variant on a light background")
	}
	SetDarkBackground(true)
	if !sameRGBA(Brand, Brand.Dark) {
		t.Error("Brand should render its dark variant again once the background is dark")
	}
}

func sameRGBA(a, b color.Color) bool {
	ar, ag, ab, aa := a.RGBA()
	br, bg, bb, ba := b.RGBA()
	return ar == br && ag == bg && ab == bb && aa == ba
}

// TestHelperStylesUsePalette verifies the helper styles are built from the
// palette colors (not redefined inline), checked via lipgloss's own accessors
// rather than rendered output.
func TestHelperStylesUsePalette(t *testing.T) {
	if fg := Title.GetForeground(); fg != color.Color(Brand) {
		t.Errorf("Title foreground = %v, want Brand %+v", fg, Brand)
	}
	if !Title.GetBold() {
		t.Error("Title should be bold")
	}
	for _, tc := range []struct {
		name string
		got  lipgloss.Style
		want color.Color
	}{
		{"OK", OK, StatusOK},
		{"Warn", Warn, StatusWarn},
		{"Bad", Bad, StatusBad},
	} {
		if fg := tc.got.GetForeground(); fg != tc.want {
			t.Errorf("%s foreground = %v, want %v", tc.name, fg, tc.want)
		}
	}
}
