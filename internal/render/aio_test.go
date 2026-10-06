package render

import (
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// readmeSources parses the templates README's table into template path →
// AIO source paths (none for a template with no AIO source).
func readmeSources(t *testing.T) map[string][]string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("templates", "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	code := regexp.MustCompile("`([^`]+)`")
	out := map[string][]string{}
	for _, line := range strings.Split(string(data), "\n") {
		cells := strings.Split(line, "|")
		if len(cells) != 4 || !strings.HasPrefix(strings.TrimSpace(cells[1]), "`") {
			continue
		}
		tmpl := code.FindStringSubmatch(cells[1])[1]
		var srcs []string
		for _, m := range code.FindAllStringSubmatch(cells[2], -1) {
			srcs = append(srcs, m[1])
		}
		if len(srcs) == 0 && strings.TrimSpace(cells[2]) != "none" {
			t.Fatalf("README: %s has neither AIO sources nor \"none\"", tmpl)
		}
		out[tmpl] = srcs
	}
	return out
}

func TestReadmeMapsEveryTemplate(t *testing.T) {
	mapped := readmeSources(t)
	var files []string
	err := fs.WalkDir(templateFS(), ".", func(name string, e fs.DirEntry, err error) error {
		if err == nil && !e.IsDir() {
			files = append(files, name)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	listed := slices.Sorted(maps.Keys(mapped))
	if slices.Sort(files); !slices.Equal(files, listed) {
		t.Errorf("README lists %v\nembedded templates are %v", listed, files)
	}

	data, err := os.ReadFile(filepath.Join("templates", "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile("(?m)^AIO commit: `[0-9a-f]{7,40}`$").Match(data) {
		t.Error("README doesn't record the AIO commit the templates were ported from")
	}
}

// TestAIOSourcesExist checks that every AIO file the templates were ported
// from still exists, in the AIO checkout beside this repo (as in the v2
// workspace) or at PICSURE_AIO_DIR. Without one it skips.
func TestAIOSourcesExist(t *testing.T) {
	dir, set := os.LookupEnv("PICSURE_AIO_DIR")
	if !set {
		dir = filepath.Join("..", "..", "..", "pic-sure-all-in-one")
	}
	if _, err := os.Stat(filepath.Join(dir, "docker-compose.yml")); err != nil {
		if set {
			t.Fatalf("PICSURE_AIO_DIR: %v", err)
		}
		t.Skipf("no AIO checkout at %s; set PICSURE_AIO_DIR to check the template sources", dir)
	}
	for tmpl, srcs := range readmeSources(t) {
		for _, src := range srcs {
			if _, err := os.Stat(filepath.Join(dir, src)); err != nil {
				t.Errorf("%s: AIO source %s: %v", tmpl, src, err)
			}
		}
	}
}
