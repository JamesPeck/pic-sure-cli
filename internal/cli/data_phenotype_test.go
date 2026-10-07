package cli

import (
	"context"
	"errors"
	"testing"

	"github.com/JamesPeck/pic-sure-cli/internal/exitcode"
	"github.com/JamesPeck/pic-sure-cli/internal/ops"
)

func TestPhenotypeRerunHint(t *testing.T) {
	auto := phenotypeArgs{file: "/data/my pheno.csv", dictionary: ops.DictionaryAuto, heap: 1024}
	custom := phenotypeArgs{file: "/data/p.csv", entry: "x's.csv", dictionary: ops.DictionaryCustom,
		datasets: "/d/datasets.csv", concepts: "/d/c.zip",
		facets: ops.FacetOptions{Categories: "/d/cat.csv", Facets: "/d/f.csv", Concepts: "/d/fc.csv"}}
	noWeights := custom
	noWeights.skipWeights, noWeights.facets = true, ops.FacetOptions{}
	dictErr := func(step string) error {
		return &ops.PhenotypeDictionaryError{Step: step, Err: errors.New("boom")}
	}
	const pic = "pic-sure --stack '/st ack' "
	cases := []struct {
		name string
		p    phenotypeArgs
		err  error
		want string
	}{
		{"hpds", auto, errors.New("boom"),
			"boom; to retry the load, run: " + pic + "data load-phenotype --file '/data/my pheno.csv' --heap 1024"},
		{"hpds custom", custom, exitcode.Precondition("boom"),
			"boom; to retry the load, run: " + pic + "data load-phenotype --file /data/p.csv --entry 'x'\\''s.csv' --dictionary custom " +
				"--datasets /d/datasets.csv --concepts /d/c.zip --facets-categories /d/cat.csv --facets /d/f.csv --facet-concepts /d/fc.csv"},
		{"usage", auto, exitcode.Usage("boom"), "boom"},
		{"hydrate", auto, dictErr(ops.StepColumnMeta),
			"dictionary step columnmeta failed: boom. HPDS has the new data (phenotype:abc); to finish the load, run: " +
				pic + "dictionary hydrate --clear --heap 1024 && " + pic + "dictionary weights"},
		{"concepts", custom, dictErr(ops.StepConcepts),
			"dictionary step concepts failed: boom. HPDS has the new data (phenotype:abc); to finish the load, run: " +
				pic + "dictionary load-csv --datasets /d/datasets.csv --concepts /d/c.zip --clear && " +
				pic + "dictionary load-facets --categories /d/cat.csv --facets /d/f.csv --concepts /d/fc.csv && " + pic + "dictionary weights"},
		{"facets", custom, dictErr(ops.StepFacets),
			"dictionary step facets failed: boom. HPDS has the new data (phenotype:abc); to finish the load, run: " +
				pic + "dictionary load-facets --categories /d/cat.csv --facets /d/f.csv --concepts /d/fc.csv && " + pic + "dictionary weights"},
		{"no weights", noWeights, dictErr(ops.StepDatasets),
			"dictionary step datasets failed: boom. HPDS has the new data (phenotype:abc); to finish the load, run: " +
				pic + "dictionary load-csv --datasets /d/datasets.csv --concepts /d/c.zip --clear"},
		{"weights", auto, dictErr(ops.StepWeights),
			"dictionary step weights failed: boom. HPDS has the new data (phenotype:abc); to finish the load, run: " + pic + "dictionary weights"},
		{"refresh", auto, dictErr(ops.StepDictionaryRefresh),
			"dictionary step dictionary-refresh failed: boom. HPDS has the new data (phenotype:abc); to finish the load, run: " + pic + "dictionary weights"},
		{"refresh without weights", noWeights, dictErr(ops.StepDictionaryRefresh),
			"dictionary step dictionary-refresh failed: boom. HPDS has the new data (phenotype:abc); to finish the load, run: " +
				pic + "dictionary load-csv --datasets /d/datasets.csv --concepts /d/c.zip --clear"},
		{"unknown step", auto, dictErr("new-step"),
			"dictionary step new-step failed: boom. HPDS has the new data (phenotype:abc); to finish the load, run: " +
				pic + "dictionary hydrate --clear --heap 1024 && " + pic + "dictionary weights"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := c.p.rerunHint("/st ack", "phenotype:abc", c.err)
			if got.Error() != c.want {
				t.Errorf("got  %s\nwant %s", got, c.want)
			}
			if exitcode.FromError(got) != exitcode.FromError(c.err) {
				t.Errorf("exit %d, want %d", exitcode.FromError(got), exitcode.FromError(c.err))
			}
		})
	}
	interrupted := &ops.PhenotypeDictionaryError{Step: ops.StepHydrate, Interrupted: true, Err: context.Canceled}
	if got := auto.rerunHint("/st", "phenotype:abc", interrupted); exitcode.FromError(got) != exitcode.CodeInterrupted {
		t.Errorf("an interrupted dictionary step exits %d: %v", exitcode.FromError(got), got)
	}
}
