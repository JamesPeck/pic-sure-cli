# pic-sure dictionary load-csv

Load a custom dictionary from CSV

Load a custom dictionary: the datasets CSV (with a ref column), then each
dataset's concepts from the concepts_*.csv files in the zip, matched on their
dataset_ref column.
The stack must be up: the operation runs a dictionary-etl container on the
stack's data network, sends it the data from short-lived curl containers,
then removes it, touches dict.update_info and restarts dictionary-api.

```
pic-sure dictionary load-csv --datasets FILE --concepts ZIP [flags]
```

## Flags

```
      --clear             empty the dictionary first
      --concepts string   a zip of concepts_*.csv files
      --datasets string   the datasets CSV
```

The [global flags](pic-sure.md#flags) apply too.

See also: [`pic-sure dictionary`](pic-sure_dictionary.md)
