# pic-sure reset

Remove the stack's containers and data volumes; keep its config

```text
Stop the stack and remove its containers and its data volumes: HPDS,
dictionary, staging, certs, truststore, and the database unless --keep-db.
The config, secrets, logs and TLS sources are kept, so the next `pic-sure up`
sets the stack up again, empty.

Use --dry-run to list what would be removed without changing anything.
A preview never prompts or needs --yes. For a real removal, confirm on a
terminal by typing the stack name; otherwise pass --yes.
```

```
pic-sure reset [flags]
```

## Flags

```
      --dry-run   preview removals without changing anything or asking for confirmation
      --keep-db   keep the database volume
```

The [global flags](pic-sure.md#flags) apply too.

See also: [`pic-sure`](pic-sure.md)
