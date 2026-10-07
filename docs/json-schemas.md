# JSON output

`status --json`, `doctor --json`, `ps --json` and `version --json` print one JSON object
on stdout with `schema_version` first. Commands that stream print NDJSON
events instead, ending with a `result` event. Within `schema_version` 2,
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
