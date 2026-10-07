package dashboard

import (
	"image/color"
	"testing"

	"charm.land/lipgloss/v2"

	"github.com/JamesPeck/pic-sure-cli/internal/styles"
)

// TestDashboardUsesSharedPalette pins that the dashboard's status and title
// styles derive from the shared styles package (U6) rather than ad-hoc
// lipgloss.Color values. Asserted via style equality, not rendered output,
// which would be brittle across terminal backgrounds.
func TestDashboardUsesSharedPalette(t *testing.T) {
	for _, tc := range []struct {
		name string
		got  lipgloss.Style
		want color.Color
	}{
		{"okStyle", okStyle, styles.StatusOK},
		{"warnStyle", warnStyle, styles.StatusWarn},
		{"badStyle", badStyle, styles.StatusBad},
	} {
		if fg := tc.got.GetForeground(); fg != tc.want {
			t.Errorf("%s foreground = %v, want shared %+v", tc.name, tc.got.GetForeground(), tc.want)
		}
	}

	// Pane and screen titles carry the brand color.
	for _, tc := range []struct {
		name string
		got  lipgloss.Style
	}{
		{"titleStyle", titleStyle},
		{"paneTitle", paneTitle},
	} {
		if fg := tc.got.GetForeground(); fg != color.Color(styles.Brand) {
			t.Errorf("%s foreground = %v, want Brand %+v", tc.name, tc.got.GetForeground(), styles.Brand)
		}
	}
}
