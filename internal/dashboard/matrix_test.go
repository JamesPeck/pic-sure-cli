package dashboard

// U13 size/color verification matrix for the dashboard help line (U12).
// Renders at 80×24, 120×30, and 200×50 in modeNormal and confirms:
//   - At <100 cols: the reduced hint set is shown.
//   - At ≥100 cols: the full legend is shown.
//   - In both cases: the view stays within the terminal box.
//
// Color emission is pinned per color profile in TestDashboardColorProfileSGR.

import (
	"fmt"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/colorprofile"
	"github.com/charmbracelet/x/ansi"

	"github.com/JamesPeck/pic-sure-cli/internal/styles/stylestest"
)

// dashMatrixSizes are the three canonical terminal sizes for U13.
var dashMatrixSizes = [][2]int{
	{80, 24},
	{120, 30},
	{200, 50},
}

// TestDashboardHelpLineMatrix renders the dashboard at each canonical size in
// modeNormal (the help-line mode that U12 targets) and asserts:
//   - At width <100: the reduced hint set ("↑/↓ select · r restart …") is used.
//   - At width ≥100: the full legend (containing "m migrate", "X destroy") is used.
//   - The view stays within the terminal box at every size.
func TestDashboardHelpLineMatrix(t *testing.T) {
	for _, sz := range dashMatrixSizes {
		w, h := sz[0], sz[1]
		t.Run(fmt.Sprintf("%dx%d", w, h), func(t *testing.T) {
			m, _ := testModel(t)
			mm, _ := m.Update(tea.WindowSizeMsg{Width: w, Height: h})
			m = mm.(*model)
			// modeNormal is the default; the helpLine switch falls through to the
			// narrow/wide normal-mode branch.

			helpLine := ansi.Strip(m.helpLine())

			if w < 100 {
				// Narrow: reduced hint set.
				if !strings.Contains(helpLine, "↑/↓ select") {
					t.Errorf("%dx%d narrow: helpLine missing '↑/↓ select': %q", w, h, helpLine)
				}
				// The full legend's cryptic shorthands must NOT appear at narrow widths.
				if strings.Contains(helpLine, "m migrate") {
					t.Errorf("%dx%d narrow: helpLine contains m migrate (full legend leaked): %q", w, h, helpLine)
				}
				if strings.Contains(helpLine, "X destroy") {
					t.Errorf("%dx%d narrow: helpLine contains X destroy (full legend leaked): %q", w, h, helpLine)
				}
			} else {
				// Wide: full legend.
				if !strings.Contains(helpLine, "m migrate") {
					t.Errorf("%dx%d wide: helpLine missing m migrate: %q", w, h, helpLine)
				}
				if !strings.Contains(helpLine, "X destroy") {
					t.Errorf("%dx%d wide: helpLine missing X destroy: %q", w, h, helpLine)
				}
			}

			// Frame must fit inside the terminal box.
			view := m.View().Content
			if fh := lipgloss.Height(view); fh > h {
				t.Errorf("%dx%d: frame height %d > terminal height %d", w, h, fh, h)
			}
			for i, line := range strings.Split(view, "\n") {
				if lw := lipgloss.Width(line); lw > w {
					t.Errorf("%dx%d: line %d width %d > terminal width %d", w, h, i, lw, w)
				}
			}
		})
	}
}

// TestDashboardColorProfileSGR pins both sides of color emission. Lip Gloss
// v2 renders full color whatever the environment, and Bubble Tea downsamples
// each frame to the terminal's color profile on output, so the test
// downsamples the frame itself:
//   - TrueColor: the styled dashboard MUST set colors (proves the styling is
//     real, so the Ascii side below cannot pass trivially);
//   - Ascii (the profile NO_COLOR selects): no colors at all.
func TestDashboardColorProfileSGR(t *testing.T) {
	for _, sz := range [][2]int{{80, 24}, {200, 50}} {
		w, h := sz[0], sz[1]
		m, _ := testModel(t)
		mm, _ := m.Update(tea.WindowSizeMsg{Width: w, Height: h})
		m = mm.(*model)
		view := m.View().Content

		if !stylestest.HasColor(stylestest.Downsample(view, colorprofile.TrueColor)) {
			t.Errorf("%dx%d TrueColor: styled dashboard set no colors (styling lost?)", w, h)
		}
		if stylestest.HasColor(stylestest.Downsample(view, colorprofile.Ascii)) {
			t.Errorf("%dx%d Ascii (NO_COLOR): colors present in output", w, h)
		}
	}
}
