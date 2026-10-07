// Package phenoinput turns the file given to `data load-phenotype --file`
// into a CSV the HPDS loader can mount (spec §9.6). The format is detected
// from the file's content, not its name, and archives are read with Go's
// own libraries, so a plain CSV, a gzip of one CSV, a tar (gzipped or not)
// and a zip are handled the same way on every OS, with no host tar, gzip or
// unzip.
package phenoinput

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
)

// Format is the kind of input file Resolve detected.
type Format string

const (
	CSV   Format = "csv"    // a plain-text CSV, loaded in place
	Gzip  Format = "gzip"   // a gzip of a single CSV
	Tar   Format = "tar"    // an uncompressed tar
	TarGz Format = "tar.gz" // a gzipped tar
	Zip   Format = "zip"
)

func (f Format) isArchive() bool { return f == Tar || f == TarGz || f == Zip }

// Options configures Resolve.
type Options struct {
	// Entry selects a CSV inside an archive (--entry). It is required when
	// the archive holds more than one CSV, and ignored with a warning for
	// input that isn't an archive.
	Entry string
	// MkdirTemp creates the per-run extraction directory, named pattern as
	// in os.MkdirTemp; pass the host cache's (*cache.Cache).TempDir. It is
	// required: extraction never falls back to $TMPDIR, which Colima and
	// Lima don't share with the Docker VM (spec §7.1).
	MkdirTemp func(pattern string) (string, error)
}

// Input is a phenotype CSV ready to mount into the loader.
type Input struct {
	// CSV is the absolute path of the CSV to load: the input file itself
	// for a plain CSV, otherwise the decompressed or extracted copy in the
	// per-run directory.
	CSV    string
	Format Format
	// Entry is the archive entry that was extracted, or "" if the input
	// isn't an archive.
	Entry string
	// Warnings are messages for the user, such as an ignored --entry.
	Warnings []string
}

// EntryError reports an archive that holds several CSVs when --entry is
// missing, or an --entry that names none of an archive's CSVs.
type EntryError struct {
	File    string
	Entry   string   // the --entry given, or "" if there was none
	Entries []string // the archive's CSV entries, sorted
}

func (e *EntryError) Error() string {
	var b strings.Builder
	if e.Entry == "" {
		fmt.Fprintf(&b, "%s holds %d CSV files; choose one with --entry:", e.File, len(e.Entries))
	} else {
		fmt.Fprintf(&b, "--entry %q is not a CSV in %s; choose one of:", e.Entry, e.File)
	}
	for _, name := range e.Entries {
		b.WriteString("\n  " + name)
	}
	return b.String()
}

// Resolve detects the format of file and returns the CSV to load. A plain
// CSV is used in place. Anything else is decompressed or extracted into a
// new directory made by opts.MkdirTemp, which the returned cleanup func removes;
// on error Resolve removes it itself. The cleanup func is never nil.
//
// Resolve rejects an empty file, binary data that isn't a supported
// archive, an archive with no .csv entries, an archive entry whose path
// would land outside the extraction directory, and a missing or unknown
// --entry (as an *EntryError).
func Resolve(ctx context.Context, file string, opts Options) (Input, func() error, error) {
	noop := func() error { return nil }
	if opts.MkdirTemp == nil {
		return Input{}, noop, errors.New("phenoinput: Options.MkdirTemp is required")
	}
	format, err := detect(ctx, file)
	if err != nil {
		return Input{}, noop, err
	}
	in := Input{Format: format}
	if !format.isArchive() && opts.Entry != "" {
		in.Warnings = append(in.Warnings,
			fmt.Sprintf("ignoring --entry %q: %s is not an archive", opts.Entry, file))
	}
	if format == CSV {
		in.CSV, err = filepath.Abs(file)
		if err != nil {
			return Input{}, noop, err
		}
		return in, noop, nil
	}

	if format.isArchive() {
		entries, err := listEntries(ctx, file, format)
		if err != nil {
			return Input{}, noop, err
		}
		if in.Entry, err = selectEntry(file, entries, opts.Entry); err != nil {
			return Input{}, noop, err
		}
	}

	made, err := opts.MkdirTemp("phenotype-")
	if err != nil {
		return Input{}, noop, err
	}
	dir, err := filepath.Abs(made)
	if err != nil {
		return Input{}, noop, errors.Join(err, os.RemoveAll(made))
	}
	cleanup := func() error { return os.RemoveAll(dir) }
	if format == Gzip {
		in.CSV, err = gunzip(ctx, file, dir)
	} else {
		in.CSV, err = extract(ctx, file, format, in.Entry, dir)
	}
	if err != nil {
		return Input{}, noop, errors.Join(err, cleanup())
	}
	return in, cleanup, nil
}

// ListCSVEntries returns the CSV entries of the archive at file, sorted, for
// the load wizard's entry picker. It returns nil for a plain CSV or a gzip
// of a single file, which have no entries to choose from, and an error for
// any input Resolve would reject before extracting anything.
func ListCSVEntries(ctx context.Context, file string) ([]string, error) {
	format, err := detect(ctx, file)
	if err != nil || !format.isArchive() {
		return nil, err
	}
	return listEntries(ctx, file, format)
}

// csvEntryName cleans an archive entry name and reports whether the entry
// is a CSV to offer. macOS metadata (AppleDouble "._" files, and the
// __MACOSX directory Finder adds to zips) is not.
func csvEntryName(raw string) (string, bool) {
	name := path.Clean(raw)
	if !strings.EqualFold(path.Ext(name), ".csv") ||
		strings.HasPrefix(path.Base(name), "._") ||
		slices.Contains(strings.Split(name, "/"), "__MACOSX") {
		return "", false
	}
	return name, true
}

// listEntries returns the archive's CSV entries, sorted. An archive with
// none, with two entries of the same name, or with an entry whose path
// leaves the extraction directory is an error.
func listEntries(ctx context.Context, file string, format Format) ([]string, error) {
	var names []string
	seen := map[string]bool{}
	err := walk(ctx, file, format, func(raw string, _ func() (io.ReadCloser, error)) error {
		name, ok := csvEntryName(raw)
		if !ok {
			return nil
		}
		if !filepath.IsLocal(name) {
			return fmt.Errorf("%s has an entry outside the archive's own directory: %q", file, raw)
		}
		if seen[name] {
			return fmt.Errorf("%s has more than one entry named %q", file, name)
		}
		seen[name] = true
		names = append(names, name)
		return nil
	})
	if err != nil {
		return nil, err
	}
	if len(names) == 0 {
		return nil, fmt.Errorf("%s has no .csv entries", file)
	}
	slices.Sort(names)
	return names, nil
}

// selectEntry picks the entry to extract: the only one, or the one --entry
// names.
func selectEntry(file string, entries []string, want string) (string, error) {
	if want == "" {
		if len(entries) == 1 {
			return entries[0], nil
		}
		return "", &EntryError{File: file, Entries: entries}
	}
	if name := path.Clean(want); slices.Contains(entries, name) {
		return name, nil
	}
	return "", &EntryError{File: file, Entry: want, Entries: entries}
}

// ctxReader stops reading once ctx is done, so a long extraction ends
// promptly on Ctrl-C.
type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (c ctxReader) Read(p []byte) (int, error) {
	if c.ctx.Err() != nil {
		return 0, context.Cause(c.ctx)
	}
	return c.r.Read(p)
}
