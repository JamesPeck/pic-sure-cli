# pic-sure migrate

Run the database migrations

```text
Start the database if it isn't running, then run the Flyway migrations:
the auth and picsure schemas, the migrations project's, and the dictionary's.
A database that is already migrated is left alone. After a migration, psama
and dictionary-api are restarted if they are running.

--dry-run verifies the migrations' inputs (the SQL directories, the dictionary
schema, the project UUIDs, the remote database settings and the compose
config) without touching the database. --repair runs Flyway repair, which
removes failed entries from the history so a fixed migration can run again.
```

```
pic-sure migrate [flags]
```

## Examples

```
  # Run the database migrations
  pic-sure migrate

  # Verify the migration inputs without running them, as JSON
  pic-sure migrate --dry-run --json

  # Run Flyway repair after a failed migration
  pic-sure migrate --repair
```

## Flags

```
      --dry-run   verify the migration inputs without running them
      --repair    run Flyway repair
```

The [global flags](pic-sure.md#flags) apply too.

See also: [`pic-sure`](pic-sure.md)
