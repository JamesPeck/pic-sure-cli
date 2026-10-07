package dialog

import (
	"image/color"

	tea "charm.land/bubbletea/v2"
	"charm.land/huh/v2"

	"github.com/JamesPeck/pic-sure-cli/internal/styles"
)

// Fit prepares an embedded form for the region it renders in. An embedded
// form only sees the messages its screen forwards, so Fit feeds it the two it
// lays out and styles from:
//
//   - a synthetic resize to width×height. huh recomputes group viewport
//     geometry only in its WindowSizeMsg handler, and only while no explicit
//     WithWidth was set: WithWidth freezes the viewports at their
//     construction-time width-80 measurement, so a field whose description
//     wraps taller at the real width gets clipped below the fold. Never call
//     WithWidth on these forms; call Fit again on every terminal resize.
//   - the terminal background, which huh's theme defaults to light until it
//     sees a tea.BackgroundColorMsg. The program receives that message once,
//     at startup, usually before the form exists.
func Fit(f *huh.Form, width, height int) *huh.Form {
	var bg color.Color = color.White
	if styles.HasDarkBackground() {
		bg = color.Black
	}
	// Form.Update has a pointer receiver and returns the form itself.
	f.Update(tea.BackgroundColorMsg{Color: bg})
	f.Update(tea.WindowSizeMsg{Width: width, Height: height})
	return f
}
