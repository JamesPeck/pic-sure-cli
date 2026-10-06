package cache_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/JamesPeck/pic-sure-cli/internal/cache"
	"github.com/JamesPeck/pic-sure-cli/internal/catalog"
)

// fakeHome points HOME and TMPDIR at separate new directories and unsets
// XDG_CACHE_HOME.
func fakeHome(t *testing.T) (home, tmp string) {
	t.Helper()
	home, tmp = t.TempDir(), t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("TMPDIR", tmp)
	t.Setenv("XDG_CACHE_HOME", "")
	return home, tmp
}

func TestDefaultRoot(t *testing.T) {
	home, _ := fakeHome(t)
	xdg := t.TempDir()
	for _, tc := range []struct{ xdg, want string }{
		{"", filepath.Join(home, ".cache", "pic-sure")},
		{xdg, filepath.Join(xdg, "pic-sure")},
		{"relative/cache", filepath.Join(home, ".cache", "pic-sure")}, // XDG says to ignore it
	} {
		t.Setenv("XDG_CACHE_HOME", tc.xdg)
		if got, err := cache.DefaultRoot(); err != nil || got != tc.want {
			t.Errorf("XDG_CACHE_HOME=%q: DefaultRoot() = %q, %v; want %q", tc.xdg, got, err, tc.want)
		}
	}
}

func TestDefaultRootRefusesTheTemporaryDirectory(t *testing.T) {
	_, tmp := fakeHome(t)
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(tmp, link); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ name, tmpdir, xdg, home string }{
		{"xdg in tmp", tmp, filepath.Join(tmp, "cache"), ""},
		{"xdg is tmp", tmp, tmp, ""},
		{"xdg through a link to tmp", tmp, filepath.Join(link, "cache"), ""},
		{"tmp through a link", link, filepath.Join(tmp, "cache"), ""},
		{"home in tmp", tmp, "", filepath.Join(tmp, "home")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("TMPDIR", tc.tmpdir)
			t.Setenv("XDG_CACHE_HOME", tc.xdg)
			if tc.home != "" {
				t.Setenv("HOME", tc.home)
			}
			if got, err := cache.DefaultRoot(); err == nil || !strings.Contains(err.Error(), "temporary directory") {
				t.Errorf("DefaultRoot() = %q, %v; want a temporary-directory error", got, err)
			}
		})
	}
}

func TestOpenMakesTheLayout(t *testing.T) {
	root := filepath.Join(t.TempDir(), "xdg", "pic-sure")
	c, err := cache.Open(root, cache.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if c.Root() != root {
		t.Errorf("Root() = %s, want %s", c.Root(), root)
	}
	want := []string{"build", "downloads", "git", "locks", "src", "tmp"}
	if got := entries(t, root); !slices.Equal(got, want) {
		t.Errorf("root holds %q, want %q", got, want)
	}
	if got := c.DownloadsDir(); got != filepath.Join(root, "downloads") {
		t.Errorf("DownloadsDir() = %s", got)
	}
	if got := c.ReleaseControlDir(); got != filepath.Join(root, "release-control") {
		t.Errorf("ReleaseControlDir() = %s", got)
	}
	if _, err := cache.Open(root, cache.Options{}); err != nil {
		t.Errorf("reopening: %v", err)
	}
	if _, err := cache.Open("relative", cache.Options{}); err == nil {
		t.Error("opened a relative root")
	}
}

func TestBuildDir(t *testing.T) {
	c := open(t, cache.Options{})
	sha := strings.Repeat("0123456789", 4)
	got, err := c.BuildDir(sha)
	if want := filepath.Join(c.Root(), "build", "012345678901"); err != nil || got != want {
		t.Errorf("BuildDir = %s, %v; want %s", got, err, want)
	}
	if _, err := c.BuildDir(sha[:12]); err == nil {
		t.Error("BuildDir took an abbreviated sha")
	}
}

func TestTempDirIsAFreshPrivateDirectoryInTheCache(t *testing.T) {
	c := open(t, cache.Options{})
	a, err := c.TempDir("phenotype-")
	if err != nil {
		t.Fatal(err)
	}
	b, err := c.TempDir("phenotype-")
	if err != nil {
		t.Fatal(err)
	}
	if a == b || filepath.Dir(a) != filepath.Join(c.Root(), "tmp") || !strings.HasPrefix(filepath.Base(a), "phenotype-") {
		t.Errorf("TempDir gave %s and %s", a, b)
	}
	if info, err := os.Stat(a); err != nil || info.Mode().Perm() != 0o700 {
		t.Errorf("mode = %v, %v; want 0700", info.Mode(), err)
	}
}

func TestTempDirRemovesOnlyTheLeftoversOfDeadRuns(t *testing.T) {
	c := open(t, cache.Options{})
	live, err := c.TempDir("live-")
	if err != nil {
		t.Fatal(err)
	}
	dead, err := c.TempDir("dead-")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dead, "allConcepts.csv"), []byte("x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-8 * 24 * time.Hour)
	if err := os.Chtimes(dead, old, old); err != nil {
		t.Fatal(err)
	}

	if _, err := c.TempDir("next-"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dead); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a week-old temporary directory is still there (%v)", err)
	}
	if _, err := os.Stat(live); err != nil {
		t.Errorf("removed a live run's temporary directory: %v", err)
	}
}

type volumeRecorder []string

func (v *volumeRecorder) VolumeCreate(_ context.Context, name string, _ map[string]string) error {
	*v = append(*v, name)
	return nil
}

func TestEnsureMavenVolumeCreatesTheCatalogsHostVolume(t *testing.T) {
	var created volumeRecorder
	err := cache.EnsureMavenVolume(context.Background(), &created)
	if err != nil || !slices.Equal(created, volumeRecorder{cache.MavenVolume}) {
		t.Fatalf("EnsureMavenVolume: %v; created %q", err, created)
	}
	v, ok := catalog.LookupVolume(cache.MavenVolume)
	if !ok || v.Scope != catalog.HostScoped || v.DockerName("any-stack") != cache.MavenVolume {
		t.Errorf("the catalog's %s is %+v, %v; want a host-scoped volume", cache.MavenVolume, v, ok)
	}
}
