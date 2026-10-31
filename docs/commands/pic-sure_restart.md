# pic-sure restart

Restart services (default: all)

```
pic-sure restart [SERVICE...]
```

## Examples

```
  # Restart every service
  pic-sure restart

  # Restart HPDS and the dictionary API
  pic-sure restart hpds dictionary-api

  # Restart HPDS, as JSON
  pic-sure restart hpds --json
```

The [global flags](pic-sure.md#flags) apply too.

See also: [`pic-sure`](pic-sure.md)
