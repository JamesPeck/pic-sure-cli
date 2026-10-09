package release_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/JamesPeck/pic-sure-cli/internal/cache"
	"github.com/JamesPeck/pic-sure-cli/internal/catalog"
	"github.com/JamesPeck/pic-sure-cli/internal/docker"
	"github.com/JamesPeck/pic-sure-cli/internal/events"
	"github.com/JamesPeck/pic-sure-cli/internal/exitcode"
	"github.com/JamesPeck/pic-sure-cli/internal/git"
	"github.com/JamesPeck/pic-sure-cli/internal/release"
	"github.com/JamesPeck/pic-sure-cli/internal/stack"
)

// execRunner runs commands for real, for these tests' git, and logs each
// git subcommand it runs.
type execRunner struct{ log *[]string }

func (r execRunner) Run(ctx context.Context, c docker.Cmd) (docker.Result, error) {
	var stdout, stderr bytes.Buffer
	code, err := r.Stream(ctx, c, &stdout, &stderr)
	return docker.Result{Stdout: stdout.Bytes(), Stderr: stderr.Bytes(), ExitCode: code}, err
}

func (r execRunner) Stream(ctx context.Context, c docker.Cmd, stdout, stderr io.Writer) (int, error) {
	if r.log != nil {
		*r.log = append(*r.log, strings.Join(c.Argv, " "))
	}
	cmd := exec.CommandContext(ctx, c.Argv[0], c.Argv[1:]...)
	cmd.Env = append(os.Environ(), c.Env...)
	cmd.Dir, cmd.Stdin, cmd.Stdout, cmd.Stderr = c.Dir, c.Stdin, stdout, stderr
	err := cmd.Run()
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode(), nil
	}
	return 0, err
}

// world is a fake GitHub: a release-control repo and one repo per
// component, reached through file:// URLs.
type world struct {
	t     *testing.T
	rc    *repo
	repos map[string]*repo // by component name
	cache *cache.Cache
	git   git.Client
	calls []string // every git command the code under test ran
}

type repo struct {
	t   *testing.T
	dir string
}

func newWorld(t *testing.T) *world {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not found")
	}
	for _, k := range []string{"GIT_DIR", "GIT_WORK_TREE", "GIT_INDEX_FILE"} {
		t.Setenv(k, "")
		_ = os.Unsetenv(k)
	}
	for _, k := range []string{"GIT_AUTHOR", "GIT_COMMITTER"} {
		t.Setenv(k+"_NAME", "Test")
		t.Setenv(k+"_EMAIL", "test@example.com")
	}
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_ALLOW_PROTOCOL", "file") // never reach the network

	w := &world{t: t, rc: newRepo(t, "james_mono"), repos: map[string]*repo{}}
	var config strings.Builder
	for _, comp := range catalog.Components() {
		r := newRepo(t, "main")
		w.repos[comp.Name] = r
		fmt.Fprintf(&config, "[url %q]\n\tinsteadOf = %s\n", "file://"+r.dir, comp.CloneURL())
	}
	path := filepath.Join(t.TempDir(), "gitconfig")
	if err := os.WriteFile(path, []byte(config.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", path)

	w.git = git.New(execRunner{log: &w.calls})
	c, err := cache.Open(filepath.Join(t.TempDir(), "cache"), cache.Options{Git: w.git})
	if err != nil {
		t.Fatal(err)
	}
	w.cache = c
	return w
}

func newRepo(t *testing.T, branch string) *repo {
	r := &repo{t: t, dir: t.TempDir()}
	r.git("init", "--quiet", "--initial-branch="+branch)
	return r
}

func (r *repo) git(args ...string) string {
	r.t.Helper()
	out, err := exec.Command("git", append([]string{"-C", r.dir}, args...)...).CombinedOutput()
	if err != nil {
		r.t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// commit writes name and commits it, returning the commit's sha.
func (r *repo) commit(name, content string) string {
	r.t.Helper()
	if err := os.WriteFile(filepath.Join(r.dir, name), []byte(content), 0o644); err != nil {
		r.t.Fatal(err)
	}
	r.git("add", name)
	r.git("commit", "--quiet", "-m", "change "+name)
	return r.git("rev-parse", "HEAD")
}

// fetches counts the git fetches and clones run since the last call.
func (w *world) fetches() int {
	n := 0
	for _, c := range w.calls {
		if strings.Contains(c, " fetch ") || strings.Contains(c, " clone ") {
			n++
		}
	}
	w.calls = nil
	return n
}

func (w *world) url() string { return "file://" + w.rc.dir }

func (w *world) fetch(opts release.Options) (*release.Release, error) {
	w.t.Helper()
	opts.Repo = w.url()
	if opts.Branch == "" {
		opts.Branch = "james_mono"
	}
	var rec events.Recorder
	return release.Fetch(context.Background(), w.cache, w.git, &rec, "release", opts)
}

const specV1 = `{"application": [
  {"project_job_git_key": "PSA", "git_hash": "v4.0.0"},
  {"project_job_git_key": "PSF", "git_hash": "main"},
  {"project_job_git_key": "PSM", "git_hash": "v2.0.0"},
  {"project_job_git_key": "PSCLI", "git_hash": "v2.0.0"}
]}`

func TestFetchReadsTheBranchHeadAndHonoursAPin(t *testing.T) {
	w := newWorld(t)
	first := w.rc.commit("build-spec.json", specV1)

	rel, err := w.fetch(release.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if rel.Commit != first || rel.Branch != "james_mono" || rel.Repo != w.url() {
		t.Errorf("release = %+v, want commit %s", rel, first)
	}
	if got, _ := rel.Spec.Ref("PSCLI"); got != "v2.0.0" {
		t.Errorf("PSCLI = %q", got)
	}

	second := w.rc.commit("build-spec.json", strings.Replace(specV1, `"PSCLI", "git_hash": "v2.0.0"`, `"PSCLI", "git_hash": "v2.1.0"`, 1))
	rel, err = w.fetch(release.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if rel.Commit != second {
		t.Errorf("after a push, commit = %s, want the new head %s", rel.Commit, second)
	}
	if got, _ := rel.Spec.Ref("PSCLI"); got != "v2.1.0" {
		t.Errorf("after a push, PSCLI = %q", got)
	}

	w.fetches()
	rel, err = w.fetch(release.Options{Commit: first[:10]})
	if err != nil {
		t.Fatal(err)
	}
	if n := w.fetches(); n != 0 {
		t.Errorf("a pin the clone has ran %d fetches, want none", n)
	}
	if got, _ := rel.Spec.Ref("PSCLI"); rel.Commit != first || got != "v2.0.0" {
		t.Errorf("pinned: commit %s PSCLI %q, want %s v2.0.0", rel.Commit, got, first)
	}
	if _, err := os.Stat(filepath.Join(w.cache.ReleaseControlDir(), "HEAD")); err != nil {
		t.Errorf("release-control isn't a bare clone in the cache: %v", err)
	}
}

func TestFetchFindsAPinPushedAfterTheClone(t *testing.T) {
	w := newWorld(t)
	w.rc.commit("build-spec.json", specV1)
	if _, err := w.fetch(release.Options{}); err != nil {
		t.Fatal(err)
	}
	later := w.rc.commit("README.md", "later\n")
	rel, err := w.fetch(release.Options{Commit: later})
	if err != nil || rel.Commit != later {
		t.Fatalf("Fetch = %+v, %v; want commit %s", rel, err, later)
	}
}

func TestFetchFindsATagOnlyPinAfterTheClone(t *testing.T) {
	w := newWorld(t)
	w.rc.commit("build-spec.json", specV1)
	if _, err := w.fetch(release.Options{}); err != nil {
		t.Fatal(err)
	}
	w.rc.git("switch", "--quiet", "-c", "hotfix")
	hotfix := w.rc.commit("build-spec.json", specV1+"\n")
	w.rc.git("tag", "v1-hotfix")
	w.rc.git("switch", "--quiet", "james_mono")
	w.rc.git("branch", "-D", "hotfix")
	rel, err := w.fetch(release.Options{Commit: hotfix})
	if err != nil || rel.Commit != hotfix {
		t.Fatalf("Fetch = %+v, %v; want commit %s", rel, err, hotfix)
	}
}

func TestFetchFailures(t *testing.T) {
	w := newWorld(t)
	w.rc.commit("README.md", "no build-spec here\n")
	tests := []struct {
		name     string
		opts     release.Options
		wantCode int
		wantErr  string
	}{
		{"malformed pin", release.Options{Commit: "--upload-pack=x"}, exitcode.CodeUsage, "is not a commit sha"},
		{"unknown pin", release.Options{Commit: strings.Repeat("0", 40)}, exitcode.CodePrecondition, "has no commit"},
		{"unknown branch", release.Options{Branch: "nope"}, exitcode.CodePrecondition, "has no branch nope"},
		{"no build-spec", release.Options{}, exitcode.CodeFailed, "no build-spec.json"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := w.fetch(tt.opts)
			if code := exitcode.FromError(err); code != tt.wantCode || !strings.Contains(fmt.Sprint(err), tt.wantErr) {
				t.Errorf("err = %v (exit %d), want exit %d containing %q", err, code, tt.wantCode, tt.wantErr)
			}
		})
	}
}

func TestResolveComponents(t *testing.T) {
	w := newWorld(t)
	w.rc.commit("build-spec.json", specV1)

	ps := w.repos[catalog.PicSure]
	tagged := ps.commit("pom.xml", "<project/>\n")
	ps.git("tag", "v4.0.0")
	ps.commit("pom.xml", "<project>next</project>\n") // main moves past the tag
	fe := w.repos[catalog.Frontend]
	feMain := fe.commit("package.json", "{}\n")
	mig := w.repos[catalog.Migrations]
	migTagged := mig.commit("V1__init.sql", "select 1;\n")
	mig.git("tag", "-a", "-m", "release", "v2.0.0") // annotated: peeled to the commit
	etl := w.repos[catalog.DictionaryETL]
	etl.commit("Dockerfile", "FROM scratch\n")
	pinned := etl.commit("Dockerfile", "FROM scratch # pinned\n")
	etl.commit("Dockerfile", "FROM scratch # head\n")

	rel, err := w.fetch(release.Options{})
	if err != nil {
		t.Fatal(err)
	}
	var rec events.Recorder
	cfg := stack.Components{}
	got, err := rel.ResolveComponents(context.Background(), w.cache, &rec, "release", cfg)
	if err != nil {
		t.Fatal(err)
	}
	etlHead := etl.git("rev-parse", "HEAD")
	want := map[string]stack.Component{
		catalog.PicSure:       {Ref: "v4.0.0", Commit: tagged},
		catalog.Frontend:      {Ref: "main", Commit: feMain},
		catalog.Migrations:    {Ref: "v2.0.0", Commit: migTagged},
		catalog.DictionaryETL: {Ref: "main", Commit: etlHead},
	}
	checkComponents(t, got, want)
	warnings := warningTexts(rec.Events())
	if len(warnings) != 1 || !strings.Contains(warnings[0], "no DICTIONARY_ETL entry") {
		t.Errorf("warnings = %q, want one about DICTIONARY_ETL", warnings)
	}

	// A branch resolves to its new head after a push, and pic-sure.yaml's
	// ref beats the build-spec's.
	feNext := fe.commit("package.json", `{"v": 2}`+"\n")
	cfg.DictionaryETL.Ref = pinned[:8]
	got, err = rel.ResolveComponents(context.Background(), w.cache, nil, "release", cfg)
	if err != nil {
		t.Fatal(err)
	}
	want[catalog.Frontend] = stack.Component{Ref: "main", Commit: feNext}
	want[catalog.DictionaryETL] = stack.Component{Ref: pinned[:8], Commit: pinned}
	checkComponents(t, got, want)

	// A full sha the clone already has needs no fetch.
	cfg = stack.Components{}
	cfg.PicSure.Ref = tagged
	w.fetches()
	if _, err := rel.ResolveComponents(context.Background(), w.cache, nil, "release", cfg); err != nil {
		t.Fatal(err)
	}
	if n := w.fetches(); n != 3 {
		t.Errorf("ran %d fetches, want 3 (none for pic-sure's known sha)", n)
	}

	// A component with a local source is left out, and its repo isn't
	// fetched.
	cfg = stack.Components{}
	cfg.Frontend.Source = "/src/frontend"
	w.fetches()
	local, err := rel.ResolveComponents(context.Background(), w.cache, nil, "release", cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := local[catalog.Frontend]; ok || len(local) != 3 {
		t.Errorf("with a frontend source, resolved %v; want every component but the frontend", local)
	}
	if n := w.fetches(); n != 3 {
		t.Errorf("ran %d fetches, want 3 (none for the frontend)", n)
	}
	only, err := rel.ResolveComponents(context.Background(), w.cache, nil, "release", stack.Components{}, catalog.Migrations)
	if err != nil {
		t.Fatal(err)
	}
	if len(only) != 1 || only[catalog.Migrations].Commit != migTagged {
		t.Errorf("resolving migrations only gave %v", only)
	}
	if n := w.fetches(); n != 1 {
		t.Errorf("resolving migrations only ran %d fetches, want 1", n)
	}

	// The resolved commits are in the cache's clones, so EnsureSource
	// needs no fetch.
	if _, err := w.cache.EnsureSource(context.Background(), catalog.Migrations, migTagged); err != nil {
		t.Errorf("EnsureSource: %v", err)
	}
}

func TestResolveComponentsUnknownRef(t *testing.T) {
	w := newWorld(t)
	w.rc.commit("build-spec.json", specV1)
	for _, r := range w.repos {
		r.commit("README.md", "hi\n")
	}
	rel, err := w.fetch(release.Options{})
	if err != nil {
		t.Fatal(err)
	}
	// pic-sure has no v4.0.0 tag.
	_, err = rel.ResolveComponents(context.Background(), w.cache, nil, "release", stack.Components{})
	if code := exitcode.FromError(err); code != exitcode.CodePrecondition || !strings.Contains(err.Error(), `"v4.0.0"`) {
		t.Errorf("err = %v (exit %d), want exit 3 naming v4.0.0", err, code)
	}
}

func checkComponents(t *testing.T, got, want map[string]stack.Component) {
	t.Helper()
	for name, w := range want {
		if got[name] != w {
			t.Errorf("%s = %+v, want %+v", name, got[name], w)
		}
	}
	if len(got) != len(want) {
		t.Errorf("got %d components, want %d", len(got), len(want))
	}
}
