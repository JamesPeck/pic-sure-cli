# pic-sure shared-data publish

Publish this stack's HPDS data as an immutable data set

```text
Copy this stack's HPDS phenotype and genomic data into the volumes NAME_hpds-data and
NAME_hpds-genomic, which any stack on this Docker host can mount read-only
(hpds.data: shared, hpds.shared_name: NAME). HPDS is stopped during the copy.
Data sets are immutable: an existing NAME is refused, so publish a new name instead.
```

```
pic-sure shared-data publish NAME
```

## Examples

```
  # Publish this stack's HPDS data as nhanes-2026
  pic-sure shared-data publish nhanes-2026

  # The same, as JSON
  pic-sure shared-data publish nhanes-2026 --json

  # Create a stack that mounts it read-only
  pic-sure init ~/picsure/team --name team --auth-mode open --admin-email you@example.com --auto-ports --hpds-data shared:nhanes-2026
```

The [global flags](pic-sure.md#flags) apply too.

See also: [`pic-sure shared-data`](pic-sure_shared-data.md)
