package cli

import "github.com/spf13/cobra"

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
	return &cobra.Command{
		Use:       "demo [nhanes|synthea|1000genomes|all]",
		Short:     "Load a demo dataset (default: nhanes)",
		ValidArgs: []string{"nhanes", "synthea", "1000genomes", "all"},
		Args:      cobra.MatchAll(cobra.MaximumNArgs(1), cobra.OnlyValidArgs),
		RunE:      notImplemented("046"),
	}
}

func newDataLoadPhenotypeCmd(a *App) *cobra.Command {
	c := &cobra.Command{
		Use:   "load-phenotype (--file F [--entry E] | --input-dir D)",
		Short: "Load phenotype data into HPDS, then the dictionary",
		Long: `Replace the stack's HPDS phenotype data with a CSV in HPDS's allConcepts
format. --file takes a CSV, or a gzip, tar, tar.gz or zip holding one; an
archive with several CSVs needs --entry. HPDS is stopped for the load and
started again once the loader has finished.

The previous load's files are removed before the loader runs, so if it
fails HPDS stays stopped with no phenotype data: fix the problem and run the
load again.`,
		Args: cobra.NoArgs,
		RunE: a.loadPhenotype,
	}
	f := c.Flags()
	f.String("file", "", "phenotype CSV, or a tar.gz, gzip or zip `FILE` holding one")
	f.String("entry", "", "the CSV `ENTRY` to load from an archive with several")
	f.String("input-dir", "", "`DIR` of phenotype files for the sequential loader (ticket 043)")
	f.Int("heap", 0, "loader JVM heap in `MB` (default 4096 for --file)")
	f.String("dictionary", "auto", "dictionary source: auto or custom (ticket 045)")
	f.Bool("skip-weights", false, "skip recomputing the search weights")
	c.MarkFlagsMutuallyExclusive("file", "input-dir")
	c.MarkFlagsOneRequired("file", "input-dir")
	return c
}

func newDataLoadGenomicCmd(a *App) *cobra.Command {
	c := &cobra.Command{
		Use:   "load-genomic --partition P --vcf-index F",
		Short: "Load VCF data into a genomic partition",
		Args:  cobra.NoArgs,
		RunE:  notImplemented("049"),
	}
	f := c.Flags()
	f.String("partition", "", "genomic partition `NAME`")
	f.String("vcf-index", "", "vcfIndex.tsv `FILE`")
	f.String("vcf-dir", "", "`DIR` holding the VCFs the index names")
	f.Bool("promote", false, "promote the loaded partitions into the live HPDS data")
	f.Bool("enable-profile", false, "switch HPDS to the genomic profile")
	_ = c.MarkFlagRequired("partition")
	_ = c.MarkFlagRequired("vcf-index")
	return c
}
