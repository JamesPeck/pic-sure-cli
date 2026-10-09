// Package filebrowser is the load wizard's file and directory picker: the
// Bubbles filepicker plus a path header, a key hint, a status line, and checks
// the filepicker leaves out (unreadable directories, selecting the current
// directory in dir mode).
package filebrowser

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"charm.land/bubbles/v2/filepicker"
	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/JamesPeck/pic-sure-cli/internal/styles"
)

// fixedChrome is the lines drawn around the filepicker's list: the path
// header, the key hint and the status line.
const fixedChrome = 3

// Options configures a Model. The zero value is valid: it browses the current
// working directory and allows any file.
type Options struct {
	// AllowedExts limits selectable files to these extensions (e.g.
	// {".csv", ".tsv"}). Each must include the leading dot. Empty means any
	// file is selectable. Ignored when DirMode is set.
	AllowedExts []string

	// DirMode selects a directory: Enter picks the one in the header, and
	// files are listed dimmed.
	DirMode bool

	// StartDir is the directory the picker opens in. Empty defaults to the
	// process working directory (or "." if that cannot be determined), matching
	// the filepicker's own default.
	StartDir string

	// Title is the heading drawn above the picker. Empty omits the heading.
	Title string
}

// Model is the file browser component. Construct it with New.
type Model struct {
	fp      filepicker.Model
	title   string
	dirMode bool

	// w is the box width from SetSize, for eliding the header and status line.
	w int

	// The parent polls Selected after each Update: the filepicker reports a
	// selection only through DidSelectFile on the same msg, not as a message.
	selectedPath string
	selected     bool

	// err is shown in the status line until the next navigation or selection.
	err error

	// hasSelectable is whether the current directory holds a file the user can
	// pick, from the read that let the user enter it. The filepicker keeps its
	// entries private.
	hasSelectable bool
}

// New builds a Model from opts. Call Init to start the filepicker's read.
func New(opts Options) Model {
	fp := filepicker.New()

	start := opts.StartDir
	if start == "" {
		if wd, err := os.Getwd(); err == nil {
			start = wd
		} else {
			start = "."
		}
	}
	fp.CurrentDirectory = start

	if opts.DirMode {
		fp.DirAllowed = true
		fp.FileAllowed = false
		// No file name ends in "/", so the filepicker draws every file dimmed.
		fp.AllowedTypes = []string{"/"}
	} else {
		fp.DirAllowed = false
		fp.FileAllowed = true
		fp.AllowedTypes = append([]string{}, opts.AllowedExts...)
	}

	fp.Styles.Cursor = fp.Styles.Cursor.Foreground(styles.Brand)
	fp.Styles.Selected = lipgloss.NewStyle().Foreground(styles.Brand).Bold(true)

	m := Model{fp: fp, title: opts.Title, dirMode: opts.DirMode}
	if entries, err := os.ReadDir(start); err != nil {
		m.err = &readError{dir: start, err: err}
	} else {
		m.hasSelectable = m.anySelectable(entries)
	}
	return m
}

// Init starts the initial directory read.
func (m Model) Init() tea.Cmd {
	return m.fp.Init()
}

// SetSize lays the picker out in a w×h box.
func (m *Model) SetSize(w, h int) {
	m.w = w
	chrome := fixedChrome
	if m.title != "" {
		chrome++
	}
	interior := h - chrome
	if interior < 1 {
		interior = 1
	}
	// AutoHeight would size the list to the whole window on WindowSizeMsg.
	m.fp.AutoHeight = false
	m.fp.SetHeight(interior)
}

// Update advances the filepicker and records a selection or error.
//
// The filepicker sets CurrentDirectory before it reads the new directory and
// silently keeps the old listing when the read fails, so Update reads the
// target first and doesn't pass the key on when it can't be read.
func (m Model) Update(msg tea.Msg) (Model, tea.Cmd) {
	if k, ok := msg.(tea.KeyPressMsg); ok {
		if m.dirMode && key.Matches(k, m.fp.KeyMap.Select) {
			m.selectDir(m.fp.CurrentDirectory)
			return m, nil
		}
		if target, ok := m.navTarget(k); ok {
			entries, err := os.ReadDir(target)
			if err != nil {
				m.err = &readError{dir: target, err: err}
				return m, nil
			}
			m.err = nil
			m.hasSelectable = m.anySelectable(entries)
		}
	}

	var cmd tea.Cmd
	m.fp, cmd = m.fp.Update(msg)

	if ok, path := m.fp.DidSelectFile(msg); ok {
		if abs, err := filepath.Abs(path); err == nil {
			path = abs
		}
		m.selectedPath = path
		m.selected = true
		m.err = nil
		return m, cmd
	}

	if ok, path := m.fp.DidSelectDisabledFile(msg); ok {
		m.err = &selectError{path: path}
	}
	return m, cmd
}

// navTarget is the directory key k would move the filepicker into: the parent
// for a back key, or the highlighted directory for an open key.
func (m Model) navTarget(k tea.KeyPressMsg) (string, bool) {
	switch {
	case key.Matches(k, m.fp.KeyMap.Back):
		return filepath.Dir(m.fp.CurrentDirectory), true
	case key.Matches(k, m.fp.KeyMap.Open):
		path := m.fp.HighlightedPath()
		if path == "" {
			return "", false
		}
		if info, err := os.Stat(path); err == nil && info.IsDir() {
			return path, true
		}
	}
	return "", false
}

// selectDir selects dir if it can still be read.
func (m *Model) selectDir(dir string) {
	if _, err := os.ReadDir(dir); err != nil {
		m.err = &readError{dir: dir, err: err}
		return
	}
	if abs, err := filepath.Abs(dir); err == nil {
		dir = abs
	}
	m.selectedPath = dir
	m.selected = true
	m.err = nil
}

// Selected reports the absolute path the user chose, if any. The parent polls
// this after each Update; ok is false until a selection has been made.
func (m Model) Selected() (path string, ok bool) {
	return m.selectedPath, m.selected
}

// Err returns the error the status line shows, or nil.
func (m Model) Err() error {
	return m.err
}

// Dir returns the directory the picker is showing.
func (m Model) Dir() string {
	return m.fp.CurrentDirectory
}

// View renders an optional title, the current directory, the list, the key
// hint and a status line. The status line is always drawn, empty or not, so
// the height matches what SetSize reserved.
func (m Model) View() string {
	var b strings.Builder

	if m.title != "" {
		b.WriteString(styles.Title.Render(m.title))
		b.WriteByte('\n')
	}

	b.WriteString(m.pathHeader())
	b.WriteByte('\n')

	b.WriteString(m.fp.View())
	b.WriteByte('\n')

	b.WriteString(m.navHint())
	b.WriteByte('\n')

	b.WriteString(m.statusLine())
	return b.String()
}

// pathHeader renders the current directory, left-elided to the box width so
// the end of a deep path stays visible.
func (m Model) pathHeader() string {
	path := m.fp.CurrentDirectory
	if m.w > 0 {
		path = elideLeft(path, m.w)
	}
	return styles.Title.Render(path)
}

func (m Model) navHint() string {
	hint := "←/h ..  ·  →/l open  ·  enter select"
	if m.dirMode {
		hint = "←/h ..  ·  →/l open  ·  enter use current dir"
	}
	if m.w > 0 {
		hint = ansi.Truncate(hint, m.w, "…")
	}
	return hintStyle.Render(hint)
}

// statusLine is an error if there is one, otherwise a notice when a file-mode
// directory holds nothing selectable.
func (m Model) statusLine() string {
	var line string
	switch {
	case m.err != nil:
		line = styles.Bad.Render(m.err.Error())
	case !m.dirMode && !m.hasSelectable:
		line = styles.Warn.Render("no matching files in this directory")
	}
	if m.w > 0 {
		line = ansi.Truncate(line, m.w, "…")
	}
	return line
}

// hintStyle dims the navigation key-hint so it reads as chrome.
var hintStyle = lipgloss.NewStyle().Faint(true)

// elideLeft cuts path from the left to display width w, prefixed with "…".
func elideLeft(path string, w int) string {
	if w <= 0 {
		return path
	}
	width := ansi.StringWidth(path)
	if width <= w {
		return path
	}
	const prefix = "…"
	// TruncateLeft keeps a wide grapheme that straddles the cut, so the result
	// can be a column too wide; drop one more until it fits.
	for drop := width - w + 1; drop < width; drop++ {
		out := ansi.TruncateLeft(path, drop, prefix)
		if ansi.StringWidth(out) <= w {
			return out
		}
	}
	// Degenerate fallback (w smaller than the prefix itself): the prefix alone.
	return prefix
}

// anySelectable reports whether entries hold a file the filepicker would
// list and let the user select.
func (m Model) anySelectable(entries []os.DirEntry) bool {
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if !m.fp.ShowHidden && strings.HasPrefix(e.Name(), ".") {
			continue
		}
		if matchesExt(e.Name(), m.fp.AllowedTypes) {
			return true
		}
	}
	return false
}

func matchesExt(name string, exts []string) bool {
	if len(exts) == 0 {
		return true
	}
	for _, ext := range exts {
		if strings.HasSuffix(name, ext) {
			return true
		}
	}
	return false
}

// selectError reports that the user tried to select a file the picker disallows
// (wrong extension), carrying the offending path for an actionable message.
type selectError struct{ path string }

func (e *selectError) Error() string {
	return "cannot select " + filepath.Base(e.path) + ": not an allowed file type"
}

// readError reports a directory the user can't read.
type readError struct {
	dir string
	err error
}

func (e *readError) Error() string {
	reason := e.err
	var pe *fs.PathError
	if errors.As(e.err, &pe) {
		reason = pe.Err
	}
	msg := "can't read " + filepath.Base(e.dir) + ": " + reason.Error()
	if runtime.GOOS == "darwin" && errors.Is(e.err, fs.ErrPermission) {
		msg += " (check the terminal's file access in macOS Privacy & Security)"
	}
	return msg
}

func (e *readError) Unwrap() error { return e.err }
