package stylestest

import (
	"testing"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/colorprofile"
)

func TestHasColor(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want bool
	}{
		{"plain", false},
		{"\x1b[1mbold\x1b[m", false},
		{"\x1b[2;7mfaint reverse\x1b[0m", false},
		{"\x1b[32mgreen\x1b[m", true},
		{"\x1b[1;91mbright red\x1b[m", true},
		{"\x1b[38;5;21mindexed\x1b[m", true},
		{"\x1b[38;2;111;158;239mtruecolor\x1b[m", true},
		{"\x1b[48:2::1:2:3mbackground\x1b[m", true},
	} {
		if got := HasColor(tc.in); got != tc.want {
			t.Errorf("HasColor(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestDownsampleToASCIIKeepsDecorationOnly(t *testing.T) {
	styled := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#6F9EEF")).Render("PIC-SURE")
	if !HasColor(Downsample(styled, colorprofile.TrueColor)) {
		t.Fatalf("TrueColor output lost its color: %q", styled)
	}
	got := Downsample(styled, colorprofile.Ascii)
	if HasColor(got) {
		t.Errorf("Ascii output still sets a color: %q", got)
	}
	if got == "PIC-SURE" {
		t.Errorf("Ascii output dropped the bold decoration: %q", got)
	}
}
