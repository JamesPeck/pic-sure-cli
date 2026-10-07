package ops_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/JamesPeck/pic-sure-cli/internal/catalog"
	"github.com/JamesPeck/pic-sure-cli/internal/docker"
	"github.com/JamesPeck/pic-sure-cli/internal/docker/fakerunner"
	"github.com/JamesPeck/pic-sure-cli/internal/events"
	"github.com/JamesPeck/pic-sure-cli/internal/exitcode"
	"github.com/JamesPeck/pic-sure-cli/internal/ops"
	"github.com/JamesPeck/pic-sure-cli/internal/render"
	"github.com/JamesPeck/pic-sure-cli/internal/stack"
)

type genomicFixture struct {
	*loaderFixture
	vcfDir      string
	index       string
	exits       map[string]int // loader step ID -> exit code
	staged      string         // genomic-list output for the staging volume
	live        string         // and for hpds-genomic
	hiddenOnce  bool           // the first probe sees no VCF
	probes      [][]string
	stageIn     []byte
	backupExit  int
	promoteExit int
	promotes    [][]string // each promote helper's argv
	promoteFx   []string   // the promote helper's partition args
}

func newGenomicFixture(t *testing.T) *genomicFixture {
	fx := &genomicFixture{loaderFixture: newLoaderFixture(t), exits: map[string]int{}}
	fx.vcfDir = t.TempDir()
	for _, name := range []string{"chr21.vcf.gz", "sub/chr22.vcf"} {
		p := filepath.Join(fx.vcfDir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("vcf "+name), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	fx.index = filepath.Join(fx.vcfDir, "vcfIndex.tsv")
	fx.writeIndex(t, "filename\tchromosome\n"+
		filepath.Join(fx.vcfDir, "chr21.vcf.gz")+"\t21\n"+
		filepath.Join(fx.vcfDir, "sub/chr22.vcf")+"\t22\r\n\n")

	if err := fx.st.SaveState(fx.state); err != nil {
		t.Fatal(err)
	}
	f := fx.f
	for _, vol := range []string{"demo_genomic-staging", "demo_hpds-genomic"} {
		out, _ := json.Marshal([]map[string]any{{"Name": vol, "CreatedAt": "2026-10-07T12:00:00Z", "Labels": map[string]string{stack.LabelStack: "demo"}}})
		f.On(fakerunner.Exact("docker", "volume", "inspect", vol)).Stdout(string(out))
	}
	f.On(fakerunner.Glob("docker run --rm --name demo-genomic-input-* --network none *")).Do(func(_ context.Context, c fakerunner.Call) (docker.Result, error) {
		fx.probes = append(fx.probes, c.Argv)
		if fx.hiddenOnce && len(fx.probes) == 1 {
			return docker.Result{Stdout: []byte("-1\n-1\n")}, nil
		}
		var sizes []string
		for _, a := range c.Argv[slices.Index(c.Argv, "sh")+4:] {
			fi, err := os.Stat(a)
			if err != nil {
				return docker.Result{}, err
			}
			sizes = append(sizes, strconv.FormatInt(fi.Size(), 10))
		}
		return docker.Result{Stdout: []byte(strings.Join(sizes, "\n") + "\n")}, nil
	})
	f.On(fakerunner.Glob("docker run -i --rm --name demo-genomic-stage-* *")).Do(func(_ context.Context, c fakerunner.Call) (docker.Result, error) {
		fx.stageIn = c.Stdin
		return docker.Result{}, nil
	})
	for _, id := range []string{ops.GenomicSplitStepID, ops.GenomicMetadataStepID, ops.GenomicFinalizeStepID} {
		f.On(fakerunner.Glob("docker run --rm --name demo-" + id + "-* --user 0:0 --network none *")).Do(func(context.Context, fakerunner.Call) (docker.Result, error) {
			return docker.Result{ExitCode: fx.exits[id]}, nil
		})
	}
	f.On(fakerunner.Glob("docker run --rm --name demo-genomic-move-* *"))
	f.On(fakerunner.Glob("docker run --rm --name demo-genomic-list-* *")).Do(func(_ context.Context, c fakerunner.Call) (docker.Result, error) {
		if slices.Contains(c.Argv, "demo_genomic-staging:/data:ro") {
			return docker.Result{Stdout: []byte(fx.staged)}, nil
		}
		return docker.Result{Stdout: []byte(fx.live)}, nil
	})
	f.On(fakerunner.Glob("docker run --rm --name demo-genomic-backup-* *")).Do(func(context.Context, fakerunner.Call) (docker.Result, error) {
		return docker.Result{Stderr: []byte("cp: write error: No space left on device\n"), ExitCode: fx.backupExit}, nil
	})
	f.On(fakerunner.Glob("docker run --rm --name demo-genomic-promote-* *")).Do(func(_ context.Context, c fakerunner.Call) (docker.Result, error) {
		fx.promotes = append(fx.promotes, c.Argv)
		if len(fx.promotes) > 1 {
			return docker.Result{}, nil
		}
		if i := slices.Index(c.Argv, "sh"); i+4 <= len(c.Argv) {
			fx.promoteFx = c.Argv[i+4:]
		}
		return docker.Result{ExitCode: fx.promoteExit}, nil
	})
	return fx
}

func (fx *genomicFixture) writeIndex(t *testing.T, data string) {
	t.Helper()
	if err := os.WriteFile(fx.index, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
}

func (fx *genomicFixture) load(opts ops.GenomicLoadOptions) ([]string, error) {
	if opts.Partition == "" {
		opts.Partition = "synth"
	}
	opts.VCFIndex = fx.index
	return ops.LoadGenomic(context.Background(), fx.d, fx.st, fx.cfg, fx.state, opts)
}

func TestLoadGenomicStagesWithoutTouchingHPDS(t *testing.T) {
	fx := newGenomicFixture(t)
	promoted, err := fx.load(ops.GenomicLoadOptions{})
	if err != nil {
		t.Fatal(err)
	}
	vcfs := "-v " + fx.vcfDir + ":" + fx.vcfDir + ":ro"
	fx.f.AssertOrder(
		fakerunner.Glob("docker run * demo-genomic-input-* "+vcfs+" alpine:* sh -c * sh "+filepath.Join(fx.vcfDir, "chr21.vcf.gz")+" "+filepath.Join(fx.vcfDir, "sub/chr22.vcf")),
		fakerunner.Glob("docker run -i * demo-genomic-stage-* -v demo_genomic-staging:/data alpine:* sh -c set -eu; rm -rf /data/all /data/merged; mkdir -p /data/all /data/merged; cat > /data/vcfIndex.tsv"),
		fakerunner.Glob("docker run * demo-genomic-split-* -e HEAPSIZE -e LOADER_NAME -v demo_genomic-staging:/opt/local/hpds "+vcfs+" hms-dbmi/pic-sure-hpds-etl:abc123abc123"),
		fakerunner.Glob("docker run * demo-genomic-metadata-* -v demo_genomic-staging:/opt/local/hpds "+vcfs+" hms-dbmi/pic-sure-hpds-etl:abc123abc123"),
		fakerunner.Glob("docker run * demo-genomic-finalize-* -v demo_genomic-staging:/opt/local/hpds hms-dbmi/pic-sure-hpds-etl:abc123abc123"),
		fakerunner.Glob("docker run * demo-genomic-move-* -v demo_genomic-staging:/data alpine:* sh -c * sh synth"),
	)
	for id, loader := range map[string]string{"demo-genomic-split-": "SplitChromosomeVcfLoader", "demo-genomic-metadata-": "VariantMetadataLoader", "demo-genomic-finalize-": "GenomicDatasetFinalizer"} {
		if env := fx.spy.env(id); !slices.Contains(env, "LOADER_NAME="+loader) || !slices.Contains(env, "HEAPSIZE=16000") {
			t.Errorf("%s env %q", id, env)
		}
	}
	data, _ := os.ReadFile(fx.index)
	if string(fx.stageIn) != string(data) {
		t.Errorf("staged index %q", fx.stageIn)
	}
	if len(promoted) != 0 {
		t.Errorf("promoted %q", promoted)
	}
	fx.f.AssertNotCalled(fakerunner.Glob("docker compose *"))
}

func TestLoadGenomicPromotesOnlyThisPartition(t *testing.T) {
	fx := newGenomicFixture(t)
	fx.staged = "older\nsynth\n"
	promoted, err := fx.load(ops.GenomicLoadOptions{Promote: true, HeapMB: 512})
	if err != nil {
		t.Fatal(err)
	}
	fx.f.AssertOrder(
		fakerunner.Glob("docker run * demo-genomic-move-*"),
		fakerunner.Glob("docker compose * stop hpds"),
		fakerunner.Glob("docker run * demo-genomic-promote-* -v demo_hpds-genomic:/live -v demo_genomic-staging:/staged:ro alpine:* *"),
		fakerunner.Glob("docker compose * up -d --wait --wait-timeout 900 hpds"),
		fakerunner.Glob("docker compose * ps --all --format json hpds"),
	)
	if !slices.Equal(fx.promoteFx, []string{"synth"}) || !slices.Equal(promoted, []string{"synth"}) {
		t.Errorf("promote args %q, promoted %q", fx.promoteFx, promoted)
	}
	if env := fx.spy.env("demo-genomic-split-"); !slices.Contains(env, "HEAPSIZE=512") {
		t.Errorf("split env %q", env)
	}
	fx.f.AssertNotCalled(fakerunner.Glob("docker run * demo-genomic-backup-*"))
	fx.f.AssertNotCalled(fakerunner.Glob("docker run * demo-genomic-list-* -v demo_genomic-staging:/data:ro *"))
}

func TestLoadGenomicPromotesAllPartitionsWithBackup(t *testing.T) {
	fx := newGenomicFixture(t)
	fx.staged = "older\nsynth\nall-bak\nbad name\n"
	promoted, err := fx.load(ops.GenomicLoadOptions{Promote: true, AllPartitions: true, Backup: true})
	if err != nil {
		t.Fatal(err)
	}
	fx.f.AssertOrder(
		fakerunner.Glob("docker run * demo-genomic-list-* -v demo_hpds-genomic:/data:ro alpine:* sh -c * sh /data"),
		fakerunner.Glob("docker run * demo-genomic-list-* -v demo_genomic-staging:/data:ro alpine:* sh -c * sh /data/genomic"),
		fakerunner.Glob("docker compose * stop hpds"),
		fakerunner.Glob("docker run * demo-genomic-backup-* -v demo_hpds-genomic:/live:ro -v demo_genomic-staging:/staged *"),
		fakerunner.Glob("docker run * demo-genomic-promote-*"),
		fakerunner.Glob("docker compose * up -d --wait --wait-timeout 900 hpds"),
	)
	if want := []string{"older", "synth"}; !slices.Equal(fx.promoteFx, want) || !slices.Equal(promoted, want) {
		t.Errorf("promote args %q, promoted %q", fx.promoteFx, promoted)
	}
}

func TestLoadGenomicRefusesMoreThanTenPartitions(t *testing.T) {
	fx := newGenomicFixture(t)
	fx.live = "p1\np2\np3\np4\np5\np6\np7\np8\np9\np10\n"
	if _, err := fx.load(ops.GenomicLoadOptions{Partition: "p3", Promote: true}); err != nil {
		t.Fatalf("replacing a live partition: %v", err)
	}
	fx = newGenomicFixture(t)
	fx.live = "p1\np2\np3\np4\np5\np6\np7\np8\n.hidden\n.promote-p9\n"
	fx.staged = "p10\n-x y\n"
	_, err := fx.load(ops.GenomicLoadOptions{Promote: true, AllPartitions: true})
	if err == nil || exitcode.FromError(err) != exitcode.CodePrecondition || !strings.Contains(err.Error(), "would leave 11 genomic partitions") {
		t.Fatalf("err = %v", err)
	}
	fx.f.AssertNotCalled(fakerunner.Glob("docker run * demo-genomic-stage-*"))
}

func TestLoadGenomicBackupFailureStopsThePromote(t *testing.T) {
	fx := newGenomicFixture(t)
	fx.backupExit = 1
	_, err := fx.load(ops.GenomicLoadOptions{Promote: true, Backup: true})
	if err == nil || !strings.Contains(err.Error(), "No space left on device") || !strings.Contains(err.Error(), "previous backup is kept") ||
		!strings.Contains(err.Error(), "HPDS is stopped") {
		t.Fatalf("err = %v", err)
	}
	fx.f.AssertNotCalled(fakerunner.Glob("docker run * demo-genomic-promote-*"))
	fx.f.AssertNotCalled(fakerunner.Glob("docker compose * up *"))
}

func TestLoadGenomicPromoteFailureRemovesThePartialCopy(t *testing.T) {
	fx := newGenomicFixture(t)
	fx.promoteExit = 1
	promoted, err := fx.load(ops.GenomicLoadOptions{Promote: true})
	if err == nil || !strings.Contains(err.Error(), "copying synth into volume demo_hpds-genomic") || !strings.Contains(err.Error(), "HPDS is stopped") {
		t.Fatalf("err = %v", err)
	}
	if len(fx.promotes) != 2 || fx.promotes[1][len(fx.promotes[1])-1] != "set -eu; cd /live; rm -rf ./.promote-*" {
		t.Errorf("promote helpers %q", fx.promotes)
	}
	if promoted != nil {
		t.Errorf("promoted %q", promoted)
	}
	fx.f.AssertNotCalled(fakerunner.Glob("docker compose * up *"))
}

func TestLoadGenomicLoaderFailureLeavesHPDSAlone(t *testing.T) {
	for _, id := range []string{ops.GenomicSplitStepID, ops.GenomicMetadataStepID, ops.GenomicFinalizeStepID} {
		t.Run(id, func(t *testing.T) {
			fx := newGenomicFixture(t)
			fx.exits[id] = 3
			_, err := fx.load(ops.GenomicLoadOptions{Promote: true, EnableProfile: true})
			if err == nil || !strings.Contains(err.Error(), "exited 3") || !strings.Contains(err.Error(), "HPDS and its data are unchanged") {
				t.Fatalf("err = %v", err)
			}
			fx.f.AssertNotCalled(fakerunner.Glob("docker run * demo-genomic-move-*"))
			fx.f.AssertNotCalled(fakerunner.Glob("docker compose *"))
		})
	}
}

func TestLoadGenomicRefusals(t *testing.T) {
	for _, tc := range []struct {
		name  string
		opts  ops.GenomicLoadOptions
		index string // replaces the index's data lines
		vcf   string // replaces --vcf-dir
		setup func(*genomicFixture)
		want  string
		code  int
	}{
		{name: "partition", opts: ops.GenomicLoadOptions{Partition: "a/b"}, want: "--partition must match", code: exitcode.CodeUsage},
		{name: "reserved partition", opts: ops.GenomicLoadOptions{Partition: "all-bak"}, want: "reserved", code: exitcode.CodeUsage},
		{name: "all without promote", opts: ops.GenomicLoadOptions{AllPartitions: true}, want: "--all-partitions needs --promote", code: exitcode.CodeUsage},
		{name: "backup without promote", opts: ops.GenomicLoadOptions{Backup: true}, want: "--backup needs --promote", code: exitcode.CodeUsage},
		{name: "heap", opts: ops.GenomicLoadOptions{HeapMB: -1}, want: "--heap", code: exitcode.CodeUsage},
		{name: "colon", vcf: "/tmp/a:b", want: "contains ':'", code: exitcode.CodeUsage},
		{name: "relative path", index: "chr21.vcf.gz\t21\n", want: "line 2: \"chr21.vcf.gz\" isn't an absolute path", code: exitcode.CodeUsage},
		{name: "outside the dir", index: "/etc/hosts\t21\n", want: "isn't under --vcf-dir", code: exitcode.CodeUsage},
		{name: "missing VCF", index: "@/missing.vcf\t21\n", want: "line 2:", code: exitcode.CodeUsage},
		{name: "a directory", index: "@/sub\t21\n", want: "isn't a regular file", code: exitcode.CodeUsage},
		{name: "empty index", index: "\n", want: "names no VCF files", code: exitcode.CodeUsage},
		{name: "shared data", setup: func(fx *genomicFixture) { fx.cfg.HPDS.Data = stack.HPDSShared }, want: "read-only", code: 1},
		{name: "no image", setup: func(fx *genomicFixture) { fx.state.Images = nil }, want: "run `pic-sure up`", code: exitcode.CodePrecondition},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fx := newGenomicFixture(t)
			if tc.index != "" {
				fx.writeIndex(t, "header\n"+strings.ReplaceAll(tc.index, "@", fx.vcfDir))
			}
			if tc.setup != nil {
				tc.setup(fx)
			}
			tc.opts.VCFDir = tc.vcf
			_, err := fx.load(tc.opts)
			if err == nil || !strings.Contains(err.Error(), tc.want) || exitcode.FromError(err) != tc.code {
				t.Fatalf("err = %v (exit %d), want %q and exit %d", err, exitcode.FromError(err), tc.want, tc.code)
			}
			fx.f.AssertNotCalled(fakerunner.Glob("docker run * demo-genomic-stage-*"))
		})
	}
}

func TestLoadGenomicCopiesVCFsTheDaemonCantSee(t *testing.T) {
	fx := newGenomicFixture(t)
	fx.hiddenOnce = true
	var copyDir string
	_, err := fx.load(ops.GenomicLoadOptions{MkdirTemp: func(p string) (string, error) {
		var err error
		copyDir, err = os.MkdirTemp(t.TempDir(), p)
		return copyDir, err
	}})
	if err != nil {
		t.Fatal(err)
	}
	if len(fx.probes) != 2 || !slices.Contains(fx.probes[1], copyDir+":"+fx.vcfDir+":ro") {
		t.Fatalf("probes %q", fx.probes)
	}
	fx.f.AssertCalled(fakerunner.Glob("docker run * demo-genomic-split-* -v " + copyDir + ":" + fx.vcfDir + ":ro *"))
	if _, err := os.Stat(copyDir); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the copy is still there: %v", err)
	}
}

func TestLoadGenomicEnablesTheProfile(t *testing.T) {
	fx := newGenomicFixture(t)
	fx.cfg.Components.PicSure.Source = t.TempDir()
	fx.cfg.Components.Migrations.Source = t.TempDir()
	fx.cfg.Auth.Mode, fx.cfg.Auth.AdminEmail = stack.AuthOpen, "admin@example.com"
	doc, err := stack.NewConfigDoc(fx.cfg)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := doc.Bytes()
	if err := fx.st.WriteConfig(data); err != nil {
		t.Fatal(err)
	}
	fx.state.InitializedAt = t0
	for _, img := range catalog.Images() {
		fx.state.Images[img.Name] = "abc123abc123"
	}
	if err := fx.st.SaveState(fx.state); err != nil {
		t.Fatal(err)
	}
	files := fx.st.Path(render.FilesDir)
	fx.f.On(fakerunner.Glob("docker compose * config *")).Stdout("services:\n" +
		"  hpds: {volumes: [{type: bind, source: " + files + ", target: /f}]}\n" +
		"  httpd: {volumes: [{type: bind, source: " + files + ", target: /f}]}\n")
	compose := fx.d.Compose
	_, err = fx.load(ops.GenomicLoadOptions{EnableProfile: true, Converge: ops.ConvergeOptions{
		CLIVersion: "test",
		Compose:    func() (docker.Composer, error) { return compose, nil },
	}})
	if err != nil {
		t.Fatal(err)
	}
	fx.f.AssertOrder(
		fakerunner.Glob("docker run * demo-genomic-list-* -v demo_hpds-genomic:/data:ro *"),
		fakerunner.Glob("docker run * demo-genomic-move-*"),
		fakerunner.Glob("docker compose * stop hpds"),
		fakerunner.Glob("docker compose * config *"),
		fakerunner.Glob("docker compose * up -d --wait --wait-timeout 900 hpds"),
	)
	fx.f.AssertNotCalled(fakerunner.Glob("docker run * demo-genomic-promote-*"))
	saved, err := fx.st.LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if saved.HPDS.Profile != ops.GenomicProfile || fx.cfg.HPDS.Profile != ops.GenomicProfile {
		t.Errorf("saved profile %q, cfg %q", saved.HPDS.Profile, fx.cfg.HPDS.Profile)
	}
	state, _ := fx.st.LoadState()
	if !slices.Equal(state.PendingRestarts, []string{"httpd"}) {
		t.Errorf("pending restarts %q, want httpd only", state.PendingRestarts)
	}
	var warnings []string
	for _, e := range fx.rec.Events() {
		if w, ok := e.(events.Warning); ok {
			warnings = append(warnings, w.Text)
		}
	}
	if len(warnings) != 2 || !strings.Contains(warnings[0], "holds no genomic partition") || !strings.Contains(warnings[1], "run `pic-sure up` to restart them") {
		t.Errorf("warnings %q", warnings)
	}
}
