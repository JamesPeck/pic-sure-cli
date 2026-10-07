package phenoinput_test

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io/fs"
	"math/rand/v2"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/JamesPeck/pic-sure-cli/internal/phenoinput"
)

const csvBody = "PATIENT_NUM,CONCEPT_PATH,NVAL_NUM,TVAL_CHAR\n1,\\demo\\age\\,42,\n"

// member is one archive entry in a generated fixture.
type member struct {
	name string
	body string
	typ  byte   // tar type flag; 0 means a regular file
	link string // symlink target
	pax  bool   // write a PAX extended header first, as macOS bsdtar does
}

func reg(name, body string) member { return member{name: name, body: body} }

func tarBytes(t *testing.T, members ...member) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, m := range members {
		hdr := &tar.Header{Name: m.name, Mode: 0o644, Size: int64(len(m.body)), Typeflag: tar.TypeReg}
		if m.typ != 0 {
			hdr.Typeflag, hdr.Size, hdr.Linkname = m.typ, 0, m.link
		}
		if m.pax {
			hdr.Format = tar.FormatPAX
			hdr.PAXRecords = map[string]string{"LIBARCHIVE.xattr.com.apple.provenance": "AQAA"}
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if hdr.Size > 0 {
			if _, err := tw.Write([]byte(m.body)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func gzipBytes(t *testing.T, data []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	if _, err := gw.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := gw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func tgzBytes(t *testing.T, members ...member) []byte {
	t.Helper()
	return gzipBytes(t, tarBytes(t, members...))
}

func zipBytes(t *testing.T, members ...member) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, m := range members {
		fh := &zip.FileHeader{Name: m.name, Method: zip.Deflate}
		body := m.body
		switch m.typ {
		case tar.TypeSymlink:
			fh.SetMode(fs.ModeSymlink | 0o777)
			body = m.link
		case tar.TypeDir:
			fh.SetMode(fs.ModeDir | 0o755)
		default:
			fh.SetMode(0o644)
		}
		w, err := zw.CreateHeader(fh)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// noisyCSV is a CSV big enough, and random enough, that its gzip is far
// larger than the 8 KiB detection reads.
func noisyCSV() string {
	r := rand.New(rand.NewPCG(1, 2))
	var b strings.Builder
	b.WriteString("PATIENT_NUM,CONCEPT_PATH,NVAL_NUM,TVAL_CHAR\n")
	for i := range 20000 {
		b.WriteString(strconv.Itoa(i) + ",\\demo\\x\\," + strconv.Itoa(r.IntN(1e9)) + ",\n")
	}
	return b.String()
}

type want struct {
	format   phenoinput.Format
	entry    string // Input.Entry
	body     string // content of Input.CSV
	inPlace  bool   // Input.CSV is the input file itself
	warning  string // substring of the only warning
	err      string // substring of the error
	entryErr *phenoinput.EntryError
}

func TestResolve(t *testing.T) {
	big := noisyCSV()
	bigTgz := tgzBytes(t, reg("big.csv", big))

	cases := []struct {
		name  string
		file  string // input file name; the content decides the format
		data  []byte
		entry string
		want  want
	}{
		{name: "tgz with zero padding after the stream", file: "padded.tgz",
			data: append(tgzBytes(t, reg("allConcepts.csv", csvBody)), make([]byte, 1024)...),
			want: want{format: phenoinput.TarGz, entry: "allConcepts.csv", body: csvBody}},
		{name: "gzip with zero padding after the stream", file: "padded.csv.gz",
			data: append(gzipBytes(t, []byte(csvBody)), make([]byte, 512)...),
			want: want{format: phenoinput.Gzip, body: csvBody}},
		{name: "gzip of two members", file: "two.csv.gz",
			data: append(gzipBytes(t, []byte("a,b\n")), gzipBytes(t, []byte("1,2\n"))...),
			want: want{format: phenoinput.Gzip, body: "a,b\n1,2\n"}},
		{name: "tgz entry with a colon in its name", file: "stamped.tgz",
			data: tgzBytes(t, reg("export_2024-01-01T10:00.csv", csvBody)),
			want: want{format: phenoinput.TarGz, entry: "export_2024-01-01T10:00.csv", body: csvBody}},
		// Plain CSV, used in place whatever its name.
		{name: "raw csv", file: "allConcepts.csv", data: []byte(csvBody),
			want: want{format: phenoinput.CSV, body: csvBody, inPlace: true}},
		{name: "raw csv with another extension", file: "pheno.txt", data: []byte(csvBody),
			want: want{format: phenoinput.CSV, body: csvBody, inPlace: true}},
		{name: "text starting like bzip2 is a csv", file: "bz.csv", data: []byte("BZh,col\n1,2\n"),
			want: want{format: phenoinput.CSV, body: "BZh,col\n1,2\n", inPlace: true}},
		{name: "raw csv ignores --entry", file: "allConcepts.csv", data: []byte(csvBody), entry: "a.csv",
			want: want{format: phenoinput.CSV, body: csvBody, inPlace: true, warning: `ignoring --entry "a.csv"`}},

		// gzip of a single CSV.
		{name: "csv.gz", file: "allConcepts.csv.gz", data: gzipBytes(t, []byte(csvBody)),
			want: want{format: phenoinput.Gzip, body: csvBody}},
		{name: "csv.gz named like a tarball", file: "pheno.tgz", data: gzipBytes(t, []byte(csvBody)),
			want: want{format: phenoinput.Gzip, body: csvBody}},
		{name: "csv.gz ignores --entry", file: "allConcepts.csv.gz", data: gzipBytes(t, []byte(csvBody)), entry: "a.csv",
			want: want{format: phenoinput.Gzip, body: csvBody, warning: "allConcepts.csv.gz is not an archive"}},
		{name: "large csv.gz", file: "big.csv.gz", data: gzipBytes(t, []byte(big)),
			want: want{format: phenoinput.Gzip, body: big}},

		// Gzipped tars.
		{name: "single-csv tgz", file: "single.tgz", data: tgzBytes(t, reg("allConcepts.csv", csvBody)),
			want: want{format: phenoinput.TarGz, entry: "allConcepts.csv", body: csvBody}},
		{name: "single-csv tgz in a subdirectory", file: "single.tar.gz",
			data: tgzBytes(t, member{name: "./data/", typ: tar.TypeDir}, reg("./data/pheno.csv", csvBody)),
			want: want{format: phenoinput.TarGz, entry: "data/pheno.csv", body: csvBody}},
		{name: "single-csv tgz with an uppercase extension", file: "single.tgz", data: tgzBytes(t, reg("PHENO.CSV", csvBody)),
			want: want{format: phenoinput.TarGz, entry: "PHENO.CSV", body: csvBody}},
		{name: "single-csv tgz with PAX headers", file: "single.tgz",
			data: tgzBytes(t, member{name: "allConcepts.csv", body: csvBody, pax: true}),
			want: want{format: phenoinput.TarGz, entry: "allConcepts.csv", body: csvBody}},
		{name: "macOS metadata entries are not CSVs", file: "mac.tgz",
			data: tgzBytes(t, reg("./._allConcepts.csv", "\x00\x05\x16\x07"), reg("./allConcepts.csv", csvBody)),
			want: want{format: phenoinput.TarGz, entry: "allConcepts.csv", body: csvBody}},
		{name: "large tgz", file: "big.tgz", data: bigTgz,
			want: want{format: phenoinput.TarGz, entry: "big.csv", body: big}},
		{name: "multi-csv tgz needs --entry", file: "multi.tgz",
			data: tgzBytes(t, reg("b.csv", "b-data\n"), reg("a.csv", "a-data\n"), reg("readme.txt", "note\n")),
			want: want{err: "multi.tgz holds 2 CSV files; choose one with --entry:\n  a.csv\n  b.csv",
				entryErr: &phenoinput.EntryError{Entries: []string{"a.csv", "b.csv"}}}},
		{name: "multi-csv tgz with --entry", file: "multi.tgz", entry: "b.csv",
			data: tgzBytes(t, reg("a.csv", "a-data\n"), reg("b.csv", "b-data\n"), reg("readme.txt", "note\n")),
			want: want{format: phenoinput.TarGz, entry: "b.csv", body: "b-data\n"}},
		{name: "--entry is matched after cleaning", file: "multi.tgz", entry: "./b.csv",
			data: tgzBytes(t, reg("./a.csv", "a-data\n"), reg("./b.csv", "b-data\n")),
			want: want{format: phenoinput.TarGz, entry: "b.csv", body: "b-data\n"}},
		{name: "multi-csv tgz with an unknown --entry", file: "multi.tgz", entry: "bogus.csv",
			data: tgzBytes(t, reg("a.csv", "a-data\n"), reg("b.csv", "b-data\n")),
			want: want{err: `--entry "bogus.csv" is not a CSV in`,
				entryErr: &phenoinput.EntryError{Entry: "bogus.csv", Entries: []string{"a.csv", "b.csv"}}}},
		{name: "single-csv tgz with an unknown --entry", file: "single.tgz", entry: "bogus.csv",
			data: tgzBytes(t, reg("allConcepts.csv", csvBody)),
			want: want{err: "choose one of:\n  allConcepts.csv",
				entryErr: &phenoinput.EntryError{Entry: "bogus.csv", Entries: []string{"allConcepts.csv"}}}},

		// Uncompressed tar.
		{name: "tar", file: "pheno.tar", data: tarBytes(t, reg("allConcepts.csv", csvBody)),
			want: want{format: phenoinput.Tar, entry: "allConcepts.csv", body: csvBody}},

		// Zip.
		{name: "single-csv zip", file: "single.zip", data: zipBytes(t, reg("allConcepts.csv", csvBody)),
			want: want{format: phenoinput.Zip, entry: "allConcepts.csv", body: csvBody}},
		{name: "zip from macOS Finder", file: "finder.zip",
			data: zipBytes(t, member{name: "pheno/", typ: tar.TypeDir}, reg("pheno/allConcepts.csv", csvBody),
				reg("__MACOSX/pheno/._allConcepts.csv", "\x00\x05\x16\x07")),
			want: want{format: phenoinput.Zip, entry: "pheno/allConcepts.csv", body: csvBody}},
		{name: "multi-csv zip needs --entry", file: "multi.zip",
			data: zipBytes(t, reg("b.csv", "b-data\n"), reg("a.csv", "a-data\n")),
			want: want{err: "holds 2 CSV files", entryErr: &phenoinput.EntryError{Entries: []string{"a.csv", "b.csv"}}}},
		{name: "multi-csv zip with --entry", file: "multi.zip", entry: "a.csv",
			data: zipBytes(t, reg("a.csv", "a-data\n"), reg("b.csv", "b-data\n")),
			want: want{format: phenoinput.Zip, entry: "a.csv", body: "a-data\n"}},

		// Rejected input.
		{name: "empty file", file: "empty.csv", data: nil, want: want{err: "empty.csv is empty"}},
		{name: "binary file", file: "image.csv", data: []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR"),
			want: want{err: "image.csv is neither a CSV nor a gzip, tar or zip archive"}},
		{name: "bzip2", file: "pheno.csv.bz2", data: []byte("BZh91AY&SY\x01\x02\x03"),
			want: want{err: "is bzip2-compressed, which isn't supported"}},
		{name: "empty bzip2", file: "pheno.csv.bz2", data: []byte("BZh9\x17rE8P\x90\x00\x00\x00\x00"),
			want: want{err: "is bzip2-compressed"}},
		{name: "zstd", file: "pheno.csv.zst", data: []byte("\x28\xb5\x2f\xfd\x20\x05\x29"),
			want: want{err: "is zstd-compressed"}},
		{name: "xz", file: "pheno.csv.xz", data: []byte("\xfd7zXZ\x00\x00\x04"), want: want{err: "is xz-compressed"}},
		{name: "gzip of binary data", file: "image.gz", data: gzipBytes(t, []byte("\x89PNG\r\n\x1a\n\x00\x00")),
			want: want{err: "decompresses to binary data"}},
		{name: "gzip of an empty file", file: "empty.csv.gz", data: gzipBytes(t, nil),
			want: want{err: "decompresses to an empty file"}},
		{name: "corrupt gzip", file: "bad.gz", data: []byte("\x1f\x8bnot really gzip"),
			want: want{err: "bad.gz as gzip: gzip: invalid header"}},
		{name: "truncated tgz", file: "cut.tgz", data: bigTgz[:len(bigTgz)*2/3],
			want: want{err: "unexpected EOF"}},
		{name: "tgz with a bad checksum after the tar's end", file: "badsum.tgz", data: badChecksum(tgzBytes(t, reg("big.csv", big))),
			want: want{err: "invalid checksum"}},
		{name: "gzip with data after the stream", file: "junk.csv.gz", data: append(gzipBytes(t, []byte(csvBody)), "junk"...),
			want: want{err: "data after the end of the compressed stream"}},
		{name: "gzip with data after zero padding", file: "padjunk.csv.gz", data: append(gzipBytes(t, []byte(csvBody)), "\x00\x00j"...),
			want: want{err: "data after the end of the compressed stream"}},
		{name: "empty tgz", file: "empty.tgz", data: tgzBytes(t), want: want{err: "empty.tgz has no .csv entries"}},
		{name: "tgz without csv entries", file: "docs.tgz", data: tgzBytes(t, reg("readme.txt", "note\n")),
			want: want{err: "has no .csv entries"}},
		{name: "empty zip", file: "empty.zip", data: zipBytes(t), want: want{err: "has no .csv entries"}},
		{name: "spreadsheet is a zip without csv entries", file: "pheno.xlsx",
			data: zipBytes(t, reg("[Content_Types].xml", "<Types/>"), reg("xl/workbook.xml", "<workbook/>")),
			want: want{err: "has no .csv entries"}},
		{name: "symlinked csv in a tgz is not a regular file", file: "link.tgz",
			data: tgzBytes(t, member{name: "link.csv", typ: tar.TypeSymlink, link: "/etc/passwd"}),
			want: want{err: "has no .csv entries"}},
		{name: "hard-linked csv in a tgz is not a regular file", file: "link.tgz",
			data: tgzBytes(t, member{name: "link.csv", typ: tar.TypeLink, link: "/etc/passwd"}),
			want: want{err: "has no .csv entries"}},
		{name: "symlinked csv in a zip is not a regular file", file: "link.zip",
			data: zipBytes(t, member{name: "link.csv", typ: tar.TypeSymlink, link: "/etc/passwd"}),
			want: want{err: "has no .csv entries"}},
		{name: "duplicate entries", file: "dup.tgz",
			data: tgzBytes(t, reg("a.csv", "one\n"), reg("./a.csv", "two\n")),
			want: want{err: `dup.tgz has more than one entry named "a.csv"`}},
		{name: "empty csv entry", file: "hollow.tgz", data: tgzBytes(t, reg("a.csv", "")),
			want: want{err: "hollow.tgz is empty"}},
		{name: "binary csv entry", file: "binary.zip", data: zipBytes(t, reg("a.csv", "\x00\x01\x02")),
			want: want{err: "is binary data, not a CSV"}},

		// Paths that escape the extraction directory.
		{name: "tgz entry escaping with ..", file: "evil.tgz", data: tgzBytes(t, reg("../evil.csv", csvBody)),
			want: want{err: `has an entry outside the archive's own directory: "../evil.csv"`}},
		{name: "tgz entry escaping through a subdirectory", file: "evil.tgz",
			data: tgzBytes(t, reg("data/../../evil.csv", csvBody)),
			want: want{err: "has an entry outside the archive's own directory"}},
		{name: "tgz entry with an absolute path", file: "evil.tgz", data: tgzBytes(t, reg("/tmp/evil.csv", csvBody)),
			want: want{err: "has an entry outside the archive's own directory"}},
		{name: "escaping entry rejected even beside a safe one", file: "evil.tgz", entry: "a.csv",
			data: tgzBytes(t, reg("a.csv", csvBody), reg("../evil.csv", csvBody)),
			want: want{err: "has an entry outside the archive's own directory"}},
		{name: "zip entry escaping with ..", file: "evil.zip", data: zipBytes(t, reg("../evil.csv", csvBody)),
			want: want{err: "has an entry outside the archive's own directory"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			base := t.TempDir()
			// The run's temp dir sits two levels down, so an entry that
			// escaped it by one or two levels would land in base.
			tempDir := filepath.Join(base, "cache", "tmp")
			file := filepath.Join(base, tc.file)
			if err := os.WriteFile(file, tc.data, 0o644); err != nil {
				t.Fatal(err)
			}

			in, cleanup, err := phenoinput.Resolve(context.Background(), file, phenoinput.Options{Entry: tc.entry, MkdirTemp: mkdirTempIn(t, tempDir)})
			if cleanup == nil {
				t.Fatal("Resolve returned a nil cleanup func")
			}
			t.Cleanup(func() { _ = cleanup() })

			if tc.want.err != "" {
				if err == nil {
					t.Fatalf("Resolve succeeded with %+v; want an error containing %q", in, tc.want.err)
				}
				if !strings.Contains(err.Error(), tc.want.err) {
					t.Fatalf("error %q does not contain %q", err, tc.want.err)
				}
				checkEntryError(t, err, file, tc.want.entryErr)
				assertNoRunDirs(t, tempDir)
				assertNotExist(t, filepath.Join(base, "evil.csv"), filepath.Join(base, "cache", "evil.csv"))
				return
			}
			if err != nil {
				t.Fatalf("Resolve: %v", err)
			}
			if in.Format != tc.want.format || in.Entry != tc.want.entry {
				t.Errorf("Resolve = format %q entry %q; want %q, %q", in.Format, in.Entry, tc.want.format, tc.want.entry)
			}
			checkWarnings(t, in.Warnings, tc.want.warning)
			if !filepath.IsAbs(in.CSV) {
				t.Errorf("CSV path %q is not absolute", in.CSV)
			}
			got, err := os.ReadFile(in.CSV)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != tc.want.body {
				t.Errorf("CSV content differs from the input CSV (%d bytes, want %d)", len(got), len(tc.want.body))
			}

			if tc.want.inPlace {
				if in.CSV != file {
					t.Errorf("CSV = %q; want the input file %q", in.CSV, file)
				}
				assertNoRunDirs(t, tempDir)
				return
			}
			rel, err := filepath.Rel(tempDir, in.CSV)
			if err != nil || !filepath.IsLocal(rel) || filepath.Dir(rel) == "." || filepath.Base(rel) != "allConcepts.csv" {
				t.Fatalf("CSV %q is not allConcepts.csv in a run directory under %q", in.CSV, tempDir)
			}
			if fi, err := os.Stat(in.CSV); err != nil || fi.Mode().Perm() != 0o644 {
				t.Errorf("CSV mode = %v (%v); want 0644 so the loader container can read it", fi.Mode().Perm(), err)
			}
			if err := cleanup(); err != nil {
				t.Fatalf("cleanup: %v", err)
			}
			assertNoRunDirs(t, tempDir)
			if err := cleanup(); err != nil {
				t.Errorf("second cleanup: %v", err)
			}
		})
	}
}

func checkEntryError(t *testing.T, err error, file string, want *phenoinput.EntryError) {
	t.Helper()
	var ee *phenoinput.EntryError
	if !errors.As(err, &ee) {
		if want != nil {
			t.Errorf("error %q is not an *EntryError", err)
		}
		return
	}
	if want == nil {
		t.Errorf("unexpected *EntryError: %v", err)
		return
	}
	if ee.File != file || ee.Entry != want.Entry || !slices.Equal(ee.Entries, want.Entries) {
		t.Errorf("EntryError = %+v; want File %q Entry %q Entries %q", ee, file, want.Entry, want.Entries)
	}
}

func checkWarnings(t *testing.T, got []string, want string) {
	t.Helper()
	if want == "" {
		if len(got) > 0 {
			t.Errorf("unexpected warnings %q", got)
		}
		return
	}
	if len(got) != 1 || !strings.Contains(got[0], want) {
		t.Errorf("warnings = %q; want one containing %q", got, want)
	}
}

// assertNoRunDirs checks that tempDir holds no run directories: Resolve
// either made none or removed it.
func assertNoRunDirs(t *testing.T, tempDir string) {
	t.Helper()
	entries, err := os.ReadDir(tempDir)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		t.Fatal(err)
	}
	for _, e := range entries {
		t.Errorf("%s left behind in the temp dir", e.Name())
	}
}

func assertNotExist(t *testing.T, paths ...string) {
	t.Helper()
	for _, p := range paths {
		if _, err := os.Lstat(p); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("%s exists after Resolve (err %v)", p, err)
		}
	}
}

func TestResolveUsesANewDirectoryPerRun(t *testing.T) {
	base := t.TempDir()
	file := filepath.Join(base, "single.tgz")
	if err := os.WriteFile(file, tgzBytes(t, reg("allConcepts.csv", csvBody)), 0o644); err != nil {
		t.Fatal(err)
	}
	opts := phenoinput.Options{MkdirTemp: mkdirTempIn(t, filepath.Join(base, "tmp"))}

	first, cleanFirst, err := phenoinput.Resolve(context.Background(), file, opts)
	if err != nil {
		t.Fatal(err)
	}
	second, cleanSecond, err := phenoinput.Resolve(context.Background(), file, opts)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cleanSecond() }()
	if first.CSV == second.CSV {
		t.Fatalf("both runs extracted to %s", first.CSV)
	}
	if err := cleanFirst(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(second.CSV); err != nil {
		t.Errorf("cleaning up the first run removed the second's CSV: %v", err)
	}
}

func TestResolveRequiresMkdirTemp(t *testing.T) {
	file := filepath.Join(t.TempDir(), "allConcepts.csv")
	if err := os.WriteFile(file, []byte(csvBody), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := phenoinput.Resolve(context.Background(), file, phenoinput.Options{}); err == nil {
		t.Fatal("Resolve without MkdirTemp succeeded")
	}
}

func TestResolveStopsWhenCanceled(t *testing.T) {
	base := t.TempDir()
	file := filepath.Join(base, "single.tgz")
	if err := os.WriteFile(file, tgzBytes(t, reg("allConcepts.csv", csvBody)), 0o644); err != nil {
		t.Fatal(err)
	}
	tempDir := filepath.Join(base, "tmp")
	interrupted := errors.New("interrupted")
	ctx, cancel := context.WithCancelCause(context.Background())
	cancel(interrupted)

	_, _, err := phenoinput.Resolve(ctx, file, phenoinput.Options{MkdirTemp: mkdirTempIn(t, tempDir)})
	if !errors.Is(err, interrupted) {
		t.Fatalf("Resolve error = %v; want the context's cause", err)
	}
	assertNoRunDirs(t, tempDir)
}

func TestResolveRejectsNonFiles(t *testing.T) {
	base := t.TempDir()
	opts := phenoinput.Options{MkdirTemp: mkdirTempIn(t, filepath.Join(base, "tmp"))}
	if _, _, err := phenoinput.Resolve(context.Background(), filepath.Join(base, "missing.tgz"), opts); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("missing file: error %v; want fs.ErrNotExist", err)
	}
	if _, _, err := phenoinput.Resolve(context.Background(), base, opts); err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Errorf("directory: error %v; want 'not a regular file'", err)
	}
}

func TestListCSVEntries(t *testing.T) {
	cases := []struct {
		name string
		data []byte
		want []string
		err  string
	}{
		{name: "multi-csv tgz, sorted", data: tgzBytes(t, reg("b.csv", "b\n"), reg("./a.csv", "a\n"), reg("readme.txt", "x\n")),
			want: []string{"a.csv", "b.csv"}},
		{name: "single-csv tgz", data: tgzBytes(t, reg("allConcepts.csv", csvBody)), want: []string{"allConcepts.csv"}},
		{name: "multi-csv zip, sorted", data: zipBytes(t, reg("z/b.csv", "b\n"), reg("a.csv", "a\n")),
			want: []string{"a.csv", "z/b.csv"}},
		{name: "raw csv has no entries", data: []byte(csvBody)},
		{name: "csv.gz has no entries", data: gzipBytes(t, []byte(csvBody))},
		{name: "empty file", data: nil, err: "is empty"},
		{name: "binary file", data: []byte("\x7fELF\x02\x01\x01\x00"), err: "neither a CSV"},
		{name: "archive without csv entries", data: tgzBytes(t, reg("readme.txt", "x\n")), err: "has no .csv entries"},
		{name: "escaping entry", data: tgzBytes(t, reg("../evil.csv", csvBody)), err: "outside the archive's own directory"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			file := filepath.Join(t.TempDir(), "input")
			if err := os.WriteFile(file, tc.data, 0o644); err != nil {
				t.Fatal(err)
			}
			got, err := phenoinput.ListCSVEntries(context.Background(), file)
			if tc.err != "" {
				if err == nil || !strings.Contains(err.Error(), tc.err) {
					t.Fatalf("error = %v; want one containing %q", err, tc.err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(got, tc.want) {
				t.Errorf("ListCSVEntries = %q; want %q", got, tc.want)
			}
		})
	}

	if _, err := phenoinput.ListCSVEntries(context.Background(), filepath.Join(t.TempDir(), "missing.tgz")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("missing file: error %v; want fs.ErrNotExist", err)
	}
}

// mkdirTempIn stands in for the cache's TempDir, making run directories
// under dir.
func mkdirTempIn(t *testing.T, dir string) func(string) (string, error) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	return func(pattern string) (string, error) { return os.MkdirTemp(dir, pattern) }
}

// badChecksum corrupts the CRC-32 in a gzip stream's trailer.
func badChecksum(gz []byte) []byte {
	gz[len(gz)-8] ^= 0xff
	return gz
}

// lateCancelCtx turns canceled once armed and its Err has been asked a
// few more times, so a test can cancel partway through a copy.
type lateCancelCtx struct {
	context.Context
	armed atomic.Bool
	left  atomic.Int32
}

func (c *lateCancelCtx) Err() error {
	if c.armed.Load() && c.left.Add(-1) < 0 {
		return context.Canceled
	}
	return nil
}

// Canceling partway through copying the CSV out stops the copy, and Resolve
// removes the run directory.
func TestResolveStopsWhenCanceledDuringExtraction(t *testing.T) {
	big := noisyCSV()
	inputs := map[string][]byte{
		"pheno.tgz":    tgzBytes(t, reg("big.csv", big)),
		"pheno.zip":    zipBytes(t, reg("big.csv", big)),
		"pheno.csv.gz": gzipBytes(t, []byte(big)),
	}
	for name, data := range inputs {
		t.Run(name, func(t *testing.T) {
			base := t.TempDir()
			file := filepath.Join(base, name)
			if err := os.WriteFile(file, data, 0o644); err != nil {
				t.Fatal(err)
			}
			tempDir := filepath.Join(base, "tmp")
			mkdir := mkdirTempIn(t, tempDir)
			ctx := &lateCancelCtx{Context: context.Background()}
			ctx.left.Store(3)
			opts := phenoinput.Options{MkdirTemp: func(pattern string) (string, error) {
				dir, err := mkdir(pattern)
				ctx.armed.Store(true)
				return dir, err
			}}

			_, _, err := phenoinput.Resolve(ctx, file, opts)
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("Resolve error = %v; want context.Canceled", err)
			}
			assertNoRunDirs(t, tempDir)
		})
	}
}
