# pic-sure shared-data list

List the published data sets

```
pic-sure shared-data list
```

## Examples

```
  # List the published data sets
  pic-sure shared-data list

  # Print their names from the JSON report
  pic-sure shared-data list --json | jq -r '.data_sets[].name'
```

The [global flags](pic-sure.md#flags) apply too.

See also: [`pic-sure shared-data`](pic-sure_shared-data.md)
