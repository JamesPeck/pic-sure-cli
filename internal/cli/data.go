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
	f.Int("heap", 0, "each loader's JVM heap in `MB` (default 4096)")
	f.Bool("promote", false, "promote the loaded partition into the live HPDS data")
	f.Bool("all-partitions", false, "with --promote, promote every staged partition")
	f.Bool("backup", false, "with --promote, first copy the live genomic data into all-bak")
	f.Bool("enable-profile", false, "switch HPDS to the genomic profile (bch-dev)")
	_ = c.MarkFlagRequired("partition")
	_ = c.MarkFlagRequired("vcf-index")
	return c
}
