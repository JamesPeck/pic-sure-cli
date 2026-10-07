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

The [global flags](pic-sure.md#flags) apply too.

See also: [`pic-sure dev`](pic-sure_dev.md)
