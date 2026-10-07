package tui

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/huh/v2"
	"charm.land/lipgloss/v2"

	"github.com/JamesPeck/pic-sure-cli/internal/dashboard"
	"github.com/JamesPeck/pic-sure-cli/internal/dialog"
	"github.com/JamesPeck/pic-sure-cli/internal/filebrowser"
	"github.com/JamesPeck/pic-sure-cli/internal/ops"
	"github.com/JamesPeck/pic-sure-cli/internal/phenoinput"
	"github.com/JamesPeck/pic-sure-cli/internal/styles"
)

// The "Load your data" screen (ScreenLoadData). It asks for what one
// `pic-sure data load-phenotype`, `data demo` or `data load-genomic` needs,
// and on consent sends loadRunMsg, which the app runs in-process on the
// run screen.
//
// Step state machine (loadStep):
//
//	loadKind ─file─▶ loadPhenoFile ─(≥2 CSVs)─▶ loadPhenoArchiveEntry ─┐
//	   │                  └──────────────────────────────────────────┤
//	   ├─dir──▶ loadPhenoDir ─────────────────────────────────────────┤
//	   │                                                              ▼
//	   │                                      loadPhenoHeap ─▶ loadPhenoDict
//	   │                                             auto ──────────┤
//	   │                         custom ─▶ loadPhenoDatasets         │
//	   │                                  ─▶ loadPhenoConcepts       │
//	   │                                  ─▶ loadPhenoFacetsAsk      │
//	   │                         no ──────────────────────┐ │ yes    │
//	   │                                                  │ ▼        │
//	   │                         loadPhenoFacetCategories            │
//	   │                                 ─▶ loadPhenoFacets          │
//	   │                                 ─▶ loadPhenoFacetConcepts   │
//	   │                                                  ▼          ▼
//	   ├─demo─▶ loadDemoDataset ─▶ loadPhenoHeap ───────▶ loadConfirm ─▶ run
//	   │
//	   ▼ genomic
//	loadGenomicIndex ─▶ loadGenomicDirAsk ──no──▶ loadGenomicPartition
//	                          │ yes                        │
//	                          ▼                            ▼
//	                    loadGenomicDir ─────────▶ loadGenomicHeap
//	                                                       │
//	                                                       ▼
//	                            loadGenomicPromote ─▶ loadGenomicProfile
//	                                                       │
//	                                                       ▼
//	                                            loadGenomicConfirm ─▶ run
type loadStep int

const (
	loadKind loadStep = iota
	loadPhenoFile
	loadPhenoArchiveEntry
	loadPhenoDir
	loadDemoDataset
	loadPhenoHeap
	loadPhenoDict
	loadPhenoDatasets
	loadPhenoConcepts
	loadPhenoFacetsAsk
	loadPhenoFacetCategories
	loadPhenoFacets
	loadPhenoFacetConcepts
	loadConfirm

	loadGenomicIndex
	loadGenomicDirAsk
	loadGenomicDir
	loadGenomicPartition
	loadGenomicHeap
	loadGenomicPromote
	loadGenomicProfile
	loadGenomicConfirm
)

// The kinds of load the first step offers.
const (
	kindFile    = "file"
	kindDir     = "dir"
	kindDemo    = "demo"
	kindGenomic = "genomic"
)

// The heap (MB) each kind's input opens with: the commands' own defaults.
var defaultHeaps = map[string]string{
	kindFile:    "4096",
	kindDir:     "8000",
	kindDemo:    "4096",
	kindGenomic: "16000",
}

// minHeapMB is the smallest heap the wizard takes. Anything below it is
// almost certainly gigabytes typed as megabytes ("16" for 16 GB).
const minHeapMB = 256

// fetchArchiveCSVs lists the CSV entries of the file picked for a phenotype
// load, and rejects a file the load would refuse. A package var so tests
// inject entries. It reads the file, so it runs in a tea.Cmd, never in
// Update.
var fetchArchiveCSVs = phenoinput.ListCSVEntries

// checkPhenotypeDir checks a directory picked for an --input-dir load, as
// the load will. A package var for tests; it runs in a tea.Cmd too.
var checkPhenotypeDir = ops.CheckPhenotypeDir

// inspectFillMsg carries the result of the async check of a picked file or
// directory. seq stamps the pick it was made for, so a result that arrives
// after the step was re-entered or the screen closed is dropped.
type inspectFillMsg struct {
	seq     int
	entries []string
	err     error
}

// openLoadDataMsg asks the app to open the load screen. kind, if set, skips
// the kind step (the developer menu's demo entry opens on the datasets).
type openLoadDataMsg struct{ kind string }

// loadDataClosedMsg tells the app to leave the load screen (aborted=true when
// the user cancelled, mirroring wizardClosedMsg's neutral-result behavior).
type loadDataClosedMsg struct{ aborted bool }

// loadRunMsg asks the app to run the load's command line on the run screen.
type loadRunMsg struct{ act dashboard.Action }

var (
	loadTitleStyle  = lipgloss.NewStyle().Bold(true).Foreground(styles.Brand).Padding(0, 1)
	loadFooterStyle = lipgloss.NewStyle().Faint(true).Padding(0, 1)
)

type loadScreen struct {
	ctx  context.Context
	root string
	step loadStep

	// Exactly one of form / fb is live per step: huh forms drive the
	// select/input/confirm steps; fb drives the file steps. A file step builds
	// a FRESH filebrowser, so its Selected() poll can never observe a stale
	// selection carried over from a previous step (the "consume once, advance"
	// pattern — see consumeFile).
	form *huh.Form
	fb   filebrowser.Model

	// Collected values.
	kind            string // kindFile, kindDir, kindDemo, kindGenomic, or "" (cancel)
	file            string
	archiveEntry    string // chosen CSV inside a multi-CSV archive (--entry); "" otherwise
	inputDir        string
	demo            string
	heap            string
	dictMode        string // "auto" | "custom"
	includeFacets   bool
	datasets        string
	concepts        string
	facetCategories string
	facets          string
	facetConcepts   string
	confirmed       bool

	// Genomic-branch collected values.
	vcfIndex      string
	includeVCFDir bool
	vcfDir        string
	partition     string
	promote       bool
	enableProfile bool

	// discarding raises the one-keystroke "Discard data load? (y/n)" confirm on
	// esc once any data has been collected, so a multi-step flow is not silently
	// thrown away by a reflexive esc. A pristine screen closes immediately.
	discarding bool

	// A picked phenotype file or directory is checked asynchronously: a file
	// for the CSVs it holds (≥2 → entry picker), a directory as --input-dir
	// would check it. While the check runs inspecting is true (the view shows
	// a placeholder and the filebrowser is parked); inspectSeq stamps each
	// check so a stale result is dropped; inspectErr shows a rejection above
	// the re-opened browser.
	inspecting     bool
	inspectSeq     int
	archiveEntries []string
	inspectErr     string

	width, height int
}

// newLoadScreen opens on the kind step, or straight on kind's first step.
func newLoadScreen(ctx context.Context, root, kind string) *loadScreen {
	// kind is pre-set to the first real option so the huh select cursor
	// starts there on first paint. Without this, s.kind="" collides with
	// Cancel's value "" and huh preselects Cancel.
	s := &loadScreen{ctx: ctx, root: root, step: loadKind, dictMode: "auto", kind: kindFile, demo: ops.DemoDatasets()[0]}
	if kind != "" {
		s.kind = kind
		s.heap = defaultHeaps[kind]
		s.step = firstStep(kind)
	}
	return s
}

// init builds and starts the opening step (the app sizes the screen first).
func (s *loadScreen) init() tea.Cmd {
	_, cmd := s.enterStep(s.step)
	return cmd
}

func firstStep(kind string) loadStep {
	switch kind {
	case kindDir:
		return loadPhenoDir
	case kindDemo:
		return loadDemoDataset
	case kindGenomic:
		return loadGenomicIndex
	}
	return loadPhenoFile
}

func (s *loadScreen) setSize(width, height int) {
	s.width, s.height = width, height
	if s.form != nil {
		s.form = s.sizeForm(s.form)
	}
	if isFileStep(s.step) {
		s.fb.SetSize(s.fbWidth(), s.fbHeight())
	}
}

// sizeForm fits the active form to the screen (dialog.Fit), as
// wizardScreen.applySize and landing.sizeForm do.
func (s *loadScreen) sizeForm(f *huh.Form) *huh.Form {
	return dialog.Fit(f, s.formWidth(), s.formHeight())
}

func (s *loadScreen) formWidth() int { return max(min(s.width-4, 76), 40) }

func (s *loadScreen) formHeight() int {
	if s.height <= 0 {
		return 40 // unsized yet: don't constrain content
	}
	return max(s.height-4, 8)
}

func (s *loadScreen) fbWidth() int { return max(s.width-4, 20) }

func (s *loadScreen) fbHeight() int {
	if s.height <= 0 {
		return 20
	}
	return max(s.height-6, 5)
}

// isFileStep reports whether step is one of the filebrowser-driven steps.
func isFileStep(step loadStep) bool {
	switch step {
	case loadPhenoFile, loadPhenoDir, loadPhenoDatasets, loadPhenoConcepts,
		loadPhenoFacetCategories, loadPhenoFacets, loadPhenoFacetConcepts,
		loadGenomicIndex, loadGenomicDir:
		return true
	}
	return false
}

func (s *loadScreen) update(msg tea.Msg) (*loadScreen, tea.Cmd) {
	// The async check's result is handled before any gate so it reaches the
	// inspecting file step regardless of which overlay (discard prompt) is up;
	// its own seq guard drops a result for a since-re-entered/closed step.
	if fill, ok := msg.(inspectFillMsg); ok {
		return s.applyInspectFill(fill)
	}

	// A discard confirm owns the keyboard until answered. Swallow every
	// non-key message (huh/filepicker ticks) so the prompt stays put.
	if s.discarding {
		key, ok := msg.(tea.KeyPressMsg)
		if !ok {
			return s, nil
		}
		switch key.String() {
		case "y", "Y":
			return s, closeLoad(true)
		case "n", "N", "esc":
			s.discarding = false
		}
		return s, nil
	}

	// huh and the filepicker both ship esc disabled, but the footer advertises
	// "esc cancel" — intercept it here (as wizardScreen does). A screen with
	// collected input asks to confirm first; a pristine one closes immediately.
	if key, ok := msg.(tea.KeyPressMsg); ok && key.String() == "esc" {
		if s.dirty() {
			s.discarding = true
			return s, nil
		}
		return s, closeLoad(true)
	}

	// While a pick is being checked the filebrowser is parked behind a
	// placeholder; only the fill (handled above) and esc/discard (above) act.
	// Swallow everything else so a stray filepicker tick can't re-select.
	if s.inspecting {
		return s, nil
	}

	if isFileStep(s.step) {
		var cmd tea.Cmd
		s.fb, cmd = s.fb.Update(msg)
		// Poll for a selection on this msg and consume it exactly once: the next
		// file step builds a fresh browser, so a fresh selection is never
		// confused with this step's.
		if path, ok := s.fb.Selected(); ok {
			return s.consumeFile(path)
		}
		return s, cmd
	}
	return s.updateForm(msg)
}

// updateForm pumps the active huh form and acts on its terminal state.
func (s *loadScreen) updateForm(msg tea.Msg) (*loadScreen, tea.Cmd) {
	form, cmd := s.form.Update(msg)
	if f, ok := form.(*huh.Form); ok {
		s.form = f
	}
	switch s.form.State {
	case huh.StateAborted:
		return s, closeLoad(true)
	case huh.StateCompleted:
		return s.formCompleted()
	}
	return s, cmd
}

// formCompleted routes a completed huh form to the next step.
func (s *loadScreen) formCompleted() (*loadScreen, tea.Cmd) {
	switch s.step {
	case loadKind:
		if s.kind == "" { // Cancel
			return s, closeLoad(true)
		}
		s.heap = defaultHeaps[s.kind]
		return s.enterStep(firstStep(s.kind))
	case loadPhenoArchiveEntry, loadDemoDataset:
		return s.enterStep(loadPhenoHeap)
	case loadPhenoHeap:
		if s.kind == kindDemo {
			return s.enterStep(loadConfirm)
		}
		return s.enterStep(loadPhenoDict)
	case loadPhenoDict:
		if s.dictMode == "custom" {
			return s.enterStep(loadPhenoDatasets)
		}
		return s.enterStep(loadConfirm)
	case loadPhenoFacetsAsk:
		if s.includeFacets {
			return s.enterStep(loadPhenoFacetCategories)
		}
		return s.enterStep(loadConfirm)
	case loadConfirm:
		if !s.confirmed {
			return s, closeLoad(true)
		}
		return s, s.dispatch()

	case loadGenomicDirAsk:
		if s.includeVCFDir {
			return s.enterStep(loadGenomicDir)
		}
		return s.enterStep(loadGenomicPartition)
	case loadGenomicPartition:
		return s.enterStep(loadGenomicHeap)
	case loadGenomicHeap:
		return s.enterStep(loadGenomicPromote)
	case loadGenomicPromote:
		return s.enterStep(loadGenomicProfile)
	case loadGenomicProfile:
		return s.enterStep(loadGenomicConfirm)
	case loadGenomicConfirm:
		if !s.confirmed {
			return s, closeLoad(true)
		}
		return s, s.dispatch()
	}
	return s, nil
}

// consumeFile stores a just-selected path into the field for the current file
// step and advances. Tests drive the state machine by calling it directly
// rather than through the real filepicker.
func (s *loadScreen) consumeFile(path string) (*loadScreen, tea.Cmd) {
	switch s.step {
	case loadPhenoFile:
		s.file = path
		s.archiveEntry = "" // a new pick invalidates any prior entry choice
		return s.startInspection(func(ctx context.Context) ([]string, error) {
			return fetchArchiveCSVs(ctx, path)
		})
	case loadPhenoDir:
		s.inputDir = path
		return s.startInspection(func(context.Context) ([]string, error) {
			return nil, checkPhenotypeDir(path)
		})
	case loadPhenoDatasets:
		s.datasets = path
		return s.enterStep(loadPhenoConcepts)
	case loadPhenoConcepts:
		s.concepts = path
		return s.enterStep(loadPhenoFacetsAsk)
	case loadPhenoFacetCategories:
		s.facetCategories = path
		return s.enterStep(loadPhenoFacets)
	case loadPhenoFacets:
		s.facets = path
		return s.enterStep(loadPhenoFacetConcepts)
	case loadPhenoFacetConcepts:
		s.facetConcepts = path
		return s.enterStep(loadConfirm)
	case loadGenomicIndex:
		s.vcfIndex = path
		return s.enterStep(loadGenomicDirAsk)
	case loadGenomicDir:
		s.vcfDir = path
		return s.enterStep(loadGenomicPartition)
	}
	return s, nil
}

// startInspection parks the file step behind a placeholder and runs check
// in a tea.Cmd. The step stays put while inspecting (the view keys off
// s.inspecting); the fill handler advances it.
func (s *loadScreen) startInspection(check func(context.Context) ([]string, error)) (*loadScreen, tea.Cmd) {
	s.inspecting = true
	s.inspectErr = ""
	s.archiveEntries = nil
	s.inspectSeq++
	seq, ctx := s.inspectSeq, s.ctx
	return s, func() tea.Msg {
		entries, err := check(ctx)
		return inspectFillMsg{seq: seq, entries: entries, err: err}
	}
}

// applyInspectFill lands the async check's result, if it is still the one
// awaited:
//   - an error reopens the browser with the reason, to pick again;
//   - ≥2 CSV entries open the entry picker;
//   - otherwise (a plain CSV, a gzip, a one-CSV archive, a directory) the
//     load needs no --entry, and the heap is next.
func (s *loadScreen) applyInspectFill(msg inspectFillMsg) (*loadScreen, tea.Cmd) {
	if !s.inspecting || msg.seq != s.inspectSeq {
		return s, nil
	}
	s.inspecting = false
	if msg.err != nil {
		what := "file"
		if s.step == loadPhenoDir {
			what = "directory"
			s.inputDir = ""
		} else {
			s.file = ""
		}
		s.inspectErr = fmt.Sprintf("can't load that %s: %v", what, msg.err)
		return s.enterStep(s.step)
	}
	if len(msg.entries) >= 2 {
		s.archiveEntries = msg.entries
		// Preselect the first entry so the select cursor starts on a real option.
		s.archiveEntry = msg.entries[0]
		return s.enterStep(loadPhenoArchiveEntry)
	}
	return s.enterStep(loadPhenoHeap)
}

// enterStep sets the step and constructs (and sizes) its form or filebrowser,
// returning the model's Init command.
func (s *loadScreen) enterStep(step loadStep) (*loadScreen, tea.Cmd) {
	s.step = step
	var form *huh.Form
	switch step {
	case loadKind:
		form = s.buildKindForm()
	case loadPhenoFile:
		// The filebrowser matches suffixes, so ".gz" also admits ".csv.gz" and
		// ".tar.gz". The load detects the format by content.
		return s.openBrowser([]string{".csv", ".gz", ".tgz", ".tar", ".zip"}, "Select the phenotype CSV, or an archive holding one")
	case loadPhenoArchiveEntry:
		form = s.buildArchiveEntryForm()
	case loadPhenoDir:
		return s.openDirBrowser("Select the directory of phenotype CSVs")
	case loadDemoDataset:
		form = s.buildDemoForm()
	case loadPhenoHeap, loadGenomicHeap:
		form = s.buildHeapForm()
	case loadPhenoDict:
		form = s.buildDictForm()
	case loadPhenoDatasets:
		return s.openBrowser([]string{".csv"}, "Select datasets.csv")
	case loadPhenoConcepts:
		return s.openBrowser([]string{".zip"}, "Select concepts.zip")
	case loadPhenoFacetsAsk:
		form = s.buildFacetsForm()
	case loadPhenoFacetCategories:
		return s.openBrowser([]string{".csv"}, "Select facet_categories.csv")
	case loadPhenoFacets:
		return s.openBrowser([]string{".csv"}, "Select facets.csv")
	case loadPhenoFacetConcepts:
		return s.openBrowser([]string{".csv"}, "Select facet_concepts.csv")
	case loadConfirm:
		form = s.buildConfirmForm()

	case loadGenomicIndex:
		return s.openBrowser([]string{".tsv"}, "Select the VCF index TSV")
	case loadGenomicDirAsk:
		form = s.buildGenomicDirAskForm()
	case loadGenomicDir:
		return s.openDirBrowser("Select the directory of VCF files")
	case loadGenomicPartition:
		form = s.buildPartitionForm()
	case loadGenomicPromote:
		form = s.buildPromoteForm()
	case loadGenomicProfile:
		form = s.buildProfileForm()
	case loadGenomicConfirm:
		form = s.buildGenomicConfirmForm()
	default:
		return s, nil
	}
	s.form = s.sizeForm(form)
	return s, s.form.Init()
}

func (s *loadScreen) openBrowser(exts []string, title string) (*loadScreen, tea.Cmd) {
	s.fb = filebrowser.New(filebrowser.Options{AllowedExts: exts, Title: title, StartDir: s.root})
	s.fb.SetSize(s.fbWidth(), s.fbHeight())
	return s, s.fb.Init()
}

// openDirBrowser opens a DirMode filebrowser for selecting a directory.
func (s *loadScreen) openDirBrowser(title string) (*loadScreen, tea.Cmd) {
	s.fb = filebrowser.New(filebrowser.Options{DirMode: true, Title: title, StartDir: s.root})
	s.fb.SetSize(s.fbWidth(), s.fbHeight())
	return s, s.fb.Init()
}

// dirty reports whether any data has been collected (any path set). The
// kind, dataset, dictionary mode and heap are not "collected input", so a
// screen with only those closes at once.
func (s *loadScreen) dirty() bool {
	return s.file != "" || s.inputDir != "" || s.datasets != "" || s.concepts != "" ||
		s.facetCategories != "" || s.facets != "" || s.facetConcepts != "" ||
		s.vcfIndex != "" || s.vcfDir != ""
}

// args is the pic-sure command line the collected values make. Paths are
// absolute, so the command doesn't depend on the working directory.
func (s *loadScreen) args() []string {
	heap := strings.TrimSpace(s.heap)
	switch s.kind {
	case kindDemo:
		return []string{"data", "demo", s.demo, "--heap", heap}
	case kindGenomic:
		args := []string{"data", "load-genomic", "--partition", strings.TrimSpace(s.partition),
			"--vcf-index", absPath(s.vcfIndex)}
		if s.includeVCFDir && s.vcfDir != "" {
			args = append(args, "--vcf-dir", absPath(s.vcfDir))
		}
		args = append(args, "--heap", heap)
		if s.promote {
			args = append(args, "--promote")
		}
		if s.enableProfile {
			args = append(args, "--enable-profile")
		}
		return args
	}
	args := []string{"data", "load-phenotype"}
	if s.kind == kindDir {
		args = append(args, "--input-dir", absPath(s.inputDir))
	} else {
		args = append(args, "--file", absPath(s.file))
		if s.archiveEntry != "" {
			args = append(args, "--entry", s.archiveEntry)
		}
	}
	args = append(args, "--heap", heap)
	if s.dictMode == "custom" {
		args = append(args, "--dictionary", "custom",
			"--datasets", absPath(s.datasets), "--concepts", absPath(s.concepts))
		if s.includeFacets {
			args = append(args, "--facets-categories", absPath(s.facetCategories),
				"--facets", absPath(s.facets), "--facet-concepts", absPath(s.facetConcepts))
		}
	}
	return args
}

func absPath(p string) string {
	if abs, err := filepath.Abs(p); err == nil {
		return abs
	}
	return p
}

// action is the run screen's title, success line and command line.
func (s *loadScreen) action() dashboard.Action {
	act := dashboard.Action{Title: "Loading phenotype data", Done: "Phenotype data loaded", Args: s.args()}
	switch s.kind {
	case kindDemo:
		act.Title, act.Done = "Loading the "+s.demo+" demo data", "Demo data loaded"
	case kindGenomic:
		act.Title, act.Done = "Loading genomic partition "+strings.TrimSpace(s.partition), "Genomic data loaded"
	}
	return act
}

func (s *loadScreen) dispatch() tea.Cmd {
	act := s.action()
	return func() tea.Msg { return loadRunMsg{act: act} }
}

func closeLoad(aborted bool) tea.Cmd {
	return func() tea.Msg { return loadDataClosedMsg{aborted: aborted} }
}

// --- form builders (Value bound before Options, per the huh gotcha) ---------

func (s *loadScreen) buildKindForm() *huh.Form {
	// No .Title here: the screen already renders the "Load your data" header via
	// loadTitleStyle, so a form title would double-render it on the kind step.
	return huh.NewForm(huh.NewGroup(
		huh.NewSelect[string]().
			Description("Choose the kind of data to load into PIC-SURE.").
			Value(&s.kind).
			Options(
				huh.NewOption("Phenotype CSV, or an archive holding one", kindFile),
				huh.NewOption("Directory of phenotype CSVs", kindDir),
				huh.NewOption("Demo dataset", kindDemo),
				huh.NewOption("Genomic data (VCF)", kindGenomic),
				huh.NewOption("Cancel", ""),
			),
	))
}

// buildArchiveEntryForm lists the CSV entries found inside a multi-CSV archive
// so the user picks which one to load (--entry). No Cancel option: esc backs
// out (the screen's esc handler owns cancel), and the cursor preselects the
// first entry.
func (s *loadScreen) buildArchiveEntryForm() *huh.Form {
	opts := make([]huh.Option[string], 0, len(s.archiveEntries))
	for _, e := range s.archiveEntries {
		opts = append(opts, huh.NewOption(e, e))
	}
	return huh.NewForm(huh.NewGroup(
		huh.NewSelect[string]().
			Title("Choose the CSV to load").
			Description("This archive holds several CSVs; pick the one to load.").
			Value(&s.archiveEntry).
			Options(opts...),
	))
}

func (s *loadScreen) buildDemoForm() *huh.Form {
	labels := map[string]string{
		"nhanes":      "NHANES",
		"synthea":     "Synthea 10k",
		"1000genomes": "1000 Genomes (phenotypes)",
		ops.DemoAll:   "All three combined",
	}
	var opts []huh.Option[string]
	for _, name := range ops.DemoDatasets() {
		label := labels[name]
		if label == "" {
			label = name
		}
		opts = append(opts, huh.NewOption(label, name))
	}
	return huh.NewForm(huh.NewGroup(
		huh.NewSelect[string]().
			Title("Demo dataset").
			Description("Downloaded once from hms-dbmi/pic-sure-public-datasets and cached.").
			Value(&s.demo).
			Options(opts...),
	))
}

func (s *loadScreen) buildHeapForm() *huh.Form {
	desc := "The loader's JVM heap in MB. 4096 suits up to about 1M rows."
	switch s.kind {
	case kindDir:
		desc = "The sequential loader's JVM heap in MB."
	case kindGenomic:
		desc = "Each VCF loader's JVM heap in MB. Raise it for large partitions."
	}
	return huh.NewForm(huh.NewGroup(
		huh.NewInput().
			Title("JVM heap size (MB)").
			Description(desc).
			Value(&s.heap).
			Validate(validateHeap),
	))
}

func (s *loadScreen) buildDictForm() *huh.Form {
	return huh.NewForm(huh.NewGroup(
		huh.NewSelect[string]().
			Title("Dictionary").
			Description("How to build the data dictionary for the loaded phenotype data.").
			Value(&s.dictMode).
			Options(
				huh.NewOption("Auto — rebuild dictionary from the loaded data (recommended)", "auto"),
				huh.NewOption("Custom — supply dictionary CSVs", "custom"),
			),
	))
}

func (s *loadScreen) buildFacetsForm() *huh.Form {
	return huh.NewForm(huh.NewGroup(
		huh.NewConfirm().
			Title("Include facet metadata?").
			Description("Optionally supply facet_categories.csv, facets.csv, and facet_concepts.csv\n(all three together, or none).").
			Affirmative("Yes").
			Negative("No").
			Value(&s.includeFacets),
	))
}

func (s *loadScreen) buildConfirmForm() *huh.Form {
	title := "⚠ Load phenotype data — this REPLACES the stack's phenotype data"
	if s.kind == kindDemo {
		title = "⚠ Load demo data — this REPLACES the stack's phenotype data"
	}
	return huh.NewForm(huh.NewGroup(
		huh.NewConfirm().
			Title(title).
			Description(s.confirmSummary()).
			Affirmative("Load").
			Negative("Cancel").
			Value(&s.confirmed),
	))
}

// --- genomic-branch form builders -------------------------------------------

func (s *loadScreen) buildGenomicDirAskForm() *huh.Form {
	return huh.NewForm(huh.NewGroup(
		huh.NewConfirm().
			Title("VCF directory").
			Description("Do the VCFs live outside the index's directory?\nEvery VCF the index names must be under the directory.").
			Affirmative("Yes").
			Negative("No").
			Value(&s.includeVCFDir),
	))
}

func (s *loadScreen) buildPartitionForm() *huh.Form {
	return huh.NewForm(huh.NewGroup(
		huh.NewInput().
			Title("Genomic partition").
			Description("letters/digits/_/- ; names the genomic dataset.").
			Value(&s.partition).
			Validate(validatePartition),
	))
}

func (s *loadScreen) buildPromoteForm() *huh.Form {
	return huh.NewForm(huh.NewGroup(
		huh.NewConfirm().
			Title("Promote this partition into HPDS's live genomic data?").
			Description("HPDS stops while the partition is copied in, and starts again.").
			Affirmative("Yes").
			Negative("No").
			Value(&s.promote),
	))
}

func (s *loadScreen) buildProfileForm() *huh.Form {
	return huh.NewForm(huh.NewGroup(
		huh.NewConfirm().
			Title("Enable the genomic HPDS profile now?").
			Description("HPDS reads genomic data only with this profile (bch-dev).").
			Affirmative("Yes").
			Negative("No").
			Value(&s.enableProfile),
	))
}

func (s *loadScreen) buildGenomicConfirmForm() *huh.Form {
	return huh.NewForm(huh.NewGroup(
		huh.NewConfirm().
			Title("⚠ Load genomic data").
			Description(s.genomicConfirmSummary()).
			Affirmative("Load").
			Negative("Cancel").
			Value(&s.confirmed),
	))
}

// validateHeap accepts a whole number of MB, at least minHeapMB.
func validateHeap(v string) error {
	v = strings.TrimSpace(v)
	if v == "" {
		return errors.New("heap is required, in MB (e.g. 4096)")
	}
	mb, err := strconv.Atoi(v)
	if err != nil || mb <= 0 {
		return errors.New("heap must be a whole number of MB, e.g. 4096")
	}
	if mb < minHeapMB {
		return fmt.Errorf("heap is in MB: %d MB is too small (4096 is 4 GB)", mb)
	}
	return nil
}

// partitionPattern is load-genomic's own partition rule.
var partitionPattern = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// validatePartition accepts a non-empty partition name of letters/digits/_/-.
func validatePartition(v string) error {
	v = strings.TrimSpace(v)
	if v == "" {
		return errors.New("partition is required")
	}
	if !partitionPattern.MatchString(v) {
		return errors.New("partition must match ^[A-Za-z0-9_-]+$ (letters/digits/_/-)")
	}
	return nil
}

// confirmSummary says what the load replaces, then lists the collected
// values: titles padded to the widest, a two-space gap, then the value.
func (s *loadScreen) confirmSummary() string {
	var lead string
	var rows [][2]string
	switch s.kind {
	case kindDemo:
		lead = "Downloads the dataset (or reuses the cached copy), replaces HPDS's\n" +
			"phenotype data with it, and rebuilds the dictionary."
		rows = append(rows, [2]string{"Dataset", s.demo})
	case kindDir:
		lead = "The sequential loader reads the directory while HPDS keeps running;\n" +
			"its output then replaces HPDS's phenotype data, and the dictionary is\n" +
			"rebuilt. A failed load leaves HPDS as it was."
		rows = append(rows, [2]string{"Directory", s.inputDir})
	default:
		lead = "HPDS stops, its phenotype data is replaced with this CSV, and the\n" +
			"dictionary is rebuilt. If the loader fails, HPDS stays stopped with no\n" +
			"phenotype data until a load succeeds."
		rows = append(rows, [2]string{"File", s.file})
		// For a multi-CSV archive the file line is the archive path; name the
		// chosen entry on its own line so the user sees exactly which CSV loads.
		if s.archiveEntry != "" {
			rows = append(rows, [2]string{"Archive entry", s.archiveEntry})
		}
	}
	rows = append(rows, [2]string{"Heap", strings.TrimSpace(s.heap) + " MB"})
	switch {
	case s.kind == kindDemo:
	case s.dictMode == "custom":
		rows = append(rows,
			[2]string{"Dictionary", "custom"},
			[2]string{"Datasets", s.datasets},
			[2]string{"Concepts", s.concepts},
		)
		if s.includeFacets {
			rows = append(rows,
				[2]string{"Facet categories", s.facetCategories},
				[2]string{"Facets", s.facets},
				[2]string{"Facet concepts", s.facetConcepts},
			)
		}
	default:
		rows = append(rows, [2]string{"Dictionary", "auto (rebuild from loaded data)"})
	}
	return lead + "\n\n" + alignRows(rows)
}

// genomicConfirmSummary says what the load does, warns when the profile is
// enabled without promoting this load, and lists the collected values.
func (s *loadScreen) genomicConfirmSummary() string {
	vcfDir := s.vcfDir
	if !s.includeVCFDir || vcfDir == "" {
		vcfDir = "(the index's directory)"
	}
	rows := [][2]string{
		{"Partition", strings.TrimSpace(s.partition)},
		{"VCF index", s.vcfIndex},
		{"VCF dir", vcfDir},
		{"Heap", strings.TrimSpace(s.heap) + " MB"},
		{"Promote", yesNo(s.promote)},
		{"Enable profile", yesNo(s.enableProfile)},
	}

	var b strings.Builder
	b.WriteString("Loads the VCFs into the partition's staging area, replacing any earlier\n" +
		"load of it. HPDS keeps running on its live data while the loaders run.\n")
	if s.enableProfile && !s.promote {
		// The screen can't know whether genomic data is already live.
		b.WriteString("⚠ You enabled the profile WITHOUT promoting this load. If HPDS has\n" +
			"  no promoted genomic data yet, the genomic profile has nothing to read.\n")
	}
	b.WriteString("\n")
	b.WriteString(alignRows(rows))
	return b.String()
}

// alignRows lays out title/value rows with the values in one column.
func alignRows(rows [][2]string) string {
	titleWidth := 0
	for _, r := range rows {
		titleWidth = max(titleWidth, lipgloss.Width(r[0]))
	}
	var b strings.Builder
	for _, r := range rows {
		pad := strings.Repeat(" ", titleWidth-lipgloss.Width(r[0]))
		fmt.Fprintf(&b, "%s%s  %s\n", r[0], pad, r[1])
	}
	return strings.TrimRight(b.String(), "\n")
}

// yesNo renders a bool as a confirm-summary "yes"/"no" cell.
func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

func (s *loadScreen) view() string {
	var body, footer string
	switch {
	case s.inspecting:
		// The check reads the picked file or directory; park the browser
		// behind a placeholder so the screen doesn't look frozen.
		body = loadTitleStyle.Render("(checking…)")
		footer = loadFooterStyle.Render("esc cancel")
	case isFileStep(s.step):
		fbView := s.fb.View()
		// Show why the previous pick was rejected above the re-opened browser.
		if s.inspectErr != "" {
			fbView = lipgloss.JoinVertical(lipgloss.Left, styles.Bad.Render(s.inspectErr), fbView)
		}
		body = fbView
		footer = loadFooterStyle.Render("enter select · esc cancel")
	default:
		body = s.form.View()
		footer = loadFooterStyle.Render("esc cancel")
	}
	if s.discarding {
		footer = loadFooterStyle.Render("Discard data load? (y/n)")
	}

	content := lipgloss.JoinVertical(lipgloss.Left,
		loadTitleStyle.Render("Load your data"), body, footer)
	if s.width == 0 || s.height == 0 {
		return content
	}
	return lipgloss.Place(s.width, s.height, lipgloss.Center, lipgloss.Center, content)
}
