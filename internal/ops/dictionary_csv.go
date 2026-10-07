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
	"slices"
	"sort"
	"strings"

	"github.com/JamesPeck/pic-sure-cli/internal/exitcode"
)

// csvLoad is the input of `dictionary load-csv`: the datasets file and the
// concepts archive, read and checked before anything is sent.
type csvLoad struct {
	datasetsPath, conceptsPath string
	// datasets is the datasets file as given; it is sent unchanged.
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

// readDatasetRefs returns the ref column of a datasets file.
func readDatasetRefs(data []byte) ([]string, error) {
	r := csv.NewReader(bytes.NewReader(bytes.TrimPrefix(data, []byte(bom))))
	r.FieldsPerRecord = -1
	header, err := r.Read()
	if errors.Is(err, io.EOF) {
		return nil, errors.New("the file is empty")
	}
	if err != nil {
		return nil, err
	}
	col := slices.Index(header, "ref")
	if col < 0 {
		return nil, errors.New(`the header has no "ref" column`)
	}
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
		if col >= len(rec) || rec[col] == "" {
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
		if in.refCol = slices.Index(header, "dataset_ref"); in.refCol < 0 {
			return errors.New(`the header has no "dataset_ref" column`)
		}
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
		if in.refCol >= len(rec) {
			line, _ := r.FieldPos(0)
			return fmt.Errorf("line %d has no dataset_ref", line)
		}
		if err := fn(rec); err != nil {
			return err
		}
	}
}

// writeDataset writes the header and the concept rows whose dataset_ref is
// exactly ref, as CSV.
func (in *csvLoad) writeDataset(w io.Writer, ref string) error {
	cw := csv.NewWriter(w)
	if err := cw.Write(in.header); err != nil {
		return err
	}
	for _, f := range in.files {
		err := in.each(f, func(rec []string) error {
			if rec[in.refCol] != ref {
				return nil
			}
			return cw.Write(rec)
		})
		if err != nil {
			return fmt.Errorf("%s: %w", f.Name, err)
		}
	}
	cw.Flush()
	return cw.Error()
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

func (in *csvLoad) fileNames() []string {
	names := make([]string, len(in.files))
	for i, f := range in.files {
		names[i] = f.Name
	}
	return names
}

func (in *csvLoad) Close() error {
	if in.zf == nil {
		return nil
	}
	err := in.zf.Close()
	in.zf = nil
	return err
}
