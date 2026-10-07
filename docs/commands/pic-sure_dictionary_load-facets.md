# pic-sure dictionary load-facets

Load facet categories, facets and facet concepts

Load facet categories, facets and facet concepts from CSV, in that order.
The stack must be up: the operation runs a dictionary-etl container on the
stack's data network, sends it the data from short-lived curl containers,
then removes it, touches dict.update_info and restarts dictionary-api.

```
pic-sure dictionary load-facets --categories FILE --facets FILE --concepts FILE [flags]
```

## Flags

```
      --categories string   the facet categories CSV
      --concepts string     the facet concepts CSV
      --facets string       the facets CSV
```

The [global flags](pic-sure.md#flags) apply too.

See also: [`pic-sure dictionary`](pic-sure_dictionary.md)
