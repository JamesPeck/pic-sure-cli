# pic-sure config get

Print one config value or section

```text
Print one config value, such as network.http_port, or a whole section,
such as network. Secrets aren't config; they live in .pic-sure/secrets.yaml.
```

```
pic-sure config get KEY
```

## Examples

```
  # Print one value
  pic-sure config get network.http_port

  # Print a whole section as JSON
  pic-sure config get network --json
```

The [global flags](pic-sure.md#flags) apply too.

See also: [`pic-sure config`](pic-sure_config.md)
