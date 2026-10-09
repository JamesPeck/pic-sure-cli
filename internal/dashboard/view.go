package dashboard

import (
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/JamesPeck/pic-sure-cli/internal/ops"
	"github.com/JamesPeck/pic-sure-cli/internal/styles"
)

var (
	// titleStyle is the top-of-screen header; paneTitle the per-pane headers.
	// Both carry the shared brand color so the dashboard reads as one product
	// with the logo, not a generic table.
	titleStyle    = lipgloss.NewStyle().Bold(true).Foreground(styles.Brand).Padding(0, 1)
	paneStyle     = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).Padding(0, 1)
	paneTitle     = lipgloss.NewStyle().Bold(true).Foreground(styles.Brand)
	selectedStyle = lipgloss.NewStyle().Reverse(true)
	// Status colors come from the shared palette (ANSI 2/3/1, theme-remappable).
	okStyle     = styles.OK
	warnStyle   = styles.Warn
	badStyle    = styles.Bad
	helpStyle   = lipgloss.NewStyle().Faint(true).Padding(0, 1)
	resultStyle = lipgloss.NewStyle().Bold(true).Padding(0, 1)
	// noteStyle is faint text inside a pane; summaryLabel heads the deep
	// check's lines.
	noteStyle    = lipgloss.NewStyle().Faint(true)
	summaryLabel = lipgloss.NewStyle().Bold(true)
)

// paneBox sizes paneStyle by the box inside its border (padding included),
// the measure all of this file's geometry is written in; Lip Gloss v2 counts
// the border in Width and Height. A zero height leaves the pane sized to its
// content.
func paneBox(w, h int) lipgloss.Style {
	st := paneStyle.Width(w + paneStyle.GetHorizontalBorderSize())
	if h > 0 {
		st = st.Height(h + paneStyle.GetVerticalBorderSize())
	}
	return st
}

// layout recomputes viewport dimensions after a resize.
func (m *model) layout() {
	rightWidth := m.rightWidth()
	logHeight := max(m.height-summaryHeight-7, 3)

	// Viewport content width = styled pane width minus its 2 padding cols.
	m.logView.SetWidth(rightWidth - 2)
	m.logView.SetHeight(logHeight)
	m.refreshLogPane()
}

// rightWidth is the right column's pane width, its padding included.
func (m *model) rightWidth() int { return max(m.width-m.leftWidth()-6, 20) }

// formWidth is the content width of the form pane: the right column minus
// the pane's 2 columns of padding. A form sized wider re-wraps inside the
// pane and the frame outgrows the terminal.
func (m *model) formWidth() int { return m.rightWidth() - 2 }

// refreshLogPane re-renders the followed logs, hard-wrapped at the viewport
// width so long docker log lines cannot blow the frame out of the terminal.
func (m *model) refreshLogPane() {
	atBottom := m.logView.AtBottom()
	content := strings.Join(m.logLines, "\n")
	if m.logView.Width() > 0 {
		content = ansi.Hardwrap(content, m.logView.Width(), true)
	}
	m.logView.SetContent(content)
	if atBottom {
		m.logView.GotoBottom()
	}
}

// View renders the dashboard frame. Terminal modes such as the alt screen
// belong to the program that embeds it.
func (m *model) View() tea.View { return tea.NewView(m.frame()) }

func (m *model) frame() string {
	if m.width == 0 {
		return "loading..."
	}

	header := titleStyle.Render(m.headerLine())

	right := lipgloss.JoinVertical(lipgloss.Left, m.summaryPane(), m.logPane())
	if m.form != nil {
		right = m.formPane()
	}

	body := lipgloss.JoinHorizontal(lipgloss.Top, m.servicesPane(), right)

	footer := helpStyle.Render(m.helpLine())
	if m.lastResult != "" && m.form == nil {
		footer = lipgloss.JoinVertical(lipgloss.Left, resultStyle.Render(m.lastResult), footer)
	}

	return lipgloss.JoinVertical(lipgloss.Left, header, body, footer)
}

func (m *model) headerLine() string {
	s := m.status
	if s == nil {
		return "PIC-SURE"
	}
	line := "PIC-SURE — " + s.Stack.Name
	if s.DB != nil {
		line += "  db:" + s.DB.Mode
	}
	if s.Auth0 != nil {
		auth := "auth0"
		if !s.Auth0.Needed {
			auth = "open"
		}
		line += "  auth:" + auth
	}
	return line
}

func (m *model) servicesPane() string {
	var b strings.Builder
	b.WriteString(paneTitle.Render("Services") + "\n")

	if m.servicesErr != nil || len(m.services) == 0 {
		b.WriteString(m.servicesEmptyState())
	}

	// Row layout: cursor(1) + service(svcCol) + space(1) + state(7) + space(1) +
	// health(9). paneBox sizes the box inside the border, so the content wrap
	// width = leftWidth − padding(1+1). The cursor takes 1 col, two
	// separator spaces take 2, and state+health are fixed at 7+9; the service
	// column flexes to absorb the rest:
	//   svcCol = (leftWidth−2) − 1 − 2 − 7 − 9 = leftWidth − 21.
	lw := m.leftWidth()
	const (
		stateCol  = 7
		healthCol = 9
	)
	svcCol := lw - 21
	for i, s := range m.services {
		health := s.Health
		if health == "" {
			health = "-"
		}
		line := fmt.Sprintf("%-*.*s %-*.*s %-*.*s",
			svcCol, svcCol, s.Service,
			stateCol, stateCol, s.State,
			healthCol, healthCol, health)
		switch {
		case s.State == "running" && (s.Health == "healthy" || s.Health == ""):
			line = okStyle.Render(line)
		case s.State == "running":
			line = warnStyle.Render(line)
		default:
			line = badStyle.Render(line)
		}
		if i == m.selected {
			line = selectedStyle.Render("▸") + line
		} else {
			line = " " + line
		}
		b.WriteString(line + "\n")
	}

	return paneBox(lw, max(m.height-5, 8)).Render(b.String())
}

// servicesEmptyState says why the services pane is empty, in at most three
// lines that fit the narrowest pane (leftWidthMin-2 = 34 columns).
func (m *model) servicesEmptyState() string {
	const width = leftWidthMin - 2
	if m.servicesErr != nil {
		msg, _, _ := strings.Cut(m.servicesErr.Error(), "\n")
		return warnStyle.Render("Services unavailable") + "\n" +
			warnStyle.Render(ansi.Truncate(msg, width, "…")) + "\n" +
			warnStyle.Render("is Docker running?") + "\n"
	}
	if !m.pollingServices || m.services != nil {
		return warnStyle.Render("No containers") + "\n" +
			warnStyle.Render("u updates and starts the stack") + "\n"
	}
	return noteStyle.Render("loading services...") + "\n"
}

// summaryPane renders the status summary severity-first: problems (red,
// then yellow) at the top, then one folded line for everything healthy,
// the deep check, and the release.
func (m *model) summaryPane() string {
	width := m.rightWidth()
	var lines []string
	switch {
	case m.statusErr != nil:
		lines = []string{badStyle.Render("status failed: " + firstLine(m.statusErr.Error()))}
	case m.status == nil:
		lines = []string{noteStyle.Render("loading status...")}
	default:
		lines = m.summaryBody()
	}
	lines = append(lines, m.deepLines()...)
	// Each line is cut to the pane, so the pane never grows past its height.
	for i, l := range lines {
		lines[i] = ansi.Truncate(l, width-2, "…")
	}
	return paneBox(width, summaryHeight-2).Render(paneTitle.Render("Status") + "\n" + strings.Join(lines, "\n"))
}

// summaryBody classifies the status report's checks. Each check gives at
// most one line, so the body is at most 6 lines: five problems, or fewer
// and the OK line, plus the release. With the title and the deep check's
// three lines that is 10, within the pane's 11 content rows.
func (m *model) summaryBody() []string {
	s := m.status
	var blockers, warnings, oks []string

	switch {
	case s.Config.Error != "":
		blockers = append(blockers, "config unreadable: "+firstLine(s.Config.Error))
	case !s.Config.Valid:
		blockers = append(blockers, fmt.Sprintf("config invalid (%d problems; see pic-sure status)", len(s.Config.Problems)))
	default:
		oks = append(oks, "config")
	}

	switch s.Versions.Gate {
	case ops.GateOK:
		oks = append(oks, "version")
	case ops.GateStackNewer:
		blockers = append(blockers, "a newer pic-sure manages this stack")
	case ops.GateUnsupportedSchema:
		blockers = append(blockers, "the stack's config schema isn't supported")
	case ops.GateMigrationsPending:
		warnings = append(warnings, "the config needs migrating; update does it")
	default:
		warnings = append(warnings, "the version gate is unknown")
	}

	missing := 0
	for _, img := range s.Images {
		if img.Present != nil && !*img.Present {
			missing++
		}
	}
	switch {
	case s.ImagesError != "":
		warnings = append(warnings, "images unknown: "+firstLine(s.ImagesError))
	case missing > 0:
		warnings = append(warnings, fmt.Sprintf("%d images missing; update builds them", missing))
	case len(s.Images) > 0:
		oks = append(oks, "images")
	}

	switch s.Migrations.Status {
	case ops.MigrationsStatusUpToDate:
		oks = append(oks, "migrations")
	case ops.MigrationsStatusPending:
		blockers = append(blockers, "migrations pending; m runs them")
	default:
		if e := s.Migrations.Error; e != "" {
			warnings = append(warnings, "migrations: couldn't check ("+firstLine(e)+")")
		}
	}

	switch t := s.Token; {
	case t.Error != "":
		warnings = append(warnings, "token unknown: "+firstLine(t.Error))
	case t.ExpiresAt == nil:
		warnings = append(warnings, "no introspection token issued")
	case t.Expired:
		blockers = append(blockers, "introspection token expired; u renews it")
	default:
		oks = append(oks, "token")
	}

	var lines []string
	for _, l := range blockers {
		lines = append(lines, badStyle.Render("✗ "+l))
	}
	for _, l := range warnings {
		lines = append(lines, warnStyle.Render("! "+l))
	}
	if len(oks) > 0 {
		lines = append(lines, okStyle.Render("✓ "+strings.Join(oks, " · ")))
	}
	release := "release: (not recorded)"
	if s.Release.Commit != "" {
		release = fmt.Sprintf("release: %s @ %s", s.Release.Branch, shortCommit(s.Release.Commit))
	}
	return append(lines, release)
}

// deepLines shows the cached deep check: when it ran, the three verdicts,
// and the first verdict that isn't good, with its reason.
func (m *model) deepLines() []string {
	switch {
	case m.deepRunning:
		return []string{noteStyle.Render("Health: checking (takes up to a minute)...")}
	case m.deepErr != nil:
		return []string{badStyle.Render("Health check failed: " + firstLine(m.deepErr.Error()))}
	case m.deep == nil:
		return []string{noteStyle.Render("Health: h checks the gateway, HPDS data and CSP")}
	}
	d := m.deep
	gateway := verdict(d.Gateway.Checked, d.Gateway.Healthy, "healthy", "unhealthy")
	data := verdict(d.Data.Checked, d.Data.Ready, "ready", "not ready")
	csp := d.HTTP.CSP
	if !d.HTTP.Checked {
		csp = "not checked"
	}
	lines := []string{
		summaryLabel.Render("Health, checked " + m.deepAt.Format("15:04:05")),
		fmt.Sprintf("gateway %s · data %s · CSP %s", gateway, data, csp),
	}
	switch {
	case d.Gateway.Healthy == nil || !*d.Gateway.Healthy:
		lines = append(lines, warnStyle.Render("gateway: "+firstLine(d.Gateway.Message)))
	case d.Data.Ready == nil || !*d.Data.Ready:
		lines = append(lines, warnStyle.Render("data: "+firstLine(d.Data.Message)))
	case d.HTTP.CSP != ops.CSPFrontend:
		lines = append(lines, warnStyle.Render("CSP: "+firstLine(d.HTTP.Message)))
	}
	return lines
}

// verdict is a deep probe's yes or no, or unknown when it didn't run or
// couldn't tell.
func verdict(checked bool, ok *bool, yes, no string) string {
	switch {
	case !checked || ok == nil:
		return "unknown"
	case *ok:
		return yes
	}
	return no
}

func (m *model) logPane() string {
	title := paneTitle.Render("Logs")
	if m.logSvc != "" {
		title = paneTitle.Render("Logs — " + m.logSvc)
	}
	return paneBox(m.rightWidth(), 0).Render(title + "\n" + m.logView.View())
}

func (m *model) formPane() string {
	return paneBox(m.rightWidth(), 0).Render(m.form.View())
}

func (m *model) helpLine() string {
	if m.form != nil {
		return "esc cancel"
	}
	// Narrow terminals (<100 cols) get a shorter legend, so it fits on one
	// row.
	if m.width < 100 {
		return "↑/↓ select · r restart · u update · h health · esc back · q quit"
	}
	return "↑/↓ · r restart · u update · m migrate · h health · l load · R reset · X destroy · pgup/dn · esc · q"
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(s, "\n")
	return line
}

func shortCommit(c string) string {
	if len(c) > 8 {
		return c[:8]
	}
	return c
}
