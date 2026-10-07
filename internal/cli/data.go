package cli

import (
	"github.com/spf13/cobra"

	"github.com/JamesPeck/pic-sure-cli/internal/ops"
)

// Each data subcommand has its own constructor because different tickets
// implement them: demo (046), load-phenotype (042, 043, 045) and
// load-genomic (049).

func newDataCmd(a *App) *cobra.Command {
	return newGroup("data", "Load data into HPDS and the dictionary",
		newDataDemoCmd(a),
		newDataLoadPhenotypeCmd(a),
		newDataLoadGenomicCmd(a),
	)
}

func newDataDemoCmd(a *App) *cobra.Command {
	c := &cobra.Command{
		Use:   "demo [nhanes|synthea|1000genomes|all]",
		Short: "Load a demo dataset (default: nhanes)",
		Long: `Replace the stack's HPDS phenotype data with a public demo dataset, then
rebuild the dictionary from it with the demo facets and search weights.

The files come from hms-dbmi/pic-sure-public-datasets at a commit pinned in
pic-sure, through the stack's proxy, and are checked against pinned SHA-256
sums and kept in the cache's downloads/. "all" loads every dataset as one.`,
		ValidArgs: ops.DemoDatasets(),
		Args:      cobra.MatchAll(cobra.MaximumNArgs(1), cobra.OnlyValidArgs),
		RunE:      a.dataDemo,
	}
	c.Flags().Int("heap", 0, "loader JVM heap in `MB` (default 4096)")
	return c
}

func newDataLoadPhenotypeCmd(a *App) *cobra.Command {
	c := &cobra.Command{
		Use:   "load-phenotype (--file F [--entry E] | --input-dir D)",
		Short: "Load phenotype data into HPDS, then the dictionary",
		Long: `Replace the stack's HPDS phenotype data with a CSV in HPDS's allConcepts
format. --file takes a CSV, or a gzip, tar, tar.gz or zip holding one; an
archive with several CSVs needs --entry. HPDS is stopped for the load and
started again once the loader has finished.

--input-dir instead takes a directory of such CSVs (and the loader's
optional config.json; other entries are left out), which the sequential
loader reads into a temporary volume while HPDS keeps running. HPDS is
then stopped, and the loader's output replaces its phenotype data.

With --file, the previous load's files are removed before the loader runs,
so if it fails HPDS stays stopped with no phenotype data: fix the problem and
run the load again. With --input-dir, a failed loader leaves HPDS as it was.

Then the dictionary is rebuilt. --dictionary auto (the default) builds it
from the loaded data. --dictionary custom loads --datasets and --concepts
(as ` + "`dictionary load-csv`" + `), and the facets if --facets-categories,
--facets and --facet-concepts are all given (as ` + "`dictionary load-facets`" + `).
Both replace the dictionary's contents. Last, the search weights are
recomputed, unless --skip-weights. If a dictionary step fails, HPDS keeps
the new data and the error gives the dictionary commands that finish the
load.`,
		Args: cobra.NoArgs,
		RunE: a.loadPhenotype,
	}
	f := c.Flags()
	f.String("file", "", "phenotype CSV, or a tar.gz, gzip or zip `FILE` holding one")
	f.String("entry", "", "the CSV `ENTRY` to load from an archive with several")
	f.String("input-dir", "", "`DIR` of phenotype CSVs for the sequential loader")
	f.Int("heap", 0, "JVM heap in `MB` of the loader and of the auto dictionary's CreateColumnmetaCSV (default 4096; 8000 for the --input-dir loader)")
	f.String("dictionary", ops.DictionaryAuto, "dictionary `SOURCE`: auto (built from the loaded data) or custom")
	f.String("datasets", "", "custom dictionary: the datasets CSV `FILE`")
	f.String("concepts", "", "custom dictionary: a zip `FILE` of concepts_*.csv")
	f.String("facets-categories", "", "custom dictionary: the facet categories CSV `FILE`")
	f.String("facets", "", "custom dictionary: the facets CSV `FILE`")
	f.String("facet-concepts", "", "custom dictionary: the facet concepts CSV `FILE`")
	f.Bool("skip-weights", false, "skip recomputing the search weights; text search finds no new concepts until `pic-sure dictionary weights` runs")
	c.MarkFlagsMutuallyExclusive("file", "input-dir")
	c.MarkFlagsOneRequired("file", "input-dir")
	return c
}

func newDataLoadGenomicCmd(a *App) *cobra.Command {
	c := &cobra.Command{
		Use:   "load-genomic --partition P --vcf-index F [--vcf-dir D] [--promote] [--enable-profile]",
		Short: "Load VCF data into a genomic partition",
		Long: `Load the VCFs a vcfIndex.tsv names into a genomic partition, staged in
the stack's genomic-staging volume, where it replaces any earlier load of
the partition. HPDS keeps running on its live data while the loaders run.

The index names each VCF by its host path, so every VCF must be under
--vcf-dir (default: the index's directory), which the loaders see at the
same path.

--promote then stops HPDS, copies the partition into its live genomic data
(every staged partition with --all-partitions, after copying the live data
into all-bak with --backup), and starts HPDS again. --enable-profile sets
hpds.profile to bch-dev, the profile that reads the genomic data, re-renders
the stack and starts HPDS on it.`,
		Args: cobra.NoArgs,
		RunE: a.loadGenomic,
	}
	f := c.Flags()
	f.String("partition", "", "genomic partition `NAME`: letters, digits, _ and -")
	f.String("vcf-index", "", "vcfIndex.tsv `FILE`")
	f.String("vcf-dir", "", "`DIR` holding the VCFs the index names (default: the index's directory)")
	f.Int("heap", 0, "each loader's JVM heap in `MB` (default 16000)")
	f.Bool("promote", false, "promote the loaded partition into the live HPDS data")
	f.Bool("all-partitions", false, "with --promote, promote every staged partition")
	f.Bool("backup", false, "with --promote, first copy the live genomic data into all-bak")
	f.Bool("enable-profile", false, "switch HPDS to the genomic profile (bch-dev)")
	_ = c.MarkFlagRequired("partition")
	_ = c.MarkFlagRequired("vcf-index")
	return c
}
