package ops

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/csv"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/JamesPeck/pic-sure-cli/internal/cache"
	"github.com/JamesPeck/pic-sure-cli/internal/events"
	"github.com/JamesPeck/pic-sure-cli/internal/exitcode"
	"github.com/JamesPeck/pic-sure-cli/internal/phenoinput"
	"github.com/JamesPeck/pic-sure-cli/internal/render"
	"github.com/JamesPeck/pic-sure-cli/internal/stack"
	"github.com/JamesPeck/pic-sure-cli/internal/steps"
)

// Demo step IDs, before the loader's and the dictionary's.
const (
	StepDemoDownload = "demo-download"
	StepDemoPrepare  = "demo-prepare"
)

// DemoDatasetsCommit is the pic-sure-public-datasets commit the demo files
// are downloaded at. Changing it means re-pinning every DemoFile's size and
// SHA-256.
const DemoDatasetsCommit = "e549e48803c31a4ab6bf985a87c1ef1d9d4a4416"

// demoDatasetsBase is the raw-file root of pic-sure-public-datasets. The
// repo stores the files as plain blobs, not Git LFS.
const demoDatasetsBase = "https://raw.githubusercontent.com/hms-dbmi/pic-sure-public-datasets/"

// DemoAll is the dataset that merges every DemoFile.
const DemoAll = "all"

// DemoFile is one demo dataset's file in pic-sure-public-datasets.
type DemoFile struct {
	Dataset string // the `data demo` argument
	Path    string // in the repo, at DemoDatasetsCommit
	Size    int64
	SHA256  string
}

// DemoFiles are the demo datasets, in the order `all` merges them. The
// sizes are the downloads'; extracted, NHANES is 425 MB and Synthea 481 MB.
var DemoFiles = []DemoFile{
	{"nhanes", "NHANES abbreviated allConcepts.csv.tgz", 19433431,
		"946305e3138255dc8725c42e3895445462d733a4bbb9c4c521e483e8ae3a138b"},
	{"synthea", "synthea_10k_picsure_format.csv.zip", 25063520,
		"48c71528011520d864693157c16a3adb291da87a5e070f638a019b9ddcc553d6"},
	{"1000genomes", "open_access-1000Genomes_allConcepts_new_search_with_data_analyzer.csv", 12081745,
		"bac15cac8bbb9acdcf8ff657a8330abebe283b9f8546af5d26ab2ff7eb9dffa0"},
}

// DemoDatasets are the names `data demo` takes.
func DemoDatasets() []string {
	names := make([]string, 0, len(DemoFiles)+1)
	for _, f := range DemoFiles {
		names = append(names, f.Dataset)
	}
	return append(names, DemoAll)
}

// demoFiles returns the files dataset needs.
func demoFiles(dataset string) ([]DemoFile, error) {
	if dataset == DemoAll {
		return DemoFiles, nil
	}
	for _, f := range DemoFiles {
		if f.Dataset == dataset {
			return []DemoFile{f}, nil
		}
	}
	return nil, exitcode.Usage("unknown demo dataset %q; choose one of %s", dataset, strings.Join(DemoDatasets(), ", "))
}

// URL is the file's raw download URL under base, which ends in a slash;
// empty means pic-sure-public-datasets at DemoDatasetsCommit.
func (f DemoFile) URL(base string) string {
	if base == "" {
		base = demoDatasetsBase + DemoDatasetsCommit + "/"
	}
	return base + url.PathEscape(f.Path)
}

// cacheName is the file's name in the cache's downloads/: its hash, so a
// re-pinned file never reuses an old download, and its name without spaces.
func (f DemoFile) cacheName() string {
	return f.SHA256[:16] + "-" + strings.ReplaceAll(f.Path, " ", "_")
}

// DemoOptions are `data demo`'s inputs.
type DemoOptions struct {
	Dataset string
	// HeapMB is the loader's and CreateColumnmetaCSV's heap; zero means
	// DefaultLoaderHeapMB.
	HeapMB int
	Cache  *cache.Cache
	// HTTP downloads the files; it carries the stack's proxy (§9.10).
	HTTP *http.Client
	// BaseURL replaces the files' root, for tests; see DemoFile.URL.
	BaseURL string
}

// demoStall is how long a download may receive nothing before it is
// abandoned.
var demoStall = time.Minute

// DataDemo is `data demo` (§9.6). The caller holds the stack lock and sets
// d.Compose. It downloads the dataset's files into the cache, or reuses
// them when their SHA-256 matches; for `all` it merges the CSVs, whose
// headers must match. Then it loads HPDS through LoadPhenotype with the
// marker demo:<name>, and rebuilds the dictionary: hydrate with the default
// facets, the demo facet configuration, the search weights, and the
// dictionary-api restart. It returns the provenance marker.
func DataDemo(ctx context.Context, d *Deps, st *stack.Stack, cfg *stack.Config, sec *stack.Secrets, state *stack.State, opts DemoOptions) (string, error) {
	if err := RefuseSharedHPDS(cfg); err != nil {
		return "", err
	}
	files, err := demoFiles(opts.Dataset)
	if err != nil {
		return "", err
	}
	facets, err := render.DemoFacetConfig()
	if err != nil {
		return "", err
	}
	x := NewDictionary(d, st, cfg, sec, state)
	defer func() {
		if cerr := x.Close(ctx); cerr != nil {
			d.Sink.Emit(events.Warning{Text: cerr.Error()})
		}
	}()
	if err := x.Preflight(ctx, WeightsOptions{Cache: opts.Cache}); err != nil {
		return "", err
	}
	dataset, err := demoLoad(ctx, d, st, cfg, state, opts, files)
	var startErr *HPDSStartError
	if err != nil && !errors.As(err, &startErr) {
		return "", err
	}
	if err := steps.Run(ctx, d.Sink, demoDictionarySteps(x, opts, facets), steps.Options{}); err != nil {
		var se *steps.Error
		if errors.As(err, &se) && !se.Interrupted {
			err = fmt.Errorf("step %s failed: %w. HPDS has the %s data; run `pic-sure data demo %s` again to rebuild the dictionary",
				se.Step, se.Err, opts.Dataset, opts.Dataset)
		}
		if startErr != nil {
			err = fmt.Errorf("%w; before that, %w", err, startErr)
		}
		return dataset, err
	}
	if startErr != nil {
		return dataset, startErr
	}
	return dataset, nil
}

// demoDictionarySteps rebuild the dictionary after a demo load. Demo loads
// always ask for the default facets plus the facet configuration; custom
// loads (045) use what was requested.
func demoDictionarySteps(x *Dictionary, opts DemoOptions, facets []byte) []steps.Step {
	list := x.HydrateSteps(HydrateOptions{IncludeDatasetFacets: true, Clear: true, Heap: opts.HeapMB})
	list = append(list, x.FacetConfigSteps(facets)...)
	list = append(list, x.WeightsSteps(WeightsOptions{Cache: opts.Cache})...)
	return append(list, x.RefreshStep())
}

// demoLoad downloads and prepares the CSV and loads it into HPDS. It holds
// the cache's use lock from picking a download until the load returns, so
// a prune can't remove the file in between.
func demoLoad(ctx context.Context, d *Deps, st *stack.Stack, cfg *stack.Config, state *stack.State, opts DemoOptions, files []DemoFile) (string, error) {
	lock, err := opts.Cache.WithEvents(d.Sink, "").LockUse(ctx)
	if err != nil {
		return "", err
	}
	defer func() { _ = lock.Unlock() }()

	var paths, names []string
	var csvPath string
	cleanup := func() error { return nil }
	defer func() { _ = cleanup() }()
	plan := []steps.Step{{
		ID:    StepDemoDownload,
		Title: "Download the demo data",
		Apply: func(ctx context.Context, sink events.Sink) error {
			for _, f := range files {
				p, err := fetchDemoFile(ctx, sink, opts, f)
				if err != nil {
					return err
				}
				paths = append(paths, p)
				names = append(names, f.Path)
			}
			return nil
		},
	}, {
		ID:    StepDemoPrepare,
		Title: "Prepare the demo CSV",
		Apply: func(ctx context.Context, sink events.Sink) error {
			p, done, err := prepareDemoCSV(ctx, sink, opts.Cache, paths, names)
			if err != nil {
				return err
			}
			csvPath, cleanup = p, done
			return nil
		},
	}}
	if err := steps.Run(ctx, d.Sink, plan, steps.Options{}); err != nil {
		return "", err
	}
	return LoadPhenotype(ctx, d, st, cfg, state, PhenotypeLoadOptions{
		CSV:        csvPath,
		Dataset:    "demo:" + opts.Dataset,
		HeapMB:     opts.HeapMB,
		LoaderArgs: DemoLoaderArgs,
		MkdirTemp:  opts.Cache.TempDir,
	})
}

// fetchDemoFile returns f's path in the cache's downloads/, downloading it
// unless a file with the pinned SHA-256 is there. A download is written
// in the cache's tmp/ and renamed into place only once its size and hash
// match the pins.
func fetchDemoFile(ctx context.Context, sink events.Sink, opts DemoOptions, f DemoFile) (string, error) {
	path := filepath.Join(opts.Cache.DownloadsDir(), f.cacheName())
	sum, err := fileSHA256(ctx, path)
	switch {
	case err == nil && sum == f.SHA256:
		sink.Emit(events.Progress{ID: StepDemoDownload, Text: "using the cached " + f.Path})
		return path, nil
	case err == nil:
		sink.Emit(events.Warning{ID: StepDemoDownload, Text: fmt.Sprintf("the cached %s doesn't match its pinned SHA-256; downloading it again", f.Path)})
	case !errors.Is(err, fs.ErrNotExist):
		return "", err
	}

	dir, err := opts.Cache.TempDir("download-*")
	if err != nil {
		return "", err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	tmp, err := os.Create(filepath.Join(dir, f.cacheName()))
	if err != nil {
		return "", err
	}
	defer func() { _ = tmp.Close() }()
	if err := download(ctx, sink, opts.HTTP, f.URL(opts.BaseURL), f, tmp); err != nil {
		return "", fmt.Errorf("downloading %s: %w", f.Path, err)
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	if err := os.Chmod(tmp.Name(), 0o644); err != nil {
		return "", err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return "", err
	}
	return path, nil
}

// download writes the body of a GET of rawURL to w, checking it against
// f's pinned size and SHA-256.
func download(ctx context.Context, sink events.Sink, client *http.Client, rawURL string, f DemoFile, w io.Writer) error {
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	errStalled := fmt.Errorf("nothing received for %s", demoStall)
	timer := time.AfterFunc(demoStall, func() { cancel(errStalled) })
	defer timer.Stop()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return demoCause(ctx, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("the server answered %s", resp.Status)
	}
	h := sha256.New()
	p := &downloadProgress{sink: sink, name: f.Path, size: f.Size, timer: timer}
	// One byte over the pinned size is enough to tell it's wrong.
	n, err := io.Copy(io.MultiWriter(w, h, p), io.LimitReader(resp.Body, f.Size+1))
	if err != nil {
		return demoCause(ctx, err)
	}
	if n != f.Size {
		return fmt.Errorf("got %d bytes, but the pinned file has %d", n, f.Size)
	}
	if sum := hex.EncodeToString(h.Sum(nil)); sum != f.SHA256 {
		return fmt.Errorf("its SHA-256 is %s, but the pinned file's is %s", sum, f.SHA256)
	}
	return nil
}

// demoCause prefers the stall (or the caller's cancel cause) over the
// transport error it caused.
func demoCause(ctx context.Context, err error) error {
	if cause := context.Cause(ctx); cause != nil {
		return cause
	}
	return err
}

// downloadProgress resets the stall timer on every write and reports each
// further tenth of the file.
type downloadProgress struct {
	sink       events.Sink
	name       string
	size, done int64
	tenth      int64
	timer      *time.Timer
}

func (p *downloadProgress) Write(b []byte) (int, error) {
	p.timer.Reset(demoStall)
	p.done += int64(len(b))
	if t := p.done * 10 / max(p.size, 1); t > p.tenth && p.done <= p.size {
		p.tenth = t
		pct := float64(t * 10)
		p.sink.Emit(events.Progress{ID: StepDemoDownload, Text: fmt.Sprintf("downloading %s (%d MB)", p.name, p.size>>20), Pct: &pct})
	}
	return len(b), nil
}

// prepareDemoCSV returns the CSV to load from the downloaded files, and a
// cleanup func for the temp files it made; names are the files' names
// for messages. One file is resolved as
// load-phenotype resolves --file (extracted from its archive, or used in
// place); several are each extracted in turn and appended to one merged
// CSV, so at most one extraction is on disk beside the merge.
func prepareDemoCSV(ctx context.Context, sink events.Sink, c *cache.Cache, paths, names []string) (string, func() error, error) {
	resolve := func(p string) (string, func() error, error) {
		in, cleanup, err := phenoinput.Resolve(ctx, p, phenoinput.Options{MkdirTemp: c.TempDir})
		if err != nil {
			return "", nil, err
		}
		for _, w := range in.Warnings {
			sink.Emit(events.Warning{ID: StepDemoPrepare, Text: w})
		}
		return in.CSV, cleanup, nil
	}
	if len(paths) == 1 {
		return resolve(paths[0])
	}

	dir, err := c.TempDir("demo-*")
	if err != nil {
		return "", nil, err
	}
	cleanup := func() error { return os.RemoveAll(dir) }
	merged := filepath.Join(dir, "allConcepts.csv")
	err = func() error {
		out, err := os.OpenFile(merged, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		if err != nil {
			return err
		}
		m := &csvMerge{w: bufio.NewWriterSize(out, 1<<20), last: '\n'}
		for i, p := range paths {
			sink.Emit(events.Progress{ID: StepDemoPrepare, Text: "merging " + names[i]})
			src, done, err := resolve(p)
			if err == nil {
				err = m.append(ctx, src, names[i])
				err = errors.Join(err, done())
			}
			if err != nil {
				_ = out.Close()
				return err
			}
		}
		if err := m.w.Flush(); err != nil {
			_ = out.Close()
			return err
		}
		return out.Close()
	}()
	if err != nil {
		_ = cleanup()
		return "", nil, err
	}
	return merged, cleanup, nil
}

// csvMerge concatenates CSVs that share a header: the first one's header
// line, then every file's rows. Headers are compared field by field after
// parsing, so quoting may differ (Synthea's header is unquoted, the others'
// quoted).
type csvMerge struct {
	w      *bufio.Writer
	header []string
	first  string // the file header came from
	last   byte   // the last byte written; '\n' before the first file
}

func (m *csvMerge) append(ctx context.Context, path, name string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	r := bufio.NewReaderSize(ctxReader{ctx, f}, 1<<20)
	line, err := r.ReadString('\n')
	if err != nil && (err != io.EOF || line == "") {
		if err == io.EOF {
			return fmt.Errorf("%s is empty", name)
		}
		return fmt.Errorf("reading %s: %w", name, err)
	}
	header, err := csv.NewReader(strings.NewReader(line)).Read()
	if err != nil {
		return fmt.Errorf("%s: reading its header: %w", name, err)
	}
	header[0] = strings.TrimPrefix(header[0], "\uFEFF")
	if m.header == nil {
		m.header, m.first = header, name
		if err := m.write(strings.TrimPrefix(line, "\uFEFF")); err != nil {
			return err
		}
	} else if !slices.Equal(header, m.header) {
		return fmt.Errorf("%s's header %q doesn't match %s's %q, so the files can't be merged", name, header, m.first, m.header)
	}
	if m.last != '\n' {
		if err := m.write("\n"); err != nil {
			return err
		}
	}
	if _, err := r.WriteTo(m); err != nil {
		return fmt.Errorf("merging %s: %w", name, err)
	}
	return nil
}

// Write writes rows, remembering their last byte.
func (m *csvMerge) Write(b []byte) (int, error) {
	if len(b) > 0 {
		m.last = b[len(b)-1]
	}
	return m.w.Write(b)
}

func (m *csvMerge) write(s string) error {
	_, err := m.Write([]byte(s))
	return err
}
