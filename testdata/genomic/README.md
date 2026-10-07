# Synthetic genomic fixture

Invented data for testing `pic-sure data load-genomic` and genomic queries
(spec D28). Nothing here comes from a real person or a real genome: the
samples are `SYN_S01`–`SYN_S08`, the genes are `SYNTHA`–`SYNTHD`, and the
positions and frequencies are made up.

Everything except this README is written by
`internal/testfixtures/genomic`. Don't edit the files by hand; change the
generator and run

    go generate ./internal/testfixtures/genomic

`TestCheckedInFixtureIsCurrent` fails if the files drift from the generator.

## Files

| File | What |
|---|---|
| `chr21.vcf.gz` | 14 variants on chr21 (genes SYNTHA, SYNTHB), BGZF-compressed |
| `chr22.vcf` | 12 variants on chr22 (genes SYNTHC, SYNTHD), uncompressed |
| `vcfIndex.tsv` | The index the loaders read |
| `phenotype.csv` | Age and sex for patients 101–110, in the CSV loader's format |
| `expected.json` | Documented queries and the patients each must return |

## Loading it

The index here names the VCFs by bare file name. The loaders open the paths
in the index from inside their container, so for a real load write a copy
whose index uses absolute paths:

    go run ./internal/testfixtures/genomic/cmd/genomic-fixture -abs /some/dir
    pic-sure data load-genomic --partition fixture \
        --vcf-index /some/dir/vcfIndex.tsv --vcf-dir /some/dir
    pic-sure data load-phenotype --file /some/dir/phenotype.csv

`TestFixtureLoadsInHPDS` runs the three genomic loaders and the phenotype
CSV loader from a real image over it:

    PICSURE_HPDS_ETL_IMAGE=hms-dbmi/pic-sure-hpds-etl:<tag> \
        go test -run LoadsInHPDS ./internal/testfixtures/genomic

## The format HPDS expects

From the loaders in pic-sure `services/pic-sure-hpds/etl/.../genotype`
(`VcfIndexFileParser`, `NewVCFLoader`, `VCFWalker`,
`SplitChromosomeVcfLoader`) and the upstream genotype-load-example repo that
AIO's `hpds_geno_load.md` links to.

**vcfIndex.tsv.** Tab-separated. The first line is a header and is skipped.
The loader reads six columns by position; later columns
(`sample_relationship`, `related_sample_ids`) are ignored.

| Column | Meaning |
|---|---|
| `filename` | Path to the VCF as the loader container sees it |
| `chromosome` | Contig label; the loader only sorts index lines by it (`ALL` sorts first). Data is filed under the VCF's CHROM |
| `annotated` | `1` if INFO carries the annotation columns below |
| `gzip` | `1` for BGZF (`bgzip`), `0` for plain text. Plain `gzip` output is rejected: htsjdk's `BlockCompressedInputStream` reads only BGZF |
| `sample_ids` | Comma-separated VCF sample column names |
| `patient_ids` | Comma-separated HPDS patient IDs, paired with `sample_ids` by position |

**Contigs.** One contig per VCF: `SplitChromosomeVcfLoader` writes each file
into the directory of the first CHROM it reads, and HPDS stores data as
`<partition>/<contig>/`. CHROM is used as written (`chr21`), and variant
specs use the same name. Records must be sorted by position. No tabix index
is needed.

**FORMAT.** `GT` only. HPDS reads exactly three characters per sample, so
every genotype must be a three-character diploid call: `0/0`, `0/1`, `1/1`, `./.`.

**INFO.** Every token must be `key=value`: the loader pairs tokens up, so a
bare flag shifts every value after it. HPDS indexes each INFO key as a
filterable column. It removes every `>` from the `##INFO` line and takes
everything after the third comma as the description, so the attributes must
come in the order ID, Number, Type, Description. The fixture uses the annotation keys
PIC-SURE's genomic filters show: `Gene_with_variant`, `Variant_severity`,
`Variant_consequence_calculated`, `Variant_class`,
`Variant_frequency_in_gnomAD` (absent for novel variants) and
`Variant_frequency_as_text` (`Novel`, `Rare` below 1%, `Common`).

**Patient IDs.** `patient_ids` must be the same IDs as `PATIENT_NUM` in the
phenotype data, so genomic and phenotype filters intersect. Here samples
`SYN_S01`–`SYN_S08` are patients 101–108. Patients 109 and 110 have
phenotypes only, so a query that dropped its genomic filter would count them.

## Expected results

`expected.json` lists the genomic patients (101–108), the phenotype patients
(101–110) and a set of named queries with the patients each returns.
`genomicFilters` use the shape of a PIC-SURE query: `key` is an INFO column
or a variant spec, and `values` are the accepted values, or zygosities
(`0/1`, `1/1`) for a variant spec. A spec key selects the variants whose
stored spec, `chr21,33001877,C,T,SYNTHA,synonymous_variant`, starts with it,
so `chr21,33001877,C,T` works too. The results follow HPDS semantics:

- HPDS evaluates the genomic filters on each contig separately and unions
  the patients. Every filter must be satisfied on the same contig, so a
  query mixing a chr21 filter with a chr22 filter returns nobody.
- On a contig, INFO filters are ANDed per variant; a patient matches if they
  are heterozygous or homozygous for any variant that passes all of them.
- Values within one filter are ORed.
- A variant-spec filter keeps patients whose genotype for a variant it
  selects is one of its zygosities. `./.` never matches.
- Phenotype filters intersect with the genomic result.

Carriers by gene: SYNTHA 102, 103, 104 (104 homozygous); SYNTHB 105, 106;
SYNTHC 103, 107, 108 (107 homozygous); SYNTHD 108 (homozygous). Patient 101
carries nothing and has no-calls on the two `stop_gained` variants that 104
and 107 carry. So `gene-SYNTHA` returns 102, 103, 104, and `gene-SYNTHC-female`
returns 103, 107.
