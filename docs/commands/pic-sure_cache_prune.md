# pic-sure cache prune

Remove cache entries no stack uses

Remove the commit-tagged and dev images, source trees, leftover build
contexts, downloads and temporary directories that nothing uses.

An item is in use, and kept, if any container (running or stopped)
references it, or if the state.json of a stack names it: one labelled on a
container, volume or network, one registered in the cache (init, up and
build register their stack), or the stack you run this in. If such a
stack's directory can't be read (it was moved, or deleted while its
containers or volumes remain), prune keeps every shared image and source
tree that stack might use, unless --force is given. A registered stack
whose directory is gone and that has nothing labelled left is forgotten. Items
made or changed in the last hour are always kept, as a running command may
not have recorded them yet. Images tagged by other tools, git clones and
lock files are never removed. prune waits up to 5 s for a running build to
finish with the cache, and otherwise fails, removing nothing.

```
pic-sure cache prune [flags]
```

## Flags

```
      --dry-run   show what would be removed, and remove nothing
      --force     also remove what a stack whose directory can't be read might use, and forget unparseable registry entries
```

The [global flags](pic-sure.md#flags) apply too.

See also: [`pic-sure cache`](pic-sure_cache.md)
