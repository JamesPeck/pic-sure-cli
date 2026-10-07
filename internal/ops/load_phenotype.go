package ops

import (
	"context"
	"errors"
	"fmt"

	"github.com/JamesPeck/pic-sure-cli/internal/cache"
	"github.com/JamesPeck/pic-sure-cli/internal/events"
	"github.com/JamesPeck/pic-sure-cli/internal/exitcode"
	"github.com/JamesPeck/pic-sure-cli/internal/stack"
	"github.com/JamesPeck/pic-sure-cli/internal/steps"
)

// Dictionary sources for `data load-phenotype --dictionary`.
const (
	DictionaryAuto   = "auto"
	DictionaryCustom = "custom"
)

// PhenotypeOptions are `data load-phenotype`'s options.
type PhenotypeOptions struct {
	// Load is the HPDS load (042).
	Load PhenotypeLoadOptions
	// Dictionary is DictionaryAuto (hydrate from the loaded data) or
	// DictionaryCustom (Datasets and Concepts, and Facets if set).
	Dictionary         string
	Datasets, Concepts string
	// Facets is all three files or none.
	Facets      FacetOptions
	SkipWeights bool
	Cache       *cache.Cache
}

// PhenotypeDictionaryError is a `data load-phenotype` failure after HPDS
// has the new data: the dictionary step Step failed, or the run stopped
// there. Re-running the dictionary steps from Step finishes the load; the
// whole command would load HPDS again.
type PhenotypeDictionaryError struct {
	Step        string
	Interrupted bool
	Err         error
}

func (e *PhenotypeDictionaryError) Error() string {
	if e.Interrupted {
		return fmt.Sprintf("stopped at dictionary step %s: %v", e.Step, e.Err)
	}
	return fmt.Sprintf("dictionary step %s failed: %v", e.Step, e.Err)
}

func (e *PhenotypeDictionaryError) Unwrap() error { return e.Err }

// DataLoadPhenotype is `data load-phenotype --file` (§9.6): the HPDS load,
// then the dictionary (hydrated from the new data, or the custom CSVs and
// facets), then the search weights unless SkipWeights. It checks the
// custom dictionary's files and what the dictionary steps need before HPDS
// is touched. It returns the provenance marker the load wrote.
func DataLoadPhenotype(ctx context.Context, d *Deps, st *stack.Stack, cfg *stack.Config, sec *stack.Secrets, state *stack.State, opts PhenotypeOptions) (string, error) {
	if err := RefuseSharedHPDS(cfg); err != nil {
		return "", err
	}
	x := NewDictionary(d, st, cfg, sec, state)
	defer func() {
		if cerr := x.Close(ctx); cerr != nil {
			d.Sink.Emit(events.Warning{Text: cerr.Error()})
		}
	}()
	var list []steps.Step
	switch opts.Dictionary {
	case DictionaryAuto:
		list = x.HydrateSteps(HydrateOptions{Clear: true, Heap: opts.Load.HeapMB})
	case DictionaryCustom:
		var err error
		list, err = x.LoadCSVSteps(LoadCSVOptions{Datasets: opts.Datasets, Concepts: opts.Concepts, Clear: true, TempDir: opts.Load.MkdirTemp})
		if err != nil {
			return "", err
		}
		if opts.Facets != (FacetOptions{}) {
			list = append(list, x.FacetSteps(opts.Facets)...)
		}
	default:
		return "", exitcode.Usage("--dictionary must be %s or %s, not %q", DictionaryAuto, DictionaryCustom, opts.Dictionary)
	}
	weights := WeightsOptions{Cache: opts.Cache}
	if opts.SkipWeights {
		if err := x.PreflightETL(ctx); err != nil {
			return "", err
		}
	} else {
		if err := x.Preflight(ctx, weights); err != nil {
			return "", err
		}
		list = append(list, x.WeightsSteps(weights)...)
	}
	list = append(list, x.RefreshStep())

	dataset, err := LoadPhenotype(ctx, d, st, cfg, state, opts.Load)
	if err != nil {
		return "", err
	}
	if err := steps.Run(ctx, d.Sink, list, steps.Options{}); err != nil {
		var se *steps.Error
		if errors.As(err, &se) {
			return dataset, &PhenotypeDictionaryError{Step: se.Step, Interrupted: se.Interrupted, Err: se.Err}
		}
		return dataset, err
	}
	return dataset, nil
}
