package tui

// U13 size/color verification matrix for the landing.
// Each matrix sub-test renders at the canonical sizes and asserts the
// view-specific properties described by the audit. Color emission is pinned
// per color profile in TestLandingColorProfileSGR.

import (
	"fmt"
	"regexp"
	"strings"
	"testing"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/colorprofile"

	"github.com/JamesPeck/pic-sure-cli/internal/styles/stylestest"
)

// ansiSGR matches any ANSI Select Graphic Rendition sequence (ESC [ … m).
var ansiSGR = regexp.MustCompile(`\x1b\[[0-9;]*m`)

// matrixSizes are the three canonical terminal sizes used in U13.
var matrixSizes = [][2]int{
	{80, 24},
	{120, 30},
	{200, 50},
}

// TestLandingNarrowLogoMatrix renders the landing at each canonical size and
// asserts:
//   - At widths where the full block-art logo doesn't fit (< logoWidth()+4 =
//     70), the compact "▌ PIC-SURE ▐" wordmark is present.
//   - At widths where the full logo fits, the block-art logo is used (no ▌/▐).
//   - The view stays within the terminal box at every size.
//
// The canonical matrix (80/120/200 cols) is all-wide for the 70-col logo
// threshold, so a 60×16 size is prepended to actually exercise the narrow
// branch (U9's whole point).
func TestLandingNarrowLogoMatrix(t *testing.T) {
	sizes := append([][2]int{{60, 16}}, matrixSizes...)
	for _, sz := range sizes {
		w, h := sz[0], sz[1]
		t.Run(fmt.Sprintf("%dx%d", w, h), func(t *testing.T) {
			l := newLanding("/tmp/x", readyStack, false)
			l.setSize(w, h)
			view := l.view()

			// Frame must fit inside the terminal box.
			if fh := lipgloss.Height(view); fh > h {
				t.Errorf("%dx%d: frame height %d > terminal height %d", w, h, fh, h)
			}
			for i, line := range strings.Split(view, "\n") {
				if lw := lipgloss.Width(line); lw > w {
					t.Errorf("%dx%d: line %d width %d > terminal width %d", w, h, i, lw, w)
				}
			}

			plain := ansiSGR.ReplaceAllString(view, "")
			if w < logoWidth()+4 {
				t.Logf("%dx%d: narrow branch exercised (compact wordmark; logo threshold %d cols)", w, h, logoWidth()+4)
				// Narrow: compact wordmark must be present.
				if !strings.Contains(plain, "PIC-SURE") {
					t.Errorf("%dx%d narrow: compact wordmark not found in view", w, h)
				}
				// The bracket glyphs from the compact variant.
				if !strings.Contains(plain, "▌") || !strings.Contains(plain, "▐") {
					t.Errorf("%dx%d narrow: bracket glyphs ▌/▐ not found in compact wordmark", w, h)
				}
			} else {
				// Wide: full block-art logo — compact brackets should NOT appear.
				if strings.Contains(plain, "▌") || strings.Contains(plain, "▐") {
					t.Errorf("%dx%d wide: compact wordmark brackets found when full logo should render", w, h)
				}
			}
		})
	}
}

// TestLandingColorProfileSGR pins both sides of color emission. Lip Gloss v2
// renders full color whatever the environment, and Bubble Tea downsamples each
// frame to the terminal's color profile on output, so the test downsamples the
// frame itself:
//   - TrueColor: the styled landing MUST set colors (proves the styling is
//     real, so the Ascii side below cannot pass trivially);
//   - Ascii (the profile NO_COLOR selects): no colors at all.
func TestLandingColorProfileSGR(t *testing.T) {
	for _, sz := range [][2]int{{60, 16}, {80, 24}, {200, 50}} {
		w, h := sz[0], sz[1]
		l := newLanding("/tmp/x", readyStack, false)
		l.setSize(w, h)
		view := l.view()

		if !stylestest.HasColor(stylestest.Downsample(view, colorprofile.TrueColor)) {
			t.Errorf("%dx%d TrueColor: styled landing set no colors (styling lost?)", w, h)
		}
		if stylestest.HasColor(stylestest.Downsample(view, colorprofile.Ascii)) {
			t.Errorf("%dx%d Ascii (NO_COLOR): colors present in output", w, h)
		}
	}
}
