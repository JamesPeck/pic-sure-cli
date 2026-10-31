# pic-sure down

Stop the stack's containers (volumes are kept)

```
pic-sure down
```

## Examples

```
  # Stop the containers; the data is kept
  pic-sure down

  # Stop the stack in another directory, as JSON
  pic-sure --stack ~/picsure/demo down --json
```

The [global flags](pic-sure.md#flags) apply too.

See also: [`pic-sure`](pic-sure.md)
