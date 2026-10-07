# pic-sure update

Update the stack to the current release: config, images, migrations

```text
Move the stack to the head of release.branch, or to --release-commit: work
out a plan (config migrations, component commits, images, database
migrations, the introspection token, services to restart), then back up
and migrate pic-sure.yaml, build or pull the images, re-render, migrate,
seed (renewing the token when it expires within 30 days), and start the
stack, restarting only the services that need it.

--dry-run prints the plan and changes nothing in the stack, though it may
start the stack's database to compare its migrations. --no-build keeps the
components and images the stack runs and doesn't move its release.
```

```
pic-sure update [flags]
```

## Flags

```
      --dry-run              print the plan and change nothing
      --ignore-cli-version   skip the CLI compatibility gate
      --no-build             skip building and pulling images
      --release-commit SHA   use this release-control SHA instead of the branch head
      --self-update          replace this binary if the release names a newer CLI
```

The [global flags](pic-sure.md#flags) apply too.

See also: [`pic-sure`](pic-sure.md)
