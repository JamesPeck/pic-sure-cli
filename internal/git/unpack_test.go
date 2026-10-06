package git_test

import (
	"archive/tar"
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/JamesPeck/pic-sure-cli/internal/git"
)

type entry struct {
	name, body, link string
	typ              byte
	mode             int64
}

func file(name, body string) entry {
	return entry{name: name, body: body, typ: tar.TypeReg, mode: 0o644}
}
func dir(name string) entry { return entry{name: name, typ: tar.TypeDir, mode: 0o755} }
func symlink(name, target string) entry {
	return entry{name: name, link: target, typ: tar.TypeSymlink, mode: 0o777}
}
func ofType(name string, typ byte) entry { return entry{name: name, typ: typ, mode: 0o644} }

func tarball(t *testing.T, entries ...entry) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, e := range entries {
		hdr := &tar.Header{Name: e.name, Linkname: e.link, Typeflag: e.typ, Mode: e.mode, Size: int64(len(e.body))}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(e.body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// unpack unpacks into a fresh dir inside a parent dir that must stay empty
// apart from it, so a write that escapes the destination is caught.
func unpack(t *testing.T, archive []byte) (string, error) {
	t.Helper()
	parent := t.TempDir()
	dest := filepath.Join(parent, "dest")
	if err := os.Mkdir(dest, 0o755); err != nil {
		t.Fatal(err)
	}
	err := git.Unpack(bytes.NewReader(archive), dest)
	if entries, _ := os.ReadDir(parent); len(entries) != 1 {
		t.Errorf("Unpack wrote outside the destination: %v", entries)
	}
	return dest, err
}

func TestUnpackRefusesEntriesOutsideTheDestination(t *testing.T) {
	for name, archive := range map[string][]entry{
		"parent file":         {file("../evil", "x")},
		"climbing file":       {file("a/../../evil", "x")},
		"absolute file":       {file("/tmp/evil", "x")},
		"absolute symlink":    {symlink("l", "/etc/passwd")},
		"climbing symlink":    {symlink("a/l", "../../evil")},
		"symlink chain":       {symlink("self", "."), symlink("up", "self/..")},
		"write through chain": {symlink("self", "."), symlink("up", "self/.."), file("up/evil", "x")},
		"dir through chain":   {symlink("self", "."), symlink("up", "self/.."), dir("up/evil/")},
		"hard link":           {file("a", "x"), {name: "b", link: "a", typ: tar.TypeLink}},
		"device":              {ofType("dev", tar.TypeChar)},
		"fifo":                {ofType("fifo", tar.TypeFifo)},
		"duplicate file":      {file("a", "x"), file("a", "y")},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := unpack(t, tarball(t, archive...)); err == nil {
				t.Error("Unpack accepted the archive")
			}
		})
	}
}

func TestUnpackKeepsLinksInsideTheTree(t *testing.T) {
	dest, err := unpack(t, tarball(t,
		dir("a/"),
		file("a/f", "x"),
		symlink("a/sibling", "f"),
		symlink("a/up", "../a/f"),
		symlink("dangling", "a/missing"),
		symlink("dir", "a"),
	))
	if err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(filepath.Join(dest, "dir", "up")); err != nil || string(got) != "x" {
		t.Errorf("dir/up = %q, %v; want the contents of a/f", got, err)
	}
	if got, _ := os.Readlink(filepath.Join(dest, "dangling")); got != "a/missing" {
		t.Errorf("dangling -> %q, want a/missing", got)
	}
}

func TestUnpackSkipsGitsGlobalHeader(t *testing.T) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeXGlobalHeader, Name: "pax_global_header",
		PAXRecords: map[string]string{"comment": "0123456789abcdef0123456789abcdef01234567"}}); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	dest, err := unpack(t, buf.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	if entries, _ := os.ReadDir(dest); len(entries) != 0 {
		t.Errorf("dest = %v, want empty", entries)
	}
}

// failingAfter yields data, then fails, like a producer that exits non-zero
// once the archive is out.
type failingAfter struct {
	data io.Reader
	err  error
}

func (f *failingAfter) Read(p []byte) (int, error) {
	n, err := f.data.Read(p)
	if errors.Is(err, io.EOF) {
		return n, f.err
	}
	return n, err
}

func TestUnpackReportsAProducerFailureAfterTheArchiveEnds(t *testing.T) {
	boom := errors.New("producer failed")
	dest := t.TempDir()
	err := git.Unpack(&failingAfter{data: bytes.NewReader(tarball(t, file("a", "x"))), err: boom}, dest)
	if !errors.Is(err, boom) {
		t.Errorf("err = %v, want the producer's error", err)
	}
}
