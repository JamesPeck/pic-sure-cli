# pic-sure dictionary weights

Recompute the search weights

```text
Recompute the dictionary's search weights with the dictionary-weights image.
The default weights file is the one in the stack's pic-sure source.
```

```
pic-sure dictionary weights [flags]
```

## Examples

```
  # Recompute the search weights from the default weights file
  pic-sure dictionary weights

  # Use your own weights file, as JSON
  pic-sure dictionary weights --weights weights.csv --json
```

## Flags

```
      --weights string   the weights CSV (default: the pic-sure source's)
```

The [global flags](pic-sure.md#flags) apply too.

See also: [`pic-sure dictionary`](pic-sure_dictionary.md)
