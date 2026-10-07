# pic-sure support-bundle

Write a redacted diagnostics archive to attach to an issue

Write a gzipped tar of diagnostics to attach to an issue: status --deep
--json, doctor --json, the newest 5 run logs, compose ps, each service's last
500 log lines, and pic-sure.yaml, state.json and manifest.json. Every value
in the stack's secrets.yaml and HPDS key file is replaced with [REDACTED]
in every file. README.txt in the archive lists what couldn't be collected.

Without a stack (no --stack, and none at or above the current directory)
the archive has the host checks alone. The archive goes to
./pic-sure-support-NAME-TIMESTAMP.tar.gz unless -o names a file, and its
path is printed. It exits 0 once the archive is written, even if parts of
it couldn't be collected.

```
pic-sure support-bundle [flags]
```

## Flags

```
  -o, --output FILE   write the archive to FILE
```

The [global flags](pic-sure.md#flags) apply too.

See also: [`pic-sure`](pic-sure.md)
