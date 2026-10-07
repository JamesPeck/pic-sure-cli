# Differences from the bash All-in-One

pic-sure v2 installs the same PIC-SURE stack as the bash scripts in
[`pic-sure-all-in-one`](https://github.com/hms-dbmi/pic-sure-all-in-one),
but it is a separate product. The bash All-in-One is unchanged and still
works on its own. pic-sure used the bash behaviour as a reference, not a
spec, and fixed its bugs rather than reproducing them. This page lists what
behaves differently, compared with `aio-compose` at f7ff8b8.

pic-sure needs only `docker` (compose ≥ 2.29.0, buildx ≥ 0.17.0) and
`git` on the host. It doesn't adopt existing bash installs. Create a new stack with
`pic-sure init` and load your data into it again.

## Setup and configuration

| Bash All-in-One | pic-sure v2 |
|---|---|
| A git checkout with `.env`, shell-sourced and dotenv-parsed | Any directory with a typed, validated `pic-sure.yaml`. There's no `.env` on disk; secrets are only in `.pic-sure/secrets.yaml` (0600). |
| Compose files plus overlays chosen per command, so a plain `up` drops a dev overlay | One merged compose file rendered per stack (`.pic-sure/render/compose.yaml`); overlays are chosen at render time and can't be dropped. Your own `overrides/*.yaml` are applied last. |
| No top-level compose `name:`; one stack per host | Every stack has its own name, ports, volumes and networks, and several run side by side. Destructive commands select by stack label, never by name prefix. |
| Fixed debug and HMR ports; the http→https redirect drops a non-443 port | Per-stack debug and HMR ports; the redirect keeps the port. |
| The `.env.example` proxy variables are documented but never read | The `proxy` block reaches every egress path: pic-sure's downloads, git, image builds, Maven, node and the services' JVMs. Proxy URLs must be `http://`. |

## Secrets and auth

| Bash All-in-One | pic-sure v2 |
|---|---|
| The Auth0 client secret is in a process's argv (the token script), and the admin email in the seed probe's | Secrets never reach argv: the token is minted in Go, SQL goes on stdin, user-supplied secrets are read from stdin only. |
| `init --force` rotates the HPDS key, and the dictionary password changes without the volume's | Nothing ever regenerates a secret. `pic-sure secrets rotate NAME` changes one in a safe order: the database first, then `secrets.yaml`, then the services that use it. Rotating the HPDS key refuses while data is loaded. |
| `update` writes a new introspection token to `.env`, then fails if the database is down | The database is updated first, then the config; the token is renewed only when it expires within 30 days. |
| — | In open mode with no client secret given, init generates a random 32-byte one, since PSAMA signs its tokens with it. Leaving open mode takes the real one, through `secrets rotate auth0-client-secret`; until then `up` and `update` refuse (exit 3). |
| Remote-database bootstrap never changes existing users' passwords and puts passwords into SQL unescaped | `db bootstrap --sync-passwords` updates them; all SQL is escaped. |
| The truststore is replaced by Let's Encrypt roots only (R3 has expired), downloaded at install, failing silently | The image's own `cacerts` plus your certs from `certs/trust/`, built with the image's own `keytool`. |
| The TLS key needs a host `chgrp 2`, which fails without root | A generated or provided certificate goes into a named volume, owned 2:2 by a helper container. |

## Files and containers

| Bash All-in-One | pic-sure v2 |
|---|---|
| Containers leave root-owned files in host directories (`.data/`, `.build-pic-sure`, jwt `target/`, `.data/vcf-load`) | Everything a container writes is in a named volume; build contexts are copied in with `docker cp`. |
| Relative input paths go to `docker -v` and silently become named volumes | Paths are made absolute and checked before anything runs. |
| The loader, columnmeta and weights containers have fixed names, so a stale one blocks the next run | Each run's containers get unique names and `--rm`. |
| `compose.sh dev up dictionary` builds only the first service | Each dev variant declares its services. |
| Teardown: `reset.sh`, `uninstall.sh` | `down`, `reset [--keep-db]`, `destroy`. `destroy` removes only what pic-sure created (`manifest.json`), never your own files, shared data sets or other stacks. |

## Data loading

| Bash All-in-One | pic-sure v2 |
|---|---|
| Orchestrator steps run under `if !`, which turns off `set -e`, so hydrate, the dictionary CSV, facets, promote and two of three VCF loaders can fail silently | Every error stops the command and names the step to re-run. |
| `etl.sh` and `load-demo-data.sh` load data in about ten different ways | One loader serves `data demo` and `data load-phenotype`. |
| `grep -q healthy` also matches "unhealthy"; `docker logs \| grep -q` can SIGPIPE under pipefail | Health and readiness are read exactly, in Go. |
| Archive support differs between bsdtar and GNU tar | Go's archive libraries detect CSV, gzip, tar and zip by content, the same on every OS. |
| The atomic `load-vcf` doesn't validate `--partition` | One implementation, always validated (`^[A-Za-z0-9_-]+$`). |
| The genomic backup goes inside the genomic volume, where HPDS loads it as a partition | `--backup` keeps it in the staging volume, replaced only once the new one is complete. |
| `load-rdbms` (SQLLoader) | Dropped in v2.0: its `sql.properties` credentials ended up in the data volume and in published sets. `--input-dir` (SequentialLoader) is kept. |
| Shared-data `publish --force` deletes before copying, and inputs such as `sql.properties` can leak into a published set | Data sets are immutable: publish a new name instead. Inputs are never copied. |
| The release's `DICTIONARY_ETL` key is ignored | All four components, dictionary-etl included, are pinned by the release. |
