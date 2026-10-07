# pic-sure compose

Run docker compose against the rendered stack (escape hatch)

Run docker compose against the rendered stack, with the same -f files and
environment the CLI uses. Put -- before the compose arguments. pic-sure
exits with compose's exit code. Its output is compose's own, so --json is
refused.

These subcommands run without the stack lock, and on a stack a newer
pic-sure rendered: ps, logs, top, config, events, images, ls, port,
version, exec, stats, wait (without --down-project) and attach. Any other
holds the stack lock until compose exits, as every command that can change
the stack does.

```
pic-sure compose -- ARGS...
```

## Examples

```
  pic-sure compose -- ps -a
  pic-sure compose -- exec hpds sh
```

The [global flags](pic-sure.md#flags) apply too.

See also: [`pic-sure`](pic-sure.md)
