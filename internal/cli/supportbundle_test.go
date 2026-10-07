package cli

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/JamesPeck/pic-sure-cli/internal/ops"
)

func TestBundlePrefix(t *testing.T) {
	for path, want := range map[string]string{
		"/x/out.tar.gz": "out", "/x/out.tgz": "out", "/x/out.tar": "out.tar",
		"/x/.tar.gz": "pic-sure-support", "/x/...tar.gz": "pic-sure-support",
	} {
		if got := bundlePrefix(path); got != want {
			t.Errorf("bundlePrefix(%q) = %q, want %q", path, got, want)
		}
	}
}

// writeBundle leaves nothing behind when the write fails, panics or is
// cancelled, and the archive only once it is complete.
func TestWriteBundleLeavesNoPartialArchive(t *testing.T) {
	ok := func(w io.Writer) (*ops.SupportBundleReport, error) {
		_, err := io.WriteString(w, "archive")
		return &ops.SupportBundleReport{}, err
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	for name, tc := range map[string]struct {
		ctx   context.Context
		write func(io.Writer) (*ops.SupportBundleReport, error)
	}{
		"failed":    {context.Background(), func(io.Writer) (*ops.SupportBundleReport, error) { return nil, errors.New("disk full") }},
		"panicked":  {context.Background(), func(io.Writer) (*ops.SupportBundleReport, error) { panic("boom") }},
		"cancelled": {cancelled, ok},
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			func() {
				defer func() { _ = recover() }()
				if _, err := writeBundle(tc.ctx, filepath.Join(dir, "b.tar.gz"), tc.write); err == nil {
					t.Error("no error")
				}
			}()
			if entries, _ := os.ReadDir(dir); len(entries) != 0 {
				t.Errorf("left %v", entries)
			}
		})
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "b.tar.gz")
	if _, err := writeBundle(context.Background(), path, ok); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("archive %v %v", info, err)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Errorf("dir holds %v", entries)
	}
}
