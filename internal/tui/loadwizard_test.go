package tui

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/huh/v2"
	"charm.land/lipgloss/v2"

	"github.com/JamesPeck/pic-sure-cli/internal/dashboard"
)

// newTestLoad opens a sized load screen on root, on kind's first step if
// kind is set.
func newTestLoad(t *testing.T, root, kind string) *loadScreen {
	t.Helper()
	s := newLoadScreen(context.Background(), root, kind)
	s.setSize(100, 35)
	_ = s.init()
	return s
}

// completeForm forces the active huh form to its completed state and pumps a
// neutral message so the screen acts on it — the same bypass the landing and
// wizard tests use, since huh's interactive completion is not unit-testable.
func completeForm(s *loadScreen) (*loadScreen, tea.Cmd) {
	s.form.State = huh.StateCompleted
	return s.update(struct{}{})
}

// stubInspect replaces the archive lister and the directory check for one
// test; nil err with entries lists them, a non-nil err rejects the pick.
func stubInspect(t *testing.T, entries []string, err error) {
	t.Helper()
	origList, origDir := fetchArchiveCSVs, checkPhenotypeDir
	fetchArchiveCSVs = func(context.Context, string) ([]string, error) { return entries, err }
	checkPhenotypeDir = func(string) error { return err }
	t.Cleanup(func() { fetchArchiveCSVs, checkPhenotypeDir = origList, origDir })
}

// pick consumes path on the current file step and, if that starts a check,
// runs it and lands its result.
func pick(t *testing.T, s *loadScreen, path string) *loadScreen {
	t.Helper()
	s, cmd := s.consumeFile(path)
	if s.inspecting {
		fill, ok := cmd().(inspectFillMsg)
		if !ok {
			t.Fatal("the check's command didn't return its result")
		}
		s, _ = s.update(fill)
	}
	return s
}

// chooseKind completes the kind step with kind.
func chooseKind(t *testing.T, s *loadScreen, kind string) *loadScreen {
	t.Helper()
	s.kind = kind
	s, _ = completeForm(s)
	if s.step != firstStep(kind) {
		t.Fatalf("kind %q: step = %v, want %v", kind, s.step, firstStep(kind))
	}
	return s
}

// run confirms the load and returns the action it asks the app to run.
func run(t *testing.T, s *loadScreen) dashboard.Action {
	t.Helper()
	if s.step != loadConfirm && s.step != loadGenomicConfirm {
		t.Fatalf("step = %v, want a confirm step", s.step)
	}
	s.confirmed = true
	_, cmd := completeForm(s)
	if cmd == nil {
		t.Fatal("confirm produced no command")
	}
	msg, ok := cmd().(loadRunMsg)
	if !ok {
		t.Fatalf("confirm sent %#v, want loadRunMsg", cmd())
	}
	return msg.act
}

func loadView(s *loadScreen) string { return wizardANSI.ReplaceAllString(s.view(), "") }

// The whole auto-dictionary path for a file: kind, file, heap, dictionary,
// confirm, then data load-phenotype with the file and the heap.
func TestLoadWizardFileAutoFlow(t *testing.T) {
	stubInspect(t, nil, nil)
	s := chooseKind(t, newTestLoad(t, "/tmp/x", ""), kindFile)
	s = pick(t, s, "/data/pheno.csv")
	if s.step != loadHeap || s.heap != "4096" {
		t.Fatalf("after the file: step %v heap %q", s.step, s.heap)
	}
	s, _ = completeForm(s)
	if s.step != loadPhenoDict {
		t.Fatalf("after the heap: step %v", s.step)
	}
	s, _ = completeForm(s) // auto
	act := run(t, s)
	want := []string{"data", "load-phenotype", "--file", "/data/pheno.csv"}
	if !eq(act.Args, want) {
		t.Errorf("args = %q, want %q", act.Args, want)
	}
	if act.Title != "Loading phenotype data" || act.Done != "Phenotype data loaded" {
		t.Errorf("title %q done %q", act.Title, act.Done)
	}
}

func TestLoadWizardCustomDictionary(t *testing.T) {
	for _, facets := range []bool{false, true} {
		stubInspect(t, nil, nil)
		s := chooseKind(t, newTestLoad(t, "/tmp/x", ""), kindFile)
		s = pick(t, s, "/data/pheno.csv")
		s.heap = " 8000 "
		s, _ = completeForm(s)
		s.dictMode = "custom"
		s, _ = completeForm(s)
		s = pick(t, s, "/data/datasets.csv")
		s = pick(t, s, "/data/concepts.zip")
		if s.step != loadPhenoFacetsAsk {
			t.Fatalf("step = %v, want the facets question", s.step)
		}
		s.includeFacets = facets
		s, _ = completeForm(s)
		if facets {
			s = pick(t, s, "/data/fc.csv")
			s = pick(t, s, "/data/f.csv")
			s = pick(t, s, "/data/fcon.csv")
		}
		view := loadView(s)
		act := run(t, s)
		want := []string{"data", "load-phenotype", "--file", "/data/pheno.csv", "--heap", "8000",
			"--dictionary", "custom", "--datasets", "/data/datasets.csv", "--concepts", "/data/concepts.zip"}
		if facets {
			want = append(want, "--facets-categories", "/data/fc.csv", "--facets", "/data/f.csv", "--facet-concepts", "/data/fcon.csv")
		}
		if !eq(act.Args, want) {
			t.Errorf("facets %v: args = %q, want %q", facets, act.Args, want)
		}
		if strings.Contains(view, "Facet concepts") != facets {
			t.Errorf("facets %v: summary:\n%s", facets, view)
		}
	}
}

// An archive with several CSVs opens the entry picker, and the entry
// reaches --entry and the summary.
func TestLoadWizardArchiveEntryPicker(t *testing.T) {
	stubInspect(t, []string{"a/one.csv", "b/two.csv"}, nil)
	s := chooseKind(t, newTestLoad(t, "/tmp/x", ""), kindFile)
	s = pick(t, s, "/data/set.tar.gz")
	if s.step != loadPhenoArchiveEntry || s.archiveEntry != "a/one.csv" {
		t.Fatalf("step %v entry %q, want the picker on the first entry", s.step, s.archiveEntry)
	}
	if v := loadView(s); !strings.Contains(v, "b/two.csv") {
		t.Errorf("picker doesn't list the entries:\n%s", v)
	}
	s.archiveEntry = "b/two.csv"
	s, _ = completeForm(s)
	if s.step != loadHeap {
		t.Fatalf("after the entry: step %v", s.step)
	}
	s, _ = completeForm(s)
	s, _ = completeForm(s)
	if v := loadView(s); !strings.Contains(v, "Archive entry") || !strings.Contains(v, "b/two.csv") {
		t.Errorf("summary doesn't name the entry:\n%s", v)
	}
	if args := run(t, s).Args; !containsPair(args, "--entry", "b/two.csv") {
		t.Errorf("args = %q, want --entry b/two.csv", args)
	}
}

// A one-CSV archive or a gzip needs no --entry.
func TestLoadWizardSingleEntryArchiveSkipsPicker(t *testing.T) {
	stubInspect(t, []string{"only.csv"}, nil)
	s := chooseKind(t, newTestLoad(t, "/tmp/x", ""), kindFile)
	s = pick(t, s, "/data/one.zip")
	if s.step != loadHeap || s.archiveEntry != "" {
		t.Fatalf("step %v entry %q", s.step, s.archiveEntry)
	}
}

// The real lister is wired in: a plain CSV goes on to the heap, and a
// file the load would refuse is rejected on the file step.
func TestLoadWizardRealInspection(t *testing.T) {
	dir := t.TempDir()
	csv := filepath.Join(dir, "pheno.csv")
	empty := filepath.Join(dir, "empty.csv")
	if err := os.WriteFile(csv, []byte("PATIENT_NUM,CONCEPT_PATH,NVAL_NUM,TVAL_CHAR\n1,\\a\\,,x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(empty, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	s := chooseKind(t, newTestLoad(t, dir, ""), kindFile)
	if s = pick(t, s, csv); s.step != loadHeap {
		t.Errorf("plain CSV: step %v, want the heap", s.step)
	}
	s = chooseKind(t, newTestLoad(t, dir, ""), kindFile)
	if s = pick(t, s, empty); s.step != loadPhenoFile || s.inspectErr == "" {
		t.Errorf("empty file: step %v err %q, want the file step with an error", s.step, s.inspectErr)
	}
}

// A rejected pick reopens the browser with the reason, and forgets the pick.
func TestLoadWizardRejectedPick(t *testing.T) {
	stubInspect(t, nil, errors.New("no CSV entries"))
	for _, kind := range []string{kindFile, kindDir} {
		s := chooseKind(t, newTestLoad(t, "/tmp/x", ""), kind)
		s = pick(t, s, "/data/thing")
		if s.step != firstStep(kind) || s.dirty() {
			t.Errorf("%s: step %v dirty %v, want the same step, nothing kept", kind, s.step, s.dirty())
		}
		if v := loadView(s); !strings.Contains(v, "can't load that") || !strings.Contains(v, "no CSV entries") {
			t.Errorf("%s: view doesn't show the reason:\n%s", kind, v)
		}
	}
}

// The heap reaches the command as the number the summary shows, and only
// when it isn't the command's default: pflag would read "08000" as octal.
func TestLoadWizardHeapArgs(t *testing.T) {
	for heap, want := range map[string][]string{
		"4096":   nil,
		"04096":  nil,
		"08000":  {"--heap", "8000"},
		" 2048 ": {"--heap", "2048"},
	} {
		s := newTestLoad(t, "/tmp/x", kindDemo)
		s, _ = completeForm(s)
		s.heap = heap
		s, _ = completeForm(s)
		if v := loadView(s); heap == "08000" && !strings.Contains(v, "8000 MB") {
			t.Errorf("summary for %q:\n%s", heap, v)
		}
		if args := run(t, s).Args; !eq(args[3:], want) {
			t.Errorf("heap %q: args %q, want %q after the dataset", heap, args, want)
		}
	}
}

// A check's result is used only by the pick it was made for.
func TestLoadWizardStaleInspection(t *testing.T) {
	stubInspect(t, []string{"a.csv", "b.csv"}, nil)
	s := chooseKind(t, newTestLoad(t, "/tmp/x", ""), kindFile)
	s, first := s.consumeFile("/data/first.tgz")
	stale := first().(inspectFillMsg)
	s.inspectSeq++ // a newer pick is in flight
	if s, _ = s.update(stale); !s.inspecting || s.step != loadPhenoFile {
		t.Errorf("a stale result was applied: step %v inspecting %v", s.step, s.inspecting)
	}
	s.inspecting = false // the screen moved on
	stale.seq = s.inspectSeq
	if s, _ = s.update(stale); s.step != loadPhenoFile {
		t.Errorf("a result after the check ended was applied: step %v", s.step)
	}
}

// Closing the screen cancels its check, and the next screen's pick never
// takes the closed screen's result.
func TestLoadWizardCheckAfterClose(t *testing.T) {
	stubInspect(t, []string{"a.csv", "b.csv"}, nil)
	old := chooseKind(t, newTestLoad(t, "/tmp/x", ""), kindFile)
	old, oldCmd := old.consumeFile("/data/old.tgz")
	old.close()
	if old.ctx.Err() == nil {
		t.Error("close didn't cancel the screen's checks")
	}
	s := chooseKind(t, newTestLoad(t, "/tmp/x", ""), kindFile)
	s, _ = s.consumeFile("/data/new.tgz")
	if s, _ = s.update(oldCmd().(inspectFillMsg)); !s.inspecting || s.step != loadPhenoFile {
		t.Errorf("the closed screen's result was applied: step %v", s.step)
	}
}

// While a pick is checked, keys don't reach the browser, and esc still
// asks before discarding.
func TestLoadWizardInspectingEsc(t *testing.T) {
	stubInspect(t, nil, nil)
	s := chooseKind(t, newTestLoad(t, "/tmp/x", ""), kindDir)
	s, _ = s.consumeFile("/data/dir")
	if !s.inspecting || !strings.Contains(loadView(s), "checking") {
		t.Fatalf("not inspecting:\n%s", loadView(s))
	}
	s, _ = s.update(tea.KeyPressMsg{Code: tea.KeyEscape})
	if !s.discarding {
		t.Error("esc while checking a pick didn't ask to discard")
	}
}

func TestLoadWizardInputDirFlow(t *testing.T) {
	stubInspect(t, nil, nil)
	s := chooseKind(t, newTestLoad(t, "/tmp/x", ""), kindDir)
	s = pick(t, s, "/data/csvs")
	if s.step != loadHeap || s.heap != "8000" {
		t.Fatalf("step %v heap %q, want the heap at 8000", s.step, s.heap)
	}
	s, _ = completeForm(s)
	s, _ = completeForm(s) // auto
	if v := loadView(s); !strings.Contains(v, "Directory") || !strings.Contains(v, "HPDS keeps running") {
		t.Errorf("summary:\n%s", v)
	}
	want := []string{"data", "load-phenotype", "--input-dir", "/data/csvs"}
	if args := run(t, s).Args; !eq(args, want) {
		t.Errorf("args = %q, want %q", args, want)
	}
}

func TestLoadWizardDemoFlow(t *testing.T) {
	s := chooseKind(t, newTestLoad(t, "/tmp/x", ""), kindDemo)
	if s.demo != "nhanes" {
		t.Errorf("dataset preselected %q, want nhanes", s.demo)
	}
	for _, opt := range []string{"NHANES", "Synthea 10k", "1000 Genomes", "All three combined"} {
		if !strings.Contains(loadView(s), opt) {
			t.Errorf("dataset picker misses %q", opt)
		}
	}
	s.demo = "synthea"
	s, _ = completeForm(s)
	if s.step != loadHeap || s.heap != "4096" {
		t.Fatalf("step %v heap %q", s.step, s.heap)
	}
	s, _ = completeForm(s)
	if s.step != loadConfirm {
		t.Fatalf("demo heap → step %v, want the confirm (no dictionary questions)", s.step)
	}
	if v := loadView(s); strings.Contains(v, "Dictionary") || !strings.Contains(v, "synthea") {
		t.Errorf("summary:\n%s", v)
	}
	act := run(t, s)
	if want := []string{"data", "demo", "synthea"}; !eq(act.Args, want) {
		t.Errorf("args = %q, want %q", act.Args, want)
	}
	if act.Title != "Loading the synthea demo data" {
		t.Errorf("title %q", act.Title)
	}
}

// The developer menu's demo entry opens the screen on the datasets, and a
// pristine esc there closes it.
func TestLoadWizardOpensOnKind(t *testing.T) {
	s := newTestLoad(t, "/tmp/x", kindDemo)
	if s.step != loadDemoDataset || s.form == nil {
		t.Fatalf("step %v, want the dataset picker", s.step)
	}
	_, cmd := s.update(tea.KeyPressMsg{Code: tea.KeyEscape})
	if msg, ok := cmd().(loadDataClosedMsg); !ok || !msg.aborted {
		t.Errorf("esc = %#v", cmd())
	}
}

// genomicInputs is what driveGenomicToConfirm enters. An empty heap keeps
// the default.
type genomicInputs struct {
	vcfIndex      string
	vcfDir        string
	partition     string
	heap          string
	promote       bool
	enableProfile bool
}

// driveGenomicToConfirm walks the genomic steps to the confirm summary.
func driveGenomicToConfirm(t *testing.T, s *loadScreen, in genomicInputs) *loadScreen {
	t.Helper()
	s = chooseKind(t, s, kindGenomic)
	s = pick(t, s, in.vcfIndex)
	if s.step != loadGenomicDirAsk {
		t.Fatalf("after the index: step %v", s.step)
	}
	s.includeVCFDir = in.vcfDir != ""
	s, _ = completeForm(s)
	if in.vcfDir != "" {
		s = pick(t, s, in.vcfDir)
	}
	s.partition = in.partition
	s, _ = completeForm(s)
	if s.step != loadHeap || s.heap != "16000" {
		t.Fatalf("step %v heap %q, want the heap at 16000", s.step, s.heap)
	}
	if in.heap != "" {
		s.heap = in.heap
	}
	s, _ = completeForm(s)
	s.promote = in.promote
	s, _ = completeForm(s)
	s.enableProfile = in.enableProfile
	s, _ = completeForm(s)
	if s.step != loadGenomicConfirm {
		t.Fatalf("step %v, want the genomic confirm", s.step)
	}
	return s
}

func TestLoadWizardGenomicFlow(t *testing.T) {
	cases := []struct {
		in   genomicInputs
		want []string
	}{
		{genomicInputs{vcfIndex: "/v/idx.tsv", partition: "p1"},
			[]string{"data", "load-genomic", "--partition", "p1", "--vcf-index", "/v/idx.tsv"}},
		{genomicInputs{vcfIndex: "/v/idx.tsv", vcfDir: "/vcfs", partition: "p2", heap: "20000", promote: true, enableProfile: true},
			[]string{"data", "load-genomic", "--partition", "p2", "--vcf-index", "/v/idx.tsv", "--vcf-dir", "/vcfs",
				"--heap", "20000", "--promote", "--enable-profile"}},
	}
	for _, c := range cases {
		s := driveGenomicToConfirm(t, newTestLoad(t, "/tmp/x", ""), c.in)
		act := run(t, s)
		if !eq(act.Args, c.want) {
			t.Errorf("args = %q, want %q", act.Args, c.want)
		}
		if act.Title != "Loading genomic partition "+c.in.partition {
			t.Errorf("title %q", act.Title)
		}
	}
}

// Enabling the profile without promoting warns; with promote it doesn't.
func TestLoadWizardGenomicProfileWarning(t *testing.T) {
	for _, promote := range []bool{false, true} {
		s := driveGenomicToConfirm(t, newTestLoad(t, "/tmp/x", ""),
			genomicInputs{vcfIndex: "/v/idx.tsv", partition: "p", promote: promote, enableProfile: true})
		if warned := strings.Contains(loadView(s), "WITHOUT promoting"); warned == promote {
			t.Errorf("promote %v: warning shown %v", promote, warned)
		}
	}
}

// Relative picks become absolute paths, so the command doesn't depend on
// the working directory.
func TestLoadWizardArgsAreAbsolute(t *testing.T) {
	stubInspect(t, nil, nil)
	s := chooseKind(t, newTestLoad(t, "/tmp/x", ""), kindFile)
	s = pick(t, s, "rel/pheno.csv")
	s, _ = completeForm(s)
	s, _ = completeForm(s)
	wd, _ := os.Getwd()
	if args := run(t, s).Args; !containsPair(args, "--file", filepath.Join(wd, "rel/pheno.csv")) {
		t.Errorf("args = %q", args)
	}
}

func TestValidateHeap(t *testing.T) {
	for _, v := range []string{"256", "4096", " 8000 "} {
		if err := validateHeap(v); err != nil {
			t.Errorf("validateHeap(%q) = %v", v, err)
		}
	}
	for v, want := range map[string]string{
		"":     "required",
		"16g":  "whole number of MB",
		"4.5":  "whole number of MB",
		"0":    "whole number of MB",
		"-1":   "whole number of MB",
		"16":   "too small",
		"255":  "too small",
		"1e10": "whole number of MB",
	} {
		if err := validateHeap(v); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("validateHeap(%q) = %v, want %q", v, err, want)
		}
	}
}

func TestValidatePartition(t *testing.T) {
	for _, v := range []string{"chr22", "a_b-c", "-dash"} {
		if err := validatePartition(v); err != nil {
			t.Errorf("validatePartition(%q) = %v", v, err)
		}
	}
	for _, v := range []string{"", "   ", "a b", "a/b", "a.b"} {
		if validatePartition(v) == nil {
			t.Errorf("validatePartition(%q) accepted", v)
		}
	}
}

func TestLoadWizardCancels(t *testing.T) {
	closed := func(t *testing.T, cmd tea.Cmd) {
		t.Helper()
		if cmd == nil {
			t.Fatal("no command")
		}
		if msg, ok := cmd().(loadDataClosedMsg); !ok || !msg.aborted {
			t.Fatalf("sent %#v, want loadDataClosedMsg{aborted: true}", cmd())
		}
	}
	// The kind step's Cancel.
	s := newTestLoad(t, "/tmp/x", "")
	s.kind = ""
	_, cmd := completeForm(s)
	closed(t, cmd)

	// Cancel at the confirm.
	s = newTestLoad(t, "/tmp/x", kindDemo)
	s, _ = completeForm(s)
	s, _ = completeForm(s)
	s.confirmed = false
	_, cmd = completeForm(s)
	closed(t, cmd)

	// esc on a pristine screen.
	_, cmd = newTestLoad(t, "/tmp/x", "").update(tea.KeyPressMsg{Code: tea.KeyEscape})
	closed(t, cmd)
}

// Once a path is collected, esc asks first: y discards, n keeps it.
func TestLoadWizardEscDirtyGuard(t *testing.T) {
	stubInspect(t, nil, nil)
	dirty := func() *loadScreen {
		s := chooseKind(t, newTestLoad(t, "/tmp/x", ""), kindFile)
		return pick(t, s, "/data/pheno.csv")
	}
	s, cmd := dirty().update(tea.KeyPressMsg{Code: tea.KeyEscape})
	if !s.discarding || cmd != nil || !strings.Contains(loadView(s), "Discard data load?") {
		t.Fatalf("esc on a dirty screen: discarding %v cmd %v", s.discarding, cmd != nil)
	}
	if _, cmd = s.update(tea.KeyPressMsg{Code: 'y', Text: "y"}); cmd == nil {
		t.Fatal("y didn't close")
	}
	s, _ = dirty().update(tea.KeyPressMsg{Code: tea.KeyEscape})
	s, cmd = s.update(tea.KeyPressMsg{Code: 'n', Text: "n"})
	if cmd != nil || s.discarding || s.file != "/data/pheno.csv" {
		t.Errorf("n: cmd %v discarding %v file %q", cmd != nil, s.discarding, s.file)
	}
}

// Across sizes, every kind of step stays inside the terminal.
func TestLoadWizardFrameStaysInBox(t *testing.T) {
	stubInspect(t, []string{"a.csv", "b.csv"}, nil)
	steps := map[string]func(*loadScreen) *loadScreen{
		"kind": func(s *loadScreen) *loadScreen { return s },
		"file": func(s *loadScreen) *loadScreen { return chooseKind(t, s, kindFile) },
		"entry": func(s *loadScreen) *loadScreen {
			return pick(t, chooseKind(t, s, kindFile), "/data/a-very-long-directory-name/set.tar.gz")
		},
		"confirm": func(s *loadScreen) *loadScreen {
			s = pick(t, chooseKind(t, s, kindFile), "/data/set.tar.gz")
			s, _ = completeForm(s)
			s, _ = completeForm(s)
			s, _ = completeForm(s)
			return s
		},
		"genomic confirm": func(s *loadScreen) *loadScreen {
			return driveGenomicToConfirm(t, s, genomicInputs{vcfIndex: "/v/idx.tsv", partition: "p", enableProfile: true})
		},
	}
	for _, sz := range [][2]int{{60, 16}, {80, 24}, {120, 30}} {
		for name, to := range steps {
			s := to(newTestLoad(t, "/tmp/x", ""))
			s.setSize(sz[0], sz[1])
			view := s.view()
			if h := lipgloss.Height(view); h > sz[1] {
				t.Errorf("%v %s: height %d", sz, name, h)
			}
			for _, line := range strings.Split(view, "\n") {
				if w := lipgloss.Width(line); w > sz[0] {
					t.Errorf("%v %s: line width %d", sz, name, w)
					break
				}
			}
		}
	}
}

// The kind select opens on its first real option, with every option shown,
// even on a short terminal: huh preselects the option equal to the bound
// value, and "" is Cancel's.
func TestLoadWizardKindSelectOpensOnFirstOption(t *testing.T) {
	for _, sz := range [][2]int{{100, 35}, {60, 16}} {
		s := newLoadScreen(context.Background(), "/tmp/x", "")
		s.setSize(sz[0], sz[1])
		_ = s.init()
		v := loadView(s)
		if !strings.Contains(v, "> Phenotype CSV") || strings.Contains(v, "> Cancel") {
			t.Errorf("%v: cursor not on the first option:\n%s", sz, v)
		}
		for _, opt := range []string{"Directory of phenotype CSVs", "Demo dataset", "Genomic data (VCF)", "Cancel"} {
			if !strings.Contains(v, opt) {
				t.Errorf("%v: option %q not shown:\n%s", sz, opt, v)
			}
		}
	}
}

// The browsers open at the stack's directory, not the process's cwd.
func TestLoadWizardBrowsersStartAtRoot(t *testing.T) {
	root := t.TempDir()
	for _, kind := range []string{kindFile, kindDir, kindGenomic} {
		s := chooseKind(t, newTestLoad(t, root, ""), kind)
		if got := s.fb.Dir(); got != root {
			t.Errorf("%s: browser opened in %q, want %q", kind, got, root)
		}
	}
}

func containsPair(args []string, flag, val string) bool {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == flag && args[i+1] == val {
			return true
		}
	}
	return false
}
