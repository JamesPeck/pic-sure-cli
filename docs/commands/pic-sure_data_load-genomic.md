# pic-sure data load-genomic

Load VCF data into a genomic partition

Load the VCFs a vcfIndex.tsv names into a genomic partition, staged in
the stack's genomic-staging volume, where it replaces any earlier load of
the partition. HPDS keeps running on its live data while the loaders run.

The index names each VCF by its host path, so every VCF must be under
--vcf-dir (default: the index's directory), which the loaders see at the
same path.

--promote then stops HPDS, copies the partition into its live genomic data
(every staged partition with --all-partitions, after copying the live data
into all-bak with --backup), and starts HPDS again. --enable-profile sets
hpds.profile to bch-dev, the profile that reads the genomic data, re-renders
the stack and starts HPDS on it.

```
pic-sure data load-genomic --partition P --vcf-index F [--vcf-dir D] [--promote] [--enable-profile] [flags]
```

## Flags

```
      --all-partitions   with --promote, promote every staged partition
      --backup           with --promote, first copy the live genomic data into all-bak
      --enable-profile   switch HPDS to the genomic profile (bch-dev)
      --heap MB          each loader's JVM heap in MB (default 16000)
      --partition NAME   genomic partition NAME: letters, digits, _ and -
      --promote          promote the loaded partition into the live HPDS data
      --vcf-dir DIR      DIR holding the VCFs the index names (default: the index's directory)
      --vcf-index FILE   vcfIndex.tsv FILE
```

The [global flags](pic-sure.md#flags) apply too.

See also: [`pic-sure data`](pic-sure_data.md)
