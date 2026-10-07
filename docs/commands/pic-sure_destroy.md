# pic-sure destroy

Remove everything the CLI created for this stack

Remove the stack's containers, every volume labelled for it, its dev
images, and the files and directories pic-sure created in the stack
directory (those manifest.json lists). Files pic-sure didn't create are
left, and listed. Shared data sets and other stacks are never touched.

Commit-tagged images are shared between stacks and left for `cache prune`;
--prune-images removes those no other stack uses, by cache prune's rules.

On a terminal you confirm by typing the stack name; otherwise pass --yes.

```
pic-sure destroy [flags]
```

## Flags

```
      --prune-images   also remove shared images no other stack uses
```

The [global flags](pic-sure.md#flags) apply too.

See also: [`pic-sure`](pic-sure.md)
