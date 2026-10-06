package phenoinput

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
)

// errStop ends a walk early without an error.
var errStop = errors.New("stop walking")

// walk calls fn with the name of each regular-file entry of the archive at
// file, in archive order, and a func that opens the entry's content. The
// opener is valid only during that call to fn. Names are as stored, so they
// may be unclean or unsafe. walk ignores ErrInsecurePath, which tar and zip
// report only under a GODEBUG setting, because callers check names
// themselves.
func walk(ctx context.Context, file string, format Format, fn func(name string, open func() (io.ReadCloser, error)) error) error {
	f, err := os.Open(file)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	if format == Zip {
		return walkZip(ctx, file, f, fn)
	}

	var r io.Reader = ctxReader{ctx, f}
	if format == TarGz {
		gz, err := gzip.NewReader(r)
		if err != nil {
			return fmt.Errorf("reading %s as gzip: %w", file, err)
		}
		defer func() { _ = gz.Close() }()
		r = gz
	}
	tr := tar.NewReader(r)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil && !errors.Is(err, tar.ErrInsecurePath) {
			return fmt.Errorf("reading %s: %w", file, err)
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		if err := fn(hdr.Name, func() (io.ReadCloser, error) { return io.NopCloser(tr), nil }); err != nil {
			return err
		}
	}
}

func walkZip(ctx context.Context, file string, f *os.File, fn func(string, func() (io.ReadCloser, error)) error) error {
	st, err := f.Stat()
	if err != nil {
		return err
	}
	zr, err := zip.NewReader(f, st.Size())
	if err != nil && !errors.Is(err, zip.ErrInsecurePath) {
		return fmt.Errorf("reading %s as zip: %w", file, err)
	}
	for _, zf := range zr.File {
		if err := context.Cause(ctx); err != nil {
			return err
		}
		if !zf.Mode().IsRegular() {
			continue
		}
		open := func() (io.ReadCloser, error) {
			rc, err := zf.Open()
			if err != nil {
				return nil, err
			}
			return struct {
				io.Reader
				io.Closer
			}{ctxReader{ctx, rc}, rc}, nil
		}
		if err := fn(zf.Name, open); err != nil {
			return err
		}
	}
	return nil
}

// extract copies the archive entry named entry, a clean name that
// listEntries returned, to the same path under dir, and returns that path.
// The copy goes through an os.Root on dir, so even a name that got past
// listEntries can't write outside it.
func extract(ctx context.Context, file string, format Format, entry, dir string) (string, error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return "", err
	}
	defer func() { _ = root.Close() }()

	found := false
	err = walk(ctx, file, format, func(raw string, open func() (io.ReadCloser, error)) error {
		if name, ok := csvEntryName(raw); !ok || name != entry {
			return nil
		}
		found = true
		if err := copyEntry(root, entry, open); err != nil {
			return fmt.Errorf("extracting %s from %s: %w", entry, file, err)
		}
		return errStop
	})
	if err != nil && !errors.Is(err, errStop) {
		return "", err
	}
	if !found {
		return "", fmt.Errorf("%s changed while it was read: entry %s is gone", file, entry)
	}

	out := filepath.Join(dir, filepath.FromSlash(entry))
	head, err := readHead(out)
	switch {
	case err != nil:
		return "", err
	case len(head) == 0:
		return "", fmt.Errorf("entry %s in %s is empty", entry, file)
	case isBinary(head):
		return "", fmt.Errorf("entry %s in %s is binary data, not a CSV", entry, file)
	}
	return out, nil
}

func copyEntry(root *os.Root, name string, open func() (io.ReadCloser, error)) error {
	if d := path.Dir(name); d != "." {
		if err := root.MkdirAll(d, 0o700); err != nil {
			return err
		}
	}
	in, err := open()
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	out, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	_, err = io.Copy(out, in)
	return errors.Join(err, out.Close())
}

// gunzip decompresses file, a gzip of a single CSV, into dir and returns
// the CSV's path. The name allConcepts.csv matches the mount target.
func gunzip(ctx context.Context, file, dir string) (string, error) {
	f, err := os.Open(file)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	gz, err := gzip.NewReader(ctxReader{ctx, f})
	if err != nil {
		return "", fmt.Errorf("reading %s as gzip: %w", file, err)
	}
	defer func() { _ = gz.Close() }()
	out := filepath.Join(dir, "allConcepts.csv")
	w, err := os.OpenFile(out, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", err
	}
	_, err = io.Copy(w, gz)
	if err = errors.Join(err, w.Close()); err != nil {
		return "", fmt.Errorf("decompressing %s: %w", file, err)
	}
	return out, nil
}

// readHead returns the first sniffLen bytes of the file at name.
func readHead(name string) ([]byte, error) {
	f, err := os.Open(name)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	return sniff(f)
}
