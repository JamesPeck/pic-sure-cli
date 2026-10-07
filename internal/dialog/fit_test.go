package dialog

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/huh/v2"

	"github.com/JamesPeck/pic-sure-cli/internal/styles"
)

func twoSelectForm() *huh.Form {
	sel := func(title string) huh.Field {
		return huh.NewSelect[string]().Title(title).Options(huh.NewOptions("alpha", "beta")...)
	}
	return huh.NewForm(huh.NewGroup(sel("First")), huh.NewGroup(sel("Second")))
}

// send delivers msg and then the messages its commands produce, as the
// program would (huh moves to the next group through a command).
func send(f *huh.Form, msg tea.Msg) {
	_, cmd := f.Update(msg)
	cmds := []tea.Cmd{cmd}
	for len(cmds) > 0 {
		c := cmds[0]
		cmds = cmds[1:]
		if c == nil {
			continue
		}
		switch m := c().(type) {
		case tea.BatchMsg:
			cmds = append(cmds, m...)
		case nil:
		default:
			_, next := f.Update(m)
			cmds = append(cmds, next)
		}
	}
}

// Unselected options use the light gray on dark terminals and the dark gray
// on light ones, in every group, not just the one on screen when the
// background was reported.
func TestThemeFollowsBackgroundInEveryGroup(t *testing.T) {
	t.Cleanup(func() { styles.SetDarkBackground(true) })
	for _, tc := range []struct {
		dark bool
		want string
	}{
		{true, "38;5;252mbeta"},
		{false, "38;5;235mbeta"},
	} {
		styles.SetDarkBackground(tc.dark)
		f := Fit(twoSelectForm(), 80, 20)
		f.Init()
		if v := f.View(); !strings.Contains(v, tc.want) {
			t.Errorf("dark=%v, first group: want %q in\n%q", tc.dark, tc.want, v)
		}
		send(f, tea.KeyPressMsg{Code: tea.KeyEnter})
		if v := f.View(); !strings.Contains(v, "Second") || !strings.Contains(v, tc.want) {
			t.Errorf("dark=%v, second group: want %q in\n%q", tc.dark, tc.want, v)
		}
	}
}
