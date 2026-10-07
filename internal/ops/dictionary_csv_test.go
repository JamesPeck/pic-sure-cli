package ops

import (
	"archive/zip"
	"bytes"
	"encoding/csv"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/JamesPeck/pic-sure-cli/internal/exitcode"
)

// writeZip writes files (name → contents) into a zip at dir/name.
func writeZip(t *testing.T, dir, name string, files map[string]string, order ...string) string {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, n := range order {
		w, err := zw.Create(n)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(files[n])); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func readAllCSV(t *testing.T, data string) [][]string {
	t.Helper()
	r := csv.NewReader(strings.NewReader(data))
	r.FieldsPerRecord = -1
	rows, err := r.ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

// The bash split matched each dataset's rows with grep "^\"*$ref", a regex
// prefix: phs1 also took phs10's rows, a "." in a ref matched any
// character, and a ref was only ever looked for at the start of the line.
// The split must match the dataset_ref column exactly.
func TestConceptSplitTrickyRefs(t *testing.T) {
	dir := t.TempDir()
	datasets := filepath.Join(dir, "datasets.csv")
	data := bom + "ref,full_name,abbreviation,description\r\n" +
		"phs1,One,O,first\r\n" +
		"phs10,Ten,T,\"ten, with a comma\"\r\n" +
		"phs1.v2,Dotted,D,dot\r\n" +
		"\"a,b\",Comma,C,quoted ref\r\n" +
		"empty,Empty,E,no concepts\r\n"
	if err := os.WriteFile(datasets, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
	header := "dataset_ref,name,display,concept_type,concept_path,parent_concept_path,values\n"
	concepts := writeZip(t, dir, "concepts.zip", map[string]string{
		"export/concepts_2.csv": header +
			"phs10,c10,C ten,categorical,\\phs10\\c10\\,,\"[\"\"x\"\"]\"\n" +
			"phs1xv2,cx,not dotted,categorical,\\phs1xv2\\,,\n",
		"export/concepts_1.csv": header +
			"\"phs1\",c1,\"C one, quoted\",continuous,\\phs1\\c1\\,,\"[1,2]\"\n" +
			"phs1.v2,cd,dotted,categorical,\\phs1.v2\\cd\\,,\n" +
			"\"a,b\",cab,\"multi\nline\",categorical,\\ab\\,,\n" +
			"other,co,mentions phs1,categorical,\\phs1\\other\\,,\n",
		"__MACOSX/export/._concepts_1.csv": "binary junk",
		"export/readme.txt":                "not a concepts file",
	}, "export/concepts_2.csv", "export/concepts_1.csv", "__MACOSX/export/._concepts_1.csv", "export/readme.txt")

	in, err := openCSVLoad(datasets, concepts)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = in.Close() }()

	if want := []string{"phs1", "phs10", "phs1.v2", "a,b", "empty"}; !reflect.DeepEqual(in.refs, want) {
		t.Fatalf("refs = %q, want %q", in.refs, want)
	}
	if want := []string{"export/concepts_1.csv", "export/concepts_2.csv"}; !reflect.DeepEqual(in.fileNames(), want) {
		t.Fatalf("files = %q, want %q", in.fileNames(), want)
	}
	if want := map[string]int{"phs1": 1, "phs10": 1, "phs1.v2": 1, "a,b": 1, "other": 1, "phs1xv2": 1}; !reflect.DeepEqual(in.counts, want) {
		t.Fatalf("counts = %v, want %v", in.counts, want)
	}
	if want := []string{"other", "phs1xv2"}; !reflect.DeepEqual(in.unmatched(), want) {
		t.Fatalf("unmatched = %q, want %q", in.unmatched(), want)
	}

	split := func(ref string) [][]string {
		var b bytes.Buffer
		if err := in.writeDataset(&b, ref); err != nil {
			t.Fatal(err)
		}
		return readAllCSV(t, b.String())
	}
	hdr := strings.Split(strings.TrimSuffix(header, "\n"), ",")
	cases := map[string][][]string{
		"phs1":    {hdr, {"phs1", "c1", "C one, quoted", "continuous", `\phs1\c1\`, "", "[1,2]"}},
		"phs10":   {hdr, {"phs10", "c10", "C ten", "categorical", `\phs10\c10\`, "", `["x"]`}},
		"phs1.v2": {hdr, {"phs1.v2", "cd", "dotted", "categorical", `\phs1.v2\cd\`, "", ""}},
		"a,b":     {hdr, {"a,b", "cab", "multi\nline", "categorical", `\ab\`, "", ""}},
		"empty":   {hdr},
	}
	for ref, want := range cases {
		if got := split(ref); !reflect.DeepEqual(got, want) {
			t.Errorf("split(%q) =\n%q\nwant\n%q", ref, got, want)
		}
	}
}

func TestOpenCSVLoadRejectsBadInput(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "datasets.csv")
	if err := os.WriteFile(good, []byte("ref,full_name,abbreviation,description\nphs1,One,O,x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	header := "dataset_ref,name,display,concept_type,concept_path,parent_concept_path,values\n"
	okZip := writeZip(t, dir, "ok.zip", map[string]string{"concepts_1.csv": header + "phs1,c,c,categorical,\\c\\,,\n"}, "concepts_1.csv")

	write := func(name, data string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	cases := []struct {
		name, datasets, concepts, want string
	}{
		{"no ref column", write("noref.csv", "name,x\na,b\n"), okZip, `no "ref" column`},
		{"no datasets", write("empty.csv", "ref,full_name\n"), okZip, "lists no datasets"},
		{"empty ref", write("blank.csv", "ref,full_name\n,x\n"), okZip, "empty ref"},
		{"duplicate ref", write("dup.csv", "ref,full_name\na,x\na,y\n"), okZip, `ref "a" twice`},
		{"not a zip", good, good, "not a zip"},
		{"no concepts files", good, writeZip(t, dir, "none.zip", map[string]string{"x.csv": header}, "x.csv"), "no concepts_*.csv"},
		{"no dataset_ref", good, writeZip(t, dir, "nocol.zip", map[string]string{"concepts_1.csv": "name\nx\n"}, "concepts_1.csv"), `no "dataset_ref" column`},
		{"headers differ", good, writeZip(t, dir, "differ.zip", map[string]string{
			"concepts_1.csv": header,
			"concepts_2.csv": "dataset_ref,name\n",
		}, "concepts_1.csv", "concepts_2.csv"), "header differs"},
		{"bad csv", good, writeZip(t, dir, "badcsv.zip", map[string]string{"concepts_1.csv": header + "phs1,\"unterminated\n"}, "concepts_1.csv"), "concepts_1.csv"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			in, err := openCSVLoad(c.datasets, c.concepts)
			if err == nil {
				_ = in.Close()
				t.Fatalf("no error, want %q", c.want)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Fatalf("error %q, want it to contain %q", err, c.want)
			}
			var ee *exitcode.Error
			if !errors.As(err, &ee) || ee.Code != exitcode.CodeUsage {
				t.Fatalf("error %v is not a usage error", err)
			}
		})
	}
}
