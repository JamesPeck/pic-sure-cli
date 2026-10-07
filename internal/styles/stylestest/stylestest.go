// Package stylestest checks styled TUI output as a terminal with a given color
// profile receives it. Lip Gloss v2 always renders full color and leaves
// downsampling to the output (Bubble Tea's renderer), so a test of what a
// NO_COLOR user sees has to downsample the rendered string itself.
package stylestest

import (
	"regexp"
	"strconv"
	"strings"

	"github.com/charmbracelet/colorprofile"
)

// Downsample converts the colors in s to profile p, as Bubble Tea does when it
// writes a frame. NO_COLOR selects colorprofile.Ascii: colors are dropped and
// text decoration (bold, faint, reverse) is kept.
func Downsample(s string, p colorprofile.Profile) string {
	var b strings.Builder
	w := &colorprofile.Writer{Forward: &b, Profile: p}
	_, _ = w.WriteString(s)
	return b.String()
}

var sgr = regexp.MustCompile(`\x1b\[([0-9;:]*)m`)

// HasColor reports whether s contains an SGR sequence that sets a foreground
// or background color (including 256-color and 24-bit forms).
func HasColor(s string) bool {
	for _, m := range sgr.FindAllStringSubmatch(s, -1) {
		for _, p := range strings.FieldsFunc(m[1], func(r rune) bool { return r == ';' || r == ':' }) {
			n, err := strconv.Atoi(p)
			if err != nil {
				continue
			}
			// 38 and 48 introduce 256-color and 24-bit colors, so they match
			// before their own sub-parameters are read as attributes.
			if (n >= 30 && n <= 38) || (n >= 40 && n <= 48) || (n >= 90 && n <= 97) || (n >= 100 && n <= 107) {
				return true
			}
		}
	}
	return false
}
