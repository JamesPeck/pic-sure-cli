# pic-sure build

Build the stack's images (default: every component)

Build (or, with images.mode pull, pull) the images of the components
(pic-sure, frontend, migrations, dictionary-etl) at the commits the stack
records, and record their tags. Built images that are already up to date
are kept; pull mode pulls each time, since a ref can be a branch. A component with components.<name>.source set is built from that
checkout, tagged dev-<stack>-<sha12>, and rebuilt every time while the
checkout has uncommitted changes. Build logs go to .pic-sure/logs/build/.

```
pic-sure build [COMPONENT...] [flags]
```

## Flags

```
      --force   rebuild even if the images exist
```

The [global flags](pic-sure.md#flags) apply too.

See also: [`pic-sure`](pic-sure.md)
