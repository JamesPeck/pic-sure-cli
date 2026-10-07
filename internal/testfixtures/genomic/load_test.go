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
// checks that each exits 0 and leaves a store per contig. It also loads the
// phenotype CSV with CSVLoaderNewSearch. It needs Docker and runs only when
// asked:
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
	// The VCF directory is mounted at its own path, so the index's paths
	// work inside the container.
	dir := t.TempDir()
	if err := Write(dir, dir); err != nil {
		t.Fatal(err)
	}
	suffix := randomHex(t, 4)
	volume := "picsure-genomic-fixture-test-" + suffix
	docker(t, "volume", "create", volume)
	t.Cleanup(func() {
		if out, err := exec.Command("docker", "volume", "rm", "-f", volume).CombinedOutput(); err != nil {
			t.Logf("removing %s: %v: %s", volume, err, out)
		}
	})

	// The loaders read /opt/local/hpds/vcfIndex.tsv and write under
	// /opt/local/hpds/all and /opt/local/hpds/merged; one volume holds all three.
	run := func(name string, mounts []string, args ...string) string {
		t.Helper()
		base := []string{"run", "--rm", "--user", "0:0", "--name", "picsure-genomic-fixture-" + name + "-" + suffix}
		for _, m := range mounts {
			base = append(base, "-v", m)
		}
		return docker(t, append(base, args...)...)
	}
	genomicMounts := []string{volume + ":/opt/local/hpds", dir + ":" + dir + ":ro"}
	run("stage", genomicMounts, "--entrypoint", "sh", image, "-c",
		`cp "$0"/vcfIndex.tsv /opt/local/hpds/ && mkdir -p /opt/local/hpds/all /opt/local/hpds/merged`, dir)
	for _, loader := range []string{"SplitChromosomeVcfLoader", "VariantMetadataLoader", "GenomicDatasetFinalizer"} {
		run(strings.ToLower(loader), genomicMounts, "-e", "HEAPSIZE=512", "-e", "LOADER_NAME="+loader, image)
	}

	var want []string
	for _, c := range contigs {
		for _, f := range []string{"variantStore.javabin", "VariantMetadata.javabin", "BucketIndexBySample.javabin"} {
			want = append(want, "all/"+c.name+"/"+f)
		}
	}
	out := run("check", genomicMounts, append([]string{"--entrypoint", "sh", image, "-c", `cd /opt/local/hpds && ls "$@"`, "sh"}, want...)...)
	t.Logf("loaded:\n%s", out)

	// The phenotype loader encrypts its store with /opt/local/hpds/encryption_key,
	// which must hold exactly 32 hex characters.
	pheno := "picsure-genomic-fixture-pheno-test-" + suffix
	docker(t, "volume", "create", pheno)
	t.Cleanup(func() {
		if out, err := exec.Command("docker", "volume", "rm", "-f", pheno).CombinedOutput(); err != nil {
			t.Logf("removing %s: %v: %s", pheno, err, out)
		}
	})
	phenoMounts := []string{pheno + ":/opt/local/hpds", filepath.Join(dir, PhenotypeFile) + ":/opt/local/hpds/allConcepts.csv:ro"}
	run("key", phenoMounts, "--entrypoint", "sh", image, "-c", `printf %s "$0" > /opt/local/hpds/encryption_key`, randomHex(t, 16))
	run("csvloader", phenoMounts, "-e", "HEAPSIZE=512", "-e", "LOADER_NAME=CSVLoaderNewSearch", image)
	run("check-pheno", phenoMounts, "--entrypoint", "ls", image, "/opt/local/hpds/allObservationsStore.javabin", "/opt/local/hpds/columnMeta.javabin")
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

func randomHex(t *testing.T, n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(b)
}
