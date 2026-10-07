package ops

import (
	"archive/zip"
	"bytes"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/JamesPeck/pic-sure-cli/internal/exitcode"
)

// csvLoad is the input of `dictionary load-csv`: the datasets file and the
// concepts archive, read and checked before anything is sent.
type csvLoad struct {
	datasetsPath, conceptsPath string
	// datasets is the datasets file without a byte order mark, which the
	// ETL would take as part of the first header.
	datasets []byte
	// refs are the datasets' refs, in file order.
	refs []string

	zf    *os.File
	files []*zip.File
	// header is the concepts files' shared header; refCol is the index of
	// its dataset_ref column.
	header []string
	refCol int
	// counts is the number of concept rows per dataset_ref.
	counts map[string]int
}

const bom = "\ufeff"

// openCSVLoad reads datasetsPath and the concepts_*.csv files inside the
// zip at conceptsPath (at any depth, in name order). It parses every row,
// so a malformed file fails here, before a --clear empties the dictionary.
// Problems with the input are usage errors.
func openCSVLoad(datasetsPath, conceptsPath string) (_ *csvLoad, err error) {
	in := &csvLoad{datasetsPath: datasetsPath, conceptsPath: conceptsPath, counts: map[string]int{}}
	defer func() {
		if err != nil {
			_ = in.Close()
		}
	}()
	if in.datasets, err = os.ReadFile(datasetsPath); err != nil {
		return nil, exitcode.Usage("--datasets: %w", err)
	}
	in.datasets = bytes.TrimPrefix(in.datasets, []byte(bom))
	if in.refs, err = readDatasetRefs(in.datasets); err != nil {
		return nil, exitcode.Usage("--datasets %s: %w", datasetsPath, err)
	}

	if in.zf, err = os.Open(conceptsPath); err != nil {
		return nil, exitcode.Usage("--concepts: %w", err)
	}
	fi, err := in.zf.Stat()
	if err != nil {
		return nil, exitcode.Usage("--concepts: %w", err)
	}
	zr, err := zip.NewReader(in.zf, fi.Size())
	if err != nil {
		return nil, exitcode.Usage("--concepts %s is not a zip archive: %w", conceptsPath, err)
	}
	for _, f := range zr.File {
		if ok, _ := path.Match("concepts_*.csv", path.Base(f.Name)); ok && !f.FileInfo().IsDir() {
			in.files = append(in.files, f)
		}
	}
	if len(in.files) == 0 {
		return nil, exitcode.Usage("--concepts %s holds no concepts_*.csv file", conceptsPath)
	}
	sort.Slice(in.files, func(i, j int) bool { return in.files[i].Name < in.files[j].Name })
	for _, f := range in.files {
		if err := in.scan(f); err != nil {
			return nil, exitcode.Usage("--concepts %s: %s: %w", conceptsPath, f.Name, err)
		}
	}
	return in, nil
}

// The columns the ETL requires (DatasetController, ConceptController).
var (
	datasetColumns = []string{"ref", "full_name", "abbreviation", "description"}
	conceptColumns = []string{"dataset_ref", "name", "display", "concept_type", "concept_path", "parent_concept_path"}
)

// requireColumns checks header has every column in want, and no column
// twice: the ETL would read a repeated one's last copy.
func requireColumns(header, want []string) error {
	for i, c := range header {
		if slices.Contains(header[:i], c) {
			return fmt.Errorf("the header has the %s column twice", c)
		}
	}
	var missing []string
	for _, c := range want {
		if !slices.Contains(header, c) {
			missing = append(missing, c)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("the header lacks the %s column(s)", strings.Join(missing, ", "))
	}
	return nil
}

// readDatasetRefs returns the ref column of a datasets file.
func readDatasetRefs(data []byte) ([]string, error) {
	r := csv.NewReader(bytes.NewReader(data))
	r.FieldsPerRecord = -1
	header, err := r.Read()
	if errors.Is(err, io.EOF) {
		return nil, errors.New("the file is empty")
	}
	if err != nil {
		return nil, err
	}
	if err := requireColumns(header, datasetColumns); err != nil {
		return nil, err
	}
	col := slices.Index(header, "ref")
	var refs []string
	for {
		rec, err := r.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		line, _ := r.FieldPos(0)
		if err := checkWidth(rec, header, line); err != nil {
			return nil, err
		}
		if rec[col] == "" {
			return nil, fmt.Errorf("line %d has an empty ref", line)
		}
		if slices.Contains(refs, rec[col]) {
			return nil, fmt.Errorf("line %d lists ref %q twice", line, rec[col])
		}
		refs = append(refs, rec[col])
	}
	if len(refs) == 0 {
		return nil, errors.New("the file lists no datasets")
	}
	return refs, nil
}

// checkWidth refuses a row narrower than its header, which the ETL indexes
// past the end of. It ignores extra fields, as the ETL does.
func checkWidth(rec, header []string, line int) error {
	if len(rec) < len(header) {
		return fmt.Errorf("line %d has %d fields; the header has %d", line, len(rec), len(header))
	}
	return nil
}

// scan checks f's header against the first file's and counts its rows by
// dataset_ref.
func (in *csvLoad) scan(f *zip.File) error {
	return in.each(f, func(rec []string) error {
		in.counts[rec[in.refCol]]++
		return nil
	})
}

// each calls fn for every row of f after the header.
func (in *csvLoad) each(f *zip.File, fn func(rec []string) error) error {
	rc, err := f.Open()
	if err != nil {
		return err
	}
	defer func() { _ = rc.Close() }()
	r := csv.NewReader(rc)
	r.FieldsPerRecord = -1
	r.ReuseRecord = true
	header, err := r.Read()
	if errors.Is(err, io.EOF) {
		return errors.New("the file is empty")
	}
	if err != nil {
		return err
	}
	header[0] = strings.TrimPrefix(header[0], bom)
	switch {
	case in.header == nil:
		in.header = slices.Clone(header)
		if err := requireColumns(header, conceptColumns); err != nil {
			return err
		}
		in.refCol = slices.Index(header, "dataset_ref")
	case !slices.Equal(header, in.header):
		return fmt.Errorf("the header differs from %s's", in.files[0].Name)
	}
	for {
		rec, err := r.Read()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		line, _ := r.FieldPos(0)
		if err := checkWidth(rec, in.header, line); err != nil {
			return err
		}
		if err := fn(rec); err != nil {
			return err
		}
	}
}

// splitBatch bounds the files split holds open at once.
var splitBatch = 200

// split writes each listed dataset's rows (dataset_ref exactly its ref) to
// its own CSV file in dir, with the header, and returns the file of each
// ref that has rows. It reads the concepts files once per splitBatch
// datasets.
func (in *csvLoad) split(dir string) (map[string]string, error) {
	var refs []string
	for _, ref := range in.refs {
		if in.counts[ref] > 0 {
			refs = append(refs, ref)
		}
	}
	paths := map[string]string{}
	for start := 0; start < len(refs); start += splitBatch {
		batch := refs[start:min(start+splitBatch, len(refs))]
		if err := in.splitPass(dir, batch, len(paths), paths); err != nil {
			return nil, err
		}
	}
	return paths, nil
}

func (in *csvLoad) splitPass(dir string, refs []string, n int, paths map[string]string) (err error) {
	type out struct {
		f *os.File
		w *csv.Writer
	}
	outs := map[string]*out{}
	defer func() {
		for _, o := range outs {
			if cerr := o.f.Close(); err == nil {
				err = cerr
			}
		}
	}()
	for i, ref := range refs {
		p := filepath.Join(dir, fmt.Sprintf("concepts-%d.csv", n+i))
		f, err := os.Create(p)
		if err != nil {
			return err
		}
		o := &out{f: f, w: csv.NewWriter(f)}
		outs[ref], paths[ref] = o, p
		if err := o.w.Write(in.header); err != nil {
			return err
		}
	}
	for _, f := range in.files {
		err := in.each(f, func(rec []string) error {
			if o := outs[rec[in.refCol]]; o != nil {
				return o.w.Write(rec)
			}
			return nil
		})
		if err != nil {
			return fmt.Errorf("%s: %w", f.Name, err)
		}
	}
	for _, o := range outs {
		o.w.Flush()
		if err := o.w.Error(); err != nil {
			return err
		}
	}
	return nil
}

// unmatched returns the dataset_refs of concept rows that name no dataset
// in the datasets file, sorted.
func (in *csvLoad) unmatched() []string {
	var out []string
	for ref := range in.counts {
		if !slices.Contains(in.refs, ref) {
			out = append(out, ref)
		}
	}
	sort.Strings(out)
	return out
}

func (in *csvLoad) Close() error {
	if in.zf == nil {
		return nil
	}
	err := in.zf.Close()
	in.zf = nil
	return err
}
