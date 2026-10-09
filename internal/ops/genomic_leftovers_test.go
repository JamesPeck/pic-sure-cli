package ops_test

import (
	"context"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/JamesPeck/pic-sure-cli/internal/docker"
	"github.com/JamesPeck/pic-sure-cli/internal/docker/fakerunner"
	"github.com/JamesPeck/pic-sure-cli/internal/events"
	"github.com/JamesPeck/pic-sure-cli/internal/exitcode"
	"github.com/JamesPeck/pic-sure-cli/internal/ops"
	"github.com/JamesPeck/pic-sure-cli/internal/stack"
	"github.com/JamesPeck/pic-sure-cli/internal/steps"
)

// leftoverFixture is a stack whose genomic store, if vol is set, is a
// volume of that name the listing script runs over for real.
type leftoverFixture struct {
	f   *fakerunner.Runner
	h   *helperScripts
	d   *ops.Deps
	st  *stack.Stack
	cfg *stack.Config
}

func newLeftoverFixture(t *testing.T, vol string, files map[string]string, edit func(*stack.Config)) *leftoverFixture {
	t.Helper()
	st, err := stack.Create(filepath.Join(t.TempDir(), "demo"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	cfg := stack.DefaultConfig()
	cfg.Name = "demo"
	if edit != nil {
		edit(&cfg)
	}
	fx := &leftoverFixture{f: fakerunner.New(t), st: st, cfg: &cfg}
	if vol != "" {
		fx.h = newLocalHelperScripts(t, vol)
		fx.h.seed(vol, files)
		fx.f.On(fakerunner.Exact("docker", "volume", "inspect", vol)).Stdout(`[{"Name":"` + vol + `"}]`)
		fx.f.On(fakerunner.Glob("docker run * --name demo-genomic-list-* *")).Do(func(ctx context.Context, c fakerunner.Call) (docker.Result, error) { return fx.h.run(ctx, c) })
	}
	fx.f.On(fakerunner.Glob("docker volume inspect *")).Exit(1).Stderr("Error response from daemon: get x: no such volume\n")
	fx.d = &ops.Deps{Runner: fx.f, Docker: docker.NewEngine(fx.f), Rand: strings.NewReader(strings.Repeat("r", 64)), Sink: events.Discard}
	return fx
}

// seeded holds a live partition, the backup's name, a file, and two
// leftovers.
var seeded = map[string]string{
	"synth/chr21/v": "", "all-bak/synth/chr21/v": "", ".promote-file": "",
	".promote-a/chr21/v": "", ".old-b/chr21/v": "",
}

func TestGenomicLeftovers(t *testing.T) {
	for _, tc := range []struct {
		name, vol string
		files     map[string]string
		edit      func(*stack.Config)
		want      []string
	}{
		{"leftovers", "demo_hpds-genomic", seeded, nil, []string{".old-b", ".promote-a"}},
		{"none", "demo_hpds-genomic", map[string]string{"synth/chr21/v": ""}, nil, nil},
		{"no volume", "", nil, nil, nil},
		{"shared", "nhanes_hpds-genomic", seeded, func(c *stack.Config) {
			c.HPDS.Data, c.HPDS.SharedName = stack.HPDSShared, "nhanes"
		}, []string{".old-b", ".promote-a"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fx := newLeftoverFixture(t, tc.vol, tc.files, tc.edit)
			got, err := ops.GenomicLeftovers(context.Background(), fx.d, fx.st, fx.cfg)
			if err != nil {
				t.Fatal(err)
			}
			if tc.vol == "" {
				fx.f.AssertCalled(fakerunner.Exact("docker", "volume", "inspect", "demo_hpds-genomic"))
				fx.f.AssertNotCalled(fakerunner.Glob("docker run *"))
			}
			if !slices.Equal(got, tc.want) {
				t.Errorf("GenomicLeftovers = %q, want %q", got, tc.want)
			}
		})
	}
}

// upStep is the genomic-leftovers step of UpSteps, which comes right
// after resolve.
func (fx *leftoverFixture) upStep(t *testing.T) steps.Step {
	t.Helper()
	list := ops.UpSteps(fx.d, fx.st, fx.cfg, &stack.Secrets{}, &stack.State{}, ops.ConvergeOptions{})
	if list[1].ID != ops.GenomicLeftoversStepID {
		t.Fatalf("UpSteps' second step is %s", list[1].ID)
	}
	return list[1]
}

func TestUpRefusesGenomicLeftovers(t *testing.T) {
	for _, tc := range []struct {
		name string
		vol  string
		edit func(*stack.Config)
		want string
	}{
		{"local", "demo_hpds-genomic", nil, "volume demo_hpds-genomic holds what an interrupted promote left (.old-b, .promote-a), " +
			"which HPDS would load as partitions; recover it with `pic-sure data load-genomic --promote` first"},
		{"shared", "nhanes_hpds-genomic", func(c *stack.Config) { c.HPDS.Data, c.HPDS.SharedName = stack.HPDSShared, "nhanes" },
			"volume nhanes_hpds-genomic holds what an interrupted promote left (.old-b, .promote-a), which HPDS would load as partitions; " +
				"shared data set nhanes can't be changed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fx := newLeftoverFixture(t, tc.vol, seeded, tc.edit)
			step := fx.upStep(t)
			err := steps.Run(context.Background(), events.Discard, []steps.Step{step}, steps.Options{})
			if exitcode.FromError(err) != exitcode.CodePrecondition || err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("up = %v (exit %d), want exit 3 containing %q", err, exitcode.FromError(err), tc.want)
			}
		})
	}
}

func TestUpPassesWithoutGenomicLeftovers(t *testing.T) {
	fx := newLeftoverFixture(t, "demo_hpds-genomic", map[string]string{"synth/chr21/v": ""}, nil)
	if done, err := fx.upStep(t).Check(context.Background()); !done || err != nil {
		t.Errorf("Check = %v, %v; want done", done, err)
	}
}
