// Package styles holds the shared PIC-SURE TUI palette so the brand identity
// (the logo's blue) and the status semantics (ok/warn/bad) are defined once and
// reused across every screen. It deliberately depends on nothing in the project
// (only lipgloss) so BOTH the tui package and the dashboard package can import
// it without an import cycle — tui imports dashboard, so the palette cannot live
// in either of them.
//
// Lip Gloss v2 has no adaptive colors of its own: it leaves the background
// query to Bubble Tea. Brand is an AdaptiveColor that picks its variant when a
// style renders, from the flag the TUI sets with SetDarkBackground once the
// terminal answers its background query. Colors degrade under NO_COLOR
// because Bubble Tea downsamples its output to the terminal's color profile.
// The status colors keep the plain ANSI 1/2/3 values the dashboard used
// before this package existed, so any terminal theme
// that remaps those slots still applies.
package styles

import (
	"image/color"
	"sync/atomic"

	"charm.land/lipgloss/v2"
)

// lightBackground is the inverse of the dark flag, so the zero value means
// "dark": the variant to use until the terminal reports its background, and
// the one Lip Gloss itself assumes when the query fails.
var lightBackground atomic.Bool

// SetDarkBackground records whether the terminal background is dark. The TUI
// calls it when Bubble Tea delivers a tea.BackgroundColorMsg.
func SetDarkBackground(dark bool) { lightBackground.Store(!dark) }

// HasDarkBackground reports the background last set by SetDarkBackground
// (true until then).
func HasDarkBackground() bool { return !lightBackground.Load() }

// AdaptiveColor is a color with a variant for light and dark backgrounds,
// chosen each time it is rendered. Lip Gloss renders any color.Color other
// than its own ANSI types as 24-bit color, so use it only for hex colors: a
// palette index wrapped in it would lose the terminal theme's remapping.
type AdaptiveColor struct {
	Light, Dark color.Color
}

// RGBA implements color.Color with the variant for the current background.
func (c AdaptiveColor) RGBA() (r, g, b, a uint32) {
	if HasDarkBackground() {
		return c.Dark.RGBA()
	}
	return c.Light.RGBA()
}

var (
	// Brand is the PIC-SURE logo hue (--color-primary-500,
	// oklch(43.14% 0.13 260.55)): exact #224D96 on light terminals; on dark
	// ones the same hue lifted to oklch 70% lightness (#6F9EEF) so it stays
	// legible over dark backgrounds. Matches the logo shine sweep.
	Brand = AdaptiveColor{Light: lipgloss.Color("#224D96"), Dark: lipgloss.Color("#6F9EEF")}

	// StatusOK / StatusWarn / StatusBad are the semantic status colors, kept on
	// ANSI 2/3/1 (green/yellow/red) so terminal themes can remap them — the same
	// values the screens used as ad-hoc lipgloss.Color("2"/"3"/"1").
	StatusOK   = lipgloss.Green
	StatusWarn = lipgloss.Yellow
	StatusBad  = lipgloss.Red
)

// Helper styles. Screens compose these (or .Foreground(Brand) etc.) rather than
// redefining the color slots inline.
var (
	// Title is a brand-colored bold style for screen titles and pane headers.
	Title = lipgloss.NewStyle().Bold(true).Foreground(Brand)

	// OK / Warn / Bad are the bare status foregrounds, matching the dashboard's
	// pre-existing okStyle/warnStyle/badStyle (no padding, no bold).
	OK   = lipgloss.NewStyle().Foreground(StatusOK)
	Warn = lipgloss.NewStyle().Foreground(StatusWarn)
	Bad  = lipgloss.NewStyle().Foreground(StatusBad)
)
