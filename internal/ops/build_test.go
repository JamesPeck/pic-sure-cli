package ops_test

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/JamesPeck/pic-sure-cli/internal/cache"
	"github.com/JamesPeck/pic-sure-cli/internal/catalog"
	"github.com/JamesPeck/pic-sure-cli/internal/docker"
	"github.com/JamesPeck/pic-sure-cli/internal/docker/fakerunner"
	"github.com/JamesPeck/pic-sure-cli/internal/events"
	"github.com/JamesPeck/pic-sure-cli/internal/exitcode"
	"github.com/JamesPeck/pic-sure-cli/internal/git"
	"github.com/JamesPeck/pic-sure-cli/internal/ops"
	"github.com/JamesPeck/pic-sure-cli/internal/render"
	"github.com/JamesPeck/pic-sure-cli/internal/stack"
)

const (
	psSHA    = "1111111111111111111111111111111111111111"
	migSHA   = "3333333333333333333333333333333333333333"
	localSHA = "abcdefabcdefabcdefabcdefabcdefabcdefabcd"
)

type buildFixture struct {
	t     *testing.T
	f     *fakerunner.Runner
	rec   *events.Recorder
	d     *ops.Deps
	st    *stack.Stack
	cfg   *stack.Config
	state *stack.State
	cache *cache.Cache
	root  string
}

func newBuildFixture(t *testing.T) *buildFixture {
	t.Helper()
	root := filepath.Join(t.TempDir(), "cache")
	f := fakerunner.New(t)
	c, err := cache.Open(root, cache.Options{Git: git.New(f), Holder: "test"})
	if err != nil {
		t.Fatal(err)
	}
	st, err := stack.Create(filepath.Join(t.TempDir(), "stack"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	cfg := stack.DefaultConfig()
	cfg.Name = "demo"
	rec := &events.Recorder{}
	x := &buildFixture{
		t:   t,
		f:   f,
		rec: rec,
		d: &ops.Deps{Runner: f, Docker: docker.NewEngine(f), Git: git.New(f), Sink: rec,
			Rand: strings.NewReader(strings.Repeat("r", 256))},
		st:  st,
		cfg: &cfg,
		state: &stack.State{Components: map[string]stack.Component{
			catalog.PicSure:       {Ref: "v1", Commit: psSHA},
			catalog.Frontend:      {Ref: "v2", Commit: feSHA},
			catalog.Migrations:    {Ref: "v3", Commit: migSHA},
			catalog.DictionaryETL: {Ref: "v4", Commit: etlSHA},
		}},
		cache: c,
		root:  root,
	}
	// Render needs these trees, so the step makes sure they exist.
	x.tree(catalog.PicSure, psSHA)
	x.tree(catalog.Migrations, migSHA)
	return x
}

func (x *buildFixture) feTag() string {
	return feSHA[:12] + "-" + ops.FrontendConfigHash(render.ViteEnv(x.cfg))[:8]
}

// image makes ref present with labels.
func (x *buildFixture) image(ref string, labels map[string]string) {
	b, err := json.Marshal(labels)
	if err != nil {
		x.t.Fatal(err)
	}
	x.f.On(fakerunner.Exact("docker", "image", "inspect", ref)).Stdout(`[{"Id":"sha256:1","Config":{"Labels":` + string(b) + `}}]`)
}

// reactorFresh makes every reactor image present at tag, built from sha.
func (x *buildFixture) reactorFresh(tag, sha string) {
	for _, img := range catalog.ImagesBuiltFrom(catalog.PicSure) {
		x.image(img.Repository()+":"+tag, map[string]string{ops.ReactorSrcLabel: sha})
	}
}

func (x *buildFixture) frontendFresh() {
	x.image("hms-dbmi/pic-sure-httpd:"+x.feTag(), map[string]string{
		ops.FrontendSrcLabel: feSHA, ops.FrontendConfigLabel: ops.FrontendConfigHash(render.ViteEnv(x.cfg))})
}

func (x *buildFixture) etlFresh(tag, sha string) {
	x.image("hms-dbmi/dictionary-etl:"+tag, map[string]string{ops.DictionaryETLSrcLabel: sha})
}

// missing makes every other image absent.
func (x *buildFixture) missing() {
	x.f.On(fakerunner.Glob("docker image inspect *")).Exit(1).Stderr("Error response from daemon: No such image: x\n")
}

// tree makes the cache's source tree of component at sha, with a Dockerfile.
func (x *buildFixture) tree(component, sha string) {
	comp, _ := catalog.LookupComponent(component)
	dir := filepath.Join(x.root, "src", comp.RepoName(), sha)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		x.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte("FROM scratch\n"), 0o644); err != nil {
		x.t.Fatal(err)
	}
}

// checkout makes a local checkout at HEAD localSHA that git reports dirty
// or clean, and returns its path.
func (x *buildFixture) checkout(dirty bool) string {
	dir := x.t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte("FROM scratch\n"), 0o644); err != nil {
		x.t.Fatal(err)
	}
	x.f.On(fakerunner.Exact("git", "--no-optional-locks", "-C", dir, "rev-parse", "--verify", "HEAD^{commit}")).Stdout(localSHA + "\n")
	status := x.f.On(fakerunner.Glob("git --no-optional-locks -C " + dir + " status *"))
	if dirty {
		status.Stdout(" M Dockerfile\x00")
	}
	return dir
}

func (x *buildFixture) build(opts ops.ImagesOptions) (*ops.BuildReport, error) {
	x.t.Helper()
	opts.Cache = x.cache
	return ops.Build(context.Background(), x.d, x.st, x.cfg, x.state, ops.BuildOptions{ImagesOptions: opts})
}

func actions(r *ops.BuildReport) map[string]string {
	out := map[string]string{}
	for _, img := range r.Images {
		out[img.Ref] = img.Action
	}
	return out
}

func TestBuildKeepsImagesThatAreUpToDate(t *testing.T) {
	x := newBuildFixture(t)
	x.reactorFresh(psSHA[:12], psSHA)
	x.frontendFresh()
	x.etlFresh(etlSHA[:12], etlSHA)

	r, err := x.build(ops.ImagesOptions{})
	if err != nil {
		t.Fatal(err)
	}
	x.f.AssertNotCalled(fakerunner.Glob("docker build *"))
	x.f.AssertNotCalled(fakerunner.Glob("docker run *"))
	x.f.AssertNotCalled(fakerunner.Glob("git *"))
	if len(r.Images) != 13 {
		t.Errorf("reported %d images, want 13", len(r.Images))
	}
	for ref, a := range actions(r) {
		if a != ops.ImageUpToDate {
			t.Errorf("%s: %s, want up_to_date", ref, a)
		}
	}
	saved, err := x.st.LoadState()
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"pic-sure-httpd": x.feTag(), "dictionary-etl": etlSHA[:12]}
	for _, img := range catalog.ImagesBuiltFrom(catalog.PicSure) {
		want[img.Name] = psSHA[:12]
	}
	if len(saved.Images) != len(want) {
		t.Errorf("saved images %v, want %v", saved.Images, want)
	}
	for k, v := range want {
		if saved.Images[k] != v {
			t.Errorf("saved %s = %q, want %q", k, saved.Images[k], v)
		}
	}
}

func TestBuildBuildsOnlyWhatIsMissing(t *testing.T) {
	x := newBuildFixture(t)
	x.reactorFresh(psSHA[:12], psSHA)
	x.frontendFresh()
	x.missing()
	x.tree(catalog.DictionaryETL, etlSHA)
	x.f.On(fakerunner.Glob("docker build *"))

	r, err := x.build(ops.ImagesOptions{})
	if err != nil {
		t.Fatal(err)
	}
	builds := x.f.CallsMatching(fakerunner.Glob("docker build *"))
	if len(builds) != 1 || !strings.Contains(builds[0].String(), "-t hms-dbmi/dictionary-etl:"+etlSHA[:12]) {
		t.Errorf("builds: %v, want only dictionary-etl", builds)
	}
	if a := actions(r)["hms-dbmi/dictionary-etl:"+etlSHA[:12]]; a != ops.ImageBuilt {
		t.Errorf("dictionary-etl: %s, want built", a)
	}
	// The build log is in the manifest, so destroy removes it.
	m, err := x.st.Manifest()
	if err != nil {
		t.Fatal(err)
	}
	if !m.Has(ops.BuildLogDir + "/dictionary-etl.log") {
		t.Error("the build log isn't in the manifest")
	}
}

func TestBuildForceRebuildsTheSelectedComponents(t *testing.T) {
	x := newBuildFixture(t)
	x.etlFresh(etlSHA[:12], etlSHA)
	x.tree(catalog.DictionaryETL, etlSHA)
	x.f.On(fakerunner.Glob("docker build *"))

	r, err := x.build(ops.ImagesOptions{Components: []string{catalog.DictionaryETL}, Force: true})
	if err != nil {
		t.Fatal(err)
	}
	x.f.AssertCalled(fakerunner.Glob("docker build * -t hms-dbmi/dictionary-etl:" + etlSHA[:12] + " *"))
	if len(r.Images) != 1 {
		t.Errorf("reported %v, want dictionary-etl only", r.Images)
	}
	x.f.AssertNotCalled(fakerunner.Glob("docker image inspect hms-dbmi/pic-sure-*"))
}

func TestBuildRefusesAnUnknownComponent(t *testing.T) {
	x := newBuildFixture(t)
	_, err := x.build(ops.ImagesOptions{Components: []string{"hpds"}})
	if exitcode.FromError(err) != exitcode.CodeUsage || !strings.Contains(err.Error(), `unknown component "hpds"`) {
		t.Errorf("err = %v, want a usage error", err)
	}
}

func TestBuildPullModePullsAllButTheFrontend(t *testing.T) {
	x := newBuildFixture(t)
	x.cfg.Images.Mode = stack.ImagesPull
	x.cfg.Images.Registry = "registry.example.com/mirror/"
	x.state.Components[catalog.DictionaryETL] = stack.Component{Ref: "v4.0.0", Commit: etlSHA}
	// Migrations has no image, so its ref needn't be a tag.
	x.state.Components[catalog.Migrations] = stack.Component{Ref: "feature/schema", Commit: migSHA}
	x.image("hms-dbmi/pic-sure-hpds:v1", nil) // already pulled
	x.frontendFresh()
	x.missing()
	x.f.On(fakerunner.Glob("docker pull *"))
	x.f.On(fakerunner.Glob("docker tag *"))

	r, err := x.build(ops.ImagesOptions{})
	if err != nil {
		t.Fatal(err)
	}
	x.f.AssertNotCalled(fakerunner.Glob("docker build *"))
	x.f.AssertNotCalled(fakerunner.Glob("docker pull *pic-sure-httpd*"))
	x.f.AssertNotCalled(fakerunner.Glob("docker pull *pic-sure-hpds:*"))
	x.f.AssertCalled(fakerunner.Exact("docker", "pull", "registry.example.com/mirror/pic-sure-psama:v1"))
	x.f.AssertCalled(fakerunner.Exact("docker", "tag", "registry.example.com/mirror/pic-sure-psama:v1", "hms-dbmi/pic-sure-psama:v1"))
	x.f.AssertCalled(fakerunner.Exact("docker", "pull", "registry.example.com/mirror/dictionary-etl:v4.0.0"))
	got := actions(r)
	if got["hms-dbmi/pic-sure-psama:v1"] != ops.ImagePulled || got["hms-dbmi/pic-sure-hpds:v1"] != ops.ImageUpToDate ||
		got["hms-dbmi/pic-sure-httpd:"+x.feTag()] != ops.ImageUpToDate {
		t.Errorf("actions = %v", got)
	}
	if x.state.Images["pic-sure-psama"] != "v1" || x.state.Images["dictionary-etl"] != "v4.0.0" {
		t.Errorf("recorded %v", x.state.Images)
	}
}

func TestBuildPullFailureSaysImagesArentPublished(t *testing.T) {
	x := newBuildFixture(t)
	x.cfg.Images.Mode = stack.ImagesPull
	x.missing()
	x.f.On(fakerunner.Glob("docker pull *")).Exit(1).Stderr("Error response from daemon: manifest unknown\n")

	_, err := x.build(ops.ImagesOptions{Components: []string{catalog.DictionaryETL}})
	if err == nil || !strings.Contains(err.Error(), "ghcr.io/hms-dbmi/dictionary-etl:v4") ||
		!strings.Contains(err.Error(), "may not be published") || !strings.Contains(err.Error(), "images.mode") {
		t.Errorf("err = %v", err)
	}
}

func TestBuildPullModeNeedsATagRef(t *testing.T) {
	x := newBuildFixture(t)
	x.cfg.Images.Mode = stack.ImagesPull
	x.state.Components[catalog.DictionaryETL] = stack.Component{Ref: "feature/x", Commit: etlSHA}
	_, err := x.build(ops.ImagesOptions{Components: []string{catalog.DictionaryETL}})
	if err == nil || !strings.Contains(err.Error(), `"feature/x" is not an image tag`) {
		t.Errorf("err = %v", err)
	}
}

func TestBuildFromALocalSourceUsesADevTag(t *testing.T) {
	x := newBuildFixture(t)
	src := x.checkout(false)
	x.cfg.Components.PicSure.Source = src
	x.cfg.Dev.Services = []string{"psama"}
	tag := "dev-demo-" + localSHA[:12]
	x.reactorFresh(tag, localSHA)
	x.state.DevImages = map[string]string{"pic-sure-hpds": "stale"}

	r, err := x.build(ops.ImagesOptions{Components: []string{catalog.PicSure}})
	if err != nil {
		t.Fatal(err)
	}
	x.f.AssertNotCalled(fakerunner.Glob("docker run *"))
	if a := actions(r)["hms-dbmi/pic-sure-psama:"+tag]; a != ops.ImageUpToDate {
		t.Errorf("psama: %q, want up_to_date; report %v", a, r.Images)
	}
	if got := x.state.Components[catalog.PicSure]; got != (stack.Component{Commit: localSHA, Source: src}) {
		t.Errorf("recorded component %+v", got)
	}
	if x.state.Images["pic-sure-psama"] != tag || x.state.DevImages["pic-sure-psama"] != tag {
		t.Errorf("images %v, dev images %v", x.state.Images, x.state.DevImages)
	}
	if _, ok := x.state.DevImages["pic-sure-hpds"]; ok || len(x.state.DevImages) != 1 {
		t.Errorf("dev images %v, want psama's only", x.state.DevImages)
	}
}

func TestBuildFromADirtySourceAlwaysRebuilds(t *testing.T) {
	x := newBuildFixture(t)
	x.cfg.Components.DictionaryETL.Source = x.checkout(true)
	tag := "dev-demo-" + localSHA[:12] + "-dirty"
	x.etlFresh(tag, localSHA)
	x.f.On(fakerunner.Glob("docker build *"))

	r, err := x.build(ops.ImagesOptions{Components: []string{catalog.DictionaryETL}})
	if err != nil {
		t.Fatal(err)
	}
	x.f.AssertCalled(fakerunner.Glob("docker build * -t hms-dbmi/dictionary-etl:" + tag + " *"))
	if a := actions(r)["hms-dbmi/dictionary-etl:"+tag]; a != ops.ImageBuilt {
		t.Errorf("dictionary-etl: %q, want built", a)
	}
	if !x.state.Components[catalog.DictionaryETL].Dirty {
		t.Error("the dirty checkout isn't recorded")
	}
}

func TestBuildResolvesCommitsOnlyWhenStateLacksThem(t *testing.T) {
	x := newBuildFixture(t)
	x.cfg.Components.Frontend.Source = x.checkout(false)
	delete(x.state.Components, catalog.Frontend) // a local source needs no resolving
	x.etlFresh(etlSHA[:12], etlSHA)
	if _, err := x.build(ops.ImagesOptions{Components: []string{catalog.DictionaryETL}}); err != nil {
		t.Fatal(err)
	}
	x.f.AssertNotCalled(fakerunner.Glob("git * fetch *"))
	x.f.AssertNotCalled(fakerunner.Glob("git clone *"))
}

func TestImagesStepCheck(t *testing.T) {
	x := newBuildFixture(t)
	x.reactorFresh(psSHA[:12], psSHA)
	x.frontendFresh()
	x.etlFresh(etlSHA[:12], etlSHA)
	ctx := context.Background()
	opts := ops.ImagesOptions{Cache: x.cache}

	done, err := ops.ImagesStep(x.d, x.st, x.cfg, x.state, opts).Check(ctx)
	if err != nil || done {
		t.Errorf("tags not recorded: done %v, %v; want not done", done, err)
	}
	if _, err := x.build(ops.ImagesOptions{}); err != nil {
		t.Fatal(err)
	}
	done, err = ops.ImagesStep(x.d, x.st, x.cfg, x.state, opts).Check(ctx)
	if err != nil || !done {
		t.Errorf("recorded and present: done %v, %v; want done", done, err)
	}
	force := opts
	force.Force = true
	if done, _ := ops.ImagesStep(x.d, x.st, x.cfg, x.state, force).Check(ctx); done {
		t.Error("--force: done")
	}

	// A frontend config change needs a new image.
	theme := x.cfg.Frontend.Theme
	x.cfg.Frontend.Theme = "bdc"
	x.missing()
	if done, _ := ops.ImagesStep(x.d, x.st, x.cfg, x.state, opts).Check(ctx); done {
		t.Error("new theme: done")
	}
	x.cfg.Frontend.Theme = theme

	// The source trees render needs must exist.
	tree := filepath.Join(x.root, "src", "PIC-SURE-Migrations", migSHA)
	if err := os.Rename(tree, tree+".away"); err != nil {
		t.Fatal(err)
	}
	if done, _ := ops.ImagesStep(x.d, x.st, x.cfg, x.state, opts).Check(ctx); done {
		t.Error("no migrations tree: done")
	}
	if err := os.Rename(tree+".away", tree); err != nil {
		t.Fatal(err)
	}

	// A dirty checkout is never done, even when its image and record are.
	src := x.checkout(true)
	x.cfg.Components.DictionaryETL.Source = src
	dirtyTag := "dev-demo-" + localSHA[:12] + "-dirty"
	x.etlFresh(dirtyTag, localSHA)
	x.state.Components[catalog.DictionaryETL] = stack.Component{Commit: localSHA, Source: src, Dirty: true}
	x.state.Images["dictionary-etl"] = dirtyTag
	if done, _ := ops.ImagesStep(x.d, x.st, x.cfg, x.state, opts).Check(ctx); done {
		t.Error("dirty source: done")
	}

	// Once the source is removed from the config, the commit recorded from
	// it is no release commit to build.
	x.cfg.Components.DictionaryETL.Source = ""
	_, err = ops.ImagesStep(x.d, x.st, x.cfg, x.state, opts).Check(ctx)
	if exitcode.FromError(err) != exitcode.CodePrecondition {
		t.Errorf("source removed: %v, want exit 3", err)
	}

	// Without a recorded commit there is nothing to build from.
	delete(x.state.Components, catalog.DictionaryETL)
	_, err = ops.ImagesStep(x.d, x.st, x.cfg, x.state, opts).Check(ctx)
	if exitcode.FromError(err) != exitcode.CodePrecondition {
		t.Errorf("no commit: %v, want exit 3", err)
	}
}

// releaseControl fakes a release-control clone at commit rel whose
// build-spec names refs v1 to v4, and component clones that resolve every
// ref to resolved[component].
func (x *buildFixture) releaseControl(rel string, resolved map[string]string) {
	x.t.Helper()
	var spec bytes.Buffer
	tw := tar.NewWriter(&spec)
	body := []byte(`{"application": [
		{"project_job_git_key": "PSA", "git_hash": "v1"}, {"project_job_git_key": "PSF", "git_hash": "v2"},
		{"project_job_git_key": "PSM", "git_hash": "v3"}, {"project_job_git_key": "DICTIONARY_ETL", "git_hash": "v4"}]}`)
	if err := tw.WriteHeader(&tar.Header{Name: "build-spec.json", Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
		x.t.Fatal(err)
	}
	if _, err := tw.Write(body); err != nil {
		x.t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		x.t.Fatal(err)
	}
	dirs := []string{x.cache.ReleaseControlDir()}
	for name, sha := range resolved {
		comp, _ := catalog.LookupComponent(name)
		bare := filepath.Join(x.root, "git", comp.RepoName()+".git")
		dirs = append(dirs, bare)
		x.f.On(fakerunner.Glob("git --git-dir=" + bare + " rev-parse *")).Stdout(sha + "\n")
	}
	for _, dir := range dirs {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			x.t.Fatal(err)
		}
	}
	rc := x.cache.ReleaseControlDir()
	x.f.On(fakerunner.Glob("git --git-dir=" + rc + " config remote.origin.url *"))
	x.f.On(fakerunner.Glob("git --git-dir=" + rc + " rev-parse *")).Stdout(rel + "\n")
	x.f.On(fakerunner.Glob("git * --git-dir=" + rc + " archive --format=tar " + rel)).Stdout(spec.String())
	x.f.On(fakerunner.Glob("git --git-dir=* fetch *"))
}

func TestBuildResolvesOnlyMissingCommitsAtTheRecordedRelease(t *testing.T) {
	x := newBuildFixture(t)
	const rel = "5555555555555555555555555555555555555555"
	x.state.Release = stack.Release{Repo: "https://example.com/rc.git", Branch: "main", Commit: rel}
	delete(x.state.Components, catalog.Frontend)
	// Built from a source that pic-sure.yaml no longer sets.
	x.state.Components[catalog.DictionaryETL] = stack.Component{Commit: localSHA, Source: "/old/etl", Dirty: true}
	moved := strings.Repeat("9", 40)
	x.releaseControl(rel, map[string]string{
		catalog.PicSure: moved, catalog.Frontend: feSHA, catalog.Migrations: moved, catalog.DictionaryETL: moved,
	})
	x.frontendFresh()

	if _, err := x.build(ops.ImagesOptions{Components: []string{catalog.Frontend}}); err != nil {
		t.Fatal(err)
	}
	// The pinned release commit is read, not the branch head.
	x.f.AssertCalled(fakerunner.Glob("git --git-dir=* rev-parse --verify --quiet " + rel + "^{commit}"))
	x.f.AssertNotCalled(fakerunner.Glob("git --git-dir=*release-control fetch *"))
	if got := x.state.Components[catalog.Frontend]; got != (stack.Component{Ref: "v2", Commit: feSHA}) {
		t.Errorf("frontend resolved to %+v", got)
	}
	if got := x.state.Components[catalog.PicSure].Commit; got != psSHA {
		t.Errorf("pic-sure moved to %s; only missing commits are resolved", got)
	}
	if got := x.state.Components[catalog.DictionaryETL]; got != (stack.Component{Ref: "v4", Commit: moved}) {
		t.Errorf("dictionary-etl, formerly from a source, resolved to %+v", got)
	}
	if x.state.Release.Commit != rel {
		t.Errorf("release moved to %s", x.state.Release.Commit)
	}
}

func TestBuildResolvesAFreshStackAtTheBranchHead(t *testing.T) {
	x := newBuildFixture(t)
	x.state = &stack.State{}
	const rel = "5555555555555555555555555555555555555555"
	x.releaseControl(rel, map[string]string{
		catalog.PicSure: psSHA, catalog.Frontend: feSHA, catalog.Migrations: migSHA, catalog.DictionaryETL: etlSHA,
	})
	x.etlFresh(etlSHA[:12], etlSHA)

	if _, err := x.build(ops.ImagesOptions{Components: []string{catalog.DictionaryETL}}); err != nil {
		t.Fatal(err)
	}
	x.f.AssertCalled(fakerunner.Glob("git --git-dir=* rev-parse --verify --quiet refs/heads/" + x.cfg.Release.Branch + "^{commit}"))
	if x.state.Release.Commit != rel || x.state.Release.Branch != x.cfg.Release.Branch {
		t.Errorf("release = %+v", x.state.Release)
	}
	if len(x.state.Components) != 4 || x.state.Components[catalog.Migrations] != (stack.Component{Ref: "v3", Commit: migSHA}) {
		t.Errorf("components = %v", x.state.Components)
	}
	saved, err := x.st.LoadState()
	if err != nil || saved.Release.Commit != rel {
		t.Errorf("saved state %+v, %v", saved, err)
	}
}

func TestBuildRefreshPullsImagesThatArePresent(t *testing.T) {
	x := newBuildFixture(t)
	x.cfg.Images.Mode = stack.ImagesPull
	x.image("hms-dbmi/dictionary-etl:v4", nil)
	x.f.On(fakerunner.Glob("docker pull *"))
	x.f.On(fakerunner.Glob("docker tag *"))

	r, err := x.build(ops.ImagesOptions{Components: []string{catalog.DictionaryETL}, Refresh: true})
	if err != nil {
		t.Fatal(err)
	}
	x.f.AssertCalled(fakerunner.Exact("docker", "pull", "ghcr.io/hms-dbmi/dictionary-etl:v4"))
	if a := actions(r)["hms-dbmi/dictionary-etl:v4"]; a != ops.ImagePulled {
		t.Errorf("dictionary-etl: %q, want pulled", a)
	}
}

func TestBuildMakesTheSourceTreesRenderNeeds(t *testing.T) {
	x := newBuildFixture(t)
	tree := filepath.Join(x.root, "src", "PIC-SURE-Migrations", migSHA)
	if err := os.RemoveAll(tree); err != nil {
		t.Fatal(err)
	}
	var archive bytes.Buffer
	tw := tar.NewWriter(&archive)
	body := []byte("select 1;\n")
	if err := tw.WriteHeader(&tar.Header{Name: "V1__init.sql", Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(body); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	bare := filepath.Join(x.root, "git", "PIC-SURE-Migrations.git")
	if err := os.MkdirAll(bare, 0o755); err != nil {
		t.Fatal(err)
	}
	x.f.On(fakerunner.Glob("git --git-dir=" + bare + " config *"))
	x.f.On(fakerunner.Glob("git --git-dir=" + bare + " rev-parse *")).Stdout(migSHA + "\n")
	x.f.On(fakerunner.Glob("git * --git-dir=" + bare + " archive --format=tar " + migSHA)).Stdout(archive.String())

	if _, err := x.build(ops.ImagesOptions{Components: []string{catalog.Migrations}}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(tree, "V1__init.sql")); err != nil {
		t.Errorf("migrations tree not made: %v", err)
	}
}

func TestBuildRefusesAStackNameTooLongForADevTag(t *testing.T) {
	x := newBuildFixture(t)
	x.cfg.Name = strings.Repeat("n", 110) // fits clean (127), not dirty (133)
	x.cfg.Components.DictionaryETL.Source = x.checkout(false)
	_, err := x.build(ops.ImagesOptions{Components: []string{catalog.DictionaryETL}})
	if exitcode.FromError(err) != exitcode.CodeUsage || !strings.Contains(err.Error(), "too long for a dev image tag") {
		t.Errorf("err = %v, want a usage error about the name", err)
	}
}

// The image step holds the cache's use lock, so it can't run while cache
// prune does (057).
func TestBuildWaitsForCachePrune(t *testing.T) {
	x := newBuildFixture(t)
	x.missing()
	c, err := cache.Open(x.root, cache.Options{Git: git.New(x.f), LockTimeout: 200 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	x.cache = c
	pruner, err := cache.Open(x.root, cache.Options{})
	if err != nil {
		t.Fatal(err)
	}
	lock, err := pruner.LockPrune(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lock.Unlock() }()

	if _, err := x.build(ops.ImagesOptions{}); !errors.Is(err, cache.ErrLockTimeout) {
		t.Fatalf("err = %v, want the use lock's timeout", err)
	}
	x.f.AssertNotCalled(fakerunner.Glob("docker build *"))
}
