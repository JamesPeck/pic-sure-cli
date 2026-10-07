# JSON output

`status --json`, `doctor --json`, `ps --json`, `support-bundle --json`,
`version --json`, `cache list --json`, `dev list --json`, `shared-data
list --json`, `migrate --check --json` and `db bootstrap --check --json`
print one JSON object on stdout with `schema_version` first, and no
`result` event. Commands that stream print NDJSON events instead, ending
with a `result` event. Within `schema_version` 2,
changes are additive only: fields are added, never renamed, removed or
retyped. Ignore fields you don't know.

## `status --json`

Exit code 0 whenever the stack is found, even if parts of it couldn't be
read; a section that couldn't be read says why in its `error` field (or
`state_error`, `images_error`, `services_error`). Exit code 3 when there
is no stack. `status` is read-only: it takes no lock, writes nothing, and
talks to nothing but the local docker daemon.

Times are RFC 3339 in UTC. "or null" means the value can be JSON `null`;
an `omitted` field is left out when empty.

| Field | Type | Meaning |
|---|---|---|
| `schema_version` | int | Always 2. |
| `stack.name` | string | `name` from pic-sure.yaml, as written. Empty if unreadable. |
| `stack.dir` | string | The stack directory, absolute. |
| `config.valid` | bool | pic-sure.yaml decodes, validates, and the files it names exist. |
| `config.problems` | array | Validation problems; empty when valid or when `config.error` is set. |
| `config.problems[].path` | string | Dotted key path, such as `network.http_port`; empty for the whole file. |
| `config.problems[].line` | int | Line in pic-sure.yaml, 0 when unknown. |
| `config.problems[].message` | string | What is wrong. |
| `config.error` | string, omitted | Why the file couldn't be read or decoded at all, such as a schema this pic-sure can't read. |
| `versions.cli` | string | This pic-sure's version. |
| `versions.schema` | int | The pic-sure.yaml schema this pic-sure reads and writes. |
| `versions.stack_cli` | string | The pic-sure version that last rendered the stack (state.json); empty before the first render. |
| `versions.stack_schema` | int | The schema the stack was last rendered with; 0 before the first render. |
| `versions.config_schema` | int | pic-sure.yaml's `schema`; 0 if unreadable. |
| `versions.gate` | string | The version gate (§10.6): `ok`; `stack_newer` (a newer pic-sure rendered the stack, so mutating commands exit 5); `migrations_pending` (run `pic-sure update`); `unsupported_schema` (pic-sure.yaml's schema is older than any migration this pic-sure has, so mutating commands exit 5); `unknown` when `versions.error` is set. |
| `versions.pending_migrations` | array of string | Summaries of the config migrations `update` would run. |
| `versions.error` | string, omitted | Why the versions couldn't be read (a corrupt state.json). |
| `state_error` | string, omitted | Why state.json couldn't be read. `release`, `components`, image refs and `last_operation` are then empty. |
| `release.repo` | string | The release-control repo the component commits came from; empty before the first `init`/`update`. |
| `release.branch` | string | Its branch. |
| `release.commit` | string | The full release-control commit. |
| `components` | array | One entry per component, in catalog order: `pic-sure`, `frontend`, `migrations`, `dictionary-etl`. |
| `components[].name` | string | The component. |
| `components[].ref` | string | The tag or branch its commit was resolved from; empty if not recorded. |
| `components[].commit` | string | The full commit the stack runs; empty if not recorded. |
| `images` | array | One entry per image pic-sure builds, in build order. |
| `images[].name` | string | The image's short name, such as `pic-sure-gateway`. |
| `images[].component` | string | The component it is built from. |
| `images[].ref` | string | `repository:tag` the stack runs: the tag state.json records, or the dev build's tag when `dev` is true; empty when none is recorded. |
| `images[].present` | bool or null | Whether docker has the image; null when unknown (no recorded tag, or docker failed). |
| `images[].dev` | bool | A dev variant in `dev.services` replaces the image: with its local build (§7.3), or, with an empty `ref`, with a third-party image (`httpd-hmr` runs `node` instead of `pic-sure-httpd`). |
| `images_error` | string, omitted | Why image presence couldn't be checked, usually an unreachable daemon. |
| `services` | array | Containers from `docker compose ps`, sorted by service. An empty list is unknown, not down: see `services_error`. |
| `services[].service` | string | The compose service. |
| `services[].container` | string | The container name. |
| `services[].state` | string | created, running, restarting, paused, exited, removing or dead. |
| `services[].health` | string | healthy, unhealthy or starting; empty with no healthcheck. |
| `services[].status` | string | Compose's summary, such as `Up 2 minutes (healthy)`. |
| `services[].exit_code` | int | The exit code of an exited container. |
| `services_error` | string, omitted | Why compose couldn't be asked, including a stack that hasn't been rendered yet. Without it, an empty `services` means compose reported no containers. |
| `db` | object or null | The database; null when the config can't be decoded or fails validation. |
| `db.mode` | string | `local` or `remote`. |
| `db.host` | string, omitted | The remote database host. |
| `db.port` | int, omitted | The remote database port. |
| `migrations.status` | string | `up_to_date` when every Flyway history records every migration in the mounted source trees with no failed entry; `pending` otherwise; `unknown` when it couldn't be checked. |
| `migrations.error` | string, omitted | Why `status` is `unknown`: the databases aren't running and healthy, the database is remote (status doesn't query it), or the check failed. Omitted when the stack isn't rendered or compose couldn't list its services. |
| `token.expires_at` | string or null | When the introspection token expires; null when none has been issued. |
| `token.expired` | bool | Whether it has expired. |
| `token.error` | string, omitted | Why secrets.yaml couldn't be read. |
| `auth0` | object or null | What to register in the Auth0 application (D35); null when the config can't be decoded or fails validation. |
| `auth0.needed` | bool | False in `open` auth mode, where nobody has to log in. |
| `auth0.callback_url` | string | Allowed callback URL: `https://HOST[:PORT]/login/loading/`. |
| `auth0.logout_url` | string | Allowed logout URL: the HTTPS origin. |
| `auth0.web_origin` | string | Allowed web origin: the HTTPS origin. |
| `auth0.dev_callback_url` | string, omitted | While the `httpd-hmr` dev variant is on, the callback URL on its Vite origin, `http://localhost:PORT/login/loading/`. |
| `auth0.dev_logout_url` | string, omitted | Its logout URL: the Vite origin. |
| `auth0.dev_web_origin` | string, omitted | Its web origin: the Vite origin. |
| `last_operation` | object or null | The last mutating command; null when none is recorded. |
| `last_operation.name` | string | The command, such as `up`. |
| `last_operation.status` | string | `running` (or interrupted, if nothing holds the lock), `ok` or `failed`. |
| `last_operation.started_at` | string | When it started. |
| `last_operation.finished_at` | string, omitted | When it finished. |
| `deep` | object, omitted | Probes run inside the running containers; only with `--deep`. A probe that couldn't run (its service isn't running, or `compose exec` failed) has `checked` false and says why in its `message`. |
| `deep.gateway.checked` | bool | Whether the gateway was probed (it must be running). |
| `deep.gateway.healthy` | bool or null | Whether the gateway's `/system/status` is `RUNNING`, which folds in every downstream service; false when it didn't answer; null when not checked. |
| `deep.gateway.status` | string | The gateway's answer, such as `RUNNING` or `ONE OR MORE COMPONENTS DEGRADED`; empty without one. |
| `deep.gateway.message` | string | The verdict in words. |
| `deep.data.checked` | bool | Whether HPDS was probed (it must be running). |
| `deep.data.ready` | bool or null | Whether HPDS has data: true when a COUNT query answers with a number and its actuator health is `UP`; false when HPDS refuses the query with HTTP 403 (its encryption key isn't loaded) or its health is `DOWN` (no data loaded, as on a fresh stack); null when unknown. |
| `deep.data.message` | string | The verdict in words, with what to do. |
| `deep.http.checked` | bool | Whether httpd was probed (it must be running). |
| `deep.http.csp` | string | The Content-Security-Policy of the frontend's HTML at `https://127.0.0.1/` inside httpd: `frontend` (one policy with a nonce), `floor` (only httpd's fallback policy), `both` (more than one policy), `none`, or `unknown` (not checked, not a single 200 HTML response, or an unrecognized policy). |
| `deep.http.message` | string | The verdict in words. |

## `ps --json`

The stack's containers, stopped ones included. Exit code 3 when there is
no stack or it hasn't been rendered, and 1 when compose can't be asked.

| Field | Type | Meaning |
|---|---|---|
| `schema_version` | int | Always 2. |
| `services` | array | The containers, in the shape and order of `status --json`'s `services`. |

## `doctor --json`

Exit code 1 when any check has status `fail`, 0 otherwise (warnings
don't count); the report is printed either way, with no `result` after
it. Exit code 3 when `--stack` names no stack.

| Field | Type | Meaning |
|---|---|---|
| `schema_version` | int | Always 2. |
| `stack` | string, omitted | The stack directory checked; omitted when run outside a stack, which checks only the host and Docker. |
| `checks` | array | The checks, in the order they ran. |
| `checks[].name` | string | Stable name, listed below. |
| `checks[].status` | string | `ok`, `warn` or `fail`. |
| `checks[].message` | string | The result in words. |
| `checks[].detail` | string, omitted | Extra guidance that may span lines, such as how to set the Docker daemon's proxy. |

Check names. Host and Docker: `docker-cli`, `docker-daemon`,
`docker-runtime`, `compose-version`, `buildx-version` (a failure only when
images are built), `git`, `disk-cache`, `disk-docker`, `memory`,
`arm64-images`. Stack: `config`, `compose-config`, `overrides`, `ports`,
`auth0`, `proxy`. With `--network`: `network-release-control`,
`network-github`, `network-maven-central`, `network-npm-registry`,
`network-alpine-cdn`, and, when a proxy is set, `network-docker-pull`.
New checks may be added; treat an unknown name like any other.

## `version --json`

| Field | Type | Meaning |
|---|---|---|
| `schema_version` | int | Always 2. |
| `version` | string | The release, such as `v2.0.0`; `dev` for an unreleased build. |
| `commit` | string | The commit it was built from. |
| `date` | string | The build time. |

## `support-bundle --json`

The archive `support-bundle` wrote. Exit code 0 once it is written, even
when parts couldn't be collected; 1 when it can't be written; 3 when
`--stack` names no stack.

| Field | Type | Meaning |
|---|---|---|
| `schema_version` | int | Always 2. |
| `path` | string | The archive's absolute path. |
| `files` | array of strings | The archive's files, relative to its top directory, such as `status.json` or `compose/logs/hpds.log`. |
| `problems` | array of strings | What couldn't be collected, and why; also in the archive's `README.txt`. Empty when everything was. |
| `short_secrets` | int | How many secrets are shorter than 4 bytes. They are redacted only where no letter or digit touches them, so check the archive for them before sharing it. |

## NDJSON events

Every command that doesn't print a report (above) and isn't `config` or
`compose`, with `--json`, prints one JSON object per line on
stdout as it runs, each with `type` first, and ends with exactly one
`result` line. Logs and warnings meant for people go to stderr, so stdout
is only events. Read lines until `result`; don't rely on the events before
it beyond what's below.

| `type` | Fields | Meaning |
|---|---|---|
| `step_started` | `id`, `title` | A step began. `id` is stable (the IDs `--skip-step` takes); `title` is for people. |
| `progress` | `id`, `text`, `pct` (omitted when unknown) | Progress within a step; `pct` is 0–100. |
| `log` | `id`, `stream` (`stdout` or `stderr`), `line` | One line of a subprocess's output, without the newline. |
| `warning` | `id` (omitted when not tied to a step), `text` | Something to look at that didn't stop the command. |
| `step_done` | `id`, `status` | A step ended: `ok`, `skipped` (already done, or `--skip-step`) or `failed`. |
| `result` | `ok`, `data` (omitted when none), `error` (omitted on success) | The last line. |
| | `error.exit_code` | The process's exit code (see docs/agents.md). |
| | `error.message` | What went wrong, the same text as the `pic-sure: ` line on stderr. |
| | `error.step` | The ID of the step that failed, when one did. |

For example, `pic-sure up --json` on a running stack:

```
{"type":"step_started","id":"resolve","title":"Resolve the component commits"}
{"type":"step_done","id":"resolve","status":"skipped"}
...
{"type":"step_started","id":"start","title":"Start the stack"}
{"type":"log","id":"start","stream":"stderr","line":" Container demo-hpds-1  Running"}
{"type":"step_done","id":"start","status":"ok"}
{"type":"result","ok":true,"data":{"stack":"demo","dir":"/home/me/picsure/demo","url":"https://localhost:8443",...}}
```

A command that fails before it starts any step (a usage error, no stack)
prints only the `result` line.

### `result.data`

Commands that report something put it in `result.data`. The shapes follow
the same additive-only rule:

| Command | `data` |
|---|---|
| `init`, `up` | `{"stack", "dir", "already_initialized" (omitted unless true), "url", "auth0", "token_expiry", "next_steps"}`; `auth0` is `status --json`'s object, and `up`'s `next_steps` is empty. |
| `update` | The plan: `{"stack", "dry_run", "config": {"from", "to", "migrations"}, "release": {"repo", "branch", "from", "to"}, "components": [{"name", "from_ref", "from_commit", "to_ref", "to_commit", "source", "changed"}], "images": [{"name", "component", "from", "to", "action"}], "migrations": {"status", "detail", "started_db"}, "token": {"expiry", "renew"}, "restarts": [{"service", "action", "reasons"}]}`. `images[].action` is `build`, `pull`, `up-to-date`, or `keep` (with `--no-build`); `migrations.status` is `pending`, `up-to-date` or `unknown`; `restarts[].action` is `recreate` or `restart`. Note the hyphen: `status --json` spells the same state `up_to_date`. |
| `data demo` | `{"dataset": "demo:<name>"}`, where `<name>` is the argument, `all` included. |
| `data load-phenotype` | `{"dataset": "phenotype:<sha256>", "dictionary": "auto" or "custom", "weights": bool}` |
| `data load-genomic` | `{"partition", "promoted": [...], "profile"}` |
| `shared-data publish` | The data set, as one entry of `shared-data list`. |
| `shared-data remove` | `{"name", "removed": [...]}` |
| `dev on`, `dev off` | `{"service", "on", "services", "port", "source"}` |
| `reset`, `destroy` | `{"stack", "volumes": [...], "kept_volumes": [...], "images": [...], "files", "pruned"}`: the volumes removed, the volumes reset kept (`--keep-db`), destroy's dev images, and for destroy `files: {"removed", "kept", "remaining", "dir_removed"}` (`remaining` lists what you added, which destroy leaves). `pruned` is there with `--prune-images`. Empty lists may be omitted. |

Other commands' `data` isn't listed here yet; treat it as informational.

`cache list`, `dev list` and `shared-data list` are reports, not streams:
`dev list --json` is `{"schema_version": 2, "variants": [...]}` and
`shared-data list --json` is `{"schema_version": 2, "data_sets": [...]}`.
