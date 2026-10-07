package ops_test

import (
	"context"
	"maps"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/JamesPeck/pic-sure-cli/internal/catalog"
	"github.com/JamesPeck/pic-sure-cli/internal/docker/fakerunner"
	"github.com/JamesPeck/pic-sure-cli/internal/exitcode"
	"github.com/JamesPeck/pic-sure-cli/internal/ops"
	"github.com/JamesPeck/pic-sure-cli/internal/stack"
	"github.com/JamesPeck/pic-sure-cli/internal/steps"
)

func TestUpStepIDsMatchUpSteps(t *testing.T) {
	st, err := stack.Create(filepath.Join(t.TempDir(), "demo"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	for _, mode := range []stack.DBMode{stack.DBLocal, stack.DBRemote} {
		cfg := stack.DefaultConfig()
		cfg.Name, cfg.DB.Mode = "demo", mode
		var ids []string
		for _, s := range ops.UpSteps(&ops.Deps{}, st, &cfg, &stack.Secrets{}, &stack.State{}, ops.ConvergeOptions{}) {
			ids = append(ids, s.ID)
		}
		if want := ops.UpStepIDs(&cfg); !slices.Equal(ids, want) {
			t.Errorf("db.mode %s: UpSteps %v, UpStepIDs %v", mode, ids, want)
		}
		// up is init's plan with the restart before start.
		init := ops.InitStepIDs(&cfg)
		if want := append(slices.Clone(init[:len(init)-1]), ops.RestartStepID, ops.StartStepID); !slices.Equal(ids, want) {
			t.Errorf("db.mode %s: UpSteps %v, want %v", mode, ids, want)
		}
	}
}

// upImages runs up's resolve and image steps on x's stack.
func (x *buildFixture) upImages() error {
	x.t.Helper()
	plan := ops.UpSteps(x.d, x.st, x.cfg, &stack.Secrets{}, x.state, ops.ConvergeOptions{Cache: x.cache})
	return steps.Run(context.Background(), x.d.Sink, plan[:2], steps.Options{})
}

func TestUpReturnsAnUnsetSourceToTheRecordedRelease(t *testing.T) {
	x := newBuildFixture(t)
	const rel = "5555555555555555555555555555555555555555"
	x.state.Release = stack.Release{Repo: "https://example.com/rc.git", Branch: "main", Commit: rel}
	// Built from a source that pic-sure.yaml no longer sets.
	x.state.Components[catalog.PicSure] = stack.Component{Commit: localSHA, Source: "/old/pic-sure", Dirty: true}
	x.state.Images = map[string]string{"pic-sure-psama": ops.DevTag("demo", localSHA, true)}
	moved := strings.Repeat("9", 40)
	x.releaseControl(rel, map[string]string{
		catalog.PicSure: psSHA, catalog.Frontend: moved, catalog.Migrations: moved, catalog.DictionaryETL: moved,
	})
	before := maps.Clone(x.state.Components)
	x.reactorFresh(psSHA[:12], psSHA)
	x.frontendFresh()
	x.etlFresh(etlSHA[:12], etlSHA)

	if err := x.upImages(); err != nil {
		t.Fatal(err)
	}
	x.f.AssertCalled(fakerunner.Glob("git --git-dir=* rev-parse --verify --quiet " + rel + "^{commit}"))
	x.f.AssertNotCalled(fakerunner.Glob("git --git-dir=*release-control fetch *"))
	// The release images present are reused, not rebuilt.
	x.f.AssertNotCalled(fakerunner.Glob("docker build *"))
	saved, err := x.st.LoadState()
	if err != nil {
		t.Fatal(err)
	}
	if got := saved.Components[catalog.PicSure]; got != (stack.Component{Ref: "v1", Commit: psSHA}) {
		t.Errorf("pic-sure resolved to %+v", got)
	}
	for _, name := range []string{catalog.Frontend, catalog.Migrations, catalog.DictionaryETL} {
		if saved.Components[name] != before[name] {
			t.Errorf("%s moved to %+v; up only resolves unset sources", name, saved.Components[name])
		}
	}
	if saved.Release.Commit != rel {
		t.Errorf("release moved to %s", saved.Release.Commit)
	}
	if got := saved.Images["pic-sure-psama"]; got != psSHA[:12] {
		t.Errorf("psama runs %s, want the release tag %s", got, psSHA[:12])
	}
}

func TestUpResolvesNothingWithoutAnUnsetSource(t *testing.T) {
	x := newBuildFixture(t)
	x.state.Release = stack.Release{Commit: "5555555555555555555555555555555555555555"}
	x.reactorFresh(psSHA[:12], psSHA)
	x.frontendFresh()
	x.etlFresh(etlSHA[:12], etlSHA)

	if err := x.upImages(); err != nil {
		t.Fatal(err)
	}
	x.f.AssertNotCalled(fakerunner.Glob("git *"))
}

func TestUpWontResolveWithoutARecordedRelease(t *testing.T) {
	x := newBuildFixture(t)
	x.state.Components[catalog.DictionaryETL] = stack.Component{Commit: localSHA, Source: "/old/etl"}

	err := x.upImages()
	if exitcode.FromError(err) != exitcode.CodePrecondition || !strings.Contains(err.Error(), "no release commit") {
		t.Errorf("err = %v, want exit 3", err)
	}
	x.f.AssertNotCalled(fakerunner.Glob("git *"))
}
