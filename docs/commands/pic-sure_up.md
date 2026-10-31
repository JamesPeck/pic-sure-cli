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

--dry-run previews Checks until the first step that would act. Later steps
depend on that change; render, start and HMR volume setup have no Check.
Nothing is written and no images are pulled.
A read-only, network-disabled genomic probe container may run and is named
in the output.
```

```
pic-sure up [flags]
```

## Examples

```
  # Start the stack, or converge it after a change
  pic-sure up

  # Preview the partial step plan without changing the stack
  pic-sure up --dry-run

  # Apply a config change, as a JSON event stream
  pic-sure config set hpds.java_opts -Xmx8g && pic-sure up --json

  # Start it without progress output, keeping warnings and the result
  pic-sure -q up
```

## Flags

```
      --dry-run   Preview the partial step plan without writing anything
```

The [global flags](pic-sure.md#flags) apply too.

See also: [`pic-sure`](pic-sure.md)
