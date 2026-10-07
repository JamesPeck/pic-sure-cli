# Stack templates

The stack definition pic-sure embeds (D5) and renders into a stack's
`.pic-sure/render/`. It is ported from the bash All-in-One
(`hms-dbmi/pic-sure-all-in-one`, branch `aio-compose`) and changed where v2
fixes the bash's behaviour (spec §6.4, §13).

AIO commit: `f7ff8b8`

The drift job (`.github/workflows/template-drift.yml`, ticket 066) runs
weekly. It diffs each AIO source below, plus any other AIO compose file,
`config/` file or `.env.example`, between this commit and the head of
`aio-compose`, and keeps one "Template drift" issue open while they differ.
It never fails the build; when the branch or this commit can't be fetched,
the run summary and the issue say so.

To catch up with upstream:

1. See what changed: `go run ./tools/templatedrift -aio <AIO checkout> -to
   aio-compose` (exit 1 means drift), or read the issue.
2. Port what applies to the templates, and update the table and the
   deviations below if they changed.
3. Set the commit above to the head you compared with (`git -C <AIO
   checkout> rev-parse --short aio-compose`). The next run closes the issue.

## Layout and format

- `compose/` holds compose fragments. Render merges `base.yaml.tmpl`, one
  fragment per catalog condition the stack meets (`local-db`, then
  `local-hpds` or `shared-hpds`, then `truststore`), one per enabled dev
  variant (`dev/<variant>`), and `service-env.yaml.tmpl` last, into the
  single `compose.yaml` (D29). `composeFragments` in `../templates.go` is
  that order.
- `files/` holds the files render writes to `.pic-sure/render/files/`, which
  the compose file bind-mounts read-only. A `.tmpl` file is executed and
  loses the suffix; any other file is copied as is.

Every `.tmpl` file is a Go `text/template`, executed with `templateData`
(`../templates.go`) and the functions `q` (a double-quoted YAML scalar, with
`$` doubled so compose doesn't interpolate it), `flow` (a map as a one-line
YAML mapping) and `path` (joins path elements). The data's methods `Image`,
`DevImage` and `DevPort` fail on a name render didn't fill in, and `Java`
gives a service's JAVA_OPTS.

**Merging.** A later fragment's mappings merge into the earlier ones key by
key; a scalar or a sequence replaces the earlier value whole. So a fragment
that changes a list restates all of it, and since no fragment can remove a
key, whatever only some stacks have lives in its own fragment, never in base.

**Values.** Config values are written into the compose file literally. Only
secrets are `${NAME}` references (`secretVars`), plus the proxy variables
(`proxyVars`, which can carry credentials) when a proxy is set. The Compose
adapter sets them on every call, so `compose.yaml` never holds a secret.

**Volumes.** Compose creates a declared volume only when a service mounts it.
A helper that creates a stack volume first (certs, truststore, the HPDS key,
genomic staging) must give it the stack labels itself, plus
`com.docker.compose.project=<name>` and `com.docker.compose.volume=<key>`;
without the project label compose warns on every `up` that the volume
wasn't created by compose.

## Templates and their AIO sources

| Template | AIO source |
|---|---|
| `compose/base.yaml.tmpl` | `docker-compose.yml` |
| `compose/local-db.yaml.tmpl` | `docker-compose.yml`, `docker-compose.remote-db.yml` |
| `compose/local-hpds.yaml.tmpl` | `docker-compose.yml` |
| `compose/shared-hpds.yaml.tmpl` | `docker-compose.shared-hpds.yml` |
| `compose/truststore.yaml.tmpl` | `docker-compose.yml` |
| `compose/service-env.yaml.tmpl` | none |
| `compose/dev/psama.yaml.tmpl` | `docker-compose.dev-psama.yml` |
| `compose/dev/hpds.yaml.tmpl` | `docker-compose.dev-hpds.yml` |
| `compose/dev/gateway.yaml.tmpl` | `docker-compose.dev-gateway.yml` |
| `compose/dev/operations.yaml.tmpl` | `docker-compose.dev-operations.yml` |
| `compose/dev/query.yaml.tmpl` | `docker-compose.dev-query.yml` |
| `compose/dev/visualization.yaml.tmpl` | `docker-compose.dev-visualization.yml` |
| `compose/dev/dictionary.yaml.tmpl` | `docker-compose.dev-dictionary.yml` |
| `compose/dev/httpd.yaml.tmpl` | `docker-compose.dev-httpd.yml` |
| `compose/dev/httpd-hmr.yaml.tmpl` | `docker-compose.dev-httpd-hmr.yml` |
| `files/httpd/httpd-vhosts.conf.tmpl` | `config/httpd/httpd-vhosts.conf` |
| `files/httpd/vite.config.dev.ts` | `config/httpd/vite.config.dev.ts` |
| `files/flyway/run-migrations.sh` | `config/flyway/run-migrations.sh` |
| `files/flyway/run-dictionary-migrations.sh` | `config/flyway/run-dictionary-migrations.sh` |
| `files/db-init/01-create-databases-and-users.sh` | `config/db-init/01-create-databases-and-users.sh` |
| `files/dictionary/facet_loader_configuration.json` | `demo-data/facet_loader_configuration.json` |

Not ported: `docker-compose.dev.yml` (the all-services dev overlay; the
per-variant fragments replace it), `config/flyway/{auth,picsure}/sql.properties`
(for `load-rdbms`, dropped by D26), `config/scripts/*` (the truststore and
introspection token are native, §9.4 and §9.5) and `.env.example` (typed
config, D9).

## Deliberate differences from AIO

Compose:
- One merged file per stack instead of overlays passed to compose, so nothing
  uses `!reset`. The remote-db overlay is inverted: base has no `picsure-db`,
  and `local-db` adds it with the waits on it.
- A top-level `name:`, and the stack labels on every service, every stack
  volume and every network. External shared-data volumes keep their own
  labels.
- No `${VAR:-default}` settings. Config values and AIO's defaults are written
  literally; `services.<name>.env` and `java_opts` override them.
- Each image is its own reference (`Image`), not one `PICSURE_IMAGE_TAG`.
- TLS files come from the `certs` volume, not `./certs/*` binds.
- psama gets a truststore only when the stack has custom certs: the
  `truststore` volume at `/truststore`, store password `changeit`. AIO always
  mounted `./config/psama/application.truststore` (password `password`).
- Logs go to named volumes, one per service (the two dictionary services
  share one), not `./.data/logs/*` binds.
- The dictionary services' Postgres settings are inline, with the password
  as `${DB_DICTIONARY_PASSWORD}`, instead of the `dictionary.env` env file.
- Bind mounts use the long syntax with absolute sources: rendered files and
  source trees from the host cache or a configured local source, not
  `./repos`, `PICSURE_SRC` or `MIGRATIONS_SRC`. Every one sets
  `create_host_path: false`, so a missing source fails the start instead of
  becoming an empty directory; AIO did this for two of them.
- The Flyway services are in the `migrate` profile and run with
  `compose run --rm`. psama, the operations service and the dictionary
  services no longer depend on them; pic-sure migrates before starting
  them. psama and the operations service wait on `picsure-db` directly
  instead (local DB only). `FLYWAY_ACTION` is `migrate`; `compose run -e`
  overrides it.
- HPDS `ID_BATCH_SIZE` is 2000, the value AIO's `.env.example` installs; the
  compose fallback was 0.
- With a proxy, psama (Auth0) and httpd-hmr (npm) get `HTTP_PROXY`,
  `HTTPS_PROXY` and `NO_PROXY` in both cases. Render adds the JVM proxy
  properties to JAVA_OPTS.
- JAVA_OPTS keeps AIO's defaults, followed by what render adds: proxy
  properties, psama's truststore properties and a dev variant's JDWP agent.
- Shared HPDS: the volume names follow the catalog (`shared-hpds-data`,
  `shared-hpds-genomic`, `hpds-genomic-copy`), and the seed's alpine is
  pinned. The seed records a finished copy as `.picsure-seeded` and compares
  the published marker with that. AIO compared it with the copied
  `.picsure-published`, which `cp -a` can write before the rest of the set,
  so an interrupted copy passed as complete.
- `LOGGING_API_KEY` has no `disabled` fallback: pic-sure always generates it.

Dev variants:
- No `build:` sections: pic-sure builds dev images itself (§7.3), and each
  variant's services run the dev image (`DevImage`).
- Debug ports come from `network.dev_ports` and bind 127.0.0.1. Every
  container listens for JDWP on 5005.
- dev psama keeps the configured JAVA_OPTS; AIO's dev overlay switched to
  `-Xms1g -Xmx2g`.
- httpd-hmr binds its port to 127.0.0.1 (AIO published 3000 on every
  interface). It gets its `VITE_*` settings as environment variables instead
  of an `hmr.env` mounted at `/app/.env`, which made Docker create a mount
  point in the frontend checkout. It replaces httpd's mounts instead of
  adding to them, so no `/app/logs` mount point appears in the checkout
  either. The `vite.config.aio.ts` mount point still does.

Files:
- The vhost's http→https redirect keeps the HTTPS port when it isn't 443.
- `run-migrations.sh` passes the Flyway credentials as `FLYWAY_USER` and
  `FLYWAY_PASSWORD`, not `-user=` and `-password=` on the command line, as
  `run-dictionary-migrations.sh` already did.
- Both Flyway scripts' error hints point at pic-sure instead of
  `clone-repos.sh`, `init.sh` and `.env`.
