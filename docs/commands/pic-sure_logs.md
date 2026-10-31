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

## Flags

```
  -f, --follow         follow log output
      --since string   show logs since a duration (10m, 2h) or RFC 3339 timestamp
      --tail string    number of lines per service, or all for the whole log (default "200")
```

The [global flags](pic-sure.md#flags) apply too.

See also: [`pic-sure`](pic-sure.md)
