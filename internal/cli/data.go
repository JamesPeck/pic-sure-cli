package cli

import "github.com/spf13/cobra"

func newDataCmd(a *App) *cobra.Command {
	demo := &cobra.Command{
		Use:       "demo [nhanes|synthea|1000genomes|all]",
		Short:     "Load a demo dataset (default: nhanes)",
		ValidArgs: []string{"nhanes", "synthea", "1000genomes", "all"},
		Args:      cobra.MatchAll(cobra.MaximumNArgs(1), cobra.OnlyValidArgs),
		RunE:      notImplemented("046"),
	}

	phenotype := &cobra.Command{
		Use:   "load-phenotype (--file F [--entry E] | --input-dir D)",
		Short: "Load phenotype data into HPDS, then the dictionary",
		Args:  cobra.NoArgs,
		RunE:  notImplemented("042"),
	}
	pf := phenotype.Flags()
	pf.String("file", "", "phenotype CSV, or a tar.gz, gzip or zip `FILE` holding one")
	pf.String("entry", "", "the CSV `ENTRY` to load from an archive with several")
	pf.String("input-dir", "", "`DIR` of phenotype files for the sequential loader (ticket 043)")
	pf.Int("heap", 0, "loader JVM heap in `MB`")
	pf.String("dictionary", "auto", "dictionary source: auto or custom (ticket 045)")
	pf.Bool("skip-weights", false, "skip recomputing the search weights")
	phenotype.MarkFlagsMutuallyExclusive("file", "input-dir")
	phenotype.MarkFlagsOneRequired("file", "input-dir")

	genomic := &cobra.Command{
		Use:   "load-genomic --partition P --vcf-index F",
		Short: "Load VCF data into a genomic partition",
		Args:  cobra.NoArgs,
		RunE:  notImplemented("049"),
	}
	gf := genomic.Flags()
	gf.String("partition", "", "genomic partition `NAME`")
	gf.String("vcf-index", "", "vcfIndex.tsv `FILE`")
	gf.String("vcf-dir", "", "`DIR` holding the VCFs the index names")
	gf.Bool("promote", false, "promote the loaded partitions into the live HPDS data")
	gf.Bool("enable-profile", false, "switch HPDS to the genomic profile")
	_ = genomic.MarkFlagRequired("partition")
	_ = genomic.MarkFlagRequired("vcf-index")

	return newGroup("data", "Load data into HPDS and the dictionary", demo, phenotype, genomic)
}
