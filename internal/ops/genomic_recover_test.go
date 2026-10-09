package ops_test

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/JamesPeck/pic-sure-cli/internal/docker/fakerunner"
	"github.com/JamesPeck/pic-sure-cli/internal/exitcode"
	"github.com/JamesPeck/pic-sure-cli/internal/ops"
	"github.com/JamesPeck/pic-sure-cli/internal/stack"
)

func (fx *genomicFixture) recover() (ops.GenomicRecovery, error) {
	return ops.RecoverGenomic(context.Background(), fx.d, fx.st, fx.cfg)
}

func TestRecoverGenomicSettlesEveryLeftover(t *testing.T) {
	genomicScriptModes(t, func(t *testing.T, fx *genomicFixture) {
		fx.h.seed(liveVol, map[string]string{
			"keep/v": "keep",
			// a complete copy, before the live partition was moved aside
			"a/v": "old a", ".promote-a/v": "new a",
			// moved aside, and no copy
			".old-b/v": "old b",
			// moved aside, and a complete copy
			".old-c/v": "old c", ".promote-c/v": "new c",
			// renamed into place, and the old one not yet removed
			"d/v": "new d", ".old-d/v": "old d",
		})
		r, err := fx.recover()
		if err != nil {
			t.Fatal(err)
		}
		assertTree(t, fx.h, liveVol, map[string]string{"keep/v": "keep", "a/v": "old a", "b/v": "old b", "c/v": "new c", "d/v": "new d"})
		want := []ops.RecoveredPartition{{Partition: "b", Result: "restored"}, {Partition: "c", Result: "completed"},
			{Partition: "d", Result: "completed"}, {Partition: "a", Result: "discarded"}}
		if !slices.Equal(r.Partitions, want) || !r.HPDSStarted || len(r.Leftovers) != 5 {
			t.Errorf("RecoverGenomic = %+v", r)
		}
		fx.f.AssertOrder(
			fakerunner.Glob("docker compose * stop hpds"),
			fakerunner.Glob("docker run * demo-genomic-recover-* -v demo_hpds-genomic:/live *"),
			fakerunner.Glob("docker compose * up -d --wait --wait-timeout 900 hpds"),
		)
	})
}

func TestRecoverGenomicWithoutLeftoversChangesNothing(t *testing.T) {
	fx := newGenomicFixture(t)
	fx.live = "keep\n"
	r, err := fx.recover()
	if err != nil || len(r.Leftovers) != 0 || len(r.Partitions) != 0 || r.HPDSStarted {
		t.Fatalf("RecoverGenomic = %+v, %v", r, err)
	}
	fx.f.AssertNotCalled(fakerunner.Glob("docker compose *"))
	if fx.recovers != 0 {
		t.Errorf("%d recover helpers", fx.recovers)
	}
}

func TestRecoverGenomicLeavesAStoppedHPDSStopped(t *testing.T) {
	fx := newGenomicFixture(t)
	fx.live = ".old-a\n"
	fx.stopped = true
	r, err := fx.recover()
	if err != nil || r.HPDSStarted {
		t.Fatalf("RecoverGenomic = %+v, %v", r, err)
	}
	fx.f.AssertCalled(fakerunner.Glob("docker compose * stop hpds"))
	fx.f.AssertNotCalled(fakerunner.Glob("docker compose * up *"))
	if fx.recovers != 1 {
		t.Errorf("%d recover helpers", fx.recovers)
	}
}

func TestRecoverGenomicFailing(t *testing.T) {
	fx := newGenomicFixture(t)
	fx.live = ".old-a\n"
	fx.recoverExit = 1
	_, err := fx.recover()
	if err == nil || !strings.Contains(err.Error(), "step genomic-recover failed") ||
		!strings.Contains(err.Error(), "HPDS may be stopped; run `pic-sure data load-genomic --recover` again before starting it") ||
		strings.Contains(err.Error(), "resume") {
		t.Errorf("err = %v", err)
	}
	fx.f.AssertNotCalled(fakerunner.Glob("docker compose * up *"))
}

func TestRecoverGenomicRefusesASharedDataSet(t *testing.T) {
	for _, tc := range []struct {
		name  string
		files map[string]string
		want  string
	}{
		{"leftovers", seeded, "shared data set nhanes can't be changed: recover it in the stack that published it with " +
			"`pic-sure data load-genomic --recover`, publish that under a new name, and set hpds.shared_name to it"},
		{"none", map[string]string{"synth/chr21/v": ""}, `the shared data set "nhanes" (hpds.data: shared), which it can't change; ` +
			"volume nhanes_hpds-genomic holds nothing an interrupted promote left"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fx := newLeftoverFixture(t, "nhanes_hpds-genomic", tc.files, func(c *stack.Config) {
				c.HPDS.Data, c.HPDS.SharedName = stack.HPDSShared, "nhanes"
			})
			_, err := ops.RecoverGenomic(context.Background(), fx.d, fx.st, fx.cfg)
			if exitcode.FromError(err) != exitcode.CodePrecondition || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v (exit %d), want exit 3 containing %q", err, exitcode.FromError(err), tc.want)
			}
			fx.f.AssertNotCalled(fakerunner.Glob("docker compose *"))
			fx.f.AssertNotCalled(fakerunner.Glob("docker run * demo-genomic-recover-*"))
		})
	}
}
