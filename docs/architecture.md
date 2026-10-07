# pic-sure v2 architecture

pic-sure v2 is one Go binary that installs and operates PIC-SURE All-in-One
stacks by driving the user's `docker` and `git`. This document covers the
package layout, the interfaces packages share, and the patterns for adding
a command or an operation. References like §10.1 or D17 point into the v2
spec, which is kept outside this repo while v2 is built.

v2 is being built as numbered tickets, many in parallel. Every package below
has its own section, and the ticket that implements a package fills in its
section. Edit only your own.

## Working in this tree

- **One owner per file.** Each command group has its own file in
  `internal/cli`, each operation its own file in `internal/ops`, and each
  command group its own testscript under `cmd/pic-sure/testdata/script/`.
  `data` is split further (`data_demo.txtar` and so on, with a constructor
  per subcommand in `data.go`) because different tickets implement its
  subcommands.
  `internal/cli/root.go` already registers every command, so you shouldn't
  need to edit it.
- **Shared code goes down, not sideways.** A helper that several operations
  need belongs in the package it wraps (the compose adapter, the docker
  engine), not in a shared file in `internal/ops`.
- **Rebase conflicts.** In `go.mod`/`go.sum`, keep both sides' requirements
  and run `go mod tidy`. In this file, keep both sections.
- `make check` runs the gofmt check, `go vet`, golangci-lint (pinned in the
  Makefile) and `go test ./...`. CI runs the same target.

## Conventions

- **Exit codes** (§10.4) come from `internal/exitcode`. Return
  `exitcode.Usage`, `Precondition`, `ConfirmRequired`, `Incompatible` or
  `Failed` from a command; any other error exits 1. Errors cobra raises
  while parsing the command line (unknown command or flag, wrong argument
  count, missing required flag, unknown help topic) exit 2. SIGINT and
  SIGTERM cancel the command's context, and the CLI then exits 128+N even
  if the command returned cleanly.
- **Secrets never go in argv** (§6.3). A secret reaches a container as a bare
  `-e NAME` in `docker.Cmd.Argv` plus `NAME=value` in `Cmd.Env`, or on
  `Cmd.Stdin`, or in a 0600 file. Runners never log env values or stdin.
- **Operations touch the world only through `ops.Deps`**: no `time.Now`,
  `crypto/rand`, `os.Stdout` or `exec.Command` in an operation, so tests
  can substitute fakes for all of it.
- **Output goes through events.** Operations emit events to `Deps.Sink`;
  they never print. The output mode (TUI, plain, NDJSON) is the cli layer's
  business (ticket 004).
- **Tests.** Unit tests use the fake runner, never real Docker. CLI
  behaviour is tested with testscript. A test that needs a real Docker
  daemon skips itself when there isn't one.

## Adding a command

Commands are already registered, each returning
`exitcode.Failed("not implemented (ticket NNN)")`. To implement one, edit its
constructor in its group's file, for example `newUpCmd` in
`internal/cli/up.go`: add its flags, and make `RunE` build the dependencies
with `a.newDeps()`, call the operation, and hand the result to the output
layer: `return a.finish(report, text)`, or `a.printReport(report, text)`
for a read-only report (see `output.go` under internal/cli). Global flags are in
`a.Global`. Then turn the command's testscript
(`cmd/pic-sure/testdata/script/up.txtar`) into real scenarios.

## Adding an operation

An operation is a function in its own file in `internal/ops`. It takes a
context, `*Deps`, the stack it acts on and an options struct, and returns an
error, plus its report if it has one. A converging operation is a list of
steps run by `steps.Run`: each step checks real state first, so a re-run
skips what is already done.

```go
// internal/ops/up.go

type UpOptions struct {
	SkipSteps []string // from --skip-step
}

func Up(ctx context.Context, d *Deps, st *stack.Stack, opts UpOptions) error {
	plan := []steps.Step{{
		ID:    "db",
		Title: "Start the database",
		Check: func(ctx context.Context) (bool, error) { /* is it healthy? */ },
		Apply: func(ctx context.Context, sink events.Sink) error { /* d.Compose... */ },
	}, {
		ID:    "migrate",
		// ...
	}}
	return steps.Run(ctx, d.Sink, plan, steps.Options{Skip: opts.SkipSteps})
}
```

Test it with the fake runner and a recording sink:

```go
func TestUpStartsTheDatabaseBeforeMigrating(t *testing.T) {
	f := fakerunner.New(t)
	f.On(fakerunner.Glob("docker compose * up -d --wait picsure-db"))
	f.On(fakerunner.Glob("docker compose * run --rm -T flyway-init"))
	var rec events.Recorder
	d := &ops.Deps{Runner: f, Sink: &rec, Clock: ops.FixedClock(t0), /* ... */}

	if err := ops.Up(ctx, d, st, ops.UpOptions{}); err != nil {
		t.Fatal(err)
	}
	f.AssertOrder(
		fakerunner.Glob("docker compose * up -d --wait picsure-db"),
		fakerunner.Glob("docker compose * run --rm -T flyway-init"),
	)
}
```

## cmd/pic-sure

Ticket 001. `main` turns the first SIGINT or SIGTERM into a context
cancellation whose cause is `exitcode.Signaled(sig)`, so the command can run
its deferred cleanups and then exit 128+N. A second signal gets the default
action and kills the process at once. It then calls `cli.Execute`. The
version variables are set with `-ldflags` (see the Makefile).

`main_test.go` is the testscript harness. It registers the binary as
`pic-sure` and the fakes from `internal/fakecmd` as `docker` and `git`, then
runs every script in `testdata/script/`. Scripts must `exec` programs
explicitly. The harness adds one command:
`exitcode N PROGRAM [ARGS...]` runs PROGRAM and requires exit status N,
because `! exec` accepts any failure.

## internal/cli

Ticket 001; each command group's file belongs to the ticket that implements
it.

- `app.go`: `App` (build info, global options, streams, and seams for the
  terminal check and the TUI), `Execute`, and the error-to-exit-code
  mapping. A signal received while a command runs decides the exit code,
  even if the command then returns cleanly. With no arguments, pic-sure
  opens the TUI when stdin and stdout are terminals and none of `--json`,
  `--plain`, `--yes` or `--non-interactive` is given. Otherwise it prints
  help.
- `globals.go`: the global flags (§5). `--yes` answers yes to every
  confirmation. `--non-interactive` only forbids prompting, so a
  destructive command still needs `--yes`. `--json` implies
  `--non-interactive`, and `--json` and `--plain` are mutually exclusive.
  `--wait-lock` (007) makes a mutating command wait for the stack lock
  instead of failing.
- `stack.go` (007): `a.openStack(cmd)` finds and opens the stack the command
  acts on (exit 3 when there is none) and applies the version gate for
  `cmd` (009), `a.initDir(args)` resolves init's
  directory, and `a.lockStack(ctx, cmd, st, sink)` takes the stack lock
  for a mutating command: exit 1 if it is held, or a wait with
  `--wait-lock`.
- `gate.go` (009): `commandClasses`, the version-gate class of every
  command (§10.6; a test keeps it complete): read-only, mutating, or
  `update`'s own class. `openStack` runs `a.gate`, so every command that
  opens a stack is gated. A read-only command's warning goes to stderr, so
  `--json` output stays clean. A new command needs a row here.
- `root.go`: registers every command. It wraps each `RunE` so that any
  error raised before a `RunE` starts is reported as a usage error. A
  `PreRunE` that fails for any other reason must return an
  `*exitcode.Error`.
- `helpers.go`: `notImplemented(ticket)`, `newGroup`, and the `help`
  command. A group run without a subcommand, or with an unknown one, is a
  usage error, and so is `help` with an unknown topic.
- `output.go` (004): output mode selection, the run's sink, and how a
  command ends. The mode is JSON for `--json`; plain for `--plain`, when
  stdin or stdout isn't a terminal, or when `CI` is set (`CI=false` and
  `CI=0` don't count); and TUI otherwise. TUI renders as plain until
  ticket 038. `newSink` returns the same sink for the whole run. Only
  `output.go` emits `Result`. A command ends in one of three ways:
  - a streaming command returns `a.finish(report, text)`. When the
    command returns nil, the run ends with a success `Result` (`--json`
    prints it last, with `report` as `data`); in the other modes `text`
    writes the human summary to stdout. A command that used the sink
    gets the success `Result` even without `finish`. A signal that
    arrives before the run ends turns it into a failed one;
  - a read-only command returns `a.printReport(report, text)`, which
    prints one `schema_version: 2` object with `--json`, and no `Result`
    after it, and calls `text` otherwise;
  - any command returns an error. `reportError` prints `pic-sure: ERR` on
    stderr in every mode. Unless `printReport` already printed the
    report, it also emits a failed `Result` with the exit code, the
    message, and the first step whose `StepDone` was `failed`; with
    `--json` that is the last line on stdout. So `doctor` can print its
    report and still exit 1 without a second object. When cobra rejects
    the command line before reaching `--json`, the arguments are scanned
    for it, so the result is JSON whatever the flag order.

  A usage error gets the "Run 'CMD --help' for usage." hint when cobra
  rejected the command line, or when the command wrapped it in
  `withUsageHint` (a missing subcommand or help topic, an unknown key). Other
  usage errors, such as an invalid config file, carry only their message.
  Output the renderer couldn't write (a full disk) fails the run with
  exit 1. `help`, `completion` and `--version` print text even with
  `--json`.
- `deps.go`: `newDeps` assembles `ops.Deps`. Each field comes from a
  constructor in its owner's file: `runner.go` (003), `output.go` (004, which
  also reports errors), `logging.go` (005), `engine.go` (016, landed) and
  `gitclient.go` (018, landed). Until the others land, the runner fails
  every call, the sink discards events and the logger discards logs.

| File | Commands | Ticket |
|---|---|---|
| `init.go` | `init` | 034 |
| `up.go` | `up` | 035 |
| `composeverbs.go` | `down`, `restart`, `ps`, `logs`, `compose` | 026 |
| `status.go` | `status` | 027, 037 (`--deep`) |
| `doctor.go` | `doctor` | 025 |
| `update.go` | `update` | 036 |
| `build.go` | `build` | 031 |
| `migrate.go` | `migrate` | 032 |
| `config.go` | `config show/get/set/edit` | 006 |
| `secrets.go` | `secrets rotate` | 058 |
| `data.go` | `data demo`, `load-phenotype`, `load-genomic` | 046, 042/043/045, 049 |
| `dictionary.go` | `dictionary hydrate/load-csv/load-facets/weights` | 044 |
| `shareddata.go` | `shared-data publish/list/remove` | 050 |
| `dev.go` | `dev list/on/off` | 052 |
| `db.go` | `db bootstrap` | 054 |
| `teardown.go` | `reset`, `destroy` | 056 |
| `cache.go` | `cache list/prune` | 057 |
| `selfupdate.go` | `self-update` | 060 |
| `supportbundle.go` | `support-bundle` | 059 |
| `version.go` | `version` | 001 (done) |

## internal/stack

_Tickets 007 (stack directory, state, manifest, lock), 008 (secrets) and
009 (version gate, config migrations) fill in the rest of this section.
File ownership is in the package doc._ `ops` imports `stack`, so `stack`
must not import `ops`: secret generation takes the `io.Reader` as an
argument.

**Config (ticket 006, `config*.go`).** `Config` is pic-sure.yaml schema 1
(§6.2); `DefaultConfig` has its defaults. YAML is `go.yaml.in/yaml/v3`; use
it for every YAML file so `yaml.Node`s are interchangeable.

- `ConfigDoc` is the file as a YAML document. `ParseConfigDoc`,
  `Stack.ReadConfigDoc` and `NewConfigDoc(*Config)` make one, and
  `Stack.WriteConfig(data)` saves it (keeping the file's mode, 0644 if new). `Set(key,
  value)` parses a command-line value and `SetValue(key, v)` takes a typed
  one; both change only that key, so `Bytes` keeps the user's comments
  (yaml.v3 drops blank lines and normalizes indentation). YAML anchors and
  aliases are rejected. `Node` is the root for migrations (009).
- `doc.Config()`, `ParseConfig` and `Stack.LoadConfig` decode strictly over
  the defaults: an unknown or duplicate key or a wrong type is a problem, a
  missing or null key keeps its default. They then `Validate`. Problems come
  back together in one `*ConfigError` (`Problems{Path, Line, Msg}`); a
  schema other than 1 is a `*SchemaVersionError` instead. Ints and bools
  must be YAML ints and bools: no 8080.5, no yes or on.
- `Validate` is pure. `CheckFiles(dir)` checks the files and directories
  the config names (provided TLS files, component sources).
  `doc.ReadOnlyChanges(before)` refuses edits to `name` and `schema`, even
  when `before` is invalid, unless the old value was empty or unusable.
- `Fields` is the field table for the wizard (039), init's flags (034) and
  the docs (065): key path (`*` matches a map key), kind, init flag, help,
  secret, read-only, enum options and `RequiredWhen`. Secrets are in it
  (with `-stdin` flags) but not in `Config`. `Validate` takes required
  fields and enums from it, and a test keeps it in step with `Config`.
- `Config.Get(key)` returns a value or section; a `*KeyError` is an unknown,
  secret or read-only key.
- `DeriveAuthFlags(mode)` is the auth-mode switch table from the bash
  `picsure_configure_auth`; `Env()` gives it as `NAME=true|false`.

The `config` commands map `*ConfigError` and `*KeyError` to exit 2 and
`*SchemaVersionError` to exit 5. `set` and `edit` take the stack lock;
`edit` holds it while the editor is open.

### Stack directory (007)

A directory is a stack when it holds `pic-sure.yaml` and `.pic-sure/`.
`*stack.Stack` is one open stack; operations take it as an argument.

- **Finding it.** `Find(stackFlag, cwd)` returns `--stack DIR` if set,
  otherwise the nearest stack at or above cwd. `InitDir(arg, stackFlag,
  cwd)` resolves `init [DIR]` (D14): DIR wins, and a `--stack` naming a
  different directory is exit 2. No stack is exit 3, wrapping
  `ErrNotFound`. `Open(dir)` opens an existing stack; `Create(dir)` is
  init's: it makes the directory if needed, then `.pic-sure/`, and starts
  the manifest. `Dir` is absolute with symlinks resolved; `Path(rel)` gives
  the host path for bind mounts and `-f`. Close the stack when done.
- **Writes.** Every write goes through an `os.Root` on the stack dir, so
  nothing escapes it. Paths are slash-separated and relative. Writes and
  removals refuse to pass through a symlinked directory even inside the
  stack, so they can't reach an operator's files. `WriteFile(rel, data,
  perm)` is atomic (temp file, fsync, rename, fsync dir), sets exactly
  `perm` whatever the umask, and replaces a symlink rather than writing
  through it. Its temp files are named `.<name>.tmp-<pid>-<seq>`
  (`IsTempName`); one survives only if pic-sure died mid-write, and isn't in
  the manifest, so destroy should remove those beside recorded paths. It
  needs the parent to exist: `MkdirAll(rel, perm)` first.
  `CreateFile` is for streamed files such as run logs. `ReadFile` and
  `FS()` read with the same confinement.
- **Manifest.** `.pic-sure/manifest.json` lists every path the CLI created
  (`{"path", "type": "file"|"dir"}`; `"."` is the stack dir, when init
  created it). A path is recorded just before it is created, and dropped
  again if creating it fails, so no failure or crash leaves a CLI-made path
  unrecorded (the stack dir itself is the exception: a crash between
  creating it and starting the manifest loses it). It is what `destroy` may
  remove (056). Overwriting a file that was already there doesn't record
  it. `Remove(rel)` deletes a recorded file or empty directory and forgets
  it. It refuses with `ErrNotCreated` a path the manifest doesn't list, or
  one that is no longer the kind (file or dir) the CLI created.
  Updates re-read the file under an flock on `.pic-sure/`, so a command
  that doesn't hold the stack lock (a read-only command's debug log) can't
  drop another's entries.
- **State.** `LoadState`/`SaveState` for `.pic-sure/state.json`:
  `cli_version`, `schema_version` (the pic-sure.yaml schema it was
  rendered with), the release commit, component commits, image tags, the
  last operation and timestamps. No secrets. `StartOperation` and
  `FinishOperation` take the time from the caller (`Deps.Clock`).
  `LoadState` wraps `fs.ErrNotExist` before init saves it.
- **Lock.** `Lock(ctx, LockOptions{Wait, Command, OnWait})` takes an flock
  on `.pic-sure/lock` for a mutating command's whole run. If another
  process holds it, Lock fails with `ErrLocked` naming the holder (command
  and pid, from the lock file), or with `Wait` polls until it is free or
  ctx ends. The kernel drops the lock when the holder exits, so it never
  goes stale. The cli layer wraps this as `a.lockStack` (`--wait-lock`).
- **Labels.** `st.Labels(name)` returns `org.hms-dbmi.picsure.stack=<name>`
  and `org.hms-dbmi.picsure.stack-dir=<Dir>` (`LabelStack`,
  `LabelStackDir`).

### Secrets (008)

`secrets*.go`: `.pic-sure/secrets.yaml` and `.pic-sure/hpds/encryption_key`
(§6.3), both 0600 and written through `WriteFile`, so they are atomic and in
the manifest.

- **`Secret`** is a string type that fmt, slog and encoding/json print as
  `[REDACTED]` (`""` when empty). `string(s)` is the value; YAML encodes
  the value. **`Secrets`** holds every secret of §6.3 as a `Secret`; the
  UUIDs are plain strings and the token expiry a `time.Time`. The local
  (`DBRootPassword`) and remote (`DBRemoteRootPassword`) MySQL root
  passwords are separate fields, so changing `db.mode` never passes one off
  as the other. A struct with `Secret` fields needs a `Format` method like
  `Secrets.Format` (fmt skips field methods when it reports a bad verb), and
  none of them belongs in an unexported field.
- **`EnsureSecrets(d.Rand, EnsureOptions{RemoteDB, Supplied})`** is the
  step for init and other converging commands. It loads secrets.yaml (or
  starts empty), stores each `Supplied` operator secret the stack doesn't
  have yet, fills every empty generated secret, creates the HPDS key file
  for a new stack, and saves if anything changed. It never replaces a
  secret: a supplied value that differs from the stored one is exit 2
  (`secrets rotate` changes secrets). With `RemoteDB` and no remote root
  password it is exit 3, as is a missing key file once secrets.yaml
  exists. A malformed key file is an error too. Neither is ever replaced. Formats follow the
  bash: 24 `[A-Za-z0-9]` characters for DB passwords, 32-byte hex for the
  query, application and logging tokens, 16-byte hex for the obfuscation
  salt, lowercase v4 UUIDs, and 32 lowercase hex characters for the HPDS
  key. The introspection token isn't generated: the caller issues it with
  `jwt.Introspection` and saves it with `SaveSecrets`.
- `LoadSecrets`/`SaveSecrets` read and write secrets.yaml (`LoadSecrets`
  wraps `fs.ErrNotExist` before there is one, ignores unknown keys, and
  reports a malformed file by line only, since yaml's messages can quote
  secrets). `LoadHPDSKey` reads the key and
  checks it is 32 hex characters.
- **User-supplied secrets** (Auth0 client secret, remote DB root password,
  email password) come from stdin or a file, never a flag value.
  `ReadUserSecret(r, "--flag-name")` strips one trailing `\n` or `\r\n` and
  returns exit 2, naming the flag, for any other CR or LF, an empty secret,
  or one over 64 KiB.
- **Redaction.** `LoadSecrets`, `SaveSecrets`, `EnsureSecrets` and
  `LoadHPDSKey` pass every non-empty secret to the function set with
  `SetSecretRegistrar`. The cli layer sets it to `log.RegisterSecrets`
  (ticket 005; until then it is a TODO in `internal/cli/deps.go`).

### Version gate and config migrations (009)

`gate.go` and `migrate.go`. D13: a stack records the pic-sure version and
schema that last rendered it, and each command class reacts to a
difference (§10.6).

- **Migrations.** A `Migration{From, Summary, Apply}` rewrites the
  document's top-level mapping from schema `From` to `From+1`; the
  framework then sets `schema`. A `Registry{Target, Steps}` chains them,
  and `ConfigMigrations()` is the one this pic-sure runs (Target
  `ConfigSchema`, no steps yet). Add a step there when `ConfigSchema` goes
  up. `Plan(doc)` lists the pending steps; a schema above Target, or one no
  step starts from, is a `*SchemaVersionError`. `Migrate(doc)` runs them in
  memory, which read-only commands use to read an older config.
  `Apply(st, now)` (for `update`, under the stack lock) migrates in memory,
  validates when Target is `ConfigSchema`, then copies `pic-sure.yaml` and
  `state.json` as they were to `.pic-sure/backups/<UTC ts>/` and writes the
  config. With nothing pending it does nothing. It leaves state.json's
  `schema_version` to the next render.
- **Comparing versions.** `CompareVersions(a, b)` orders versions as semver,
  ignoring a leading `v`, `+build` and a git-describe suffix (`-N-gSHA`,
  `-dirty`). It reports not-ok for anything else, such as `dev`. 028 uses it
  for PSCLI.
- **The gate.** `st.CheckVersions(cli, reg)` reads state.json (missing is
  fine, corrupt is an error) and pic-sure.yaml's schema. On the result,
  `Newer()` is true if either schema is above this pic-sure's or
  `cli_version` is a later version, and `Pending` lists the migrations.
  `Gate(class)`: `ReadOnly` always runs, with a warning when newer;
  `Mutating` is exit 5 when newer or when migrations are pending ("run
  pic-sure update"); `Migrating` (update) is exit 5 only when newer. A
  config without a readable schema doesn't gate; the command reports it.
- `doc.Raw(key)` reads a value as written, with no defaults. `config
  show/get` use it on a newer schema, which this pic-sure can't decode.

## internal/render

Ticket 020 added the templates; ticket 021 adds rendering and the goldens.

**Templates** (`templates/`, embedded; `templates.go`). Its README maps each
template to the AIO file it was ported from and records the AIO commit, for
the drift job (066), and lists every deliberate difference from AIO.

- `compose/` holds compose fragments, Go `text/template`s that produce YAML.
  `composeFragments(mode, dev)` picks them in merge order: `base`, one per
  catalog `Condition` the stack meets (`local-db`, `local-hpds` or
  `shared-hpds`, `truststore`), `dev/<variant>` for each enabled dev variant,
  and `service-env` (the `services.<name>.env` overrides) last.
  `renderCompose(data, fragments)` executes and merges them into the one
  `compose.yaml`: mappings merge key by key, scalars and sequences replace
  whole, and replacing a mapping with anything else is an error. No fragment
  can remove a key, so whatever only some stacks have lives in its own
  fragment.
- `files/` holds what render writes to `render/files/`: the httpd vhost (a
  template), the Vite dev config, the Flyway scripts, the MySQL init script
  and the demo facet config.
- Templates are executed with `templateData`: names, labels, ports, image
  references, source and render paths, and the config switches the services
  read. It holds no secrets. A secret appears in the compose file only as
  `${NAME}`, one of `secretVars`, and so do `proxyVars` when a proxy is set,
  because proxy URLs can carry credentials. The Compose adapter supplies all
  of them on every call. Every other value is written in literally, quoted
  by `q` so compose doesn't interpolate it.
- JAVA_OPTS is `Java(service, default)`: the configured options or AIO's
  default, then `JVMExtra[service]`, where render puts the proxy properties,
  psama's `trustJavaOpts` and a dev variant's `debugJavaOpts`.
- The Flyway services are in the `migrate` profile: `compose up` skips them,
  `compose run --rm flyway-init` starts what they depend on.
- Compose creates a declared volume only when a service mounts it, so a
  helper that creates a stack volume first (certs, truststore, the HPDS key,
  genomic staging) must give it the stack labels, plus
  `com.docker.compose.project` and `com.docker.compose.volume` so compose
  adopts it without a warning.
- Every bind mount sets `create_host_path: false`: a missing source fails
  the start instead of becoming an empty directory.

`templates_test.go` renders every mode and dev variant and checks the result
against the catalog (services, images, networks, volumes, labels, profiles),
checks that the only `${NAME}` references are secrets, and runs
`docker compose config` over a few renders when the docker CLI is
installed. `aio_test.go` checks that the README maps every template and that
each AIO source still exists in the AIO checkout beside this repo (or
`PICSURE_AIO_DIR`); it skips without one.

_Ticket 021 documents rendering here._

## internal/catalog

Ticket 010. One table per concept, so nothing else keeps its own list of
repos, images or services. It imports nothing from this module. Each table
is a function that returns a fresh copy (`Components()`, `Images()`,
`Services()`, `Networks()`, `Volumes()`, `DevVariants()`), with a
`Lookup<Thing>(name)` beside it. Refer to entries by name, and add new
images, services or volumes here rather than in the package that uses them.

- **Components** (`components.go`): `pic-sure`, `frontend`, `migrations` and
  `dictionary-etl` (constants `PicSure` and so on), each with its GitHub repo
  and build-spec key. `ComponentBySpecKey` maps PSA, PSF, PSM and
  DICTIONARY_ETL back to components; `CLISpecKey` (PSCLI) names the CLI and
  isn't a component. `RepoName()` is the name the host cache keys by.
- **Images** (`images.go`): the 11 reactor images in the bash's build order
  (`ImagesBuiltFrom(catalog.PicSure)`), `pic-sure-httpd` (frontend) and
  `dictionary-etl`, each with its context and Dockerfile; plus the pinned
  third-party images (`mysql`, `postgres`, `flyway`, `alpine`, `maven`,
  `node`) in `Ref`. `node` has no tag there: httpd-hmr takes it from the
  frontend's `.nvmrc`. A built image's repository is `hms-dbmi/<name>`; its
  tag is the build's business. `ServicesUsing(image)` goes the other way.
- **Services** (`services.go`): every compose service with its image,
  networks and volumes, its `Phase` (`PhaseDB`, `PhaseMigrate` one-shots run
  with `compose run --rm`, then `PhaseApp` for `up -d --wait`), `OneShot`, and
  `RestartAfterMigrate` (psama, dictionary-api).
- **Modes.** Some services and volumes exist only in some stacks: no
  `picsure-db` with a remote DB, the shared-HPDS volumes and
  `hpds-genomic-seed` only in shared mode, `truststore` only with custom
  certs. Each entry's `When` is a `Condition` (`Always`, `LocalDBOnly` and
  so on); build a `Mode` from the config and use `ServicesIn(mode)` and
  `Service.VolumesIn(mode)`.
- **Volumes** (`volumes.go`): logical name (the compose key), `Scope`
  (`StackScoped`; `SharedData`, the external `<set>_hpds-data` and
  `<set>_hpds-genomic`; `HostScoped`, the `pic-sure-m2` Maven volume), `Kind`
  (database, data, TLS, logs, cache) for teardown to select on, and what it
  holds. `DockerName(owner)` gives the Docker name. Logs go to named volumes
  instead of host binds: one per service that writes logs, except that
  dictionary-api and dictionary-dump share `dictionary-logs`.
- **Dev variants** (`dev.go`): what `dev on NAME` takes, using the bash's
  overlay names (`psama`, `hpds`, `gateway`, `operations`, `query`,
  `visualization`, `dictionary`, `httpd`, `httpd-hmr`). Each names the
  component whose local source it needs, every service it replaces (its
  build context is that service's image's), and its port as an offset from
  `dev_ports.base` (`DevPortSpan` is 7; `NoPort` for none). Debug port
  offsets follow the order of the bash's ports 5005–5010, so offset 1 is
  free; httpd-hmr's Vite port is offset 6. Container-side ports are
  render's choice.

`aio_test.go` compares the reactor images, the base services' images and
networks, and the dev overlays with the AIO checkout beside this repo (or
`PICSURE_AIO_DIR`), and skips without one. A failure there means AIO has
changed: update the catalog or note the deliberate difference.

## internal/ops

Ticket 001 owns `Deps` (`deps.go`). Each operation is owned by its ticket;
the package doc lists the files.

`Deps` holds the `Runner`, the docker `Engine` (`Docker`), the `Composer`
for the stack being acted on (`Compose`, nil until the command has a
rendered stack), the git `Client` (`Git`), a `Clock`, `Rand` (an
`io.Reader`; `crypto/rand.Reader` in production), the event `Sink` and the
`*slog.Logger`. `SystemClock` is the real clock; `FixedClock` is for tests.
Tickets 016, 017 and 018 filled in `docker.Engine`, `docker.Composer` and
`git.Client` in their own packages, without editing `Deps`.

## internal/steps

Ticket 011, on the `Step` type and `Run` signature from 001.

A `Step` has a stable kebab-case `ID` (users pass it to `--skip-step`), a
`Title`, a `Check` that reports whether the step is already done without
changing anything (nil means always apply), and an `Apply` that does the
work, reports through the sink, and is safe to re-run after a failure.

`steps.Run(ctx, sink, steps, steps.Options{Skip: ...})` emits
`StepStarted` for each step, then one of:

- the step is named by `--skip-step`: a `Warning` ("skipped by
  --skip-step") and `StepDone{skipped}`, without calling `Check` or `Apply`;
- `Check` reports it done: `StepDone{skipped}`;
- otherwise `Apply` runs: `StepDone{ok}` or `StepDone{failed}`.

It stops at the first failure and returns a `*steps.Error` with the step's
`ID` in `Step`. The message names the step and says to re-run; a re-run
resumes there because `Check` skips what is done. The error wraps what
`Check` or `Apply` returned, so a step that returns
`exitcode.Precondition(...)` makes the command exit 3. The cli layer
fills `ErrorInfo.Step` from the first `StepDone{failed}`. When the run was
interrupted between steps, no step failed: `Error.Step` is the next step,
named only in the message.

- **Validation.** Before running anything, `Run` rejects a `Skip` ID that
  names none of its steps with `exitcode.Usage` (exit 2), listing the
  valid IDs. It checks against the list it is given, so an operation passes
  all its steps to one `Run`, concatenating shared lists (`up` reusing
  `init`'s steps 8–12) rather than calling `Run` twice. A malformed list (an
  empty, duplicate or non-kebab-case ID, or a nil `Apply`) is a plain error,
  because it is a bug. `steps.Validate(steps, opts)` runs the same checks
  alone: an operation that prompts, takes a lock or fetches before `Run`
  calls it first, so a mistyped `--skip-step` fails before that work.
- **Cancellation.** `Run` checks `ctx` before each step and before `Apply`,
  and starts no more steps once it's done. It never abandons a running
  `Check` or `Apply`: it waits for it to return, so the step's deferred
  cleanups run first. If it fails, or `Check` finds the step not done, the
  step gets `StepDone{failed}`, and the error has `Interrupted` set and
  wraps `context.Cause(ctx)` instead of the step's own error, so the exit
  code comes from the cause: 130 for a cancellation, 128+N for a signal. A
  step that finishes anyway counts as done, so if it was the last one `Run`
  returns nil. On a signal, the cli reports the command's error when it
  wraps the signal cause, as a `*steps.Error` does, so the message names
  the step to resume from; the exit code is still the signal's. A cleanup
  that has to run commands after cancellation needs a live context:
  `context.WithTimeout(context.WithoutCancel(ctx), d)`.
- **Plan mode.** `steps.Plan(ctx, steps, opts)` returns a `[]Planned`
  (`ID`, `Title`, `Status`, `Error`, with JSON tags) without applying
  anything or emitting events. `Status` is `apply`, `done` (`Check` says
  done), `skipped` (`--skip-step`) or `unknown` (`Check` failed; `Error`
  says why). A failing `Check` doesn't stop the plan. Plan's `skipped`
  means `--skip-step` only; `done` is what `Run` reports as
  `StepDone{skipped}`. `update --dry-run` (036) uses it.

## internal/docker

Ticket 001 defines the runner contract (`runner.go`). Ticket 003 adds the
exec runner, ticket 016 the `Engine`, ticket 017 the `Composer`/`Compose`
adapter.

- `Cmd{Argv, Env, Stdin, Dir}`: `Env` entries are added to the runner's
  base environment, and their values are never logged.
- `Result{Stdout, Stderr, ExitCode}`.
- `Runner.Run(ctx, Cmd) (Result, error)` captures output;
  `Runner.Stream(ctx, Cmd, stdout, stderr) (int, error)` copies it as it
  arrives (wrap a sink in `events.NewLogWriter` to turn it into `Log`
  events). A non-zero exit is not an error: the error is set only when the
  process couldn't start or ctx ended first, and death by signal N is exit
  code 128+N.
- `RunChecked` turns a non-zero exit into an `*ExitError`, whose message
  carries the argv and the last stderr line. `FormatArgv` renders argv for
  messages and logs.

**fakerunner** (`internal/docker/fakerunner`) is the `Runner` for unit
tests. `f := fakerunner.New(t)`; `f.On(matcher)` adds a rule, configured with
`.Stdout`, `.Stderr`, `.Exit`, `.Err`, `.Times(n)` (sequenced responses) or
`.Do(fn)`. Matchers are `Exact(argv...)`, `Glob("docker compose * up *")`
and `Regex(...)`; globs and regexes match the space-joined argv. Rules are
tried in order, and a call that none matches fails the test. Recorded
`Call`s hold argv, env names (never values), stdin and dir. Assertions:
`AssertCalled`, `AssertNotCalled`, and `AssertOrder` (a subsequence check).

**Engine** (016, `engine*.go`). `docker.NewEngine(runner)` returns the
`Engine` that `ops.Deps.Docker` holds; tests build one over the fakerunner.

- **System.** `Version` and `Info` parse `docker version|info --format json`.
  When the CLI works but the daemon doesn't answer, they return what the
  client knows (`Client`, `ClientInfo` with the context and plugin versions)
  and an error matching `ErrDaemonUnreachable`.
- **Images.** `ImageExists`, `ImageID`, `ImageLabels`, `Build(BuildOpts)`
  (streams output), `Pull`, `RemoveImage`.
- **Volumes.** `VolumeCreate(name, labels)` (a no-op if the volume exists,
  whatever its labels), `VolumeInspect`, `VolumeList(labelFilters...)`,
  `VolumeRemove`, and `ContainersUsingVolume`, which includes stopped
  containers.
- **Containers.** `Run(RunOpts)`, `Start` (attached) and `Exec` return the
  workload's exit code. When docker itself failed instead (`docker run`'s
  exit 125, or docker's own message ending stderr, as for a missing
  container or command or an unreachable daemon), they also return an
  `*ExitError`. `Create` takes the same
  `RunOpts` minus the run-only fields. There are also `CpFrom` (docker cp's
  layout rules), `Rm` and `ContainerInspect` (compare `Health` exactly).
  `UniqueName(prefix, d.Rand)` names a one-off container.
- **Logs.** `Logs(container, follow)` is a reader over stdout and stderr
  merged; always `Close` it. `WaitForLogLine(container, substr, timeout)`
  follows the logs from the start and matches in Go.

Rules every method follows:
- **Errors.** A failed query or change returns an `*ExitError` whose message
  is docker's own (its "Run 'docker … --help'" hint is dropped). If docker
  says the object doesn't exist, the error also matches `ErrNotFound`.
  Removals (`Rm`, `VolumeRemove`, `RemoveImage`) treat a missing object as
  removed.
- **Env.** `RunOpts.Env`, `ExecOpts.Env` and `BuildOpts.BuildArgs` are
  `NAME=value`. Docker gets a bare `-e NAME` (or `--build-arg NAME`) and the
  value through `Cmd.Env`, so no value, secret or not, reaches argv. Names
  the docker CLI reads for itself (`HOME`, `PATH`, `XDG_RUNTIME_DIR`,
  `SSH_AUTH_SOCK`, `DOCKER_*`) are refused.
- **Mounts.** `Mount{Source, Target, ReadOnly}` becomes
  `-v SOURCE:TARGET[:ro]`. A source is an absolute host path or a volume
  name. A host path must exist, since docker would create a missing one as
  a root-owned directory, and must not contain `:`.

_Ticket 003 documents its part here._

### Compose (ticket 017)

`Composer` (`Deps.Compose`) is the only code that builds `docker compose`
argv. `Compose` implements it over a `Runner`.

- `NewCompose(runner, stackDir, env)` builds one for a stack: `-f` for
  `.pic-sure/render/compose.yaml`, then for each `overrides/*.yaml` in
  lexical order (dotfiles skipped), all absolute. If the stack hasn't been
  rendered, the error wraps `ErrNotRendered`, which a command reports as
  "run `pic-sure up`".
- Every call adds `--project-directory <stack> --env-file /dev/null` after
  the `-f` list, runs in the stack directory, and gets `Env()`'s entries in
  `Cmd.Env`. `--env-file /dev/null` stops a stray `.env` in the stack
  directory from renaming the project or supplying values. The adapter also
  relies on the runner not passing the user's `COMPOSE_*` variables through.
- `Progress` sets `--progress` on up, down, stop, restart, pull and run. The
  command picks it by output mode: `ProgressJSON` under `--json`, otherwise
  `ProgressPlain` (the zero value).
- `Up`, `Down`, `Stop`, `Restart`, `Pull` and `Logs` copy compose's
  output, both streams, to one writer (wrap the sink in
  `events.NewLogWriter`) and return an `*ExitError` carrying compose's
  message when it fails. `Down` always passes `--remove-orphans`.
- `Run` (`run [--rm] -T`) and `Exec` (`exec -T`) stream stdout and stderr
  separately and return the command's exit code, plus an `*ExitError` only
  when docker or compose itself failed (no such service, service not
  running, a dependency that failed to start), as `Engine.Run` does.
  `Passthrough` adds nothing after the global flags, hands its writers to
  the runner unwrapped (so a terminal reaches compose), and returns
  compose's exit code.
- Under `--progress json` compose ends stderr with `{"error":true}` after
  any failure, adding a `message` when the failure was its own. For calls
  that passed `--progress json`, the adapter swaps that line for the
  message, or drops it, so errors read and classify the same in every
  output mode.
- `Env()` entries naming a variable docker or compose reads for itself
  (`DOCKER_*`, `COMPOSE_*`, `HOME`, `PATH`, ...) fail the call, as in the
  Engine.
- Option types are `ComposeUpOpts`, `ComposeDownOpts`, `ComposeRunOpts`,
  `ComposeExecOpts` and `ComposeLogsOpts`; the `Compose` prefix keeps them
  apart from the Engine's `RunOpts` and `ExecOpts`. Compose shares the
  Engine's `runCmd`, `streamCmd`, `exitError` and `workloadResult`.
- `Ps` runs `ps --all --format json` with a 10 s timeout. `ParseComposePs`
  accepts the JSON-lines form (compose 2.21 and later), the older array
  form, nulls and unknown fields. `Health` is empty for a container without
  a healthcheck; compare it exactly.
- `Config(quiet)`: `config --quiet` validates. Without quiet,
  `config --no-interpolate` returns the merged YAML, with no secret values
  in it.
- Compose versions these flags need: `--wait-timeout` 2.17, `--progress`
  2.19, JSON-lines `ps` 2.21, `--progress json` 2.29.

In tests, let a glob skip the global flags:
`fakerunner.Glob("docker compose * up -d --wait picsure-db")`.

## internal/git

Ticket 018. `git.New(runner)` returns the `Client` in `ops.Deps.Git`. It
runs the user's `git` through the Runner, so their credential helpers, SSH
setup and `insteadOf` rewrites apply (D24). Nothing may wait for typed
input, because a child outside the foreground process group is stopped when
it reads the terminal. So every call sets `GIT_TERMINAL_PROMPT=0` and
`SSH_ASKPASS_REQUIRE=force`, with `SSH_ASKPASS=false` unless the user has
their own askpass. Credential helpers and ssh-agent keys still work; a git
password prompt, an ssh passphrase or an unknown host key is an error.
`WithEnv(...)` returns a client that adds variables to every call; commands
use it for the proxy variables (§9.10) once they've loaded the stack's
config.

- `EnsureBare(url, dir)`: a missing `dir` is `git clone --bare`d (remote
  `origin`, whatever `clone.defaultRemoteName` says) into a temporary
  sibling and renamed into place. An existing one only has its origin URL
  set.
- `Fetch(dir, refspecs, tags)`: nil refspecs means every branch. `tags` adds
  `+refs/tags/*:refs/tags/*`, so a tag moved upstream moves here too.
  Deleted refs are pruned.
- `ResolveRef(dir, ref)`: a tag (peeled), branch, or full or short sha to
  the full commit sha. A tag beats a branch of the same name. A missing
  ref wraps `ErrUnknownRef`, so a caller can fetch and retry.
- `LsRemote(url)`: branches and tags at `url` as `[]Ref{Name, SHA}`, with
  annotated tags peeled to their commit.
- `Archive(dir, sha)`: `git archive --format=tar` as an `io.ReadCloser`,
  with files at 0644 or 0755 and no line-ending conversion, whatever the
  user's `tar.umask`, `core.autocrlf` or `core.eol`. A git failure is the
  error from `Read`; `Close` stops git.
- `Unpack(r, dest)`: writes a tar into `dest` through `os.Root`. Files keep
  their mode, directories get 0755, and symlinks are kept if they resolve
  inside `dest` (or dangle). It refuses the whole archive for an entry that
  leaves `dest`, a link that points out (including through other links) or
  loops, a link target with `..` after a name, a duplicate, or anything but
  files, directories and symlinks.

Arguments that start with `-` are refused, so a URL or ref can't become a
git option. The cache (019) owns locking and the `src/<repo>/<sha>`
layout.

## internal/cache

Ticket 019; 057 adds `cache list/prune`. `cache.DefaultRoot()` is
`$XDG_CACHE_HOME/pic-sure`, or `~/.cache/pic-sure` when that is unset or
relative. It refuses a root inside `$TMPDIR` (symlinks resolved), which
Colima and Lima don't share with their VMs. `cache.Open(root, Options{Git,
Holder, LockTimeout})` creates the layout. It takes any absolute root, so
tests open one under `t.TempDir()`. `WithEvents(sink, stepID)` returns a
copy that reports to a step's sink: `Progress` while cloning, fetching
and unpacking, and "waiting for the … lock held by …" when another
command holds a lock.

- `git/<repo>.git`: bare clones. `src/<repo>/<sha>/`: source trees. `<repo>`
  is the catalog's `RepoName()`.
- `EnsureSource(ctx, component, sha)` takes a catalog component name and a
  full lowercase commit sha, and returns the tree's path. A missing tree is
  made under the repo's fetch lock: clone, or fetch every branch and tag if
  the clone lacks the commit, then `git archive` unpacked into a temporary
  sibling and renamed into place. A tree that exists is complete and is
  never rewritten; mount it read-only. Leftover `*.tmp-*` directories of a
  dead run are removed under the lock. Nothing fsyncs the tree, so after a
  power loss a tree may be incomplete; deleting its directory makes the next
  `EnsureSource` rebuild it.
- `ReleaseControlDir()` (028 clones into it under `LockRepo(ctx,
  "release-control")`), `DownloadsDir()`, `BuildDir(sha)` (`build/<sha12>`,
  not created: the build makes and removes it under the reactor lock).
- `TempDir(pattern)` makes a fresh 0700 directory under `tmp/` for one run's
  temporary files, such as an extracted phenotype CSV, that a container may
  need to mount. The caller removes it. Anything in `tmp/` unmodified for a
  week is taken to be a dead run's and removed by the next `TempDir`, so
  don't keep using one longer than that after last changing its entries.
- **Locks** are flocks on files in `locks/`, so they exclude goroutines and
  processes alike, and die with their holder. `LockRepo(ctx, repo)`,
  `LockReactor(ctx)` (any Maven run) and `LockImage(ctx, tag)` wait up to
  `RepoLockTimeout` (15 min), `ReactorLockTimeout` (1 h) and
  `ImageLockTimeout` (30 min), or `Options.LockTimeout`, and then fail with
  an error wrapping `ErrLockTimeout` that names the holder. A cancelled ctx
  stops the wait. Lock files are never deleted: removing one while someone
  waits on it would let two holders in. The locks cover one cache root, but
  images and `pic-sure-m2` belong to the Docker daemon, so two users with
  their own caches on one daemon don't exclude each other.
- `EnsureMavenVolume(ctx, d.Docker)` creates `MavenVolume` (`pic-sure-m2`).
  Mount the volume only under the reactor lock.

## internal/release

_Ticket 028 fills this in; 060 adds the gate's self-update action._

## internal/pki

Ticket 012. Pure functions over PEM bytes; the caller does the file I/O and
fills the certs volume (024).

- `Generate(rand, hostname, now) (Files, error)` makes an RSA-2048 key
  (PKCS #8) and a self-signed server certificate valid for `Validity`
  (365 days) from `now`. SANs are `localhost`, `127.0.0.1` and the
  hostname (an IP SAN when it parses as an IP). CN is the hostname, or is
  left out when the hostname is longer than RFC 5280's 64 characters. The
  serial is a random 128-bit number, and the certificate is a non-CA with
  server-auth key usage. `Files.Chain` is the certificate itself, as the
  bash does. A hostname that isn't an IP or a valid DNS name is an error,
  and so is one whose last label is a number (`10.1.2.300`, `x.0x1f`),
  which browsers read as an IPv4 address. Pass `Deps.Rand` and
  `Deps.Clock.Now()`; a fixed `Rand` doesn't make the key deterministic.
- `Validate(files, hostname, now) (Report, error)` checks
  `tls.mode: provided` files: the key (PKCS #8, PKCS #1 or SEC 1,
  unencrypted) must be the certificate's, and the certificate must be valid
  at `now`. The first certificate in `Files.Cert` is the server's; any
  after it are intermediates. A non-empty `Files.Chain` must parse, and a
  PEM block that doesn't decode is an error in the certificate or chain
  file (OpenSSL's key reader skips such blocks, so the key file may have
  them). Errors
  wrap `ErrKeyMismatch`, `ErrExpired`, `ErrNotYetValid` or
  `ErrEncryptedKey`, and a mismatch and an expiry are reported together. A
  certificate that doesn't name the hostname passes with a
  `Report.Warnings` entry, which the caller emits as a `Warning` event.

## internal/jwt

Ticket 013. `jwt.Introspection(secret, appUUID, now, ttl)` returns the
PSAMA introspection token and its expiry (§9.4), or an error. It replaces
v1's jwt-creator container. Pass `ops.Deps.Clock`'s time as `now` and
`jwt.DefaultTTL` (365 days) as `ttl`.

- The token is HS256 with header `{"alg":"HS256"}` and claims
  `sub=PSAMA_APPLICATION|<uuid>`, `iss="bar"`, `exp`, `iat` (Unix seconds)
  and `jti="Foo"`, in jwt-creator's order, so for the same issue second it
  matches the JAR's output byte for byte. Segments are base64url without
  padding.
- The HMAC key is the first line of `secret` (split at `\n`, `\r` or `\r\n`),
  as jwt-creator read it. PSAMA verifies with the whole secret it is
  configured with, line breaks included, so the token works only if that
  secret is exactly this first line. Strip the newline from a secret read
  from stdin before storing it.
- It rejects a key shorter than `jwt.MinSecretLen` (32 bytes, since PSAMA's
  jjwt refuses shorter HS256 keys), an `appUUID` that isn't a UUID, and a
  non-positive `ttl`. Error messages never contain the secret.
- The returned expiry is in UTC and truncated to the second, like the `exp`
  claim. Store it next to the token so `update` can renew it within 30 days
  of expiry (§9.3).

## internal/sql

_Ticket 014 fills this in._

## internal/netproxy

Ticket 015. `netproxy.New(cfg, services)` resolves the config's `proxy`
block (`netproxy.Config` has the fields of `stack.Proxy`) into a `*Proxy`
with an output for each egress path (§9.10). `services` are the compose
service names in the rendered stack; callers without one pass
`netproxy.CatalogServices()`. With neither `proxy.http` nor `proxy.https`
set, every output is empty. Each proxy covers only its own scheme, so with
just `proxy.http` set, https traffic goes direct.

- **No-proxy list.** The user's entries, then the service names (sorted),
  `localhost` and `127.0.0.1`, lower case and without duplicates. An entry
  is `*`, a host or domain name (it and its subdomains), `.example.com` or
  `*.example.com` (subdomains only, written `.example.com`), an IP address
  or a CIDR range; a name or address may end in `:port`.
- `ProxyURL` is an `http.Transport.Proxy` for the CLI's own HTTP, with
  Go's NO_PROXY rules: localhost and loopback addresses always go direct.
- `Env()`: `HTTP_PROXY`, `HTTPS_PROXY` and `NO_PROXY`, each also in lower
  case, for git (`git.Client.WithEnv`), node and runtime containers.
  `BuildArgs()` are the same `NAME=value` entries for
  `docker.BuildOpts.BuildArgs`. The proxy URLs keep their user and password,
  so treat the values as secrets: `Cmd.Env`, or `${VAR}` in the rendered
  compose file, never argv or a rendered file.
- `JVMOpts()`: `-Dhttp.proxyHost/Port`, `-Dhttps.proxyHost/Port` and
  `-Dhttp.nonProxyHosts` for `JAVA_OPTS`, without white space or
  credentials (the JVM has no property for them). The JVM and Maven speak
  plain HTTP to the proxy, even for an `https://` proxy URL.
- `MavenSettings()`: a `settings.xml` with a `<proxy>` per scheme, for the
  reactor container's `/root/.m2`. It holds the credentials: write it 0600.
  Maven sends https through an http proxy when it has no https one, so with
  only `proxy.http` set it is nil and Maven's (all https) downloads go
  direct, as https does on every other path.
- The JVM and Maven get the no-proxy list as `|`-separated patterns whose
  only wildcard is a leading or trailing `*`: `example.com` becomes
  `example.com|*.example.com`, ports are dropped, IPv4 ranges become prefix
  patterns (`172.16.0.0/12` is `172.16.*` to `172.31.*`), and IPv6 ranges
  are left out. The JVM gets IPv6 addresses in brackets; Maven can't match
  IPv6 hosts and gets none. Both get `127.*` (and the JVM `[::1]`), because
  setting the property replaces the JVM's own loopback defaults.
- `String()` and `LogValue()` redact the passwords, so a `*Proxy` can be
  logged.

`ParseURL` and `ParseNoProxy` are the parsers `New` uses. `stack.Validate`
calls them too, so a config that validates always resolves. A proxy host
must be an IP address or a host name whose last label isn't all digits.
The package imports only the catalog.

## internal/selfupdate

_Ticket 060 fills this in._

## internal/events

Ticket 001 defines the event types and `Sink`; ticket 004 adds the plain
and NDJSON renderers, and ticket 038 the TUI renderer.

- Events: `StepStarted{ID, Title}`, `Progress{ID, Text, Pct}` (`Pct` is
  0–100, or nil when unknown), `Log{ID, Stream, Line}`, `Warning{ID, Text}`,
  `StepDone{ID, Status}` (`ok`, `skipped` or `failed`), and
  `Result{OK, Data, Error}` with `ErrorInfo{ExitCode, Message, Step}`. Each
  event's `Type()` is its NDJSON `type` value, and its JSON tags are the
  wire format.
- Who emits what: the step engine emits `StepStarted` and `StepDone`, an
  operation's steps emit `Progress`, `Log` and `Warning`, and the cli layer
  emits one `Result`, last, after the operation returns.
- `Sink` has one method, `Emit(Event)`, and must be safe for concurrent
  use. `SinkFunc` adapts a function, `Discard` drops everything, and
  `Recorder` keeps events for tests (`Events()`, `Types()`).
- `NewLogWriter(sink, id, stream)` is an `io.Writer` that emits one `Log`
  per line; `Close` flushes a final partial line.

Ticket 004 added the plain and NDJSON renderers, both Sinks:

- `NewPlain(w, PlainOptions{Color, Now})` writes one line per event,
  `15:04:05 [MARK] text`, to stderr. The marks are `[ .. ]` (a step
  started, or progress without a percentage), `[ 42%]`, `[ OK ]`,
  `[SKIP]`, `[FAIL]` and `[WARN]`. Log lines are indented under them as
  `| line`. `StepDone` repeats the step's title, and `Result` prints
  nothing. `Color` colours only the marks; the cli layer turns it off for
  `NO_COLOR`, `TERM=dumb` and a stderr that isn't a terminal.
- `NewNDJSON(w)` writes each event as one JSON object per line, its JSON
  form with `"type"` first. An event that can't be encoded becomes a
  `warning` line, so the output stays valid NDJSON. A non-finite `Pct` is
  dropped by both renderers, and plain clamps it to 0–100. Both keep
  going after a failed write and report the first error from `Err()`.
- `WriteReport(w, report)` prints a read-only command's report (`status`,
  `doctor`, `version`) as one object with `"schema_version": 2` first.
  The report must encode as an object and must not set `schema_version`
  itself.

Goldens for both renderers are in `testdata/`; `go test -update` rewrites
them.

## internal/log

_Ticket 005 fills this in._

## internal/tui

The v1 TUI shell: landing, setup wizard host, activity screen and load
wizard. `tui.Run` takes the command's context and turns off Bubble Tea's
signal handler, so SIGINT and SIGTERM end the TUI through the context and
the CLI exits 128+N. Ticket 001 removed its script layer: every action fails
to start with "not implemented in v2 yet (ticket NNN)", the release-branch
and dev-overlay lookups return nothing, and the archive lister fails. Ticket
002 moves it to Bubble Tea v2, and tickets 038, 039 and 047 rewire it onto
in-process operations.

## internal/dashboard

The v1 dashboard. Its polls, log follower and actions report "not
implemented in v2 yet" until ticket 040 rewires them. `pollCmd` and
`TestPollCmdNotWedgedByOrphanGrandchild` stay only as the model for ticket
003's grandchild handling; 040 deletes them.

## internal/wizard

The v1 setup form over `.env` keys. It no longer writes anything; ticket
039 moves it onto the config schema (006).

## internal/filebrowser

The file picker used by the load wizard. Ticket 002 ports it to Bubble Tea
v2.

## internal/styles

The shared palette and styles. Ticket 002 ports it to lipgloss v2.

## internal/exitcode

Ticket 001. The exit codes (§10.4), the `Error` type that carries one, the
constructors `Failed`, `Usage`, `Precondition`, `ConfirmRequired`,
`Incompatible` and `Signaled`, and `FromError`, which maps any error to its
code. The constructors take `fmt.Errorf` arguments, so `%w` wraps a cause.

## internal/fakecmd

Ticket 001. The fake `docker` and `git` the testscript harness puts on
`PATH`. Each reads rules from `$HOME/<name>.scenario` and appends every
call's argv to `$HOME/<name>.log` (quoting arguments with spaces, as
`docker.FormatArgv` does); the harness sets `HOME` to the script's work
directory. A lock file serializes choosing the rule, so concurrent calls
keep the log in order and `times=N` exact. Rule syntax:

```
PATTERN => EXIT [stdout=FILE] [stderr=FILE] [times=N]
```

PATTERN is a glob over the space-joined, unquoted argv (program name
included), or a regex after `re:`. FILE is relative to the scenario's
directory. `times=N` retires a rule after N matches. A call that no rule
matches exits 97 with the reason on stderr.
`cmd/pic-sure/testdata/script/fakes.txtar` is a worked example.

The fakes find their scenario through `HOME`, which the exec runner (003)
passes through to subprocesses.

## v1 leftovers

These packages exist only until the TUI tickets replace what uses them:

- `internal/actions`: the TUI's action descriptions. `Ticket` names the v2
  ticket behind each one, and `Args` keeps the v1 script arguments the TUI
  tests assert on.
- `internal/contract`: the v1 status and compose-ps JSON types the dashboard
  renders (040 removes them).
- `internal/dialog`: the reset confirmation dialog.
- `internal/tty`: the terminal check behind `App.IsTerminal`, which output
  mode selection (004) also uses. It uses isatty, so `/dev/null` on stdin
  doesn't count as a terminal.
