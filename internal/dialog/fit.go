package dialog

import (
	tea "charm.land/bubbletea/v2"
	"charm.land/huh/v2"
	"charm.land/lipgloss/v2"

	"github.com/JamesPeck/pic-sure-cli/internal/styles"
)

// Fit prepares an embedded form for the region it renders in:
//
//   - the width. huh.NewForm pins a form to 80 columns when TERM is dumb,
//     and a pinned form ignores the width of every WindowSizeMsg, so Fit
//     sets the width itself.
//   - a synthetic resize to width×height. huh recomputes group viewport
//     heights only in its WindowSizeMsg handler; without it, the viewports
//     keep their construction-time measurement at 80 columns, and a field
//     whose description wraps taller at the real width gets clipped below
//     the fold. Call Fit again on every terminal resize.
//   - Theme, so the form follows the terminal background.
func Fit(f *huh.Form, width, height int) *huh.Form {
	f.WithTheme(Theme)
	f.WithWidth(width)
	// Form.Update has a pointer receiver and returns the form itself.
	f.Update(tea.WindowSizeMsg{Width: width, Height: height})
	return f
}

// Theme is huh's Charm theme for the background styles.HasDarkBackground
// reports. huh tracks the background per group and passes it only to the
// group on screen, so the theme ignores huh's flag: the later groups of a
// multi-group form would otherwise stay on the light variant. huh v2.0.3 also
// swaps the light and dark grays of unselected options and blurred buttons
// (near-black on dark terminals); Theme restores v1's values.
var Theme huh.Theme = huh.ThemeFunc(func(bool) *huh.Styles {
	dark := styles.HasDarkBackground()
	t := huh.ThemeCharm(dark)
	lightDark := lipgloss.LightDark(dark)
	normalFg := lightDark(lipgloss.Color("235"), lipgloss.Color("252"))
	for _, fs := range []*huh.FieldStyles{&t.Focused, &t.Blurred} {
		fs.Option = fs.Option.Foreground(normalFg)
		fs.UnselectedOption = fs.UnselectedOption.Foreground(normalFg)
		fs.BlurredButton = fs.BlurredButton.Foreground(normalFg).
			Background(lightDark(lipgloss.Color("252"), lipgloss.Color("237")))
	}
	return t
})
