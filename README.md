# pic-sure: install and run PIC-SURE All-in-One

`pic-sure` is one Go binary that installs, runs, updates, loads data into
and tears down [PIC-SURE](https://pic-sure.org) All-in-One stacks on macOS
and Linux (amd64 and arm64). It drives your `docker` and `git` directly,
with no bash scripts and no checkout of `pic-sure-all-in-one`. Several
stacks can run side by side on one host.

- On a terminal, `pic-sure` with no arguments opens a TUI: a setup wizard
  in an empty directory, and a dashboard with day-2 actions on a stack.
- Every action is also a plain command that runs without a terminal,
  offers `--json`, and has documented exit codes.

This README is for people running stacks. If you're scripting pic-sure or
are an agent, read [docs/agents.md](docs/agents.md) as well. Every command
and flag is in the [command reference](docs/commands/README.md).

> **v1.** This is the `v2` branch. pic-sure v1 (the wrapper around the
> bash All-in-One scripts) lives on `main` and is frozen: it gets no
> further releases. v2 doesn't read v1 or bash All-in-One installs; create
> new stacks with `pic-sure init`. What changed compared with the bash
> All-in-One is in
> [docs/differences-from-bash-aio.md](docs/differences-from-bash-aio.md).

## Requirements

- macOS or Linux. Windows isn't supported.
- Docker with the compose plugin, at least **compose 2.29.0**, and
  **buildx 0.17.0** (images are built from source). Docker Desktop,
  Colima, OrbStack and a native Linux engine all work.
- `git`.
- Disk: about 20 GB free for the images and caches (`doctor` warns below
  20 GB and fails below 5 GB).
- Memory: HPDS's default heap is 16 GB (`-Xmx16g`). Give the Docker VM
  enough for it, or choose a smaller heap at init with
  `--set hpds.java_opts=-Xmx2g` (enough for the demo data).

`pic-sure doctor` checks all of this.

## Install

```sh
curl -fsSL https://raw.githubusercontent.com/JamesPeck/pic-sure-cli/v2/install.sh | bash
```

That installs the newest stable v2 release into `~/.local/bin`, creating
it if needed. If that directory isn't on your `PATH`, the installer says so
and prints the line to add. It needs no terminal, and exits non-zero if
anything fails. Options go after `bash -s --`:

```sh
curl -fsSL https://raw.githubusercontent.com/JamesPeck/pic-sure-cli/v2/install.sh \
  | bash -s -- --bin-dir /usr/local/bin --version v2.0.0
```

| Option | Default |
|---|---|
| `--bin-dir DIR` | `~/.local/bin` |
| `--version vX.Y.Z` | the newest stable v2 release |
| `--repo OWNER/NAME` | `JamesPeck/pic-sure-cli` |

The installer checks the archive's SHA-256 against the release's
`checksums.txt`. If `cosign` is installed (**cosign 2.4 or newer**; older
2.x releases can't read the release's signature bundle), it also verifies
`checksums.txt` against its Sigstore signature and stops if that fails.
Without cosign it warns and relies on the checksum. It ends by printing
the commands to verify the release by hand.

Later, `pic-sure self-update` installs the newest stable v2 release with
the same checks (`--to VERSION` for a specific one).

**From source** (Go 1.26, or let `GOTOOLCHAIN=auto` fetch it):

```sh
git clone --branch v2 https://github.com/JamesPeck/pic-sure-cli.git
cd pic-sure-cli
make build                  # writes bin/pic-sure
```

## Quick start

A stack is a directory. Create one, load demo data and check it:

```sh
pic-sure init ~/picsure/demo --name demo --auth-mode open \
  --admin-email you@example.com --auto-ports --set hpds.java_opts=-Xmx2g
cd ~/picsure/demo
pic-sure data demo          # NHANES; also synthea, 1000genomes or all
pic-sure status
```

- `init` checks the host, fetches the release, writes `pic-sure.yaml` and
  the secrets, builds the images, sets up TLS and the database, and starts
  everything. **The first init builds every image from source and takes
  30 to 60 minutes**; later stacks reuse the images. It ends by printing
  the URL, e.g. `https://localhost:8443`. The certificate is self-signed,
  so your browser will warn once.
- `--name` (the stack's name, fixed once created) and `--admin-email` are
  always required. Outside open mode so are `--auth0-client-id` and the
  client secret.
- `--auth-mode open` needs no Auth0 application. Anyone who can reach the
  URL can use it, and the ports are published on every network interface,
  so keep it to local evaluation on a trusted network. See
  [Auth0](#auth0-and-auth-modes) for the other modes.
- `--auto-ports` takes the first free pair from 8080/8443, 8081/8444 and
  so on. Without it, init uses 80 and 443 and fails (exit 3) if they're busy.
- Every other command acts on the stack containing the current directory,
  or on `--stack DIR`.
- `data demo` downloads the dataset, loads it, rebuilds the search
  dictionary and returns once HPDS is healthy again. It takes a few
  minutes.
- `status` shows the services, the release, migrations and the stack's
  URL with what to register in Auth0. `status --deep` also probes inside
  the containers, including whether HPDS has data loaded.

On a terminal you can also run `pic-sure` in an empty directory and let the
wizard ask for all of this.

If init fails, fix the cause and run the same command again: it resumes
from the step that failed. It reuses the `pic-sure.yaml` it already wrote,
so to change a setting use `pic-sure config set KEY VALUE` rather than a
different flag.

### Everyday commands

| Command | Does |
|---|---|
| `pic-sure up` | Start the stack and converge it: builds missing images, re-renders, migrates, seeds. Safe to re-run. |
| `pic-sure down` | Stop the containers; data is kept. |
| `pic-sure restart [SVC...]`, `ps`, `logs [-f] [SVC]` | The usual compose verbs, for this stack. |
| `pic-sure update` | Move to the newest release: plan, back up the config, build, migrate, restart what changed. `--dry-run` shows the plan. |
| `pic-sure data load-phenotype --file F` | Load your own phenotype CSV (plain, `.gz`, `.tar.gz` or `.zip`; `--entry` picks a file in an archive) or `--input-dir D`, then rebuild the dictionary. |
| `pic-sure data load-genomic --partition P --vcf-index F` | Load VCFs into a genomic partition; `--promote` and `--enable-profile` make it live. |
| `pic-sure config show` / `set KEY VALUE` / `edit` | Read and change `pic-sure.yaml`, validated. Run `up` afterwards to apply. |
| `pic-sure doctor [--network]` | Check the host, Docker, the stack and the network. |
| `pic-sure support-bundle` | A redacted diagnostics archive to attach to an issue. |

The [command reference](docs/commands/README.md) has every command.

## The stack directory

```
<stack>/
  pic-sure.yaml        the config: edit with pic-sure config, or by hand
  certs/trust/         optional extra CA certs for the services' truststore
  certs/tls/           optional server certificate (tls.mode: provided)
  overrides/*.yaml     optional compose overrides, applied last
  .pic-sure/           managed by pic-sure; don't edit
    state.json         the release, component commits and images the stack runs
    manifest.json      every path pic-sure created (what destroy removes)
    secrets.yaml       generated passwords and keys (mode 0600)
    render/compose.yaml   the rendered compose file (no secret values)
    logs/              a debug log per run, secrets redacted
```

There is no `.env`. Secrets live only in `.pic-sure/secrets.yaml` and reach
the containers through the environment. For raw compose access use
`pic-sure compose -- ARGS`, which supplies the same environment
(`pic-sure compose -- exec hpds sh`).

## Auth0 and auth modes

`auth.mode` (`--auth-mode` at init) is one of:

- `required` (the default): nobody gets in without logging in through Auth0.
- `open`: the Discover page works without logging in; no export or API.
- `explore`: the query builder works without logging in; export asks for a
  login.

`open` and `explore` make the data queryable without a login.

Outside open mode, init needs `--auth0-client-id` and the client secret,
which is read from stdin only (it's never a command-line argument):

```sh
printf '%s\n' "$AUTH0_CLIENT_SECRET" | pic-sure init ~/picsure/prod --name prod \
  --admin-email you@gmail.com --auth0-client-id "$AUTH0_CLIENT_ID" \
  --auth0-client-secret-stdin
```

The secret must be at least 32 bytes. With Auth0 the admin email must be a
Google account. Each stack's HTTPS origin must be registered in the Auth0
application: `init` and `status` print the exact callback URL, logout URL
and web origin. Stacks on different ports need separate entries.

In open mode with no secret given, init generates a random one, since
PSAMA signs its tokens with it. To move such a stack to `required` later:

```sh
pic-sure config set auth.mode required
pic-sure config set auth.auth0.client_id "$AUTH0_CLIENT_ID"
printf '%s\n' "$AUTH0_CLIENT_SECRET" | pic-sure --yes secrets rotate auth0-client-secret
pic-sure up
```

Until the real secret is supplied, `up` and `update` refuse to run (exit 3)
and say so.

## Several stacks on one host

Each stack has its own name, ports, containers, volumes and networks; images
and the build cache are shared. Give each a different `--name` and either
explicit ports or `--auto-ports`. Ports another stack's `pic-sure.yaml`
records count as taken even while that stack is stopped, so concurrent
inits get different ports. If a port `--auto-ports` chose is taken by the
time the stack starts, init picks again once. Ports you give explicitly are
never changed.

A data set loaded into one stack can be published and mounted read-only by
others: `pic-sure shared-data publish NAME` in the source stack, then
`init --hpds-data shared:NAME` for the new stack. Data sets are immutable;
publish a new name to change one.

## Proxy

If outbound traffic must go through a proxy, set it at init
(`--http-proxy`, `--https-proxy`, `--no-proxy`) or later with
`pic-sure config set proxy.https http://proxy.example.com:3128` and `up`.
pic-sure wires it into every path that goes out: its own downloads, git,
image builds, Maven, node, and the services' JVMs.

- Proxy URLs start with `http://`, including `proxy.https`: the proxy is
  spoken to in plain HTTP. An `https://` URL is rejected.
- `proxy.https` doesn't fall back to `proxy.http`. Nearly everything is
  HTTPS, so set `proxy.https`.
- Credentials in the URL work for everything except the services' JVMs
  (psama's calls to Auth0). Allow-list Auth0 on the proxy or put it in
  `no_proxy`.

Image pulls are made by the Docker daemon, which pic-sure can't configure.
`pic-sure doctor --network` tries a pull through it and, if that fails,
prints how to set the daemon's proxy for Docker Desktop, systemd, Colima or
OrbStack. [docs/testing-proxy.md](docs/testing-proxy.md) describes the
proxy end-to-end test.

## Developing PIC-SURE with a stack

- `pic-sure init --source pic-sure="$HOME/src/pic-sure"` (or later,
  `pic-sure config set components.pic-sure.source PATH` and `pic-sure build`)
  builds a component from your checkout instead of the release.
- `pic-sure dev list` shows the services with a dev mode. `pic-sure dev on
  hpds` (once its component has a source) rebuilds it from your checkout
  and gives it a JDWP debug port on 127.0.0.1; `dev on httpd-hmr` serves
  the frontend from your checkout with live reload. Run `dev on` again
  after changing the source. `dev off SERVICE` turns it off.
- A source applies to the whole component, so after `dev off` the service
  still runs your build. To go back to the release images, unset the
  source and run `up`:
  `pic-sure config set components.pic-sure.source '' && pic-sure up`.

## Updating

`pic-sure update` moves the stack to the head of its release branch
(`release.branch`; `--release-commit SHA` for a specific one). Run
`update --dry-run` first to see the plan. If the release needs a newer
pic-sure, update asks on a terminal; without one it exits 5 unless you pass
`--self-update`. `--yes` alone never replaces the binary.

A newer pic-sure still runs read-only commands on an older stack, and
points mutating ones at `pic-sure update` when the stack's config needs
migrating. An older pic-sure refuses mutating commands on a stack a newer
one rendered (exit 5).

## Tearing down

| Command | Removes | Keeps |
|---|---|---|
| `pic-sure down` | containers | everything else |
| `pic-sure reset [--keep-db]` | containers and the stack's volumes: HPDS, dictionary, staging, certs, truststore, and the database unless `--keep-db` | config, secrets, logs and TLS sources: `up` sets it up again, empty |
| `pic-sure destroy [--prune-images]` | containers, every volume and dev image of this stack, and the files pic-sure created in the stack directory | files you added (listed), shared data sets, other stacks |

`reset` and `destroy` ask you to type the stack name on a terminal;
otherwise pass `--yes`. Images are shared between stacks, so `destroy`
leaves them; `pic-sure cache prune` removes images, source trees and
downloads that no stack uses (`--dry-run` first to see what). It keeps
anything made in the last hour, so right after a first build it frees
little. The cache is
in `$XDG_CACHE_HOME/pic-sure` (default `~/.cache/pic-sure`).

To uninstall pic-sure itself: `destroy` each stack, `cache prune`, then
delete the binary (`~/.local/bin/pic-sure`) and the cache directory.

## Troubleshooting

- Start with `pic-sure doctor` (add `--network` for connectivity) and
  `pic-sure status --deep`.
- Each mutating command writes a debug log under `.pic-sure/logs/`, with
  secrets redacted.
- A failed converge names the step that failed; fix the cause and re-run
  the same command. Completed steps are skipped.
- `pic-sure support-bundle` writes `pic-sure-support-<name>-<time>.tar.gz`
  with status, doctor, recent logs, compose output and the config. Every
  value from `secrets.yaml`, the HPDS key and the admin email are replaced
  with `[REDACTED]`, but secrets shorter than 4 bytes are only redacted
  where they stand alone, and the command warns when there are any. Check
  the archive before sharing it.
- "port is already allocated" or exit 3 at init: the ports are busy. Use
  `--auto-ports` or other `--http-port`/`--https-port`.
- Another pic-sure command holds the stack lock: wait for it, or pass
  `--wait-lock`.

## Development

Package layout, the shared interfaces and how to add a command or an
operation are in [docs/architecture.md](docs/architecture.md). Requires
Go 1.26 (`GOTOOLCHAIN=auto` fetches it).

| Target | Does |
|---|---|
| `make build` | build `bin/pic-sure` with version ldflags |
| `make test` | `go test ./...`: unit tests, goldens, testscript scenarios and, outside CI, the PTY smoke tests (set `PICSURE_PTY_TEST=1` to run those in CI) |
| `make lint` | golangci-lint, at the version pinned in the Makefile |
| `make check` | gofmt check, `go vet`, lint and test: what CI runs |
| `make docs` | regenerate the [command reference](docs/commands/README.md) from the cobra help; CI fails when it's stale (`make docs-check`) |
| `make compose-check` | `docker compose config` over every render golden |
| `make snapshot` | a local, unsigned dry run of the release into `dist/` |
| `make install-test` | `make snapshot`, then `install.sh` against it |

End-to-end tests against real Docker stacks are the `scripts/e2e-*.sh`
scripts, run nightly by `.github/workflows/e2e.yml`; `scripts/e2e-lib.sh`
documents their settings. They build images and take a long time.

Releases are tagged `v2.*`: GoReleaser builds the `linux/darwin ×
amd64/arm64` archives, signs `checksums.txt` with cosign (keyless), and
publishes them with SBOMs and build attestations.
