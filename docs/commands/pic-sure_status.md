# pic-sure status

Report the stack's config, versions, images, services and migrations

```text
Report on the stack without changing it: config validity, the version
gate, the release and component commits, which images are present, the
services compose reports, the database mode, migrations, the introspection
token's expiry, and the URLs to register in Auth0. It exits 0 whenever it
finds the stack, even if parts of it couldn't be read.

--deep also runs probes inside the running containers, which take a few
seconds: the gateway's /system/status, a COUNT query that tells whether
HPDS has data loaded, and which Content-Security-Policy the frontend's
HTML gets.
```

```
pic-sure status [flags]
```

## Flags

```
      --deep   also probe inside the running containers: the gateway's health, whether HPDS has data, and the frontend's CSP
```

The [global flags](pic-sure.md#flags) apply too.

See also: [`pic-sure`](pic-sure.md)
