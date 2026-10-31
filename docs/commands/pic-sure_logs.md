# pic-sure logs

Show service logs

```text
Show service logs (default: every service) on stdout. With -f, keep
following new lines until Ctrl-C. With --json, each line is a log event of
step "logs". Shows the last 200 lines per service by default; use --tail all
for the whole log. --since also filters by time and still respects --tail;
use --tail all --since DUR for every line since that time.
```

```
pic-sure logs [SERVICE] [flags]
```

## Examples

```
  # Show the last 200 lines of every service
  pic-sure logs

  # Follow HPDS's log until Ctrl-C
  pic-sure logs -f hpds

  # Show every line from the last ten minutes
  pic-sure logs --tail all --since 10m

  # Show HPDS's last 50 lines as JSON log events, without progress output
  pic-sure -q --json logs --tail 50 hpds
```

## Flags

```
  -f, --follow         follow log output
      --since string   show logs since a duration (10m, 2h) or RFC 3339 timestamp
      --tail string    number of lines per service, or all for the whole log (default "200")
```

The [global flags](pic-sure.md#flags) apply too.

See also: [`pic-sure`](pic-sure.md)
