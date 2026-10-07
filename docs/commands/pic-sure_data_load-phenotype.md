# pic-sure data load-phenotype

Load phenotype data into HPDS, then the dictionary

Replace the stack's HPDS phenotype data with a CSV in HPDS's allConcepts
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
(as `dictionary load-csv`), and the facets if --facets-categories,
--facets and --facet-concepts are all given (as `dictionary load-facets`).
Both replace the dictionary's contents. Last, the search weights are
recomputed, unless --skip-weights. If a dictionary step fails, HPDS keeps
the new data and the error gives the dictionary commands that finish the
load.

```
pic-sure data load-phenotype (--file F [--entry E] | --input-dir D) [flags]
```

## Flags

```
      --concepts FILE            custom dictionary: a zip FILE of concepts_*.csv
      --datasets FILE            custom dictionary: the datasets CSV FILE
      --dictionary SOURCE        dictionary SOURCE: auto (built from the loaded data) or custom (default "auto")
      --entry ENTRY              the CSV ENTRY to load from an archive with several
      --facet-concepts FILE      custom dictionary: the facet concepts CSV FILE
      --facets FILE              custom dictionary: the facets CSV FILE
      --facets-categories FILE   custom dictionary: the facet categories CSV FILE
      --file FILE                phenotype CSV, or a tar.gz, gzip or zip FILE holding one
      --heap MB                  JVM heap in MB of the loader and of the auto dictionary's CreateColumnmetaCSV (default 4096; 8000 for the --input-dir loader)
      --input-dir DIR            DIR of phenotype CSVs for the sequential loader
      --skip-weights             skip recomputing the search weights; text search finds no new concepts until pic-sure dictionary weights runs
```

The [global flags](pic-sure.md#flags) apply too.

See also: [`pic-sure data`](pic-sure_data.md)
