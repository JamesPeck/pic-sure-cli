# pic-sure up

Converge an existing stack to running

```text
Bring the stack to running: build any missing images, install the TLS
certificate and truststore if their volumes need them, re-render the compose
file, then start and migrate the database, seed it, install the HPDS key and
start the services. Each step is skipped when it is already done, so on a
running, current stack up only verifies. After a reset it re-migrates,
re-seeds and re-keys HPDS.

Running services whose certificate, truststore or rendered files changed
are restarted before the services are started and waited for.

A component whose components.<c>.source was unset goes back to its release
images: up resolves it at the stack's recorded release, builds or pulls
the images and recreates its services. It never moves the stack to
another release; that is pic-sure update.
```

```
pic-sure up
```

The [global flags](pic-sure.md#flags) apply too.

See also: [`pic-sure`](pic-sure.md)
