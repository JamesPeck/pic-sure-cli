package filebrowser

import (
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

// drainInit runs the cmd returned by Init (the filepicker's readDir) and feeds
// the resulting msg through Update, so the model ends up with a populated entry
// list — the state a real program reaches after the first paint. It returns the
// advanced model. Driving the real readDir cmd (rather than fabricating the
// filepicker's unexported readDirMsg) is the only way to load entries without
// reaching into bubbles internals.
func drainInit(t *testing.T, m Model) Model {
	t.Helper()
	cmd := m.Init()
	if cmd == nil {
		t.Fatal("Init returned nil cmd")
	}
	msg := cmd()
	m, _ = m.Update(msg)
	return m
}

func TestNewMapsAllowedExts(t *testing.T) {
	m := New(Options{AllowedExts: []string{".csv", ".tsv"}})
	got := m.fp.AllowedTypes
	if len(got) != 2 || got[0] != ".csv" || got[1] != ".tsv" {
		t.Fatalf("AllowedTypes = %v, want [.csv .tsv]", got)
	}
	if m.fp.DirAllowed {
		t.Error("DirAllowed should be false in file mode")
	}
	if !m.fp.FileAllowed {
		t.Error("FileAllowed should be true in file mode")
	}
}

func TestNewEmptyExtsAllowsAll(t *testing.T) {
	m := New(Options{})
	if len(m.fp.AllowedTypes) != 0 {
		t.Fatalf("AllowedTypes = %v, want empty", m.fp.AllowedTypes)
	}
}

func TestNewDirMode(t *testing.T) {
	m := New(Options{DirMode: true, AllowedExts: []string{".csv"}})
	if !m.fp.DirAllowed {
		t.Error("DirAllowed should be true in dir mode")
	}
	if m.fp.FileAllowed {
		t.Error("FileAllowed should be false in dir mode")
	}
	if len(m.fp.AllowedTypes) != 1 || m.fp.AllowedTypes[0] != "/" {
		t.Errorf("AllowedTypes = %v in dir mode, want [/] so files are dimmed", m.fp.AllowedTypes)
	}
}

func TestNewStartDirDefaultsToCwd(t *testing.T) {
	m := New(Options{})
	wd, err := os.Getwd()
	if err != nil {
		t.Skipf("Getwd failed: %v", err)
	}
	if m.fp.CurrentDirectory != wd {
		t.Errorf("CurrentDirectory = %q, want cwd %q", m.fp.CurrentDirectory, wd)
	}
}

func TestNewStartDirHonored(t *testing.T) {
	dir := t.TempDir()
	m := New(Options{StartDir: dir})
	if m.fp.CurrentDirectory != dir {
		t.Errorf("CurrentDirectory = %q, want %q", m.fp.CurrentDirectory, dir)
	}
}

// viewLines is the rendered height of m.View(). SetSize feeds the filepicker an
// interior height and it pads its list to fill it, so the rendered line count is
// the observable proxy for "did SetSize take": asserting on it avoids reading
// the filepicker's deprecated Height field directly.
func viewLines(m Model) int {
	return strings.Count(m.View(), "\n") + 1
}

func TestSetSizeGrowsViewWithHeight(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "data.csv"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	small := New(Options{StartDir: dir, Title: "Pick", AllowedExts: []string{".csv"}})
	small.SetSize(80, 10)
	small = drainInit(t, small)

	big := New(Options{StartDir: dir, Title: "Pick", AllowedExts: []string{".csv"}})
	big.SetSize(80, 30)
	big = drainInit(t, big)

	if viewLines(big) <= viewLines(small) {
		t.Errorf("taller SetSize did not grow the view: small=%d big=%d",
			viewLines(small), viewLines(big))
	}
	// The wrapper draws title + path header + filepicker + nav hint + status
	// slot. The filepicker pads its list with an inclusive loop (one extra line
	// beyond its interior height), so a view sized to h renders h+1 lines
	// regardless of how much of that h the chrome consumes. Pin that contract so
	// a sizing regression (e.g. wrong chrome subtraction) is caught.
	if got := viewLines(big); got != 31 {
		t.Errorf("View height = %d, want 31 (h=30 + filepicker's inclusive pad)", got)
	}
}

func TestSetSizeClampsTiny(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "data.csv"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, h := range []int{0, 1, 2, -5} {
		m := New(Options{StartDir: dir, Title: "x", AllowedExts: []string{".csv"}})
		m.SetSize(0, h)
		m = drainInit(t, m)
		if viewLines(m) < 1 {
			t.Errorf("SetSize(0,%d) -> %d view lines, want >=1", h, viewLines(m))
		}
	}
}

func TestSelectedDefaults(t *testing.T) {
	m := New(Options{})
	if path, ok := m.Selected(); ok || path != "" {
		t.Errorf("Selected() = (%q, %v), want (\"\", false)", path, ok)
	}
	if m.Err() != nil {
		t.Errorf("Err() = %v, want nil", m.Err())
	}
}

func TestSelectFileReturnsAbsPath(t *testing.T) {
	dir := t.TempDir()
	csv := filepath.Join(dir, "data.csv")
	if err := os.WriteFile(csv, []byte("a,b\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	m := New(Options{StartDir: dir, AllowedExts: []string{".csv"}})
	m.SetSize(80, 20)
	m = drainInit(t, m) // loads the single entry, cursor at index 0

	// Enter both navigates/opens and selects in the filepicker; on a plain file
	// it sets Path, which DidSelectFile then reports. Feed it through our Update.
	m, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})

	path, ok := m.Selected()
	if !ok {
		t.Fatal("Selected() ok = false after selecting a file")
	}
	if !filepath.IsAbs(path) {
		t.Errorf("selected path %q is not absolute", path)
	}
	if path != csv {
		t.Errorf("selected path = %q, want %q", path, csv)
	}
	if m.Err() != nil {
		t.Errorf("Err() = %v after successful selection, want nil", m.Err())
	}
}

func TestSelectDisabledFileSetsErr(t *testing.T) {
	dir := t.TempDir()
	// Only a .txt present; with AllowedExts {.csv} it is selectable-disabled.
	if err := os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	m := New(Options{StartDir: dir, AllowedExts: []string{".csv"}})
	m.SetSize(80, 20)
	m = drainInit(t, m)

	m, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})

	if _, ok := m.Selected(); ok {
		t.Error("Selected() ok = true for a disabled file, want false")
	}
	if m.Err() == nil {
		t.Fatal("Err() = nil after selecting a disabled file, want an error")
	}
	if !strings.Contains(m.Err().Error(), "notes.txt") {
		t.Errorf("Err() = %q, want it to name the offending file", m.Err())
	}
}

func TestHintNoMatchingFiles(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	m := New(Options{StartDir: dir, AllowedExts: []string{".csv"}})
	m.SetSize(80, 20)
	m = drainInit(t, m)

	if !strings.Contains(m.View(), "no matching files") {
		t.Errorf("View() should warn about no matching files; got:\n%s", m.View())
	}
}

func TestHintAbsentWhenSelectablePresent(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "data.csv"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	m := New(Options{StartDir: dir, AllowedExts: []string{".csv"}})
	m.SetSize(80, 20)
	m = drainInit(t, m)

	if strings.Contains(m.View(), "no matching files") {
		t.Errorf("View() should not warn when a .csv is present; got:\n%s", m.View())
	}
}

func TestHintAbsentInDirMode(t *testing.T) {
	dir := t.TempDir() // empty of selectable files
	if err := os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	m := New(Options{StartDir: dir, DirMode: true})
	m.SetSize(80, 20)
	m = drainInit(t, m)

	if strings.Contains(m.View(), "no matching files") {
		t.Errorf("dir mode should never show the no-matching-files hint; got:\n%s", m.View())
	}
}

func TestViewNoPanicAtVariousSizes(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "data.csv"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ w, h int }{
		{0, 0}, {1, 1}, {2, 2}, {80, 24}, {200, 60},
	} {
		m := New(Options{StartDir: dir, Title: "Pick", AllowedExts: []string{".csv"}})
		m.SetSize(tc.w, tc.h)
		m = drainInit(t, m)
		_ = m.View() // must not panic
	}
}

func TestViewBeforeInitNoPanic(t *testing.T) {
	dir := t.TempDir()
	m := New(Options{StartDir: dir, Title: "Pick", AllowedExts: []string{".csv"}})
	_ = m.View() // unsized, un-inited
}

func TestViewShowsCurrentDirectory(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "data.csv"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	m := New(Options{StartDir: dir, Title: "Pick", AllowedExts: []string{".csv"}})
	m.SetSize(120, 20)
	m = drainInit(t, m)

	if !strings.Contains(m.View(), filepath.Base(dir)) {
		t.Errorf("View() should show the current directory %q; got:\n%s", dir, m.View())
	}
}

func TestViewHeaderReflectsNavigation(t *testing.T) {
	// After navigating into a subdir the path header must update to the new
	// directory. The filepicker's CurrentDirectory is the single source of truth
	// the header reads, so driving it (as a real navigation does) and re-rendering
	// is enough to assert the header tracks navigation.
	root := t.TempDir()
	sub := filepath.Join(root, "demo-data")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}

	m := New(Options{StartDir: root, DirMode: true})
	m.SetSize(120, 20)
	m = drainInit(t, m)

	m, _ = m.Update(keyRight)

	if !strings.Contains(m.View(), "demo-data") {
		t.Errorf("View() header should reflect navigation into %q; got:\n%s", sub, m.View())
	}
}

func TestNavHintShowsUpAffordance(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "data.csv"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	m := New(Options{StartDir: dir, AllowedExts: []string{".csv"}})
	m.SetSize(120, 20)
	m = drainInit(t, m)

	view := m.View()
	if !strings.Contains(view, "..") {
		t.Errorf("file-mode nav hint should show the \"..\" up affordance; got:\n%s", view)
	}
	if !strings.Contains(view, "←/h") {
		t.Errorf("file-mode nav hint should show the ←/h up keys; got:\n%s", view)
	}
	if !strings.Contains(view, "select") {
		t.Errorf("file-mode nav hint should say \"select\"; got:\n%s", view)
	}
	if strings.Contains(view, "use current dir") {
		t.Errorf("file-mode nav hint must not say \"use current dir\"; got:\n%s", view)
	}
}

func TestNavHintDirModeWording(t *testing.T) {
	dir := t.TempDir()

	m := New(Options{StartDir: dir, DirMode: true})
	m.SetSize(120, 20)
	m = drainInit(t, m)

	view := m.View()
	if !strings.Contains(view, "..") || !strings.Contains(view, "←/h") {
		t.Errorf("dir-mode nav hint should show the \"..\" up affordance; got:\n%s", view)
	}
	if !strings.Contains(view, "use current dir") {
		t.Errorf("dir-mode nav hint should say \"use current dir\"; got:\n%s", view)
	}
}

func TestPathHeaderLeftElidedToWidth(t *testing.T) {
	// A path far deeper than the box width must be left-elided so its rendered
	// width never exceeds the box and the tail (where the user is) is preserved.
	root := t.TempDir()
	deep := filepath.Join(root,
		"alpha-very-long-component", "beta-very-long-component",
		"gamma-very-long-component", "delta-very-long-component", "tail-dir")
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatal(err)
	}

	const boxW = 30
	m := New(Options{StartDir: root, DirMode: true})
	m.SetSize(boxW, 20)
	m = drainInit(t, m)
	m.fp.CurrentDirectory = deep

	header := strings.SplitN(m.View(), "\n", 2)[0]
	if w := lipgloss.Width(header); w > boxW {
		t.Errorf("path header width %d exceeds box width %d; header=%q", w, boxW, header)
	}
	if !strings.Contains(header, "tail-dir") {
		t.Errorf("left-elided header should preserve the tail dir; header=%q", header)
	}
	if !strings.Contains(header, "…") {
		t.Errorf("over-long header should be marked elided with …; header=%q", header)
	}
}

func TestElideLeftWideCharsFitWidth(t *testing.T) {
	// Wide (display-width-2) graphemes must not push the elided result past the
	// box: ansi.TruncateLeft keeps a grapheme straddling the cut boundary whole,
	// so a naive drop can return w+1. elideLeft must trim the extra column.
	//
	// This Japanese path's tail components are kana, each width 2, so several of
	// the target widths (e.g. 30) land the cut on a wide grapheme — the boundary
	// case that reproduced the original off-by-one overflow.
	const path = "/データ/ゲノム/フォルダ/末尾ディレクトリ"
	for _, w := range []int{30, 40, 80, 9, 8, 7, 6, 5, 4, 3, 2, 1} {
		got := elideLeft(path, w)
		if rw := lipgloss.Width(got); rw > w {
			t.Errorf("elideLeft(%q, %d) width %d exceeds box width %d; got=%q", path, w, rw, w, got)
		}
	}
}

func TestPathHeaderWideCharsFitWidth(t *testing.T) {
	// End-to-end mirror of the elideLeft wide-char case through the rendered
	// header: at widths where a wide grapheme straddles the cut boundary the
	// header's rendered width must still be <= the box width.
	root := t.TempDir()
	m := New(Options{StartDir: root, DirMode: true})
	m = drainInit(t, m)
	// Point the picker at a wide-character path directly; the header reads
	// CurrentDirectory, so the dir need not exist on disk for the layout assertion.
	m.fp.CurrentDirectory = "/データ/ゲノム/フォルダ/末尾ディレクトリ"
	for _, boxW := range []int{30, 40, 80} {
		m.SetSize(boxW, 20)
		header := strings.SplitN(m.View(), "\n", 2)[0]
		if w := lipgloss.Width(header); w > boxW {
			t.Errorf("wide-char header width %d exceeds box width %d; header=%q", w, boxW, header)
		}
	}
}

func TestViewNeverOverflowsSmallBox(t *testing.T) {
	// At a small box height the total rendered View height must not exceed the box
	// the parent reserved (box height + the filepicker's inclusive one-line pad).
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "data.csv"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, h := range []int{6, 8, 10, 12} {
		m := New(Options{StartDir: dir, Title: "Pick", AllowedExts: []string{".csv"}})
		m.SetSize(80, h)
		m = drainInit(t, m)
		if got := viewLines(m); got > h+1 {
			t.Errorf("SetSize(80,%d) -> %d view lines, want <= %d (h + inclusive pad)", h, got, h+1)
		}
	}
}

func mkdirs(t *testing.T, dirs ...string) {
	t.Helper()
	for _, d := range dirs {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
}

func writeFiles(t *testing.T, files ...string) {
	t.Helper()
	for _, f := range files {
		if err := os.WriteFile(f, []byte("a,b\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

var (
	keyEnter = tea.KeyPressMsg{Code: tea.KeyEnter}
	keyRight = tea.KeyPressMsg{Code: tea.KeyRight}
	keyLeft  = tea.KeyPressMsg{Code: tea.KeyLeft}
)

// lockedDir makes root/a-locked unreadable (sorted before z.csv) and restores
// it for TempDir's cleanup.
func lockedDir(t *testing.T, root string) string {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("root can read a mode-0000 directory")
	}
	locked := filepath.Join(root, "a-locked")
	mkdirs(t, locked)
	writeFiles(t, filepath.Join(locked, "inside.csv"))
	if err := os.Chmod(locked, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })
	return locked
}

func TestUnreadableDirStaysInParent(t *testing.T) {
	for _, k := range []tea.KeyPressMsg{keyEnter, keyRight} {
		root := t.TempDir()
		lockedDir(t, root)
		writeFiles(t, filepath.Join(root, "z.csv"))

		m := New(Options{StartDir: root, AllowedExts: []string{".csv"}})
		m.SetSize(200, 20)
		m = drainInit(t, m)

		m, cmd := m.Update(k)
		if cmd != nil {
			t.Errorf("%s on an unreadable dir returned a cmd, want none", k)
		}
		if m.Dir() != root {
			t.Errorf("%s: Dir() = %q, want to stay in %q", k, m.Dir(), root)
		}
		if m.Err() == nil || !strings.Contains(m.Err().Error(), "can't read a-locked") {
			t.Fatalf("%s: Err() = %v, want can't read a-locked", k, m.Err())
		}
		if !strings.Contains(m.View(), "can't read a-locked") {
			t.Errorf("%s: status line should show the error; got:\n%s", k, m.View())
		}

		// The listing is still the parent's, so moving to z.csv selects it.
		m, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyDown})
		m, _ = m.Update(keyEnter)
		if path, ok := m.Selected(); !ok || path != filepath.Join(root, "z.csv") {
			t.Errorf("Selected() = (%q, %v), want z.csv in %q", path, ok, root)
		}
	}
}

func TestUnreadableStartDir(t *testing.T) {
	root := t.TempDir()
	locked := lockedDir(t, root)

	m := New(Options{StartDir: locked, DirMode: true})
	m.SetSize(200, 20)
	m = drainInit(t, m)
	if m.Err() == nil {
		t.Fatal("Err() = nil for an unreadable start dir")
	}
	m, _ = m.Update(keyEnter)
	if path, ok := m.Selected(); ok {
		t.Errorf("Selected() = %q for an unreadable dir, want none", path)
	}

	m, _ = m.Update(keyLeft)
	if m.Dir() != root || m.Err() != nil {
		t.Errorf("after back: Dir() = %q, Err() = %v; want %q and nil", m.Dir(), m.Err(), root)
	}
}

func TestDirModeEnterSelectsCurrentDirWithOnlyFiles(t *testing.T) {
	root := t.TempDir()
	writeFiles(t, filepath.Join(root, "a.csv"), filepath.Join(root, "b.csv"))

	m := New(Options{StartDir: root, DirMode: true})
	m.SetSize(80, 20)
	m = drainInit(t, m)
	m, _ = m.Update(keyEnter)

	if path, ok := m.Selected(); !ok || path != root {
		t.Errorf("Selected() = (%q, %v), want (%q, true)", path, ok, root)
	}
}

func TestDirModeEnterSelectsCurrentDirNotHighlighted(t *testing.T) {
	root := t.TempDir()
	sub := filepath.Join(root, "sub")
	mkdirs(t, filepath.Join(sub, "deeper"))

	m := New(Options{StartDir: root, DirMode: true})
	m.SetSize(80, 20)
	m = drainInit(t, m)

	m, _ = m.Update(keyEnter)
	if path, ok := m.Selected(); !ok || path != root {
		t.Fatalf("Selected() = (%q, %v), want the header dir %q", path, ok, root)
	}

	m = New(Options{StartDir: root, DirMode: true})
	m.SetSize(80, 20)
	m = drainInit(t, m)
	m, cmd := m.Update(keyRight)
	if m.Dir() != sub {
		t.Fatalf("→ opened %q, want %q", m.Dir(), sub)
	}
	if _, ok := m.Selected(); ok {
		t.Fatal("→ selected a dir, want it only to open")
	}
	m, _ = m.Update(cmd())
	m, _ = m.Update(keyEnter)
	if path, ok := m.Selected(); !ok || path != sub {
		t.Errorf("Selected() = (%q, %v), want (%q, true)", path, ok, sub)
	}
}

func TestSelectRemovedFileIsRefused(t *testing.T) {
	root := t.TempDir()
	csv := filepath.Join(root, "gone.csv")
	writeFiles(t, csv)

	m := New(Options{StartDir: root, AllowedExts: []string{".csv"}})
	m.SetSize(80, 20)
	m = drainInit(t, m)
	if err := os.Remove(csv); err != nil {
		t.Fatal(err)
	}
	m, _ = m.Update(keyEnter)

	if path, ok := m.Selected(); ok {
		t.Errorf("Selected() = %q for a removed file, want none", path)
	}
}

func TestNavigationKeysWaitForTheRead(t *testing.T) {
	root := t.TempDir()
	child := filepath.Join(root, "child")
	mkdirs(t, child)
	writeFiles(t, filepath.Join(child, "a.csv"))

	m := New(Options{StartDir: child, AllowedExts: []string{".csv"}})
	m.SetSize(80, 20)
	m = drainInit(t, m)

	// Back, then Enter before the parent's read lands: the listing still holds
	// child's a.csv, which doesn't exist in root.
	m, cmd := m.Update(keyLeft)
	m, _ = m.Update(keyEnter)
	if path, ok := m.Selected(); ok {
		t.Fatalf("Enter during the read selected %q", path)
	}
	m, _ = m.Update(cmd())
	if m.Dir() != root {
		t.Fatalf("Dir() = %q, want %q", m.Dir(), root)
	}
	m, _ = m.Update(keyEnter) // opens child, the only entry
	if m.Dir() != child {
		t.Errorf("after the read, Enter opened %q, want %q", m.Dir(), child)
	}
}

func TestReadFromAnotherBrowserIgnored(t *testing.T) {
	dir := t.TempDir()
	other := New(Options{StartDir: dir})
	m := New(Options{StartDir: dir})
	m, _ = m.Update(other.Init()())
	if !m.reading {
		t.Error("another browser's read ended this one's")
	}
}

func TestReadErrorMacOSPrivacyHint(t *testing.T) {
	tcc := (&readError{path: "/Users/x/Documents", err: &fs.PathError{Op: "open", Path: "/Users/x/Documents", Err: syscall.EPERM}}).Error()
	mode := (&readError{path: "/tmp/a-locked", err: &fs.PathError{Op: "open", Path: "/tmp/a-locked", Err: syscall.EACCES}}).Error()
	if mode != "can't read a-locked: permission denied" {
		t.Errorf("EACCES: %q", mode)
	}
	want := "can't read Documents: operation not permitted"
	if runtime.GOOS == "darwin" {
		want = "can't read Documents: give the terminal access in Privacy & Security"
	}
	if tcc != want {
		t.Errorf("EPERM: %q, want %q", tcc, want)
	}
}
