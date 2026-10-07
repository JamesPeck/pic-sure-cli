package log

import (
	"bytes"
	"encoding/json"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var t0 = time.Date(2026, 10, 6, 15, 30, 45, 123_000_000, time.UTC)

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func readRecords(t *testing.T, path string) []map[string]any {
	t.Helper()
	var recs []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(readFile(t, path)), "\n") {
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("%v: %s", err, line)
		}
		recs = append(recs, m)
	}
	return recs
}

func messages(recs []map[string]any) []string {
	var msgs []string
	for _, r := range recs {
		msgs = append(msgs, r["msg"].(string))
	}
	return msgs
}

func TestParseLevel(t *testing.T) {
	for in, want := range map[string]slog.Level{
		"debug": slog.LevelDebug, "INFO": slog.LevelInfo, "Warn": slog.LevelWarn, "error": slog.LevelError,
	} {
		if got, err := ParseLevel(in); err != nil || got != want {
			t.Errorf("ParseLevel(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	for _, in := range []string{"", "verbose", "warning", "DEBUG-4"} {
		if _, err := ParseLevel(in); err == nil {
			t.Errorf("ParseLevel(%q) succeeded", in)
		}
	}
}

func TestStderrGetsOnlyTheLevel(t *testing.T) {
	var stderr bytes.Buffer
	run := New(Options{Level: slog.LevelWarn, Stderr: &stderr, File: true, Redactor: &Redactor{}})
	run.Logger().Info("quiet")
	run.Logger().Warn("loud")
	if got := stderr.String(); strings.Contains(got, "quiet") || !strings.Contains(got, `level=WARN msg=loud`) {
		t.Errorf("stderr = %q", got)
	}
}

func TestOpenFileWritesEveryRecordFromTheStart(t *testing.T) {
	dir := t.TempDir()
	run := New(Options{Level: slog.LevelError, File: true, Redactor: &Redactor{}})
	run.Logger().Debug("before")
	path, err := run.OpenFile(dirStore(dir), t0)
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(dir, ".pic-sure", "logs", "cli-20261006T153045.123Z.log"); path != want {
		t.Errorf("path = %q, want %q", path, want)
	}
	run.Logger().Info("after")
	if again, err := run.OpenFile(dirStore(t.TempDir()), t0); again != path || err != nil {
		t.Errorf("second OpenFile = %q, %v; want the first file", again, err)
	}
	if err := run.Close(); err != nil {
		t.Fatal(err)
	}
	run.Logger().Info("closed")

	if got := messages(readRecords(t, path)); strings.Join(got, ",") != "before,after" {
		t.Errorf("records = %q, want before,after", got)
	}
	for p, want := range map[string]os.FileMode{path: 0o600, filepath.Dir(path): 0o700, filepath.Join(dir, ".pic-sure"): 0o700} {
		if fi, err := os.Stat(p); err != nil || fi.Mode().Perm() != want {
			t.Errorf("%s: mode %v, %v; want %v", p, fi.Mode().Perm(), err, want)
		}
	}
}

func TestOpenFileNameCollision(t *testing.T) {
	dir := t.TempDir()
	var paths []string
	for range 2 {
		run := New(Options{File: true, Redactor: &Redactor{}})
		p, err := run.OpenFile(dirStore(dir), t0)
		if err != nil {
			t.Fatal(err)
		}
		paths = append(paths, filepath.Base(p))
		_ = run.Close()
	}
	if paths[0] != "cli-20261006T153045.123Z.log" || paths[1] != "cli-20261006T153045.123Z-1.log" {
		t.Errorf("names = %q", paths)
	}
}

func TestNoFileWithoutTheOption(t *testing.T) {
	dir := t.TempDir()
	run := New(Options{Level: slog.LevelDebug, Redactor: &Redactor{}})
	run.Logger().Info("x")
	if path, err := run.OpenFile(dirStore(dir), t0); path != "" || err != nil {
		t.Errorf("OpenFile = %q, %v; want no file", path, err)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Errorf("stack dir has %v, want nothing", entries)
	}
}

func TestOpenFileAfterCloseWritesNothing(t *testing.T) {
	dir := t.TempDir()
	run := New(Options{File: true, Redactor: &Redactor{}})
	_ = run.Close()
	if path, err := run.OpenFile(dirStore(dir), t0); path != "" || err != nil {
		t.Errorf("OpenFile = %q, %v; want no file", path, err)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Errorf("stack dir has %v, want nothing", entries)
	}
}

func TestEarlyRecordsAreBounded(t *testing.T) {
	run := New(Options{File: true, Redactor: &Redactor{}})
	big := strings.Repeat("x", maxEarly/2)
	run.Logger().Info("first", "pad", big)
	run.Logger().Info("second", "pad", big) // doesn't fit
	run.Logger().Info("third")
	path, err := run.OpenFile(dirStore(t.TempDir()), t0)
	if err != nil {
		t.Fatal(err)
	}
	_ = run.Close()
	recs := readRecords(t, path)
	if got := messages(recs); strings.Join(got, ",") != "first,third,dropped early log records" {
		t.Fatalf("records = %q", got)
	}
	if n := recs[2]["count"]; n != float64(1) {
		t.Errorf("count = %v, want 1", n)
	}
}

func TestOpenFileFailureLeavesStderr(t *testing.T) {
	var stderr bytes.Buffer
	run := New(Options{Stderr: &stderr, File: true, Redactor: &Redactor{}})
	missing := filepath.Join(t.TempDir(), "missing")
	if path, err := run.OpenFile(dirStore(missing), t0); err == nil || path != "" {
		t.Errorf("OpenFile = %q, %v; want an error", path, err)
	}
	run.Logger().Info("still here")
	if !strings.Contains(stderr.String(), "still here") {
		t.Errorf("stderr = %q", stderr.String())
	}
}

// dirStore is a Store on a plain directory, without a manifest.
type dirStore string

func (d dirStore) MkdirAll(rel string, perm fs.FileMode) error {
	r, err := os.OpenRoot(string(d))
	if err != nil {
		return err
	}
	defer func() { _ = r.Close() }()
	return r.MkdirAll(rel, perm)
}

func (d dirStore) CreateFile(rel string, perm fs.FileMode) (*os.File, error) {
	r, err := os.OpenRoot(string(d))
	if err != nil {
		return nil, err
	}
	defer func() { _ = r.Close() }()
	return r.OpenFile(rel, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
}

func (d dirStore) Remove(rel string) error {
	r, err := os.OpenRoot(string(d))
	if err != nil {
		return err
	}
	defer func() { _ = r.Close() }()
	return r.Remove(rel)
}

func (d dirStore) Path(rel string) string { return filepath.Join(string(d), filepath.FromSlash(rel)) }
