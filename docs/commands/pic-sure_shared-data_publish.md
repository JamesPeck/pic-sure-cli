# pic-sure shared-data publish

Publish this stack's HPDS data as an immutable data set

Copy this stack's HPDS phenotype and genomic data into the volumes NAME_hpds-data and
NAME_hpds-genomic, which any stack on this Docker host can mount read-only
(hpds.data: shared, hpds.shared_name: NAME). HPDS is stopped during the copy.
Data sets are immutable: an existing NAME is refused, so publish a new name instead.

```
pic-sure shared-data publish NAME
```

The [global flags](pic-sure.md#flags) apply too.

See also: [`pic-sure shared-data`](pic-sure_shared-data.md)
