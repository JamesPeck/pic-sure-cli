package ops

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/JamesPeck/pic-sure-cli/internal/cache"
	"github.com/JamesPeck/pic-sure-cli/internal/catalog"
	"github.com/JamesPeck/pic-sure-cli/internal/docker"
	"github.com/JamesPeck/pic-sure-cli/internal/events"
	"github.com/JamesPeck/pic-sure-cli/internal/exitcode"
	"github.com/JamesPeck/pic-sure-cli/internal/sql"
	"github.com/JamesPeck/pic-sure-cli/internal/stack"
	"github.com/JamesPeck/pic-sure-cli/internal/steps"
)

// Dictionary step IDs, for --skip-step.
const (
	StepColumnMeta        = "columnmeta"
	StepHydrate           = "hydrate"
	StepDictionaryClear   = "dictionary-clear"
	StepDatasets          = "datasets"
	StepConcepts          = "concepts"
	StepFacets            = "facets"
	StepFacetConfig       = "facet-config"
	StepWeights           = "weights"
	StepDictionaryRefresh = "dictionary-refresh"
)

const (
	dictionaryAPI = "dictionary-api"
	// dictionaryETLAlias is the ETL container's name on the stack's data
	// network, where the curl containers reach it.
	dictionaryETLAlias = "dictionaryetl"
	dictionaryETLURL   = "http://" + dictionaryETLAlias + ":8086"
	// DefaultColumnMetaHeap is CreateColumnmetaCSV's heap in MB, as in AIO.
	DefaultColumnMetaHeap = 4096
	// defaultWeightsFile is the weights file in the pic-sure tree.
	defaultWeightsFile = "services/picsure-dictionary/dictionaryweights/weights.csv"
)

var (
	// dictionaryETLReady bounds the wait for the ETL's actuator health to
	// be UP; dictionaryETLPoll is how often it is asked.
	dictionaryETLReady = 120 * time.Second
	dictionaryETLPoll  = 2 * time.Second
	// dictionaryAPIWait bounds the wait for dictionary-api to be healthy
	// again after its restart; dictionaryAPIPoll is how often compose ps
	// is asked.
	dictionaryAPIWait = 5 * time.Minute
	dictionaryAPIPoll = 2 * time.Second
)

// Dictionary runs the dictionary operations (§9.6) on one stack. Its steps
// share one dictionary-etl container, started by the first step that needs
// it; Close removes it, so callers defer Close. Other operations (the
// phenotype and demo loads) concatenate its steps with their own.
type Dictionary struct {
	d     *Deps
	st    *stack.Stack
	cfg   *stack.Config
	sec   *stack.Secrets
	state *stack.State

	etl     string // the ETL container's name once started
	scanned string // the ETL container whose log Close has scanned
	closers []io.Closer
}

// NewDictionary returns the dictionary operations for st. d.Compose must
// be set; state is state.json as the command loaded it.
func NewDictionary(d *Deps, st *stack.Stack, cfg *stack.Config, sec *stack.Secrets, state *stack.State) *Dictionary {
	return &Dictionary{d: d, st: st, cfg: cfg, sec: sec, state: state}
}

// Close removes the ETL container, if one was started, and closes the
// inputs the steps opened. It works after ctx is cancelled. Before the
// removal it scans the ETL's log for errors the ETL logged but didn't
// report (scanETLLog).
func (x *Dictionary) Close(ctx context.Context) error {
	var errs []error
	if x.etl != "" {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
		defer cancel()
		if x.scanned != x.etl {
			x.scanned = x.etl
			x.scanETLLog(ctx)
		}
		if err := x.d.Docker.Rm(ctx, x.etl, true); err != nil {
			errs = append(errs, fmt.Errorf("removing %s: %w", x.etl, err))
		} else {
			x.etl = ""
		}
	}
	for _, c := range x.closers {
		errs = append(errs, c.Close())
	}
	x.closers = nil
	return errors.Join(errs...)
}

// HydrateOptions are `dictionary hydrate`'s flags.
type HydrateOptions struct {
	IncludeDatasetFacets bool
	Clear                bool
	// Heap is CreateColumnmetaCSV's heap in MB; zero means
	// DefaultColumnMetaHeap.
	Heap int
}

// HydrateSteps builds the dictionary from the HPDS data. With local data,
// `columnmeta` first runs CreateColumnmetaCSV over hpds-data; with a shared
// data set, which is read-only, it only checks the set has the
// columnMeta.csv its publisher generated. `hydrate` then posts
// /load/initialize, with the request AIO's `etl.sh hydrate_dictionary`
// sends.
func (x *Dictionary) HydrateSteps(opts HydrateOptions) []steps.Step {
	columnMeta := steps.Step{ID: StepColumnMeta, Title: "Generate columnMeta.csv from the HPDS data", Apply: x.columnMeta(opts)}
	req := map[string]any{"includeDefaultFacets": opts.IncludeDatasetFacets, "clearDatabase": opts.Clear}
	if x.shared() {
		columnMeta.Title = "Check the shared data set's columnMeta.csv"
		// The data volume is read-only, so the error report goes to /tmp.
		req["errorDirectory"] = "/tmp/columnMetaErrors.csv"
	}
	return []steps.Step{columnMeta, {
		ID:    StepHydrate,
		Title: "Load the dictionary from columnMeta.csv",
		Apply: func(ctx context.Context, sink events.Sink) error {
			// The ETL answers "Success" even when it can't read the file.
			if err := x.checkColumnMeta(ctx, sink, StepHydrate); err != nil {
				return err
			}
			body, err := json.Marshal(req)
			if err != nil {
				return err
			}
			resp, err := x.request(ctx, sink, StepHydrate, "POST", "/load/initialize", "application/json", bytes.NewReader(body))
			if err != nil {
				return err
			}
			// The ETL answers 200 even when it didn't load: "This task
			// is already running", or a complaint about the request.
			if strings.TrimSpace(resp) != "Success" {
				return fmt.Errorf("dictionary-etl didn't hydrate: %s", strings.TrimSpace(resp))
			}
			// It also answers "Success" after logging a failed load. An
			// empty dictionary afterwards catches that for a first or
			// --clear hydrate; over existing concepts it can't be told.
			return x.requireConcepts(ctx, sink, StepHydrate)
		},
	}}
}

func (x *Dictionary) columnMeta(opts HydrateOptions) func(context.Context, events.Sink) error {
	return func(ctx context.Context, sink events.Sink) error {
		// Fail before the slow part if the dictionary can't be written.
		if _, err := x.dictionaryDB(ctx); err != nil {
			return err
		}
		if x.shared() {
			return x.checkColumnMeta(ctx, sink, StepColumnMeta)
		}
		vol, err := x.dataVolume(ctx)
		if err != nil {
			return err
		}
		image, err := x.image(ctx, "pic-sure-hpds-etl")
		if err != nil {
			return err
		}
		heap := opts.Heap
		if heap == 0 {
			heap = DefaultColumnMetaHeap
		}
		code, err := x.run(ctx, sink, StepColumnMeta, docker.RunOpts{
			Image:   image,
			User:    "0:0",
			Network: "none",
			Mounts:  []docker.Mount{{Source: vol, Target: "/opt/local/hpds"}},
			Env: []string{
				"JAVA_OPTS=-Dlogback.log.level=INFO",
				"HEAPSIZE=" + strconv.Itoa(heap),
				"LOADER_NAME=CreateColumnmetaCSV",
			},
		}, "columnmeta")
		if err != nil {
			return err
		}
		if code != 0 {
			return fmt.Errorf("CreateColumnmetaCSV exited %d", code)
		}
		return nil
	}
}

// LoadCSVOptions are `dictionary load-csv`'s flags. The paths are absolute.
type LoadCSVOptions struct {
	Datasets, Concepts string
	Clear              bool
	// TempDir makes the directory the concepts are split into, which is
	// removed afterwards; nil means os.MkdirTemp in the system's.
	TempDir func(pattern string) (string, error)
}

// LoadCSVSteps loads a custom dictionary: with Clear, `dictionary-clear`
// empties it; `datasets` sends the datasets file, and `concepts` sends each
// dataset's concepts, split out of the zip's concepts_*.csv files by exact
// dataset_ref. It reads and checks both inputs first, so a bad file is a
// usage error before anything is cleared.
func (x *Dictionary) LoadCSVSteps(opts LoadCSVOptions) ([]steps.Step, error) {
	in, err := openCSVLoad(opts.Datasets, opts.Concepts)
	if err != nil {
		return nil, err
	}
	x.closers = append(x.closers, in)
	var list []steps.Step
	if opts.Clear {
		list = append(list, steps.Step{
			ID:    StepDictionaryClear,
			Title: "Clear the dictionary",
			Apply: func(ctx context.Context, sink events.Sink) error {
				_, err := x.request(ctx, sink, StepDictionaryClear, "DELETE", "/clear/all", "", nil)
				return err
			},
		})
	}
	return append(list, steps.Step{
		ID:    StepDatasets,
		Title: "Load the datasets",
		Apply: func(ctx context.Context, sink events.Sink) error {
			_, err := x.request(ctx, sink, StepDatasets, "PUT", "/api/dataset/csv", "text/plain", bytes.NewReader(in.datasets))
			return err
		},
	}, steps.Step{
		ID:    StepConcepts,
		Title: "Load the concepts",
		Apply: func(ctx context.Context, sink events.Sink) error {
			if refs := in.unmatched(); len(refs) > 0 {
				quoted := make([]string, len(refs))
				for i, r := range refs {
					quoted[i] = strconv.Quote(r)
				}
				sink.Emit(events.Warning{ID: StepConcepts, Text: fmt.Sprintf("skipping concepts whose dataset_ref %s doesn't list: %s",
					filepath.Base(in.datasetsPath), strings.Join(quoted, ", "))})
			}
			mkdir := opts.TempDir
			if mkdir == nil {
				mkdir = func(pattern string) (string, error) { return os.MkdirTemp("", pattern) }
			}
			dir, err := mkdir("dictionary-concepts-*")
			if err != nil {
				return err
			}
			defer func() { _ = os.RemoveAll(dir) }()
			files, err := in.split(dir)
			if err != nil {
				return fmt.Errorf("splitting %s: %w", in.conceptsPath, err)
			}
			for _, ref := range in.refs {
				if files[ref] == "" {
					sink.Emit(events.Warning{ID: StepConcepts, Text: fmt.Sprintf("dataset %q has no concepts", ref)})
					continue
				}
				sink.Emit(events.Progress{ID: StepConcepts, Text: fmt.Sprintf("%s: %d concepts", ref, in.counts[ref])})
				if err := x.putFile(ctx, sink, StepConcepts, "/api/concept/csv?datasetRef="+url.QueryEscape(ref), files[ref]); err != nil {
					return fmt.Errorf("dataset %q: %w", ref, err)
				}
			}
			return nil
		},
	}), nil
}

// putFile PUTs a CSV file to the ETL.
func (x *Dictionary) putFile(ctx context.Context, sink events.Sink, step, path, file string) error {
	f, err := os.Open(file)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	_, err = x.request(ctx, sink, step, "PUT", path, "text/plain", f)
	return err
}

// FacetOptions are `dictionary load-facets`'s files, absolute.
type FacetOptions struct {
	Categories, Facets, Concepts string
}

// FacetSteps loads facet categories, facets and facet concepts, in that
// order, as one step, `facets`. It reads and checks the three files first
// (checkFacetFiles), so a file the ETL would reject is a usage error before
// anything changes.
func (x *Dictionary) FacetSteps(opts FacetOptions) ([]steps.Step, error) {
	if err := checkFacetFiles(opts); err != nil {
		return nil, err
	}
	return []steps.Step{{
		ID:    StepFacets,
		Title: "Load the facets",
		Apply: func(ctx context.Context, sink events.Sink) error {
			for _, put := range []struct{ file, path string }{
				{opts.Categories, "/api/facet/category/csv"},
				{opts.Facets, "/api/facet/csv"},
				{opts.Concepts, "/api/facet/concept/csv"},
			} {
				sink.Emit(events.Progress{ID: StepFacets, Text: "loading " + filepath.Base(put.file)})
				in, err := openWithoutBOM(put.file)
				if err != nil {
					return err
				}
				_, err = x.request(ctx, sink, StepFacets, "PUT", put.path, "text/plain", in)
				_ = in.Close()
				if err != nil {
					return err
				}
			}
			return nil
		},
	}}, nil
}

// FacetConfigSteps posts a facet loader configuration (JSON, as AIO's
// demo-data/facet_loader_configuration.json) to /api/facet/loader/load, as
// one step, `facet-config`.
func (x *Dictionary) FacetConfigSteps(config []byte) []steps.Step {
	return []steps.Step{{
		ID:    StepFacetConfig,
		Title: "Load the facet configuration",
		Apply: func(ctx context.Context, sink events.Sink) error {
			resp, err := x.request(ctx, sink, StepFacetConfig, "POST", "/api/facet/loader/load", "application/json", bytes.NewReader(config))
			if err != nil {
				return err
			}
			var r struct {
				CategoriesCreated, CategoriesUpdated, FacetsCreated, FacetsUpdated *int
			}
			err = json.Unmarshal([]byte(resp), &r)
			if err != nil || r.CategoriesCreated == nil || r.CategoriesUpdated == nil || r.FacetsCreated == nil || r.FacetsUpdated == nil {
				return fmt.Errorf("dictionary-etl's answer to the facet configuration isn't its result: %s", truncate(strings.TrimSpace(resp), 500))
			}
			sink.Emit(events.Progress{ID: StepFacetConfig, Text: fmt.Sprintf("%d facet categories created, %d updated; %d facets created, %d updated",
				*r.CategoriesCreated, *r.CategoriesUpdated, *r.FacetsCreated, *r.FacetsUpdated)})
			return nil
		},
	}}
}

// WeightsOptions are `dictionary weights`'s inputs.
type WeightsOptions struct {
	// Weights is the weights file, absolute; empty means the pic-sure
	// tree's, from components.pic-sure.source or the cache.
	Weights string
	Cache   *cache.Cache
}

// WeightsSteps recomputes the search weights with the dictionary-weights
// image the reactor builds, as one step, `weights`.
func (x *Dictionary) WeightsSteps(opts WeightsOptions) []steps.Step {
	return []steps.Step{{
		ID:    StepWeights,
		Title: "Recompute the search weights",
		Apply: func(ctx context.Context, sink events.Sink) error {
			file, err := x.weightsFile(opts)
			if err != nil {
				return err
			}
			if _, err := x.dictionaryDB(ctx); err != nil {
				return err
			}
			image, err := x.image(ctx, "dictionary-weights")
			if err != nil {
				return err
			}
			if err := x.markRestart(); err != nil {
				return err
			}
			code, err := x.run(ctx, sink, StepWeights, docker.RunOpts{
				Image:   image,
				Network: x.dataNetwork(),
				Env:     x.dbEnv(),
				Mounts:  []docker.Mount{{Source: file, Target: "/weights.csv", ReadOnly: true}},
			}, "dictionary-weights")
			if err != nil {
				return err
			}
			if code != 0 {
				return fmt.Errorf("dictionary-weights exited %d", code)
			}
			return nil
		},
	}}
}

func (x *Dictionary) weightsFile(opts WeightsOptions) (string, error) {
	if opts.Weights != "" {
		return opts.Weights, nil
	}
	var root string
	if src := componentSource(x.cfg, catalog.PicSure); src != "" {
		root = src
		if !filepath.IsAbs(root) {
			root = filepath.Join(x.st.Dir, root)
		}
	} else {
		commit := x.state.Components[catalog.PicSure].Commit
		if commit == "" {
			return "", exitcode.Precondition("state.json records no commit of %s; run `pic-sure build`", catalog.PicSure)
		}
		var err error
		if root, err = opts.Cache.SourceDir(catalog.PicSure, commit); err != nil {
			return "", err
		}
	}
	file := filepath.Join(root, filepath.FromSlash(defaultWeightsFile))
	if _, err := os.Stat(file); err != nil {
		return "", exitcode.Precondition("the default weights file is missing (%v); pass --weights FILE, or run `pic-sure build` to fetch the pic-sure source", err)
	}
	return file, nil
}

// Preflight checks, without changing anything, what HydrateSteps and
// WeightsSteps need: a healthy dictionary-db, its password, the ETL and
// weights images and the weights file. A load that replaces the HPDS data
// first calls it, so a missing piece fails before the data is gone.
func (x *Dictionary) Preflight(ctx context.Context, weights WeightsOptions) error {
	if err := x.PreflightETL(ctx); err != nil {
		return err
	}
	if _, err := x.image(ctx, "dictionary-weights"); err != nil {
		return err
	}
	_, err := x.weightsFile(weights)
	return err
}

// PreflightETL is Preflight for a load that skips the weights: a healthy
// dictionary-db, its password and the ETL image.
func (x *Dictionary) PreflightETL(ctx context.Context) error {
	if _, err := x.dictionaryDB(ctx); err != nil {
		return err
	}
	if x.sec.DictionaryDBPassword == "" {
		return exitcode.Precondition("secrets.yaml has no dictionary_db_password; run `pic-sure up`")
	}
	_, err := x.image(ctx, "dictionary-etl")
	return err
}

// RefreshStep, ID "dictionary-refresh", ends every dictionary operation:
// it removes the ETL container, touches dict.update_info (the dictionary's
// last-updated time, which dictionary-dump serves), and restarts
// dictionary-api if it is running, waiting until it is healthy. The write
// steps record the restart in state.json's pending_restarts first, so if
// this step never runs, the next `up` restarts it.
func (x *Dictionary) RefreshStep() steps.Step {
	return steps.Step{
		ID:    StepDictionaryRefresh,
		Title: "Refresh dictionary-api",
		Apply: func(ctx context.Context, sink events.Sink) error {
			// Free the ETL's memory before the restart; if removing it
			// fails, the caller's deferred Close retries and reports it.
			_ = x.Close(ctx)
			db, err := x.dictionaryDB(ctx)
			if err != nil {
				return err
			}
			err = sql.ExecPostgres(ctx, x.d.Docker, x.dbTarget(db), "UPDATE dict.update_info SET last_updated = NOW()")
			if err != nil {
				return fmt.Errorf("touching dict.update_info: %w", err)
			}
			api, err := composeService(ctx, x.d, dictionaryAPI)
			if err != nil {
				return err
			}
			if api != nil && api.State == "running" {
				sink.Emit(events.Progress{ID: StepDictionaryRefresh, Text: "restarting dictionary-api"})
				out := events.NewLogWriter(sink, StepDictionaryRefresh, events.StreamStderr)
				err := x.d.Compose.Restart(ctx, out, dictionaryAPI)
				_ = out.Close()
				if err == nil {
					err = x.waitAPI(ctx)
				}
				if err != nil {
					return fmt.Errorf("restarting dictionary-api: %w; run `pic-sure restart dictionary-api`", err)
				}
			}
			state, err := x.st.LoadState()
			if err != nil {
				return err
			}
			state.PendingRestarts = slices.DeleteFunc(state.PendingRestarts, func(s string) bool { return s == dictionaryAPI })
			return x.st.SaveState(state)
		},
	}
}

// DictionaryHydrate is `dictionary hydrate`.
func DictionaryHydrate(ctx context.Context, d *Deps, st *stack.Stack, cfg *stack.Config, sec *stack.Secrets, state *stack.State, opts HydrateOptions, skip []string) error {
	x := NewDictionary(d, st, cfg, sec, state)
	return x.runSteps(ctx, x.HydrateSteps(opts), skip)
}

// DictionaryLoadCSV is `dictionary load-csv`.
func DictionaryLoadCSV(ctx context.Context, d *Deps, st *stack.Stack, cfg *stack.Config, sec *stack.Secrets, state *stack.State, opts LoadCSVOptions, skip []string) error {
	x := NewDictionary(d, st, cfg, sec, state)
	list, err := x.LoadCSVSteps(opts)
	if err != nil {
		return err
	}
	return x.runSteps(ctx, list, skip)
}

// DictionaryLoadFacets is `dictionary load-facets`.
func DictionaryLoadFacets(ctx context.Context, d *Deps, st *stack.Stack, cfg *stack.Config, sec *stack.Secrets, state *stack.State, opts FacetOptions, skip []string) error {
	x := NewDictionary(d, st, cfg, sec, state)
	list, err := x.FacetSteps(opts)
	if err != nil {
		return err
	}
	return x.runSteps(ctx, list, skip)
}

// DictionaryWeights is `dictionary weights`.
func DictionaryWeights(ctx context.Context, d *Deps, st *stack.Stack, cfg *stack.Config, sec *stack.Secrets, state *stack.State, opts WeightsOptions, skip []string) error {
	x := NewDictionary(d, st, cfg, sec, state)
	return x.runSteps(ctx, x.WeightsSteps(opts), skip)
}

// runSteps makes the ownership check, then runs list and the refresh step,
// then closes x.
func (x *Dictionary) runSteps(ctx context.Context, list []steps.Step, skip []string) (err error) {
	if _, err := CheckOwnership(ctx, x.d, x.st, x.cfg.Name); err != nil {
		return err
	}
	defer func() {
		if cerr := x.Close(ctx); cerr != nil {
			x.d.Sink.Emit(events.Warning{Text: cerr.Error()})
		}
	}()
	return steps.Run(ctx, x.d.Sink, append(list, x.RefreshStep()), steps.Options{Skip: skip})
}

// request sends one HTTP request to the ETL from a curl container on the
// data network, starting the ETL first if needed, and returns the response
// body. body, if not nil, goes to curl on stdin. A non-2xx answer is an
// error carrying the body.
func (x *Dictionary) request(ctx context.Context, sink events.Sink, step, method, path, contentType string, body io.Reader) (string, error) {
	if err := x.startETL(ctx, sink, step); err != nil {
		return "", err
	}
	curl, _ := catalog.LookupImage("curl")
	if err := x.pull(ctx, sink, step, curl.Ref); err != nil {
		return "", err
	}
	args := []string{"-sS", "--fail-with-body", "-X", method}
	if contentType != "" {
		args = append(args, "-H", "Content-Type: "+contentType)
	}
	if body != nil {
		args = append(args, "--data-binary", "@-")
	}
	args = append(args, dictionaryETLURL+path)
	var out, errOut bytes.Buffer
	code, err := x.run(ctx, sink, step, docker.RunOpts{
		Image: curl.Ref, Network: x.dataNetwork(), Args: args, Stdin: body, Stdout: &out, Stderr: &errOut,
	}, "dictionary-curl")
	if err != nil {
		return "", err
	}
	if code != 0 {
		msg := strings.TrimSpace(errOut.String())
		if b := strings.TrimSpace(out.String()); b != "" {
			msg += ": " + truncate(b, 500)
		}
		return "", fmt.Errorf("%s %s: %s", method, path, msg)
	}
	return out.String(), nil
}

// startETL starts the dictionary-etl container on the data network, as
// dictionaryetl, unless it is running, and waits until its actuator
// health is UP. It first removes any ETL container of this stack an
// earlier run left (killed, or whose removal failed), which would answer
// to the same alias. It marks dictionary-api for a restart, since every
// operation that starts the ETL writes the dictionary.
func (x *Dictionary) startETL(ctx context.Context, sink events.Sink, step string) error {
	if x.etl != "" {
		return nil
	}
	if _, err := x.dictionaryDB(ctx); err != nil {
		return err
	}
	if x.sec.DictionaryDBPassword == "" {
		return exitcode.Precondition("secrets.yaml has no dictionary_db_password; run `pic-sure up`")
	}
	image, err := x.image(ctx, "dictionary-etl")
	if err != nil {
		return err
	}
	vol, err := x.dataVolume(ctx)
	if err != nil {
		return err
	}
	if err := x.markRestart(); err != nil {
		return err
	}
	prefix := x.cfg.Name + "-dictionaryetl"
	leftover := func(c string) bool { return strings.HasPrefix(c, prefix+"-") }
	if err := RemoveHelperContainers(ctx, x.d, sink, step, x.st, x.cfg.Name, leftover); err != nil {
		return err
	}
	name, err := docker.UniqueName(prefix, x.d.Rand)
	if err != nil {
		return err
	}
	sink.Emit(events.Progress{ID: step, Text: "starting dictionary-etl"})
	// Set before the run, so Close removes a container a failed run left.
	x.etl = name
	_, err = x.d.Docker.Run(ctx, docker.RunOpts{
		Image:          image,
		Name:           name,
		Detach:         true,
		Network:        x.dataNetwork(),
		NetworkAliases: []string{dictionaryETLAlias},
		Labels:         x.st.Labels(x.cfg.Name),
		Env:            x.dbEnv(),
		Mounts:         []docker.Mount{{Source: vol, Target: "/opt/local/hpds", ReadOnly: x.shared()}},
	})
	if err != nil {
		return fmt.Errorf("starting dictionary-etl: %w", err)
	}
	return x.waitETL(ctx, sink, step)
}

// waitETL polls the ETL's /actuator/health from inside its container until
// it reports UP. It fails, showing the ETL's last log lines, when the
// container stops or dictionaryETLReady passes.
func (x *Dictionary) waitETL(ctx context.Context, sink events.Sink, step string) error {
	wait, cancel := context.WithTimeout(ctx, dictionaryETLReady)
	defer cancel()
	for {
		info, err := x.d.Docker.ContainerInspect(wait, x.etl)
		if err == nil && !info.Running {
			x.logTail(ctx, sink, step)
			return fmt.Errorf("dictionary-etl exited %d before it was ready", info.ExitCode)
		}
		if err == nil {
			var out bytes.Buffer
			code, _ := x.d.Docker.Exec(wait, docker.ExecOpts{
				Container: x.etl,
				Args:      []string{"wget", "-qO-", "http://localhost:8086/actuator/health"},
				Stdout:    &out,
			})
			if code == 0 && healthUp(out.Bytes()) {
				return nil
			}
		}
		select {
		case <-wait.Done():
			if ctx.Err() != nil {
				return context.Cause(ctx)
			}
			x.logTail(ctx, sink, step)
			if err != nil {
				return fmt.Errorf("dictionary-etl wasn't ready within %s: %w", dictionaryETLReady, err)
			}
			return fmt.Errorf("dictionary-etl wasn't ready within %s", dictionaryETLReady)
		case <-time.After(dictionaryETLPoll):
		}
	}
}

func healthUp(body []byte) bool {
	var h struct{ Status string }
	return json.Unmarshal(body, &h) == nil && h.Status == "UP"
}

// logTail shows the ETL's last 30 log lines as Log events.
func (x *Dictionary) logTail(ctx context.Context, sink events.Sink, step string) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	rc := x.d.Docker.Logs(ctx, x.etl, false)
	defer func() { _ = rc.Close() }()
	var lines []string
	sc := bufio.NewScanner(rc)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		lines = append(lines, sc.Text())
		if len(lines) > 30 {
			lines = lines[1:]
		}
	}
	for _, l := range lines {
		sink.Emit(events.Log{ID: step, Stream: events.StreamStderr, Line: l})
	}
}

// etlLogRecord matches the first line of a Spring Boot log record,
// capturing its level, the logger (abbreviated) and the message.
var etlLogRecord = regexp.MustCompile(`^\d{4}-\d\d-\d\d[T ]\S+\s+(TRACE|DEBUG|INFO|WARN|ERROR)\s.*?(\S+)\s+:\s(.*)$`)

// etlSwallowed reports whether a dictionary-etl log record is an error the
// ETL caught without failing the request (dictionary-etl c97a813).
// DictionaryLoaderService answers a hydrate with Success after logging
// whatever the load threw, as INFO, and logs nothing else but "Processing
// Studies"; ConceptService logs a failure to link concepts to their
// parents as an ERROR and carries on.
func etlSwallowed(level, logger, msg string) bool {
	switch logger {
	case "DictionaryLoaderService":
		return !strings.HasPrefix(msg, "Processing Studies:")
	case "ConceptService":
		return level == "ERROR"
	}
	return false
}

// etlLogLimit bounds one excerpt of the ETL's log.
const etlLogLimit = 200

// swallowedErrors finds the records etlSwallowed matches in an ETL log and
// returns each as an excerpt: its first line plus the lines up to the next
// record (a stack trace), at most etlLogLimit lines.
func swallowedErrors(r io.Reader) ([][]string, error) {
	var found [][]string
	in := false
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Text()
		if m := etlLogRecord.FindStringSubmatch(line); m != nil {
			logger := m[2][strings.LastIndex(m[2], ".")+1:]
			in = etlSwallowed(m[1], logger, m[3])
			if in {
				found = append(found, []string{line})
			}
			continue
		}
		if in && len(found[len(found)-1]) < etlLogLimit {
			found[len(found)-1] = append(found[len(found)-1], line)
		}
	}
	return found, sc.Err()
}

// scanETLLog warns about errors the ETL logged without failing the request
// (see etlSwallowed): the warning quotes the first, and the run log gets
// every excerpt (debug records, which only the file keeps at the default
// --log-level). The command's exit status doesn't change; a log it can't
// read is only noted in the run log.
func (x *Dictionary) scanETLLog(ctx context.Context) {
	rc := x.d.Docker.Logs(ctx, x.etl, false)
	found, err := swallowedErrors(rc)
	_ = rc.Close()
	if err != nil && x.d.Log != nil {
		x.d.Log.Debug("reading dictionary-etl's log", "container", x.etl, "err", err)
	}
	if len(found) == 0 {
		return
	}
	if x.d.Log != nil {
		for _, lines := range found {
			x.d.Log.Debug("dictionary-etl logged an error it didn't report", "container", x.etl, "excerpt", strings.Join(lines, "\n"))
		}
	}
	msg := etlLogRecord.FindStringSubmatch(found[0][0])[3]
	more := ""
	if len(found) > 1 {
		more = fmt.Sprintf(" (and %d more)", len(found)-1)
	}
	x.d.Sink.Emit(events.Warning{Text: fmt.Sprintf("dictionary-etl logged an error but reported success%s: %q. The dictionary may be incomplete; the run log has the full excerpt",
		more, truncate(msg, 300))})
}

// run runs a one-off container with a unique name, --rm and the stack's
// labels, sending what opts doesn't capture to Log events. It returns the
// container's exit code; the error is docker's own failure.
func (x *Dictionary) run(ctx context.Context, sink events.Sink, step string, opts docker.RunOpts, prefix string) (int, error) {
	name, err := docker.UniqueName(x.cfg.Name+"-"+prefix, x.d.Rand)
	if err != nil {
		return 0, err
	}
	opts.Name, opts.Remove, opts.Labels = name, true, x.st.Labels(x.cfg.Name)
	if opts.Stdout == nil {
		w := events.NewLogWriter(sink, step, events.StreamStdout)
		defer func() { _ = w.Close() }()
		opts.Stdout = w
	}
	if opts.Stderr == nil {
		w := events.NewLogWriter(sink, step, events.StreamStderr)
		defer func() { _ = w.Close() }()
		opts.Stderr = w
	}
	code, err := x.d.Docker.Run(ctx, opts)
	if err != nil {
		// The container may outlive an interrupted docker run.
		rmCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
		_ = x.d.Docker.Rm(rmCtx, name, true)
		cancel()
		return code, fmt.Errorf("running %s: %w", opts.Image, err)
	}
	return code, nil
}

// pull pulls a third-party image the stack's build didn't, if missing.
func (x *Dictionary) pull(ctx context.Context, sink events.Sink, step, ref string) error {
	ok, err := x.d.Docker.ImageExists(ctx, ref)
	if err != nil || ok {
		return err
	}
	sink.Emit(events.Progress{ID: step, Text: "pulling " + ref})
	w := events.NewLogWriter(sink, step, events.StreamStderr)
	defer func() { _ = w.Close() }()
	if err := x.d.Docker.Pull(ctx, ref, w); err != nil {
		return fmt.Errorf("pulling %s: %w", ref, err)
	}
	return nil
}

// image returns the reference of a built image the stack records, its dev
// build's while one is recorded, and checks it is present.
func (x *Dictionary) image(ctx context.Context, name string) (string, error) {
	img, _ := catalog.LookupImage(name)
	tag := x.state.DevImages[name]
	if tag == "" {
		tag = x.state.Images[name]
	}
	if tag == "" {
		return "", exitcode.Precondition("state.json records no %s image; run `pic-sure build`", name)
	}
	ref := img.Repository() + ":" + tag
	ok, err := x.d.Docker.ImageExists(ctx, ref)
	if err != nil {
		return "", err
	}
	if !ok {
		return "", exitcode.Precondition("image %s is missing; run `pic-sure build`", ref)
	}
	return ref, nil
}

// dataVolume returns the HPDS data volume: the stack's hpds-data, or the
// shared data set's. It must exist.
func (x *Dictionary) dataVolume(ctx context.Context) (string, error) {
	var name string
	if x.shared() {
		v, _ := catalog.LookupVolume("shared-hpds-data")
		name = v.DockerName(x.cfg.HPDS.SharedName)
	} else {
		v, _ := catalog.LookupVolume(hpdsDataVolume)
		name = v.DockerName(x.cfg.Name)
	}
	if _, err := x.d.Docker.VolumeInspect(ctx, name); errors.Is(err, docker.ErrNotFound) {
		return "", exitcode.Precondition("HPDS data volume %s doesn't exist; load data first (`pic-sure data`)", name)
	} else if err != nil {
		return "", err
	}
	return name, nil
}

// dictionaryDB returns the ID of the stack's dictionary-db container,
// which must be running and healthy.
func (x *Dictionary) dictionaryDB(ctx context.Context) (string, error) {
	svc, err := composeService(ctx, x.d, dictionaryDB)
	if err != nil {
		return "", err
	}
	if svc == nil || svc.State != "running" || svc.Health != "healthy" {
		return "", exitcode.Precondition("dictionary-db isn't running and healthy; run `pic-sure up`")
	}
	return svc.ID, nil
}

// markRestart records dictionary-api in state.json's pending_restarts.
func (x *Dictionary) markRestart() error {
	return (&upRestarts{st: x.st}).mark(dictionaryAPI)
}

func (x *Dictionary) shared() bool { return x.cfg.HPDS.Data == stack.HPDSShared }

// dataNetwork is the compose network the dictionary tier is on.
func (x *Dictionary) dataNetwork() string { return x.cfg.Name + "_data" }

// dbEnv is the dictionary database connection the ETL and weights images
// read, as compose gives it to dictionary-api.
func (x *Dictionary) dbEnv() []string {
	return []string{
		"POSTGRES_HOST=" + dictionaryDB,
		"POSTGRES_DB=dictionary",
		"POSTGRES_USER=picsure",
		"POSTGRES_PASSWORD=" + string(x.sec.DictionaryDBPassword),
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return strings.ToValidUTF8(s[:n], "") + "…"
}

// checkColumnMeta checks the HPDS data volume holds a non-empty
// columnMeta.csv, mounted read-only.
func (x *Dictionary) checkColumnMeta(ctx context.Context, sink events.Sink, step string) error {
	vol, err := x.dataVolume(ctx)
	if err != nil {
		return err
	}
	alpine, _ := catalog.LookupImage("alpine")
	code, err := x.run(ctx, sink, step, docker.RunOpts{
		Image:   alpine.Ref,
		Network: "none",
		Mounts:  []docker.Mount{{Source: vol, Target: "/data", ReadOnly: true}},
		Args:    []string{"test", "-s", "/data/columnMeta.csv"},
	}, "columnmeta-check")
	if err != nil {
		return err
	}
	if code == 0 {
		return nil
	}
	if x.shared() {
		return exitcode.Precondition("shared data set %s has no columnMeta.csv; publish it again from a stack that hydrated its dictionary", x.cfg.HPDS.SharedName)
	}
	return exitcode.Precondition("%s has no columnMeta.csv; run the %s step (don't skip it)", vol, StepColumnMeta)
}

// requireConcepts fails, showing the ETL's log, when the dictionary holds
// no concepts.
func (x *Dictionary) requireConcepts(ctx context.Context, sink events.Sink, step string) error {
	db, err := x.dictionaryDB(ctx)
	if err != nil {
		return err
	}
	rows, err := sql.QueryPostgres(ctx, x.d.Docker, x.dbTarget(db), "SELECT count(*) FROM dict.concept_node")
	if err != nil {
		return fmt.Errorf("counting the dictionary's concepts: %w", err)
	}
	if len(rows) == 1 && len(rows[0]) == 1 && rows[0][0] != "0" {
		return nil
	}
	x.logTail(ctx, sink, step)
	return errors.New("dictionary-etl reported success, but the dictionary has no concepts; its log is above")
}

// waitAPI waits until dictionary-api is healthy again after its restart.
// It polls rather than running `compose up --wait`, which would recreate
// the service if the stack's render had moved on since its last up.
func (x *Dictionary) waitAPI(ctx context.Context) error {
	wait, cancel := context.WithTimeout(ctx, dictionaryAPIWait)
	defer cancel()
	var lastErr error
	for {
		svc, err := composeService(wait, x.d, dictionaryAPI)
		if err != nil && wait.Err() == nil {
			lastErr = err // a call cut short by the deadline would hide the daemon's own error
		}
		switch {
		case err == nil && svc != nil && svc.State == "running" && svc.Health == "healthy":
			return nil
		case err == nil && (svc == nil || svc.State == "exited" || svc.State == "dead"):
			return errors.New("dictionary-api stopped after its restart; see `pic-sure logs dictionary-api`")
		}
		select {
		case <-wait.Done():
			if ctx.Err() != nil {
				return context.Cause(ctx)
			}
			if lastErr != nil {
				return fmt.Errorf("dictionary-api wasn't healthy within %s of its restart: %w", dictionaryAPIWait, lastErr)
			}
			return fmt.Errorf("dictionary-api wasn't healthy within %s of its restart", dictionaryAPIWait)
		case <-time.After(dictionaryAPIPoll):
		}
	}
}

func (x *Dictionary) dbTarget(container string) sql.PostgresTarget {
	return sql.PostgresTarget{Container: container, User: "picsure", Password: string(x.sec.DictionaryDBPassword), Database: "dictionary"}
}
