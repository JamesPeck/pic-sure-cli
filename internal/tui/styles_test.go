package tui

import (
	"image/color"
	"testing"

	"charm.land/lipgloss/v2"

	"github.com/JamesPeck/pic-sure-cli/internal/styles"
)

// TestTUIUsesSharedPalette pins that the tui screens' brand and status styles
// derive from the shared styles package (U6). Asserted via style equality
// rather than rendered output, which would vary by terminal background.
func TestTUIUsesSharedPalette(t *testing.T) {
	// The logo shine and the screen titles carry the brand hue.
	for _, tc := range []struct {
		name string
		got  lipgloss.Style
	}{
		{"logoShineStyle", logoShineStyle},
		{"activityTitleStyle", activityTitleStyle},
		{"wizardTitleStyle", wizardTitleStyle},
	} {
		if fg := tc.got.GetForeground(); fg != color.Color(styles.Brand) {
			t.Errorf("%s foreground = %v, want Brand %+v", tc.name, tc.got.GetForeground(), styles.Brand)
		}
	}

	// Activity status footers reuse the shared ANSI status colors.
	for _, tc := range []struct {
		name string
		got  lipgloss.Style
		want color.Color
	}{
		{"activityOKStyle", activityOKStyle, styles.StatusOK},
		{"activityWarnStyle", activityWarnStyle, styles.StatusWarn},
		{"activityBadStyle", activityBadStyle, styles.StatusBad},
	} {
		if fg := tc.got.GetForeground(); fg != tc.want {
			t.Errorf("%s foreground = %v, want shared %+v", tc.name, tc.got.GetForeground(), tc.want)
		}
	}
}
