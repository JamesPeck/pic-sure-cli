package ops_test

import (
	"context"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/JamesPeck/pic-sure-cli/internal/docker"
	"github.com/JamesPeck/pic-sure-cli/internal/events"
	"github.com/JamesPeck/pic-sure-cli/internal/exitcode"
	"github.com/JamesPeck/pic-sure-cli/internal/ops"
)

const (
	liveVol    = "demo_hpds-genomic"
	stagingVol = "demo_genomic-staging"
)

// genomicScriptModes runs test once with the helper scripts under the local
// sh and once in alpine containers.
func genomicScriptModes(t *testing.T, test func(t *testing.T, fx *genomicFixture)) {
	for _, mode := range []string{"sh", "docker"} {
		t.Run(mode, func(t *testing.T) {
			fx := newGenomicFixture(t)
			if mode == "sh" {
				fx.h = newLocalHelperScripts(t, liveVol, stagingVol)
			} else {
				fx.h = newDockerHelperScripts(t, liveVol, stagingVol)
			}
			test(t, fx)
		})
	}
}

func assertTree(t *testing.T, h *helperScripts, vol string, want map[string]string) {
	t.Helper()
	if got := h.tree(vol); !maps.Equal(got, want) {
		t.Errorf("%s holds %s\nwant %s", vol, treeString(got), treeString(want))
	}
}

func TestLoadGenomicPromoteScripts(t *testing.T) {
	genomicScriptModes(t, func(t *testing.T, fx *genomicFixture) {
		fx.h.seed(liveVol, map[string]string{"synth/chr21/variants": "old synth", "keep/chr1/variants": "keep"})
		fx.h.seed(stagingVol, map[string]string{"genomic/older/chr21/variants": "new older", "merged/x": "m"})
		promoted, err := fx.load(ops.GenomicLoadOptions{Promote: true, AllPartitions: true, Backup: true})
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(promoted, []string{"older", "synth"}) {
			t.Errorf("promoted %q", promoted)
		}
		live := map[string]string{"synth/chr21/variants": "new synth", "older/chr21/variants": "new older", "keep/chr1/variants": "keep"}
		assertTree(t, fx.h, liveVol, live)
		staged := map[string]string{"genomic/older/chr21/variants": "new older", "genomic/synth/chr21/variants": "new synth",
			"all-bak/synth/chr21/variants": "old synth", "all-bak/keep/chr1/variants": "keep"}
		assertTree(t, fx.h, stagingVol, staged)

		// Again, replacing the backup and one partition.
		if _, err := fx.load(ops.GenomicLoadOptions{Partition: "keep", Promote: true, Backup: true}); err != nil {
			t.Fatal(err)
		}
		for name, content := range live {
			staged["all-bak/"+name] = content
		}
		staged["genomic/keep/chr21/variants"] = "new keep"
		assertTree(t, fx.h, stagingVol, staged)
		delete(live, "keep/chr1/variants")
		live["keep/chr21/variants"] = "new keep"
		assertTree(t, fx.h, liveVol, live)
	})
}

func TestLoadGenomicRecoversAnInterruptedPromote(t *testing.T) {
	genomicScriptModes(t, func(t *testing.T, fx *genomicFixture) {
		fx.h.seed(liveVol, map[string]string{
			// Interrupted between the renames: the copy is complete.
			".old-a/c/v": "old a", ".promote-a/c/v": "new a",
			// Interrupted during the copy of a new partition.
			".promote-b/c/v": "partial b",
			// Interrupted while removing the replaced partition.
			".old-d/c/v": "old d", "d/c/v": "new d",
			// Only the replaced partition is left.
			".old-e/c/v": "old e",
			"f/c/v":      "f",
		})
		if _, err := fx.load(ops.GenomicLoadOptions{Promote: true}); err != nil {
			t.Fatal(err)
		}
		assertTree(t, fx.h, liveVol, map[string]string{"a/c/v": "new a", "d/c/v": "new d", "e/c/v": "old e", "f/c/v": "f",
			"synth/chr21/variants": "new synth"})
		var progress []string
		for _, e := range fx.rec.Events() {
			if p, ok := e.(events.Progress); ok {
				progress = append(progress, p.Text)
			}
		}
		for _, want := range []string{
			"demo_hpds-genomic holds what an interrupted promote left (.old-a, .old-d, .old-e, .promote-a, .promote-b); it is recovered once HPDS stops",
			"completed a; completed d; restored e",
		} {
			if !slices.Contains(progress, want) {
				t.Errorf("no progress %q in %q", want, progress)
			}
		}
	})
}

// promoteState matches the state a failed promote's error reports.
var promoteState = regexp.MustCompile(`\(every partition in demo_hpds-genomic is whole(?:; promoted: ([^;)]*))?(?:; not promoted: ([^;)]*))?\)`)

// TestLoadGenomicPromoteSurvivesAKillAtAnyPoint kills the backup and
// promote scripts at each cp, mv and rm they run in turn (before it, after
// it, and for cp part way through), as `docker rm -f` would, and checks
// that every partition and the backup are whole afterwards, the old or the
// new, with nothing left over, and that the error says which.
func TestLoadGenomicPromoteSurvivesAKillAtAnyPoint(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh")
	}
	shims := t.TempDir()
	for _, cmd := range []string{"cp", "mv", "rm"} {
		real, err := exec.LookPath(cmd)
		if err != nil {
			t.Skip(err)
		}
		// $PPID is the script's sh, unless it exec'd this as its last
		// command; then the parent is the test, and exiting is the kill.
		script := `#!/bin/sh
n=$(($(cat "$SHIM_COUNTER") + 1)); echo "$n" > "$SHIM_COUNTER"
die() { if [ "$PPID" != "$SHIM_TEST_PID" ]; then kill -9 "$PPID"; fi; exit 137; }
if [ "$n" = "$SHIM_KILL_AT" ] && [ "$SHIM_KILL" != after ]; then
  if [ "$SHIM_KILL" = mid ] && [ ` + cmd + ` = cp ]; then for last; do :; done; mkdir -p "$last"; fi
  die
fi
` + real + ` "$@" || exit
if [ "$n" = "$SHIM_KILL_AT" ]; then die; fi
`
		if err := os.WriteFile(filepath.Join(shims, cmd), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	oldLive := map[string]string{"synth/chr21/variants": "old synth", "keep/chr1/variants": "keep"}
	oldBackup := map[string]string{"all-bak/x/v": "bak0"}
	for _, when := range []string{"before", "mid", "after"} {
		for n := 1; ; n++ {
			fx := newGenomicFixture(t)
			fx.h = newLocalHelperScripts(t, liveVol, stagingVol)
			fx.h.seed(liveVol, oldLive)
			fx.h.seed(stagingVol, maps.Clone(oldBackup))
			fx.h.seed(stagingVol, map[string]string{"genomic/older/chr21/variants": "new older"})
			counter := filepath.Join(t.TempDir(), "n")
			if err := os.WriteFile(counter, []byte("0"), 0o644); err != nil {
				t.Fatal(err)
			}
			fx.shim = []string{"PATH=" + shims + string(os.PathListSeparator) + os.Getenv("PATH"), "SHIM_COUNTER=" + counter,
				"SHIM_KILL_AT=" + strconv.Itoa(n), "SHIM_KILL=" + when, "SHIM_TEST_PID=" + strconv.Itoa(os.Getpid())}
			_, err := fx.load(ops.GenomicLoadOptions{Promote: true, AllPartitions: true, Backup: true})
			data, _ := os.ReadFile(counter)
			calls, _ := strconv.Atoi(strings.TrimSpace(string(data)))
			if err == nil && n > calls {
				if n < 10 {
					t.Fatalf("only %d calls", calls)
				}
				break
			}
			where := when + " call " + strconv.Itoa(n)
			if err == nil {
				t.Fatalf("%s: the load succeeded", where)
			}

			live := fx.h.tree(liveVol)
			var newOnes []string
			for _, p := range []string{"older", "synth"} {
				name := p + "/chr21/variants"
				switch live[name] {
				case "new " + p:
					newOnes = append(newOnes, p)
				case oldLive[name]:
				default:
					t.Errorf("%s: %s holds %q", where, name, live[name])
				}
				delete(live, name)
			}
			if !maps.Equal(live, map[string]string{"keep/chr1/variants": "keep"}) {
				t.Errorf("%s: %s also holds %s", where, liveVol, treeString(live))
			}
			if m := promoteState.FindStringSubmatch(err.Error()); m == nil || m[1] != strings.Join(newOnes, ", ") {
				t.Errorf("%s: promoted %q, but err = %v", where, newOnes, err)
			}
			if !strings.Contains(err.Error(), "or `pic-sure up` to start HPDS") {
				t.Errorf("%s: err = %v", where, err)
			}

			staged := fx.h.tree(stagingVol)
			backup := map[string]string{}
			for name, content := range staged {
				if strings.HasPrefix(name, "all-bak") {
					backup[name] = content
				}
			}
			if !maps.Equal(backup, oldBackup) && !maps.Equal(backup, map[string]string{"all-bak/synth/chr21/variants": "old synth", "all-bak/keep/chr1/variants": "keep"}) {
				t.Errorf("%s: the backup is %s", where, treeString(backup))
			}
		}
	}
}

func TestLoadGenomicInterruptedPromoteSaysWhatIsLeft(t *testing.T) {
	for _, unrecovered := range []bool{false, true} {
		t.Run("unrecovered="+strconv.FormatBool(unrecovered), func(t *testing.T) {
			fx := newGenomicFixture(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			fx.onPromote = func() (docker.Result, error) {
				cancel()
				return docker.Result{}, context.Canceled
			}
			if unrecovered {
				fx.recoverExit = 1
			}
			_, err := fx.loadCtx(ctx, ops.GenomicLoadOptions{Promote: true})
			if err == nil || exitcode.FromError(err) != exitcode.CodeInterrupted || !strings.HasPrefix(err.Error(), "stopped at step genomic-promote: context canceled: copying synth") ||
				strings.Contains(err.Error(), "resume") || fx.recovers != 1 {
				t.Fatalf("err = %v (exit %d), %d recover helpers", err, exitcode.FromError(err), fx.recovers)
			}
			want := []string{"not promoted: synth", "HPDS is stopped: run the load again, or `pic-sure up` to start HPDS"}
			if unrecovered {
				want = []string{"HPDS would load its leftover .promote-* and .old-* directories",
					"HPDS is stopped; don't start it until a load with --promote has recovered demo_hpds-genomic"}
			}
			for _, w := range want {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("err = %v, want %q", err, w)
				}
			}
		})
	}
}

func TestLoadGenomicWarnsOfLeftoversWithoutPromote(t *testing.T) {
	fx := newGenomicFixture(t)
	fx.live = ".old-a\n"
	// Only the input step's warnings matter; the render fails without a
	// stack config.
	_, _ = fx.load(ops.GenomicLoadOptions{EnableProfile: true, Converge: ops.ConvergeOptions{CLIVersion: "test"}})
	var warnings []string
	for _, e := range fx.rec.Events() {
		if w, ok := e.(events.Warning); ok {
			warnings = append(warnings, w.Text)
		}
	}
	want := "demo_hpds-genomic holds what an interrupted promote left (.old-a), which HPDS loads as partitions; a load with --promote recovers it"
	if !slices.Contains(warnings, want) || slices.ContainsFunc(warnings, func(w string) bool { return strings.Contains(w, "holds no genomic partition") }) {
		t.Errorf("warnings %q", warnings)
	}
	if fx.recovers != 0 {
		t.Errorf("%d recover helpers without --promote", fx.recovers)
	}
}

// TestLoadGenomicCopiesVCFsLinkedFromOutside checks that a VCF symlinked
// to a file outside --vcf-dir, which a container mounting only --vcf-dir
// can't follow, is copied without asking the daemon first.
func TestLoadGenomicCopiesVCFsLinkedFromOutside(t *testing.T) {
	fx := newGenomicFixture(t)
	outside := filepath.Join(t.TempDir(), "chr22.vcf")
	if err := os.WriteFile(outside, []byte("vcf outside"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(fx.vcfDir, "sub/chr22.vcf")
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	var copyDir string
	_, err := fx.load(ops.GenomicLoadOptions{MkdirTemp: func(p string) (string, error) {
		var err error
		copyDir, err = os.MkdirTemp(t.TempDir(), p)
		if err == nil {
			// Checked here, since the load removes the copy.
			if data, rerr := os.ReadFile(link); rerr != nil || string(data) != "vcf outside" {
				t.Errorf("link reads %q, %v", data, rerr)
			}
		}
		return copyDir, err
	}})
	if err != nil {
		t.Fatal(err)
	}
	if len(fx.probes) != 1 || !slices.Contains(fx.probes[0], copyDir+":"+fx.vcfDir+":ro") {
		t.Fatalf("probes %q", fx.probes)
	}
	var progress []string
	for _, e := range fx.rec.Events() {
		if p, ok := e.(events.Progress); ok {
			progress = append(progress, p.Text)
		}
	}
	if want := link + " links to a file outside " + fx.vcfDir + ", which the loaders can't see; copying the VCFs into the cache"; !slices.Contains(progress, want) {
		t.Errorf("progress %q", progress)
	}
}
