package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWriteCommandDocsReplacesThePages(t *testing.T) {
	dir := t.TempDir()
	stale := filepath.Join(dir, "pic-sure_gone.md")
	if err := os.WriteFile(stale, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := WriteCommandDocs(dir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Errorf("stale page survived: %v", err)
	}
	index, err := os.ReadFile(filepath.Join(dir, "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"(pic-sure_init.md)", "(pic-sure_data_demo.md)"} {
		if !strings.Contains(string(index), want) {
			t.Errorf("index lacks %s", want)
		}
	}
	for _, unwanted := range []string{"pic-sure_help.md"} {
		if _, err := os.Stat(filepath.Join(dir, unwanted)); err == nil {
			t.Errorf("%s was written", unwanted)
		}
	}
	page, err := os.ReadFile(filepath.Join(dir, "pic-sure_data_demo.md"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"# pic-sure data demo\n", "--heap MB", "```text\nReplace the stack's HPDS", "See also: [`pic-sure data`](pic-sure_data.md)\n"} {
		if !strings.Contains(string(page), want) {
			t.Errorf("data demo page lacks %q", want)
		}
	}
}
