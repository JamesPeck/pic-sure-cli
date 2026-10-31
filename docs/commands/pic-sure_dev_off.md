# pic-sure dev off

Remove SERVICE's dev variant and debug port

```text
Remove SERVICE from dev.services, re-render and recreate SERVICE without
its debug port. While components.<component>.source is set, SERVICE keeps
running the build of that checkout, since a source applies to the whole
component. To return to the release images, unset the source and run
pic-sure up.
```

```
pic-sure dev off SERVICE
```

## Examples

```
  # Put HPDS back on its release image
  pic-sure dev off hpds

  # The same, as JSON
  pic-sure dev off hpds --json
```

The [global flags](pic-sure.md#flags) apply too.

See also: [`pic-sure dev`](pic-sure_dev.md)
