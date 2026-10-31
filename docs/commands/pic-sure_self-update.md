# pic-sure self-update

Replace this binary with a verified release

```
pic-sure self-update [flags]
```

## Examples

```
  # Install the newest stable v2 release
  pic-sure self-update

  # Install a specific version
  pic-sure self-update --to 2.1.0

  # Fail unless the signature can be checked, as JSON
  pic-sure self-update --require-signature --json
```

## Flags

```
      --require-signature   fail, rather than warn, when cosign 3.0 or newer isn't installed to check the signature (also PIC_SURE_REQUIRE_SIGNATURE=1)
      --to VERSION          install this VERSION instead of the newest stable v2 release
```

The [global flags](pic-sure.md#flags) apply too.

See also: [`pic-sure`](pic-sure.md)
