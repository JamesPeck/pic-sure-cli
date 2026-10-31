# pic-sure ps

List the stack's containers

```
pic-sure ps
```

## Examples

```
  # List the stack's containers
  pic-sure ps

  # Print each service's state and health from the JSON report
  pic-sure ps --json | jq -r '.services[] | "\(.service) \(.state) \(.health)"'
```

The [global flags](pic-sure.md#flags) apply too.

See also: [`pic-sure`](pic-sure.md)
