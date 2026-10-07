# pic-sure db bootstrap

Create the databases, users and grants on a remote MySQL

For a stack with db.mode remote: connect to the remote MySQL as
db.remote.root_user and create the auth and picsure databases and the
picsure, auth and airflow users, with their passwords from secrets.yaml and
all privileges on their databases. What already exists is left alone, so an
existing user keeps its password; --sync-passwords sets each user's password
to the one in secrets.yaml. init, up, update and migrate run the same step.

--check reports the databases, users, grants and logins without changing
anything, and exits 3 when bootstrap has work to do.

The mysql client runs in a mysql:8.0 container, and the stack's services
connect from theirs, so db.remote.host must be reachable from a container:
localhost or 127.0.0.1 is the container itself. For a MySQL on the Docker
host, use host.docker.internal where the runtime provides it (Docker Desktop,
OrbStack).

```
pic-sure db bootstrap [flags]
```

## Flags

```
      --check            report schemas, users and grants without changing anything
      --sync-passwords   set the users' passwords to match secrets.yaml
```

The [global flags](pic-sure.md#flags) apply too.

See also: [`pic-sure db`](pic-sure_db.md)
