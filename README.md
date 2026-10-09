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

> **v1.** pic-sure v1 (the wrapper around the bash All-in-One scripts)
> lives on the `wrapper` branch and is frozen: it gets no further releases. v2 doesn't read v1 or bash All-in-One installs; create
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
  `--set hpds.java_opts=-Xmx2g`. The demo data loads with HPDS at 1 GB
  and a 1 GB loader heap (`data demo --heap 1024`; the default loader heap
  is 4096 MB), which is what the nightly end-to-end tests use. Besides HPDS
  the stack runs about 4 GB of other services.

`pic-sure doctor` checks all of this.

## Install

```sh
curl -fsSL https://raw.githubusercontent.com/JamesPeck/pic-sure-cli/main/install.sh | bash
```

That installs the newest stable v2 release into `~/.local/bin`, creating
it if needed. If that directory isn't on your `PATH`, the installer says so
and prints the line to add. It needs no terminal, and exits non-zero if
anything fails. Options go after `bash -s --`:

```sh
curl -fsSL https://raw.githubusercontent.com/JamesPeck/pic-sure-cli/main/install.sh \
  | bash -s -- --bin-dir /usr/local/bin --version v2.0.0
```

| Option | Default |
|---|---|
| `--bin-dir DIR` | `~/.local/bin` |
| `--version vX.Y.Z` | the newest stable v2 release |
| `--repo OWNER/NAME` | `JamesPeck/pic-sure-cli` |

The installer checks the archive's SHA-256 against the release's
`checksums.txt`. If `cosign` is installed (**cosign 3.0 or newer**; older
releases can't read the release's signature bundle), it also verifies
`checksums.txt` against its Sigstore signature and stops if that fails.
cosign fetches Sigstore's trust root each time, so it needs the network
(through `HTTPS_PROXY` if you use a proxy). Without cosign, or with an
older one, it warns and relies on the checksum; set
`PIC_SURE_REQUIRE_SIGNATURE=1` to make that a failure instead. It ends by
printing the commands to verify the release by hand.

```sh
curl -fsSL https://raw.githubusercontent.com/JamesPeck/pic-sure-cli/main/install.sh \
  | PIC_SURE_REQUIRE_SIGNATURE=1 bash
```

Later, `pic-sure self-update` installs the newest stable v2 release with
the same checks (`--to VERSION` for a specific one). It honours
`PIC_SURE_REQUIRE_SIGNATURE` too, as does `--require-signature`, and
inside a stack cosign uses the stack's proxy.

**From source** (Go 1.26, or let `GOTOOLCHAIN=auto` fetch it):

```sh
git clone https://github.com/JamesPeck/pic-sure-cli.git
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
wizard ask for all of this; see [The TUI](#the-tui).

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

## The TUI

Run `pic-sure` with no arguments on a terminal to open the TUI. It works on
the stack containing the current directory, or on `--stack DIR`. Anywhere
else it offers to create a stack in that directory. It doesn't open when
stdin or stdout isn't a terminal, or with `--yes`, `--non-interactive`,
`--json` or `--plain`; `pic-sure` prints its help instead.

The first screen's menu depends on the directory:

| The directory holds | The menu offers |
|---|---|
| No stack | **Set up PIC-SURE**: the setup wizard. **Preflight check**: `doctor` on the host and Docker. |
| A stack whose init didn't finish | **Resume setup**: init again with the stack's `pic-sure.yaml`, from the step that failed. **Developer options…** |
| A stack | **Dashboard**. **Update**: `update`, after asking. **Load your data…**: the load wizard. **Developer options…** |

Move with the arrow keys or `j`/`k`, choose with Enter, go back with Esc
and quit with `q`.

- **Setup wizard.** It asks for what `init`'s flags would set: the stack's
  name (suggested from the directory's), the release-control branch and
  frontend theme, the auth mode and admin email, the Auth0 application
  outside open mode, the ports (free ones are filled in), a local or
  remote database, HPDS's JVM options and a proxy. It then shows a summary
  and asks before creating anything. On yes it runs init and shows its
  steps. Esc or Ctrl-C leaves the wizard, asking first if you changed
  anything. If init fails, fix the cause and pick **Resume setup**, or
  **Set up PIC-SURE** again if it failed before writing `pic-sure.yaml`:
  the wizard reopens with your answers.
- **Developer options.** Each item runs one command:

  | Item | Runs |
  |---|---|
  | Preflight check | `doctor` |
  | Preview update | `update --dry-run`, after asking |
  | Switch release branch… | `config set release.branch B`, prefilled with the current branch; choose Update afterwards |
  | Run migrations | `migrate`, after asking |
  | Load demo data… | the load wizard, on its demo datasets |
  | Rebuild dictionary… | `dictionary hydrate` or `dictionary weights` |
  | Dev mode on… / off… | `dev on SERVICE` / `dev off SERVICE`, from a list of the services `dev list` shows |
  | Reset… | `reset`, keeping or removing the database |
  | Destroy… | `destroy` |

  Reset and Destroy make you type the stack's name, as on the dashboard.
- **Dashboard.** The stack's services and their state, a status summary
  (config, version, images, migrations, with a warning when one can't be
  checked), and the logs of the selected service.

  | Key | Does |
  |---|---|
  | ↑/↓ or `j`/`k` | Select a service; the log pane follows it. |
  | PgUp/PgDn, Home/End | Scroll the logs. |
  | `h` | Check the gateway, HPDS's data and the CSP. Takes up to a minute. |
  | `r` | Restart the selected service. |
  | `u` | Run `update`. |
  | `m` | Run `migrate`. |
  | `l` | Open the load wizard. |
  | `R` | Run `reset`, keeping or removing the database. |
  | `X` | Run `destroy`. |
  | Esc / `q` | Back to the menu / quit. |

  `r`, `u` and `m` ask first. `R` and `X` make you type the stack's name.
- **Load wizard.** It loads a phenotype CSV file or directory
  (`data load-phenotype`), a demo dataset (`data demo`) or genomic VCFs
  (`data load-genomic`). It asks for what the command's flags would set,
  with a file browser for paths, and asks before it starts. In the browser,
  → or `l` opens the highlighted directory and ← or `h` goes up. Where the
  wizard asks for a directory, Enter uses the one shown at the top. A
  directory pic-sure can't read stays closed, with the error below the list;
  on macOS, folders such as Documents or Downloads need the terminal to have
  file access in System Settings, Privacy & Security.

Commands run from the TUI show their steps on a run screen; Enter goes back
when they finish. Run from a shell, long commands such as `init`, `up`,
`update` and `data` show the same live checklist and leave it in the
scrollback. Ctrl-C asks to confirm, a second Ctrl-C cancels and lets the
command clean up, and a third while it stops quits at once with exit 130,
in the TUI too. A forced quit names the step that was running; containers
it started may still be running, so check with `pic-sure status`.

| To get | Use |
|---|---|
| Plain timestamped lines instead of the live view | `--plain`; also the default off a terminal, or when `CI` is set (to anything but `false` or `0`) |
| No animation | `--no-animations` or `PIC_SURE_NO_ANIMATIONS=1`. Off by default over SSH; `PIC_SURE_NO_ANIMATIONS=0` turns it back on |
| No colour | `NO_COLOR` set to any value |

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
pic-sure config set auth.auth0.client_id "$AUTH0_CLIENT_ID"   # first: required mode needs it
pic-sure config set auth.mode required
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
- Once the config sets a proxy, it replaces the one in your shell
  (`HTTPS_PROXY` and the rest) for everything pic-sure runs. With only
  `proxy.http` set, https goes direct.
- On Linux behind a proxy that intercepts TLS, export `SSL_CERT_FILE` (or
  `SSL_CERT_DIR`) and `GIT_SSL_CAINFO` naming your CA bundle; pic-sure
  passes them on to the programs it runs, such as git and cosign. Put the
  CA in `certs/trust/` for the services.
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

**Copies and moves.** A stack's Docker resources carry its directory and a
random stack ID that `init` records in `.pic-sure/state.json`. A copy of
the directory (`cp -r a b`) has the same name and ID, so in `b` every
command that would change the stack's containers, volumes or data refuses
with exit 3, naming `a`; `status`, `ps`, `logs` and `doctor` say so too.
`destroy` in `b` removes only `b`'s files and leaves `a` alone. To run two
stacks, `init` a second one with another `--name`. A moved stack
(`mv a c`) adopts the resources labelled with its old directory, with a
note, once nothing at `a` holds a stack with its ID. Docker can't relabel
a volume, so the moved stack's volumes keep `a` in their labels for good:
pic-sure renders each existing volume with the labels it has, so compose
never asks to recreate it. `pic-sure compose --` doesn't re-render, so
if the last render doesn't match a volume's labels (a stack rendered by an
older pic-sure), it refuses with exit 3 until `pic-sure up` re-renders.
`pic-sure compose --` always runs on the stack's own project: it refuses
`-p`, and a top-level `name:` in a file added with `-f` or in
`overrides/*.yaml` doesn't change it (`doctor` warns about the latter).
A stack directory
deleted without `destroy` leaves its containers and volumes behind;
`init` at the same path and name refuses until you remove them, and
prints the `docker` commands that do.

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
- "stack belongs to another user" (exit 3): pic-sure found a stack in the
  current directory or above it, or stack files init would take over, that
  you don't own, such as some planted in `/tmp`. It won't run another
  user's compose files without being told to. If you trust the stack, name
  it with `--stack DIR`.

## For AI agents

[skills/pic-sure/SKILL.md](skills/pic-sure/SKILL.md) is an
[Agent Skill](https://agentskills.io) that teaches a coding agent with a
shell (Claude Code, Codex and the like) to install, run, load data into,
check, update and tear down stacks with pic-sure: the rules, the standard
flow, what to do for each exit code, and troubleshooting. To install it,
copy `skills/pic-sure/` into the agent's skills directory. For Claude
Code that's `~/.claude/skills/pic-sure/`, or `.claude/skills/pic-sure/` in
a project:

```sh
mkdir -p ~/.claude/skills/pic-sure
curl -fsSL -o ~/.claude/skills/pic-sure/SKILL.md \
  https://raw.githubusercontent.com/JamesPeck/pic-sure-cli/main/skills/pic-sure/SKILL.md
```

## Development

Package layout, the shared interfaces and how to add a command or an
operation are in [docs/architecture.md](docs/architecture.md). Requires
Go 1.26. CI and releases build with the patch release on go.mod's
`toolchain` line, which `GOTOOLCHAIN=auto` fetches.

| Target | Does |
|---|---|
| `make build` | build `bin/pic-sure` with version ldflags |
| `make test` | `go test ./...`: unit tests, goldens, testscript scenarios and, outside CI, the PTY smoke tests (set `PICSURE_PTY_TEST=1` to run those in CI) |
| `make lint` | golangci-lint, at the version pinned in the Makefile |
| `make check` | gofmt check, `go vet`, lint and test: CI's main job, on Linux and macOS |
| `make vulncheck` | govulncheck over the code and its dependencies; needs the network, and its standard-library findings depend on the Go that runs it |
| `make docs` | regenerate the [command reference](docs/commands/README.md) from the cobra help; CI fails when it's stale (`make docs-check`) |
| `make compose-check` | `docker compose config` over every render golden |
| `make clean-test-docker` | remove what a killed test run left in Docker: everything labelled `org.hms-dbmi.picsure.test=1`, and nothing else |
| `make snapshot` | a local, unsigned dry run of the release into `dist/` |
| `make install-test` | `make snapshot`, then `install.sh` against it |

A few tests in `internal/ops` build small images, create volumes and run
short-lived containers on the local Docker engine. They run by default and
skip under `go test -short` or when `docker info` fails. They label
everything they create `org.hms-dbmi.picsure.test=1` and remove it when they
finish; after a killed run, `make clean-test-docker` removes the leftovers.

Other tests are opt-in, or change behaviour, through environment variables:

| Variable | Effect |
|---|---|
| `PICSURE_PTY_TEST=1` | run the PTY smoke tests in `smoke/` under CI, which skips them otherwise |
| `PICSURE_REQUIRE_COMPOSE=1` | fail, instead of skip, the render goldens' compose checks without `docker compose` (`make compose-check`) |
| `PICSURE_AIO_DIR` | the All-in-One checkout the AIO drift tests in `internal/render` and `internal/catalog` read, when it isn't beside this repo; without either they skip |
| `PICSURE_REAL_IMAGE_BUILD=1` | build the real frontend and dictionary-etl images from `main` into the user's cache, and keep them |
| `PICSURE_REACTOR_SHA`, `PICSURE_REACTOR_FORCE=1` | build the pic-sure backend images at that commit into the user's cache; `FORCE` rebuilds up-to-date ones |
| `PICSURE_HPDS_ETL_IMAGE` | load the genomic test fixture with this HPDS ETL image |
| `PIC_SURE_TEST_DEMO_DOWNLOADS=1` | download the demo files (56 MB) and check them against their pins |
| `PICSURE_IT_PROJECT`, `PICSURE_IT_KEEP`, `PICSURE_IT_RELEASE_COMMIT` | for the `-tags integration` database tests: the compose project, keep it afterwards, and the release-control commit |
| `PICSURE_TUI_INIT_DIR`, `_PORTS`, `_BRANCH`, `_CAPTURE` | drive the setup wizard to a real stack in that directory |
| `PICSURE_TUI_LANDING_DIR`, `_NAME`, `_CAPTURE` | drive the landing's actions on a throwaway stack, which it destroys |
| `PICSURE_TUI_LOAD_DIR`, `_FLOW`, `_CAPTURE` | load data into a running stack through the load wizard |
| `PICSURE_TUI_DASH_DIR`, `_CAPTURE`, `_DESTROY` | drive the dashboard on a running stack, and destroy it if `_DESTROY` names it |

The `_CAPTURE` variables name a file the test writes the screens it saw to.

End-to-end tests against real Docker stacks are the `scripts/e2e-*.sh`
scripts, run nightly by `.github/workflows/e2e.yml`; `scripts/e2e-lib.sh`
documents their settings. They build images and take a long time.

CI (`.github/workflows/ci.yml`) runs `make check` on Linux and macOS,
`make compose-check`, `make docs-check`, `make vulncheck`, shellcheck over
every script, and the snapshot release with the `install.sh` tests. It also
runs nightly, so a newly published vulnerability fails it.

Releases are tagged `v2.*`. The release workflow refuses a tag whose commit
isn't on `main`, then runs all of CI against it. GoReleaser builds the
`linux/darwin × amd64/arm64` archives, signs `checksums.txt` with cosign
(keyless) with cosign 3, and publishes them with SBOMs and build
attestations. Before publishing it installs its own archive with
`install.sh` and `PIC_SURE_REQUIRE_SIGNATURE=1`. Afterwards an
informational job verifies the published bundle with older cosign
releases (2.4.1 through 3.0) and lists the results in its job summary,
the evidence for ever lowering the cosign 3.0 minimum.
