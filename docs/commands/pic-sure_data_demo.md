# pic-sure data demo

Load a demo dataset (default: nhanes)

```text
Replace the stack's HPDS phenotype data with a public demo dataset, then
rebuild the dictionary from it with the demo facets and search weights.

The files come from hms-dbmi/pic-sure-public-datasets at a commit pinned in
pic-sure, through the stack's proxy, and are checked against pinned SHA-256
sums and kept in the cache's downloads/. "all" loads every dataset as one.
```

```
pic-sure data demo [nhanes|synthea|1000genomes|all] [flags]
```

## Flags

```
      --heap MB   loader JVM heap in MB (default 4096)
```

The [global flags](pic-sure.md#flags) apply too.

See also: [`pic-sure data`](pic-sure_data.md)
