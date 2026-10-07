# pic-sure config set

Validate and set one config value

```text
Set one config value and save pic-sure.yaml, keeping its comments. The
whole config is validated first, and nothing is written if it's invalid.
A list takes comma-separated values or [a, b]; an empty VALUE clears it.
Flags go before KEY, so a VALUE such as -Xmx4g needs no quoting.
```

```
pic-sure config set KEY VALUE
```

The [global flags](pic-sure.md#flags) apply too.

See also: [`pic-sure config`](pic-sure_config.md)
