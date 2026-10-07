# pic-sure secrets rotate

Rotate a secret everywhere it is used

```text
Replace one secret: first in the database that checks it, then in
secrets.yaml, then in the running services whose compose config uses it,
which are recreated. NAME is one of:

  db-root, db-picsure, db-auth, db-airflow   MySQL passwords (ALTER USER)
  dictionary-db                              the dictionary Postgres password
  introspection-token                        PSAMA's token, re-issued
  query-service-token, application-token,
  logging-key, obfuscation-salt              read by the services only
  hpds-key                                   the HPDS encryption key
  auth0-client-secret                        the Auth0 application's secret

New values are generated, except the Auth0 client secret and, with a remote
database, the root password: those are read from stdin and need --yes. The
Auth0 client secret must be at least 32 bytes; supplying it re-issues the
introspection token, and it is how a stack leaves open mode: set
auth.auth0.client_id, then auth.mode, rotate auth0-client-secret, then run `pic-sure up`. With a remote database,
db-root records a password the DBA has already changed, after checking it
logs in.

hpds-key refuses while HPDS has data loaded, since the new key can't read
it: --discard-data --yes deletes the data first. Load it again afterwards.

The database must be running for a database secret. On a terminal you
confirm; otherwise pass --yes.
```

```
pic-sure secrets rotate NAME [flags]
```

## Flags

```
      --discard-data   with hpds-key: delete the loaded HPDS data, which the new key can't read (needs --yes)
```

The [global flags](pic-sure.md#flags) apply too.

See also: [`pic-sure secrets`](pic-sure_secrets.md)
