package genomic

import (
	"crypto/rand"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestFixtureLoadsInHPDS runs the three HPDS genomic loaders from a real
// pic-sure-hpds-etl image over the fixture, the way load-genomic does, and
// checks that each exits 0 and leaves a store per contig. It needs Docker
// and runs only when asked:
//
//	PICSURE_HPDS_ETL_IMAGE=hms-dbmi/pic-sure-hpds-etl:<tag> go test -run LoadsInHPDS ./internal/testfixtures/genomic
func TestFixtureLoadsInHPDS(t *testing.T) {
	image := os.Getenv("PICSURE_HPDS_ETL_IMAGE")
	if image == "" {
		t.Skip("PICSURE_HPDS_ETL_IMAGE not set")
	}
	if err := exec.Command("docker", "info").Run(); err != nil {
		t.Skip("no Docker daemon")
	}
	// The index names the VCFs by the path the container sees, and the VCF
	// directory is mounted at that same path, so resolve /var -> /private/var
	// symlinks first or Docker Desktop mounts a path the index doesn't use.
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := Write(dir, dir); err != nil {
		t.Fatal(err)
	}
	suffix := randomSuffix(t)
	volume := "picsure-genomic-fixture-test-" + suffix
	docker(t, "volume", "create", volume)
	t.Cleanup(func() {
		if out, err := exec.Command("docker", "volume", "rm", "-f", volume).CombinedOutput(); err != nil {
			t.Logf("removing %s: %v: %s", volume, err, out)
		}
	})

	// The loaders read /opt/local/hpds/vcfIndex.tsv and write under
	// /opt/local/hpds/all and /opt/local/hpds/merged; one volume holds all three.
	run := func(name string, args ...string) string {
		t.Helper()
		base := []string{"run", "--rm", "--user", "0:0", "--name", "picsure-genomic-fixture-" + name + "-" + suffix,
			"-v", volume + ":/opt/local/hpds", "-v", dir + ":" + dir + ":ro"}
		return docker(t, append(base, args...)...)
	}
	run("stage", "--entrypoint", "sh", image, "-c",
		`cp "$0"/vcfIndex.tsv /opt/local/hpds/ && mkdir -p /opt/local/hpds/all /opt/local/hpds/merged`, dir)
	for _, loader := range []string{"SplitChromosomeVcfLoader", "VariantMetadataLoader", "GenomicDatasetFinalizer"} {
		run(strings.ToLower(loader), "-e", "HEAPSIZE=512", "-e", "LOADER_NAME="+loader, image)
	}

	var want []string
	for _, c := range contigs {
		for _, f := range []string{"variantStore.javabin", "VariantMetadata.javabin", "BucketIndexBySample.javabin"} {
			want = append(want, "all/"+c.name+"/"+f)
		}
	}
	out := run("check", append([]string{"--entrypoint", "sh", image, "-c", `cd /opt/local/hpds && ls "$@"`, "sh"}, want...)...)
	t.Logf("loaded:\n%s", out)
}

func docker(t *testing.T, args ...string) string {
	t.Helper()
	out, err := exec.Command("docker", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("docker %s: %v\n%s", strings.Join(args, " "), err, tail(string(out), 40))
	}
	return string(out)
}

func tail(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	return strings.Join(lines[max(0, len(lines)-n):], "\n")
}

func randomSuffix(t *testing.T) string {
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(b)
}
