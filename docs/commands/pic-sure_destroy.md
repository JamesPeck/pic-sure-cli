# pic-sure destroy

Remove everything the CLI created for this stack

```text
Remove the stack's containers, every volume labelled for it, its dev
images, and the files and directories pic-sure created in the stack
directory (those manifest.json lists). Files pic-sure didn't create are
left, and listed. Shared data sets and other stacks are never touched.

Commit-tagged images are shared between stacks and left for `cache prune`;
--prune-images removes those no other stack uses, by cache prune's rules.

Use --dry-run to list what would be removed without changing anything.
A preview never prompts or needs --yes. For a real removal, confirm on a
terminal by typing the stack name; otherwise pass --yes.
```

```
pic-sure destroy [flags]
```

## Examples

```
  # Preview what destroy would remove
  pic-sure destroy --dry-run

  # Destroy the stack in another directory without a prompt, as JSON
  pic-sure --stack ~/picsure/demo destroy --yes --json

  # Also remove shared images no other stack uses
  pic-sure destroy --prune-images
```

## Flags

```
      --dry-run        preview removals without changing anything or asking for confirmation
      --prune-images   also remove shared images no other stack uses
```

The [global flags](pic-sure.md#flags) apply too.

See also: [`pic-sure`](pic-sure.md)
