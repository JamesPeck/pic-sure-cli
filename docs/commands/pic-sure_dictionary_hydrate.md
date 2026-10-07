# pic-sure dictionary hydrate

Build the dictionary from the loaded HPDS data

Build the dictionary from the HPDS data. With local data, CreateColumnmetaCSV
first writes columnMeta.csv into the hpds-data volume; a shared data set is
read-only and must already hold one.
The stack must be up: the operation runs a dictionary-etl container on the
stack's data network, sends it the data from short-lived curl containers,
then removes it, touches dict.update_info and restarts dictionary-api.

```
pic-sure dictionary hydrate [flags]
```

## Flags

```
      --clear                    empty the dictionary first
      --heap MB                  CreateColumnmetaCSV's heap in MB (default 4096)
      --include-dataset-facets   also create the default facets
```

The [global flags](pic-sure.md#flags) apply too.

See also: [`pic-sure dictionary`](pic-sure_dictionary.md)
