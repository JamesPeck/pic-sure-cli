package tui

import (
	"fmt"
	"os"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/huh/v2"
	"charm.land/lipgloss/v2"

	"github.com/JamesPeck/pic-sure-cli/internal/dashboard"
	"github.com/JamesPeck/pic-sure-cli/internal/dialog"
	"github.com/JamesPeck/pic-sure-cli/internal/ops"
	"github.com/JamesPeck/pic-sure-cli/internal/stack"
	"github.com/JamesPeck/pic-sure-cli/internal/styles"
)

// openDashboardMsg asks the app to open the dashboard.
type openDashboardMsg struct{}

var (
	landingBoxStyle    = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).Padding(1, 4)
	landingFooterStyle = lipgloss.NewStyle().Faint(true)
	landingResultStyle = lipgloss.NewStyle().Bold(true)
)

// readConfig reads the stack's pic-sure.yaml for the branch prefill, the
// dev pickers and the typed confirmation. Tests replace it.
var readConfig = func(root string) (*stack.Config, error) {
	st, err := stack.Open(root)
	if err != nil {
		return nil, err
	}
	defer func() { _ = st.Close() }()
	return st.LoadConfig()
}

// landing is the starfield + logo + menu home screen. Each action runs one
// pic-sure command through the app's run screen (dashboard.RunMsg), as the
// dashboard's do.
type landing struct {
	root       string
	status     stackStatus
	animations bool

	star *starfield
	logo *logo
	menu *menu
	dev  bool // in the developer-options submenu

	form      *huh.Form
	confirmOK bool
	pending   *dashboard.Action // the yes/no dialog's action
	picked    string
	// pickerMake builds the action for the picked value while a picker is
	// open; inputMake for the typed value while the branch input is.
	pickerMake func(string) dashboard.Action
	inputVal   string
	inputMake  func(string) dashboard.Action
	// The typed confirmation for reset or destroy.
	teardown        bool
	teardownName    string
	teardownDestroy bool
	keepDB          bool
	confirmText     string

	// leaving drops input from when the landing asks the app for another
	// screen or a run until the app shows it again, so keys that arrive in
	// one read can't ask twice.
	leaving bool

	result string
	// notice is a standing message, such as why the stack here can't be
	// used; navigation doesn't clear it.
	notice        string
	width, height int
}

func newLanding(root string, status stackStatus, animations bool) *landing {
	l := &landing{
		root:       root,
		status:     status,
		animations: animations,
		star:       newStarfield(starGlyphs(os.Getenv)),
		logo:       newLogo(),
	}
	l.rebuildMenu()
	return l
}

func (l *landing) rebuildMenu() {
	switch {
	case l.dev:
		l.menu = newMenu(
			menuItem{ID: "preflight", Label: "Preflight check"},
			menuItem{ID: "dryrun", Label: "Preview update"},
			menuItem{ID: "branch", Label: "Switch release branch…"},
			menuItem{ID: "migrate", Label: "Run migrations"},
			menuItem{ID: "demo", Label: "Load demo data…"},
			menuItem{ID: "dictionary", Label: "Rebuild dictionary…"},
			menuItem{ID: "devon", Label: "Dev mode on…"},
			menuItem{ID: "devoff", Label: "Dev mode off…"},
			menuItem{ID: "reset", Label: "Reset…"},
			menuItem{ID: "destroy", Label: "Destroy…"},
			menuItem{ID: "back", Label: "Back"},
		)
	case l.status == readyStack:
		l.menu = newMenu(
			menuItem{ID: "dashboard", Label: "Dashboard"},
			menuItem{ID: "update", Label: "Update"},
			menuItem{ID: "loaddata", Label: "Load your data…"},
			menuItem{ID: "devmenu", Label: "Developer options…"},
			menuItem{ID: "quit", Label: "Quit"},
		)
	case l.status == untrustedStack:
		l.menu = newMenu(
			menuItem{ID: "preflight", Label: "Preflight check"},
			menuItem{ID: "quit", Label: "Quit"},
		)
	case l.status == partStack:
		l.menu = newMenu(
			menuItem{ID: "resume", Label: "Resume setup"},
			menuItem{ID: "devmenu", Label: "Developer options…"},
			menuItem{ID: "quit", Label: "Quit"},
		)
	default:
		l.menu = newMenu(
			menuItem{ID: "setup", Label: "Set up PIC-SURE"},
			menuItem{ID: "preflight", Label: "Preflight check"},
			menuItem{ID: "quit", Label: "Quit"},
		)
	}
}

// startAnimations (re)starts the starfield/logo chains; no-op chains when the
// kill switch is on (static frame still renders).
func (l *landing) startAnimations() tea.Cmd {
	if !l.animations {
		return nil
	}
	return tea.Batch(l.star.startTicks(), l.logo.startShine(true))
}

// stopAnimations halts both the starfield and logo animation chains.
func (l *landing) stopAnimations() {
	l.star.stopTicks()
	l.logo.stopShine()
}

// setStatus refreshes the menu for what is in the directory now (after
// setup or an action).
func (l *landing) setStatus(status stackStatus) {
	if l.status == status {
		return
	}
	l.status = status
	l.dev = false
	l.rebuildMenu()
}

func (l *landing) setSize(width, height int) {
	l.width, l.height = width, height
	l.star.resize(width, height)
	// An open dialog is sized once at open time; huh recomputes its group
	// viewport geometry only in its WindowSizeMsg handler, so re-feed the
	// live form the synthetic resize on every change (as wizardScreen does).
	// Without this, shrinking the terminal while a dialog is open leaves it
	// laid out for the old size, clipping content below the fold.
	if l.form != nil {
		l.form = l.sizeForm(l.form)
	}
}

func (l *landing) update(msg tea.Msg) (*landing, tea.Cmd) {
	// Animation ticks MUST be handled before the form gate: tick chains only
	// continue when each tick is rescheduled, so letting an open confirm
	// swallow one would freeze the starfield permanently.
	switch msg := msg.(type) {
	case starTickMsg:
		return l, l.star.update(msg)
	case logoShineStartMsg, logoShineStepMsg:
		return l, l.logo.update(msg)
	}

	if l.leaving {
		return l, nil
	}

	// Confirm dialog consumes everything else while open.
	if l.form != nil {
		return l.updateForm(msg)
	}

	if key, ok := msg.(tea.KeyPressMsg); ok {
		return l.handleKey(key)
	}
	return l, nil
}

func (l *landing) handleKey(msg tea.KeyPressMsg) (*landing, tea.Cmd) {
	switch msg.String() {
	case "q", "ctrl+c":
		return l, tea.Quit
	case "up", "k":
		l.menu.move(-1)
	case "down", "j":
		l.menu.move(1)
	case "esc":
		if l.dev {
			l.dev = false
			l.rebuildMenu()
		}
	case "enter":
		return l.choose(l.menu.selectedItem().ID)
	}
	return l, nil
}

func (l *landing) choose(id string) (*landing, tea.Cmd) {
	l.result = "" // any navigation retires the last result line
	switch id {
	case "quit":
		return l, tea.Quit
	case "back":
		l.dev = false
		l.rebuildMenu()
		return l, nil
	case "devmenu":
		l.dev = true
		l.rebuildMenu()
		return l, nil
	case "dashboard":
		return l.leave(func() tea.Msg { return openDashboardMsg{} })
	case "setup":
		return l.leave(func() tea.Msg { return openWizardMsg{} })
	case "resume":
		return l.leave(func() tea.Msg { return resumeSetupMsg{} })
	case "preflight":
		// Read-only, so it runs without asking.
		return l.leave(runAction(preflightAction(l.status == noStack || l.status == untrustedStack)))
	case "dryrun":
		return l.startConfirm(dryRunAction())
	case "update":
		return l.startConfirm(dashboard.UpdateAction())
	case "migrate":
		return l.startConfirm(dashboard.MigrateAction())
	case "loaddata":
		return l.leave(func() tea.Msg { return openLoadDataMsg{} })
	case "demo":
		return l.leave(func() tea.Msg { return openLoadDataMsg{kind: kindDemo} })
	case "dictionary":
		return l.startPicker("Rebuild the dictionary",
			"The stack must be up. Loading a dictionary CSV or facets is\n"+
				"CLI-only: see pic-sure dictionary --help.",
			"hydrate",
			[]huh.Option[string]{
				huh.NewOption("Rebuild it from the HPDS data (dictionary hydrate)", "hydrate"),
				huh.NewOption("Recompute the search weights (dictionary weights)", "weights"),
				huh.NewOption("Cancel", ""),
			},
			dictionaryAction)
	case "devon":
		return l.startDevPicker(true)
	case "devoff":
		return l.startDevPicker(false)
	case "branch":
		return l.startBranchInput()
	case "reset":
		return l.startTeardown(false)
	case "destroy":
		return l.startTeardown(true)
	}
	return l, nil
}

func (l *landing) leave(cmd tea.Cmd) (*landing, tea.Cmd) {
	l.leaving = true
	return l, cmd
}

func runAction(act dashboard.Action) tea.Cmd {
	return func() tea.Msg { return dashboard.RunMsg{Action: act} }
}

// preflightAction is `doctor`; with noStack it checks only the host and
// Docker.
func preflightAction(noStack bool) dashboard.Action {
	return dashboard.Action{
		Title:   "Preflight check",
		Done:    "Preflight check passed",
		Args:    []string{"doctor"},
		NoStack: noStack,
	}
}

func dryRunAction() (dashboard.Action, dashboard.Confirmation) {
	return dashboard.Action{
			Title: "Planning the update",
			Done:  "Update plan (nothing was changed)",
			Args:  []string{"update", "--dry-run"},
		}, dashboard.Confirmation{Question: "Preview the update?",
			Describe: "Shows what Update would change: the config, the component commits,\n" +
				"the images, the migrations, the token and the restarts. It changes\n" +
				"nothing in the stack, but may start the database to compare its\n" +
				"migrations."}
}

func dictionaryAction(sub string) dashboard.Action {
	if sub == "weights" {
		return dashboard.Action{Title: "Recomputing the search weights", Done: "Search weights recomputed",
			Args: []string{"dictionary", "weights"}}
	}
	return dashboard.Action{Title: "Rebuilding the dictionary", Done: "Dictionary rebuilt",
		Args: []string{"dictionary", "hydrate"}}
}

func devAction(on bool, service string) dashboard.Action {
	if on {
		return dashboard.Action{Title: "Turning on dev mode for " + service,
			Done: service + " runs from local source", Args: []string{"dev", "on", service}}
	}
	return dashboard.Action{Title: "Turning off dev mode for " + service,
		Done: "Dev mode is off for " + service, Args: []string{"dev", "off", service}}
}

func branchAction(branch string) dashboard.Action {
	return dashboard.Action{
		Title: "Switching the release branch",
		Done:  fmt.Sprintf("release.branch is now %s; choose Update to move the stack to it", branch),
		Args:  []string{"config", "set", "release.branch", branch},
	}
}

// sizeForm fits a landing dialog form to the landing's centre column
// (dialog.Fit).
func (l *landing) sizeForm(f *huh.Form) *huh.Form {
	width := max(min(l.width-4, 76), 40) // floor: l.width is 0 pre-resize
	height := 40
	if l.height > 0 {
		height = max(l.height-4, 8)
	}
	return dialog.Fit(f, width, height)
}

// startPicker opens a single-select dialog; picking a value is the consent.
// Value is bound before Options, or huh's cursor starts on the wrong row.
func (l *landing) startPicker(title, description, preselect string, opts []huh.Option[string], makeAction func(string) dashboard.Action) (*landing, tea.Cmd) {
	l.picked = preselect
	l.pickerMake = makeAction
	l.form = l.sizeForm(huh.NewForm(huh.NewGroup(
		huh.NewSelect[string]().
			Title(title).
			Description(description).
			Value(&l.picked).
			Options(opts...),
	)))
	return l, l.form.Init()
}

// startDevPicker opens the service picker for `dev on` (every service with
// a dev mode, as `dev list` shows them) or `dev off` (those that are on).
func (l *landing) startDevPicker(on bool) (*landing, tea.Cmd) {
	cfg, err := readConfig(l.root)
	if err != nil {
		l.result = "can't read the stack's config: " + err.Error()
		return l, nil
	}
	var opts []huh.Option[string]
	for _, v := range ops.DevList(cfg) {
		switch {
		case !on && v.On:
			opts = append(opts, huh.NewOption(v.Name, v.Name))
		case on && v.Source == "":
			opts = append(opts, huh.NewOption(v.Name+" (no source set)", v.Name))
		case on && v.On:
			opts = append(opts, huh.NewOption(v.Name+" (on)", v.Name))
		case on:
			opts = append(opts, huh.NewOption(v.Name, v.Name))
		}
	}
	if len(opts) == 0 {
		l.result = "no service is in dev mode"
		if on {
			l.result = "no service has a dev mode"
		}
		return l, nil
	}
	first := opts[0].Value
	opts = append(opts, huh.NewOption("Cancel", ""))
	if on {
		return l.startPicker("Dev mode on",
			"Builds the service's component from its local checkout and recreates\n"+
				"it with a debug port. Set the checkout first: pic-sure config set\n"+
				"components.<component>.source DIR.",
			first, opts, func(s string) dashboard.Action { return devAction(true, s) })
	}
	return l.startPicker("Dev mode off",
		"Recreates the service without its debug port. It keeps running the\n"+
			"build of its source until you unset the source and run pic-sure up.",
		first, opts, func(s string) dashboard.Action { return devAction(false, s) })
}

// startBranchInput asks for the release branch, prefilled with the current
// one. Typing a branch is the consent; an empty one cancels.
func (l *landing) startBranchInput() (*landing, tea.Cmd) {
	cfg, err := readConfig(l.root)
	if err != nil {
		l.result = "can't read the stack's config: " + err.Error()
		return l, nil
	}
	l.inputVal = cfg.Release.Branch
	l.inputMake = branchAction
	l.form = l.sizeForm(huh.NewForm(huh.NewGroup(
		huh.NewInput().
			Title("Switch the release branch").
			Description("Sets release.branch in pic-sure.yaml. Choose Update afterwards to\n" +
				"move the stack to the branch's release (empty input cancels).").
			Value(&l.inputVal),
	)).WithShowHelp(true))
	return l, l.form.Init()
}

func (l *landing) startConfirm(act dashboard.Action, c dashboard.Confirmation) (*landing, tea.Cmd) {
	l.pending = &act
	l.confirmOK = false
	l.form = l.sizeForm(dialog.ConfirmForm(c.Question, c.Describe, &l.confirmOK))
	return l, l.form.Init()
}

// startTeardown opens the dashboard's typed confirmation for reset or
// destroy: the user types the stack's name.
func (l *landing) startTeardown(destroy bool) (*landing, tea.Cmd) {
	cfg, err := readConfig(l.root)
	if err != nil {
		l.result = "can't read the stack's config: " + err.Error()
		return l, nil
	}
	l.teardown = true
	l.teardownName = cfg.Name
	l.teardownDestroy = destroy
	l.keepDB = false
	l.confirmText = ""
	l.form = l.sizeForm(dialog.TeardownForm(cfg.Name, l.root, destroy, &l.keepDB, &l.confirmText))
	return l, l.form.Init()
}

func (l *landing) closeForm() {
	l.form, l.pending, l.pickerMake, l.inputMake = nil, nil, nil, nil
	l.teardown = false
}

func (l *landing) updateForm(msg tea.Msg) (*landing, tea.Cmd) {
	// huh ships its esc binding disabled (only ctrl+c aborts a form), but
	// every dialog here advertises "esc cancels": intercept it, exactly as
	// the wizard screen does.
	if key, ok := msg.(tea.KeyPressMsg); ok && key.String() == "esc" {
		l.closeForm()
		return l, nil
	}

	form, cmd := l.form.Update(msg)
	if f, ok := form.(*huh.Form); ok {
		l.form = f
	}
	switch l.form.State {
	case huh.StateCompleted:
		var act *dashboard.Action
		switch {
		case l.teardown:
			// The form's own validation gates real input.
			if l.confirmText == l.teardownName {
				a := dashboard.TeardownAction(l.teardownDestroy, l.keepDB)
				act = &a
			}
		case l.inputMake != nil:
			if val := strings.TrimSpace(l.inputVal); val != "" {
				a := l.inputMake(val)
				act = &a
			}
		case l.pickerMake != nil:
			if l.picked != "" { // "" is Cancel
				a := l.pickerMake(l.picked)
				act = &a
			}
		case l.confirmOK:
			act = l.pending
		}
		l.closeForm()
		if act == nil {
			return l, nil
		}
		return l.leave(runAction(*act))
	case huh.StateAborted:
		l.closeForm()
		return l, nil
	}
	return l, cmd
}

// view composites the content block over the starfield: full starfield rows
// above/below, starfield margins beside each content row. When the content
// is taller than the terminal, the logo is dropped first so a confirm form
// is never cut off below the fold.
func (l *landing) view() string {
	if l.width < 20 || l.height < 10 {
		return "PIC-SURE\n(terminal too small)"
	}
	l.star.computeGrid()

	content := l.contentLines(true)
	if len(content) > l.height {
		content = l.contentLines(false)
	}
	return l.composite(content)
}

func (l *landing) contentLines(withLogo bool) []string {
	var content []string
	switch {
	case !withLogo:
		// nothing — body starts immediately
	case logoWidth()+4 <= l.width:
		content = append(content, strings.Split(l.logo.view(), "\n")...)
		content = append(content, "")
	default:
		// Compact one-line wordmark when the block art doesn't fit (~20 cols
		// minimum): styled with the brand hue + bold so the identity survives
		// the narrow-terminal fallback. Plain-font brackets (▌/▐) provide a
		// touch of visual structure without requiring Nerd Font glyphs.
		content = append(content, lipgloss.NewStyle().Bold(true).Foreground(styles.Brand).Render("▌ PIC-SURE ▐"), "")
	}

	if l.form != nil {
		content = append(content, strings.Split(l.form.View(), "\n")...)
	} else {
		menuWidth := min(max(l.width/3, 28), l.width-8)
		box := landingBoxStyle.Render(l.menu.view(menuWidth))
		content = append(content, strings.Split(box, "\n")...)
	}
	if l.notice != "" {
		content = append(content, "", landingResultStyle.Width(min(l.width-4, 80)).Render(l.notice))
	}
	if l.result != "" {
		content = append(content, "", landingResultStyle.Render(l.result))
	}
	content = append(content, "", landingFooterStyle.Render(l.footer()))
	return content
}

func (l *landing) footer() string {
	if l.form != nil {
		return "↑/↓ · enter · esc cancel"
	}
	if l.dev {
		return "↑/↓ select · enter · esc back · q quit"
	}
	return "↑/↓ select · enter · q quit"
}

func (l *landing) composite(content []string) string {
	top := max((l.height-len(content))/2, 0)
	rows := make([]string, 0, l.height)
	for row := 0; row < l.height; row++ {
		ci := row - top
		if ci < 0 || ci >= len(content) {
			rows = append(rows, l.star.renderRow(row, 0, l.width))
			continue
		}
		line := content[ci]
		lw := lipgloss.Width(line)
		left := max((l.width-lw)/2, 0)
		rows = append(rows,
			l.star.renderRow(row, 0, left)+line+l.star.renderRow(row, left+lw, l.width))
	}
	return strings.Join(rows, "\n")
}
