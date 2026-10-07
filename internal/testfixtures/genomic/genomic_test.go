package genomic

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

const checkedIn = "../../../testdata/genomic"

// TestCheckedInFixtureIsCurrent fails when testdata/genomic differs from
// what the generator writes. Run go generate ./internal/testfixtures/genomic
// after changing the generator.
func TestCheckedInFixtureIsCurrent(t *testing.T) {
	dir := t.TempDir()
	if err := Write(dir, ""); err != nil {
		t.Fatal(err)
	}
	names := generatedNames(t, dir)
	for _, name := range names {
		want, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		got, err := os.ReadFile(filepath.Join(checkedIn, name))
		if err != nil {
			t.Fatalf("%s: %v (run go generate ./internal/testfixtures/genomic)", name, err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("testdata/genomic/%s is stale; run go generate ./internal/testfixtures/genomic", name)
		}
	}
	var extra []string
	for _, name := range generatedNames(t, checkedIn) {
		if name != "README.md" && !slices.Contains(names, name) {
			extra = append(extra, name)
		}
	}
	if len(extra) > 0 {
		t.Errorf("testdata/genomic has files the generator doesn't write: %v", extra)
	}
}

func generatedNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

func TestIndexDirPrefixesFileNames(t *testing.T) {
	idx := string(vcfIndex("/data/vcf"))
	for _, c := range contigs {
		if !strings.Contains(idx, "\n/data/vcf/"+c.file+"\t") {
			t.Errorf("index doesn't name /data/vcf/%s:\n%s", c.file, idx)
		}
	}
}

// TestVCFsMatchLoaderRules checks the constraints HPDS's loaders put on
// their input, reading the files back the way the loader does.
func TestVCFsMatchLoaderRules(t *testing.T) {
	dir := t.TempDir()
	if err := Write(dir, ""); err != nil {
		t.Fatal(err)
	}
	nSamples := len(genomicPatients())
	total := 0
	for _, c := range contigs {
		f, err := os.Open(filepath.Join(dir, c.file))
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = f.Close() }()
		var r io.Reader = f
		if c.bgzip {
			// gzip.Reader reads a multi-member stream, which every BGZF file is.
			zr, err := gzip.NewReader(f)
			if err != nil {
				t.Fatal(err)
			}
			r = zr
		}
		lastPos := 0
		sc := bufio.NewScanner(r)
		for sc.Scan() {
			line := sc.Text()
			if strings.HasPrefix(line, "#") {
				continue
			}
			cols := strings.Split(line, "\t")
			if len(cols) != 9+nSamples {
				t.Fatalf("%s: %d columns, want %d: %s", c.file, len(cols), 9+nSamples, line)
			}
			if cols[0] != c.name {
				t.Errorf("%s: CHROM %s, want only %s", c.file, cols[0], c.name)
			}
			pos, _ := strconv.Atoi(cols[1])
			if pos <= lastPos {
				t.Errorf("%s: position %d not after %d", c.file, pos, lastPos)
			}
			lastPos = pos
			for _, kv := range strings.Split(cols[7], ";") {
				if !strings.Contains(kv, "=") {
					t.Errorf("%s:%d: INFO token %q isn't key=value", c.file, pos, kv)
				}
			}
			for _, gt := range cols[9:] {
				if len(gt) != 3 {
					t.Errorf("%s:%d: genotype %q isn't 3 characters", c.file, pos, gt)
				}
			}
			total++
		}
		if err := sc.Err(); err != nil {
			t.Fatalf("%s: %v", c.file, err)
		}
	}
	if total != len(variants) {
		t.Errorf("read %d variants, want %d", total, len(variants))
	}
}

func TestBGZFBlocksSplitLargeInput(t *testing.T) {
	data := bytes.Repeat([]byte("synthetic\n"), 2*bgzfBlockData/10+7)
	out := bgzf(data)
	zr, err := gzip.NewReader(bytes.NewReader(out))
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(zr)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, data) {
		t.Fatal("round trip differs")
	}
	if !bytes.HasSuffix(out, bgzfEOF) {
		t.Error("no BGZF EOF block")
	}
	// Each block's BSIZE field (offset 16) is its length minus one, which is
	// how BGZF readers find the next block.
	blocks := 0
	for rest := out; len(rest) > 0; blocks++ {
		size := int(rest[16]) | int(rest[17])<<8
		rest = rest[size+1:]
	}
	if blocks != 4 { // three data blocks and the EOF block
		t.Errorf("%d blocks, want 4", blocks)
	}
}

func TestExpectedQueries(t *testing.T) {
	want := map[string][]int{
		"gene-SYNTHA":                 {102, 103, 104},
		"gene-SYNTHD":                 {108},
		"gene-unknown":                {},
		"gene-SYNTHA-and-stop_gained": {104},
		"variant-chr21-33001877-het":  {102, 103},
		"variant-chr21-33001877-hom":  {104},
		"gene-SYNTHC-female":          {103, 107},
	}
	for _, q := range expected().Queries {
		if w, ok := want[q.Name]; ok && !slices.Equal(q.Patients, w) {
			t.Errorf("%s: patients %v, want %v", q.Name, q.Patients, w)
		}
	}
}
