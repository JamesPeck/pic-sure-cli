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
action and kills the process at once. SIGPIPE is caught on a channel of
its own (068), so a write to a closed stdout or stderr fails with EPIPE
instead of killing the process; subprocesses still get the default
SIGPIPE. It then calls `cli.Execute`. The
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
  `--wait-lock`. Holding the lock, it applies the gate again (009), since
  the command it waited for may have been a newer pic-sure.
- `gate.go` (009): `commandClasses`, the version-gate class of every
  command (§10.6; a test keeps it complete): read-only, mutating, or
  `update`'s own class. `openStack` runs `a.gate`, so every command that
  opens a stack is gated, and `lockStack` runs it again under the lock. A
  read-only command's warning goes to stderr, so `--json` output stays
  clean. A new command needs a row here.
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
  `CI=0` don't count); and TUI otherwise. TUI mode draws with
  `progress.Renderer` (038) when stderr is a terminal and `TERM` isn't
  `dumb`, and as plain otherwise. `newSink` returns the same sink for the
  whole run. Only
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
- `pipe.go` (068): `a.stdout()` and `a.stderr()` are the streams for what
  the CLI writes itself: the renderers, reports, log records, warnings
  and `logs`. The first write that fails with EPIPE (`| head` went away)
  cancels the command with cause `exitcode.Signaled(SIGPIPE)`, so it runs
  its cleanups and exits 141, and later writes to that stream are
  dropped. A subprocess that takes over the terminal (`compose`, `config
  edit`) gets the raw `a.Stdout`/`a.Stderr`, and so does the TUI, whose
  stderr is a terminal.
- `deps.go`: `newDeps` assembles `ops.Deps`. Each field comes from a
  constructor in its owner's file: `runner.go` (003), `output.go` (004, which
  also reports errors), `logging.go` (005, landed), `engine.go` (016,
  landed) and `gitclient.go` (018, landed). The runner is built with the
  logger, so it can log argv. Until the others land, the sink discards
  events.

- `composeverbs.go` (026, 069, 070): `a.stackCompose(cmd, runner, st)` is the
  compose adapter for every command on a stack, `status`, `doctor` and
  `migrate` included (`stackComposeConfig` also returns the config and
  secrets it read): exit 3 with "run `pic-sure up`", wrapping
  `docker.ErrNotRendered`, when it isn't rendered; `render.ComposeEnv` from
  the config and secrets; and `ProgressJSON` under `--json`. A read-only
  command, by `commandClass`, that can't read them warns on stderr and
  carries on with the secrets empty (and the default config if the config
  is unreadable), so `ps`, `status` and `doctor` work on a newer stack or
  one without secrets.yaml. A mutating command without secrets.yaml is
  exit 3, pointing at `init`. `status` and `doctor` pass their reports
  through `log.Redact`, since compose's errors can quote a secret.
  `down` and `restart` take the stack lock and run as one step
  whose `Log` events are compose's output. `ps --json` uses status's
  service shape (`ops.StatusServices`). `logs` writes the logs to stdout and compose's own
  messages to stderr (a `logs` step under `--json`), and reports Ctrl-C as
  the signal alone. `compose` refuses `--json`, runs in the foreground
  runner and exits with compose's code. Its class for the gate and the lock
  comes from its compose subcommand (`composeClass` in `composeclass.go`,
  skipping compose's global flags): `ps`, `logs`, `exec` and the other
  read-only ones run without the lock and on a newer stack, and write a run
  log file only at debug level, like `ps`; any other, an unknown one or
  none, and `wait --down-project`, holds the stack lock until compose
  exits. The passthrough's output goes straight to the terminal, never to
  the run log.

- `init.go` (034): `init [DIR]`. Its flags come from `stack.Fields` (a
  secret's is a bool that reads stdin through `ReadUserSecret`; with both
  `--auth0-client-secret-stdin` and `--db-root-password-stdin`, stdin holds
  one secret per line, in that order), plus `--auto-ports`, `--source
  COMPONENT=PATH`, the gate's `--self-update` and `--ignore-cli-version`,
  and `--set KEY=VALUE` (072) for any non-secret key, through
  `ConfigDoc.Set` after the flags; a key a flag also sets must get the same
  value, and a `--set` port or `network.dev_ports.base` is used as given.
  Usage problems are exit 2 naming the flag, before docker is asked
  anything: a client secret under `jwt.MinSecretLen`, a
  `--skip-step` that isn't in `ops.InitStepIDs(cfg)`, shared HPDS data
  (until 051), `--self-update` with a stdin flag. A DIR whose state.json
  has `initialized_at` gets "already initialised" and exit 0; a DIR with a
  `pic-sure.yaml` is resumed with that config as it is (config flags are
  ignored with a warning; a `--set` that would change it is exit 2). Then
  three unskippable steps run:
  `preconditions` (doctor's host checks with the new config, `memory` only
  a warning; `ops.StackNameInUse`; on a new stack `ops.ChoosePorts` and
  `ChooseDevPortsBase`, on a resumed one its ports must be free or its own;
  a loopback remote `--db-host` warns), `release` (`release.Fetch` at a
  resumed stack's recorded commit, else the branch head, then `Gate` with
  `newSelfUpdater`) and `config` (`stack.Create`, the run log, the stack
  lock, pic-sure.yaml, `EnsureSecrets` with `OpenAuth`, state.json with
  the release and the operation). Then `ops.InitSteps` with `--skip-step`,
  and `initialized_at` once they succeed. `r.compose` builds the adapter
  with a lazy env over init's `*Secrets`, so `compose up` sees the token
  seed issued; `up` (035) can copy it. `startRunLog` registers
  `--admin-email` with the redactor before it logs the flags.
- `up.go` (035): `up`. Usage problems first: a `--skip-step` not in
  `ops.UpStepIDs(cfg)` is exit 2. Under the stack lock: the config with
  `CheckFiles`; shared HPDS data is exit 2 until 051; a stack without `initialized_at` or without secrets.yaml is
  exit 3 pointing at `init DIR`; outside open mode a client secret that is
  generated (`auth0_client_secret_generated`) or missing is exit 3 pointing
  at `secrets rotate auth0-client-secret` (§9.11); then `EnsureSecrets`
  with `OpenAuth` (fills a generated secret a newer pic-sure added, never
  replaces one). `ops.StackNameInUse` refuses a name another project uses,
  and a stack port that is busy but not published by the stack's own
  containers is exit 3. Then `ops.UpSteps` with the cache and a lazy-env
  Composer like init's, recording the `up` operation in state.json. The
  version gate is openStack's: pending config migrations are exit 5 ("run
  `pic-sure update`"); §6.2's up prompt isn't implemented.

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
  rendered with), the release commit, component commits (with the local
  checkout and dirty flag of one built from a source, 031), image tags, what
  the TLS step installed (`tls`, 024), the last operation and timestamps.
  `hpds_key` (034) records what the HPDS key step copied, and
  `initialized_at` (034) when init finished.
  No secrets. `StartOperation` and
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
  `LabelStackDir`). `st.VolumeLabels(name, key)` adds compose's
  `com.docker.compose.project` and `com.docker.compose.volume` for a stack
  volume a helper creates before compose does (024). `st.EnsureVolume(ctx, engine,
  name, vol, key)` creates such a volume with those labels if it's missing
  and refuses (exit 3) one not labelled for this stack.

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
- **Open mode (034).** With `EnsureOptions.OpenAuth` and no client secret
  stored or supplied, a random 32-byte hex one is generated and
  `auth0_client_secret_generated` set, since PSAMA signs the introspection
  token with it. Without `OpenAuth`, a generated one is exit 3 asking for
  `--auth0-client-secret-stdin`; a supplied secret replaces a generated one
  (the one exception to never replacing) and clears the token issued from
  it. Doctor's `auth0` check fails on a generated secret outside open mode.
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
  `state.json` as they were, with their modes, to
  `.pic-sure/backups/<UTC ts>/` and writes the config. With nothing pending it does nothing. It leaves state.json's
  `schema_version` to the next render.
- **Comparing versions.** `CompareVersions(a, b)` orders versions as semver,
  ignoring a leading `v`, `+build` and a git-describe suffix (`-N-gSHA`,
  `-dirty`). It reports not-ok for anything else, such as `dev`. 028 uses it
  for PSCLI.
- **The gate.** `st.CheckVersions(cli, reg)` reads state.json's two
  version fields (missing is fine, corrupt is an error) and pic-sure.yaml's
  schema. On the result,
  `Newer()` is true if either schema is above this pic-sure's or
  `cli_version` is a later version, and `Pending` lists the migrations.
  `Gate(class)`: `ReadOnly` always runs, with a warning when newer;
  `Mutating` is exit 5 when newer or when migrations are pending ("run
  pic-sure update"); `Migrating` (update) is exit 5 only when newer. A
  config without a readable schema doesn't gate; the command reports it.
- `doc.Raw(key)` reads a value as written, with no defaults. `config
  show/get` use it on a schema this pic-sure can't decode or migrate.

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

**Rendering** (ticket 021, `render.go`).

- `Render(Input)` is pure: from the config, state and stack dir it returns
  the `[]File` to write under `.pic-sure/render/` (`ComposeFile` first, then
  `files/*`), each with its path relative to the stack dir and its mode.
  `Write(st, files)` saves them through the stack (atomic, in the manifest)
  and removes the files an earlier render wrote that this one didn't, such
  as `files/maven/settings.xml` (0600, the proxy credentials) once the proxy
  is off. Callers record `cli_version` and `schema_version` in state.json
  after a render.
- `Input`: `StackDir`, `Config`, `State` (image tags from `Images`, node's
  being the `.nvmrc` tag httpd-hmr needs; dev builds' tags from
  `DevImages`, keyed by image), `Sources` (the cache tree of each component
  without a configured `source`; render needs pic-sure and migrations) and
  `CustomTrust` (the trust dir holds certs) and `SharedProfile` (the shared
  data set's recorded HPDS profile, used when `hpds.profile` is empty).
  Render does no I/O, so the caller resolves those.
- Bind sources (the stack dir, sources, a relative `source` resolved
  against the stack dir) must be absolute without `:` or line breaks; the
  rendered file is checked again after the merge. A service override for a
  service this stack's mode doesn't have (`picsure-db` with a remote DB) is
  skipped; an unknown service, dev variant, missing image tag or dev build
  is an error, as is `java_opts` for a service that takes no JAVA_OPTS.
  `services.hpds.java_opts` wins over `hpds.java_opts`.
- JAVA_OPTS extras, in order: the proxy properties (psama only, the one
  JVM that calls out), the truststore properties (psama, custom certs) and
  the JDWP agent (services of a dev variant with a debug port).
- `ComposeEnv(cfg, secrets)` is the `env` for `docker.NewCompose`: every
  `secretVars` name, always, plus `proxyVars` when a proxy is set (empty
  when only one scheme is). `DB_ROOT_PASSWORD` is the local or remote root
  password by `db.mode`.
- `ViteEnv(cfg)` is the frontend's `VITE_*` set without `VITE_ORIGIN`: the
  auth-mode flags, ToS, the Auth0 login module when `client_id` is set,
  analytics and the theme. httpd-hmr gets it plus `VITE_ORIGIN`; the
  frontend build (030) bakes it in and hashes it for the image tag.

**Goldens.** `render_test.go` renders 11 stacks that cover every pair of
auth mode, dev (none, hpds, httpd-hmr), db mode, HPDS data, proxy and
overrides, plus an everything-on stack and one with dev httpd and custom
certs. Each is `testdata/golden/<case>.txtar`: compose.yaml and the
rendered files (the copied ones are only checked to be copies). Regenerate
with `go test ./internal/render -run TestGoldens -update` and review the
diff. The test fails if a golden holds any secret value, and runs
`docker compose config --quiet` on each (with `testdata/overrides.yaml` for
the override cases) when the docker CLI is installed. `make compose-check`
runs the render tests with `PICSURE_REQUIRE_COMPOSE=1`, which makes the
compose checks fail instead of skipping. CI's Linux-only compose job runs it
under the oldest compose doctor accepts.

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

**TLS (024, `tls.go`).** `TLSStep(d, st, cfg)` is §9.1 step 6's first half,
ID `tls`: it fills the stack's `certs` volume with `server.key` (0640),
`server.crt` and `server.chain` (0644), all owned 2:2 for httpd. Generated
mode keeps its PEMs in `TLSDir` (`.pic-sure/tls/`, 0600) and makes new ones
when those are missing, invalid, don't name `network.hostname`, or expire
within 30 days. Provided mode validates the operator's files with
`pki.Validate` (warnings become `Warning` events) and copies them; with no
`chain_file`, the certificate is the chain. The files reach an alpine helper
(`--network none`) as a tar on stdin, never through a bind mount or argv.
It creates the volume with `st.VolumeLabels` if compose hasn't, and refuses
one labelled for another stack. `state.json`'s `tls` records a hash of what
was copied and the volume's `CreatedAt`; `Check` is done while both match,
so a changed file, hostname or re-created volume re-copies. An installed
provided certificate isn't re-validated, so its expiry doesn't block `up`.
The step doesn't restart httpd: a command that runs it on a live stack
must restart httpd when the step applied.

**Truststore (023, `truststore.go`).** `CustomCerts(st, cfg)` reads the
operator's CA certs from `trust.custom_certs_dir` (relative to the stack;
`*.crt|*.pem|*.cer|*.der`, PEM bundles split, DER read whole, hidden files
skipped, a missing directory is none) and names them `custom-<n>-<file>`.
Every PEM block in a file must decode and be a CERTIFICATE. Render (021)
should set `catalog.Mode.CustomTrust` when it returns any.
`TruststoreStep(d, st, cfg, psamaImage)`, ID `truststore`, is the step
init/up/update (034–036) add once the psama image is present and before
psama starts: with no certs it does nothing; otherwise it gets
`<name>_truststore` with `st.EnsureVolume` and runs a helper container from
the psama image (`--entrypoint sh`, `--network none`, user 0) that copies
the image's `cacerts` into the volume and imports each cert with the
image's `keytool`. The certs go in as PEM on stdin, not a bind mount, so
the daemon needn't see the stack directory. `state.json`'s `truststore`
records a hash of the certs, the script and the image ID, plus the volume's
`CreatedAt`; Check is done while all of them match, and Apply forgets the
record before the helper runs. A running psama needs a restart to read a
new truststore.

**Reactor build** (029, `reactor.go`). `BuildReactor(ctx, d,
ReactorOptions{Cache, SHA, ...})` builds the 11 pic-sure images as
`hms-dbmi/<image>:<sha12>` (§7.2), labelled `ReactorSrcLabel=<sha>`, and
does nothing when all of them already carry that label;
`ReactorUpToDate` is the same check for a step's `Check`. Only stale
images are built unless `Force` is set, so a build from local, dirty
sources (§7.3) must pass `Force`. It takes the source tree from the cache
unless `Source` is given, holds `LockReactor` from the Maven run to the
last image and `LockImage` per image, and refuses a commit without the
monorepo's contexts before running Maven.

- **Container.** Maven runs through `docker exec` in a container named
  `ReactorContainer` (`pic-sure-reactor`), started with `docker run -d`.
  Its own process waits on a heartbeat file the build touches every 15 s
  and exits a minute or two after the beats stop, so the container runs
  until it has been copied from and removed, and a killed build's
  container stops soon after. Another build that finds it running waits
  (a Progress event; ctx, or `ReactorLockTimeout`, ends the wait). A
  stopped one, or one left in the created state for two minutes, is a
  dead build's and is removed. A `docker run` that fails after creating
  the container removes it, found by its run label. The proxy's
  `settings.xml` is bind-mounted at `/pic-sure/settings.xml` (not in
  `/root/.m2`, which is the shared `pic-sure-m2` volume) and passed with
  `-s`.
- **Contexts** are copied with `docker cp` into `cache.BuildDir(sha)`,
  each once (a context inside another comes with it), and the directory
  is removed after a success. A failed build leaves it for inspection;
  the next build clears it.
- **Logs.** With `LogDir` set, Maven's output goes to
  `pic-sure-reactor.log` and each image's to `<image>.log`. A failure
  emits the last 30 lines as `Log` events and puts the log path in the
  error, plus the bash's Alpine-pin hint for hpds-etl. Maven's module
  lines become `Progress` events. The unexported `partOutput` (log file,
  tail, per-line callback) and `ensureImage` (pull with progress) are
  there for the other image builds (030) to reuse.

**Status (027, `status.go`).** `Status(ctx, d, st, StatusOptions)` builds
the read-only `StatusReport` (`status --json`, documented field by field in
`docs/json-schemas.md`; a test keeps the two in step). It reads
pic-sure.yaml (migrated in memory), the version gate, state.json's release,
components, image tags and last operation, and the token expiry from
secrets.yaml; it asks docker only `image inspect` per built image with a
recorded tag, or its dev build's tag while a dev variant runs it (each
with `docker.PsTimeout`, stopping at the first docker failure), and
`compose ps`. It takes no lock, writes nothing and never fetches
release-control. A section it can't read carries an error string instead
of failing, so the command exits 0. The command leaves `Deps.Compose` nil for an unrendered stack and
builds it with no env, since `ps` needs no secret. Migrations are
`unknown` until 032 adds its check; `--deep` is 037. 027 added
`VersionCheck.MigrationErr` (a config schema older than every migration)
and `render.HMROrigin` (httpd-hmr's Vite origin, which status prints for
Auth0) for it.

**Frontend and dictionary-etl images (030, `images.go`).** §7.2 steps 4
and 5, the two images built outside the reactor.
`BuildFrontend(ctx, d, cfg, ImageBuildOptions)` builds
`hms-dbmi/pic-sure-httpd:<sha12>-<cfghash8>`. `FrontendConfigHash` is the
sha256 of `render.ViteEnv(cfg)` (theme included) as sorted JSON, so stacks
with the same frontend commit and config share the image. The source is
copied (symlinked root resolved) to the cache's `FrontendBuildDir(tag)`
(`build/frontend-<tag>`, keyed like the image lock), `FrontendDotEnv`'s
output replaces the copy's `.env` without following a symlink, and the copy
is removed after the build. Each value is quoted so dotenv and
dotenv-expand return it unchanged: single quotes (backquotes if it holds a
`'`), `$` as `\$`; a line break, or both `'` and a backquote, is an error.
The copy leaves out a root `.git` and `node_modules` (031), which a local
checkout has and the Dockerfile never copies.
`BuildDictionaryETL(ctx, d, ImageBuildOptions)` builds
`hms-dbmi/dictionary-etl:<sha12>` from its tree. Both run under the image's
`LockImage`, skip when the image exists with matching labels
(`FrontendSrcLabel` and `FrontendConfigLabel` with the full hash;
`DictionaryETLSrcLabel`), pass the proxy build args, take the cache tree
unless `Source` is set (`Tag` and `Force` serve §7.3 builds), and write
`<image>.log` to `LogDir` through 029's `partOutput`, showing the tail on
failure. They return `ImageBuildResult{Tag, Ref, Built}`; 031 records the
tags in state.json.

**Doctor (025, `doctor.go`).** `Doctor(ctx, d, DoctorOptions)` returns a
`*DoctorReport`: a list of `Check{Name, Status, Message, Detail}` with
status `ok`, `warn` or `fail`. Names are stable. A failing check is in the
report, not an error; the command exits 1 when `report.Failed()`. Init's
preconditions (034) can call it with no `Stack` and `Building: true`.

- Host and Docker: `docker-cli`, `docker-daemon`, `compose-version`
  (`MinComposeVersion`, 2.29.0, the first with `--progress json`),
  `buildx-version` (`MinBuildxVersion` 0.17.0; a warning unless `Building`
  or the stack builds its images), `docker-runtime` (Docker Desktop,
  Colima, OrbStack, Podman with a warning, or Docker Engine), `git`,
  `disk-cache`, `disk-docker`, `memory` and `arm64-images`. Versions come
  from `docker info`'s plugin list. `disk-docker` runs `df` in a
  throwaway, uniquely named `--rm --network none` alpine container, whose
  root file system is on Docker's data root wherever the daemon runs, and
  removes it again; it only warns when alpine isn't pulled, so plain
  doctor never downloads. `memory` sums the last `-Xmx` of every running
  stack's HPDS (`docker ps` by the stack label and compose service)
  against `docker info`'s `MemTotal` (fail when over), plus this stack's
  when it isn't running (only a warning, since `-Xmx` is a ceiling).
  `arm64-images` inspects only the pinned images already pulled.
- With a `Stack`: `config` (including `CheckFiles`), `compose-config` (`d.Compose.Config(quiet)`;
  a warning before the first render, from `ComposeErr`), `overrides`
  (`overrides/*.yml`, which the adapter ignores), `ports` (busy only when
  binding says `EADDRINUSE`, on the wildcard address or loopback; a busy
  port published by this stack per `compose ps` is fine; the dev ports too when dev
  services are on), `auth0` (tenant, client ID and the client secret
  unless open mode) and `proxy` (warns on an http-only proxy and on
  credentials psama can't use, §9.10).
- `Network`: `network-github`, `-maven-central`, `-npm-registry`,
  `-alpine-cdn` (an HTTP HEAD through the stack's proxy; any status counts,
  but a 407 or a refused CONNECT is a failure naming credentials) and `-release-control` (`git ls-remote` with the proxy env).
  The repo URL's user info is masked in messages.
  With a proxy, `network-docker-pull` pulls alpine and on failure puts the
  runtime's daemon proxy instructions in `Detail` (D36).
- `Host` is the seam for PATH lookups, free disk, port binding and HTTP;
  the cli layer's `systemHost` is the real one.

**Build and the image step (031, `build.go`).** `ImagesStep(d, st, cfg,
state, ImagesOptions{Cache, Components, Force, Refresh})`, ID `images`, is §7.2's
image step for init, up and update. For each selected component (all by
default; an unknown name is exit 2) it builds, or pulls, the images at
the release commit `state.Components` records (exit 3 if none, or if it
was recorded from a source the config no longer sets), through 029's
`BuildReactor` and 030's `BuildFrontend`/`BuildDictionaryETL`, which skip
what is up to date. It also makes sure the cache has the pic-sure and
migrations trees, which render bind-mounts. It records each part's commit
and tags in `state` and saves it as soon as that part is done. Apply holds
the cache's use lock (057) throughout, so `cache prune` can't remove what
it uses before state.json records it. Build logs
go to `BuildLogDir` (`.pic-sure/logs/build/<part>.log`, made through the stack
so they are in the manifest; each holds that part's last build). `Check`
is done when every image is present with its labels and recorded and
those trees exist, which is what `up` needs.

- **Local sources (§7.3).** A component with `components.<c>.source`
  (relative to the stack) is built from that checkout: `git.WorkTree`
  gives its HEAD and whether it has changes. Its images are tagged
  `DevTag` = `dev-<stack>-<sha12>`, plus `-dirty` with changes, when it
  is always rebuilt (`Force`) and never `Check`-done. state.json records
  `{commit, source, dirty}` for it and no ref, and its tags in both
  `images` and, for the images an enabled dev variant builds, `dev_images`.
  The release's commit for it is not resolved: `ResolveComponents` leaves
  it out.
- **Pull mode (§7.4).** Every image but the frontend's (always built,
  since it bakes in config) is pulled as
  `<images.registry or DefaultRegistry>/<image>:<ref>`, the ref being the
  component's build-spec or config ref, and tagged `hms-dbmi/<image>:<ref>`
  for the rendered compose. An image already present is kept unless
  `Force` or `Refresh` (set by `build` and update, since a ref can be a
  branch). A failed pull says the images may not be published yet.
  Migrations' ref needn't be a tag.
- `Build(ctx, d, st, cfg, state, BuildOptions)` is the `build` command: a
  `resolve` step, done when state records a release commit for every
  component without a source. Otherwise it fetches release-control at
  state's release commit (the `release.branch` head for a stack with none,
  which it then records) and resolves only the missing components, so `build` never moves a recorded commit (that is
  `update`'s job) and runs no CLI gate. Then the image step without its
  `Check`, so the `BuildReport` lists every selected
  image as `built`, `pulled` or `up-to-date`. The command holds the stack
  lock, records the operation in state.json and saves it even on failure.

**DB and migrations (032, `migrate.go`).** §9.1 steps 8 and 9, for
init, up and update to add, and the `migrate` command.

- `DBStep(d, cfg, sec, DBOptions)`, ID `db`: `compose up -d picsure-db`,
  a poll of `compose ps` until `Health` is exactly `healthy`, then
  `SELECT 1` as root over TCP (`-h 127.0.0.1`, 014's client), retried
  while the entrypoint's socket-only temporary server runs. `ERROR 1045`
  fails at once with exit 3, naming the `picsure-db-data` volume: MySQL
  sets the root password only when it initialises an empty volume. A
  container that exits or restarts (`restart: always` turns a failed
  start into restarts), or the timeout (5 min, wall time), shows its last
  30 log lines.
  Check is a healthy container plus a passing probe. With a remote DB the
  step only probes it; `BootstrapStep` (054) prepares it.
- `MigrateStep(d, cfg, sec, MigrateOptions{Action, NoRestart})`, ID
  `migrate`: `compose run --rm flyway-init`, then `flyway-dictionary-init`.
  A non-zero exit fails the step and shows the output's last lines. Repair
  passes `-e FLYWAY_ACTION=repair` (`ComposeRunOpts.Env`, added here) and
  has no Check. After a migrate, the catalog's `RestartAfterMigrate`
  services (psama, dictionary-api) are restarted if running; since Check
  would skip a re-run, a failed restart is a warning naming the command to
  run, not a failure.
- `MigrationsUpToDate` is the migrate step's Check, and status's
  `migrations.status`. It reads the one-shots' bind mounts from `compose
  config --no-interpolate` (so overrides count) and lists the `V*__*.sql`
  versions in each, then reads the five Flyway histories (four in MySQL,
  checked in `information_schema` first, so a missing table is "not
  migrated", and the dictionary's in Postgres with 014's new
  `QueryPostgres`). Up to date means every file version is recorded, or at
  or below the pass's baseline, and no row failed. A database that isn't
  running and healthy, a missing table, an `R__` repeatable migration (no
  checksum compare), or a mount source that is missing or still holds a
  `${VAR}` (compose interpolates it only when it runs) means not up to
  date, so the step applies and Flyway decides. Both steps' Checks, and
  status, bound it at 30 s; status reports `unknown` unless both databases
  are healthy, and skips a remote database.
- `MigrateCheck` is `migrate --check`, ported from AIO's
  `run-migrations.sh --check`: the mounted SQL directories, dictionary-db's
  `schema.sql`, the project UUIDs, the remote DB settings, and `compose
  config --quiet`. It runs nothing but compose config. Legacy Jenkins
  UUID tokens in the project migrations, and a source holding a `${VAR}`,
  are warnings.
- `Migrate` runs `DBSteps` then `migrate` for the command. `migrate` uses the
  existing render; an unrendered stack is exit 3 ("run `pic-sure up`").

**Seed (033, `seed.go`).** `SeedStep(d, st, cfg, sec)`, ID `seed`, is
§9.1 step 10, which init, up and update add after `migrate`. There is no
`seed` command. It fails with exit 3 and the `migrate` / `migrate --repair`
hint unless both custom Flyway histories exist (looked up in
`information_schema` first) and have a non-baseline row. It creates the
admin user with 014's `SeedAdminUser` when no user has
`auth.admin_email`. Then it makes `auth.application`'s PICSURE token equal
secrets.yaml's: a stored token valid for more than `TokenRenewBefore` (30
days) is written back as it is (after `reset`); otherwise `jwt.Introspection`
issues one (exit 3 without a client secret, which PSAMA needs even in
open mode), which goes to the database first, then to secrets.yaml and `sec`.
If saving fails, the next run issues another, so it converges. A running
psama is restarted after a change (a failure is a warning). Check is done
when the user exists, the token is valid for more than 30 days and the database holds
it. The step registers the admin email, and any token it issues, with the
log redactor. A new token changes `render.ComposeEnv(cfg, sec)`
(gateway's `PICSURE_INTROSPECTION_TOKEN`), so a Composer whose env was
computed before the step must recompute it.

**Remote database (054, `db.go`).** §9.1 step 8 for `db.mode: remote`, and
`db bootstrap`.

- `DBSteps(d, cfg, sec, DBOptions)` is step 8 for init, up, update and
  migrate to put before the migrate step: `[db]` for a local database,
  `[db, db-bootstrap]` for a remote one.
- `BootstrapStep(d, cfg, sec, BootstrapOptions{SyncPasswords})`, ID
  `db-bootstrap`: as `db.remote.root_user`, through 014's remote client
  (`docker run --rm mysql:8.0`), it runs `sql.Bootstrap`: the auth and
  picsure databases, the picsure, auth and airflow users from secrets.yaml,
  and all privileges on their databases (AIO's `bootstrap-remote-db.sh`).
  An existing user keeps its password unless `SyncPasswords` adds an
  `ALTER USER`. Afterwards each user must log in with its secrets.yaml
  password, or the step fails with exit 3 suggesting `--sync-passwords`, so
  a stack never silently points at a shared server's users with the wrong
  passwords. Check is `CheckBootstrap` finding nothing missing.
- `CheckBootstrap` is `db bootstrap --check` (`BootstrapReport`: `ok`,
  `server`, `version`, `checks[]{name, ok, problem}`). One root query
  (`sql.BootstrapStateQuery`) reads the databases, the `name@'%'` users and
  their database-level ALL PRIVILEGES from information_schema, `mysql.user`
  and `mysql.db`, so the root account needs SELECT on `mysql`; then each
  existing user logs in with its password and `USE`s its databases. It
  changes nothing. A root login that fails is an error (exit 3 for access
  denied).
- `Bootstrap` is the command: `[db, db-bootstrap]`. It needs no render.
- The remote client and the stack's services resolve `db.remote.host` from
  their containers, where `localhost` is the container itself.
  `LoopbackHint` adds that, and the `host.docker.internal` suggestion, to
  remote connection errors (here and in the db step).
- Both refuse secrets.yaml without the root or an application password,
  since an empty one would make a passwordless account. A login refused
  while the user has accounts other than `name@'%'` names them instead of
  suggesting `--sync-passwords`, which changes only `name@'%'`.

**Init and converge steps (034, `init.go`, `hpdskey.go`).** §9.1 for
init, and the parts `up` and `update` reuse.

- `InitSteps(d, st, cfg, sec, state, ConvergeOptions{Cache, CLIVersion,
  Compose})`: `resolve` (`ResolveStep`, 031's), `images`, `tls`,
  `truststore` (`StackTruststoreStep`, which reads the psama tag from
  state when it runs), `render`, then `ConvergeSteps`. `InitStepIDs(cfg)`
  lists their IDs for checking `--skip-step` early.
- `RenderStep`, ID `render`, always applies: it renders from a fresh
  state.json (the TLS and truststore steps save it themselves), writes the
  files, records `cli_version` and `schema_version`, copies the state into
  the caller's, and sets `d.Compose` to nil.
- `ConvergeSteps` are steps 8–12: `DBSteps` (`db`, plus `db-bootstrap` for
  a remote database), `migrate`, `seed`, `hpds-key` and `start`, each
  wrapped so it sets `d.Compose` from `opts.Compose` when it is nil. The
  Composer's env must be computed per call from the `*Secrets` the seed
  step updates.
- `StartStep`, ID `start`: `compose up -d --wait` (15 min) for
  `StartServices(cfg)`, the mode's services without the one-shots.
- `HPDSKeyStep`, ID `hpds-key`: copies `.pic-sure/hpds/encryption_key`
  into the `hpds-data` volume as `encryption_key` (0600, root) through an
  alpine helper fed on stdin, creating the volume with `st.EnsureVolume`.
  state.json's `hpds_key` records the key's hash and the volume's
  `CreatedAt`, so a re-created volume (after `reset`) is keyed again. With
  `hpds.data: shared` it does nothing: the data set carries its key.
- `ChoosePorts(host, http, https, auto)` and `ChooseDevPortsBase(host,
  avoid...)` are §6.5's port rules: a port not given is 80 or 443, or with
  `auto` the first free pair from 8080/8443. `StackNameInUse(ctx, d, name,
  dir)` finds a container or volume of compose project `name`, or a volume
  labelled for stack `name`, whose stack-dir label isn't `dir`, and returns
  the host ports `dir`'s own containers publish.
- Doctor's new `DoctorOptions.Config` is init's config, used without a
  `Stack`: `memory` counts its HPDS heap once (a running hpds of a stack of
  that name is taken for it).
- `Summary` is init's report (URL, Auth0 URLs, token expiry, next steps);
  `PeekState(dir)` reads state.json without opening the stack.

**Up (035, `up.go`).** §9.2. `UpSteps(d, st, cfg, sec, state,
ConvergeOptions)` is init's plan without `resolve` (the commits are
recorded), with a `restart` step before `start`: `images`, `tls`,
`truststore`, `render`, `db`[, `db-bootstrap`], `migrate`, `seed`,
`hpds-key`, `restart`, `start`; `UpStepIDs(cfg)` lists the IDs. On a
running, current stack every step but `render` and `start` is skipped and
`compose up` recreates nothing. The TLS and truststore steps don't restart
their readers, and compose doesn't recreate a container whose rendered
files changed, so up records the services that must restart in state.json's
`pending_restarts` (added by 035): httpd once `tls` applies, psama once
`truststore` does, and, when the render changes, adds or removes a file
under `render/files`, every service that bind-mounts it or a directory
holding it per `compose config` (every start service if that can't be
read). `restart` restarts the pending services that are running and then
clears them, so `start`'s `--wait` covers the restarted services, and a
run that fails before then leaves them pending for the next. `bindMounts`
(migrate.go) is the shared `compose config` parse.

**Cache list and prune (057, `cache.go`).** §7.1's in-use rules.
`CacheInventory(ctx, d, c, CacheOptions{Stacks})` returns a `CacheReport`:
the stacks it found and every `CacheItem` with a status.

- **Items.** Images are the built catalog images (`hms-dbmi/<name>`) with a
  commit tag (`<sha12>` or `<sha12>-<cfghash8>`) or a dev tag
  (`dev-<stack>-<sha12>[-dirty]`). Tags other tools made (`ws-als-13142`,
  `catch-up`), pull-mode refs and third-party images are never listed. The
  rest are `cache.Entries()`.
- **Stacks.** Every distinct `stack-dir` label on a container (running or
  stopped), volume or network, plus `CacheOptions.Stacks` (the cli passes
  the stack the command runs in, which may have built images before it has
  any containers). A stack is readable when `stack.Open` succeeds and
  `LoadState` does too or finds no state.json (a stack not built yet names
  nothing).
- **Status**, in order:
  - `in-use`: a container references it (an image by ID or reference, an
    entry by a bind mount of it, inside it or above it), whether or not
    the container is labelled, or a readable state names it (`images`,
    `dev_images`, a component's commit for a source tree);
  - `recent`: made or changed within `RecentCacheAge` (1 h), because a
    build may not have saved its state yet;
  - `unknown-stack`: an unreadable stack might use it. That covers every
    commit-tagged image and source tree, and that stack's own dev images.
    Build contexts, downloads and `tmp/` belong to no stack;
  - `unused`.

`PruneCache(ctx, d, c, PruneOptions{DryRun, Force})` runs one step,
`prune`. It removes `unused` items, and with `Force` also `unknown-stack`
ones; `in-use` and `recent` items are never removed. Unless `DryRun`, it
holds the cache's prune lock (`LockPrune`) from before the inventory to the
last removal, and fails, removing nothing, if an image step still holds the
use lock after the cache's `LockTimeout`. Images go through `docker
image rm` under the image's lock, which also refuses an image a container
started meanwhile. Entries go through `cache.RemoveEntry`. A lock still
busy after the cache's `LockTimeout` (the cli uses 5 s) skips the item with
a warning. Other failures don't stop the rest, and the step then fails
naming them. `Freed` counts an image's size once, and only when its last
tag goes.

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
  (streams output), `Pull`, `Tag` (031), `RemoveImage`, and `ImageList(ref
  filter)` (057), one `Image{Ref, ID, RepoTags, Size, Created, Labels}`
  per tag.
- **Networks.** `NetworkList(labelFilters...)` (057).
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
  layout rules), `Rm`, `ContainerInspect` (compare `Health` exactly) and
  `ContainerList` (057: every container, stopped ones included, with its
  image ID and `Mounts`, the bind sources and volume names).
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

**ExecRunner** (ticket 003, `exec.go`) is the production `Runner`;
`cli.newRunner` builds it with the command's logger. `&docker.ExecRunner{}`
is ready to use.

- **Process group.** Each child runs in its own process group. When ctx
  ends, the group gets SIGTERM, and whatever is still in it `WaitDelay`
  (default 5 s) later gets SIGKILL. The call returns once the group is
  empty or killed, so a grandchild (the compose plugin under `docker`, a
  backgrounded process in a script) can't outlive a cancelled command or
  hold its pipes open. If a command exits on its own but leaves a background
  process holding stdout or stderr, the runner stops reading `WaitDelay`
  after the exit and returns the command's result, leaving that process
  alone.
- **Stdin.** A `Cmd.Stdin` that isn't an `*os.File` is copied in by the
  runner, so a reader that blocks forever can't hold up a cancelled call.
  A read error fails the call.
- **No terminal.** Because the group is in the background, a child that
  opens `/dev/tty` to prompt (ssh passphrase, git credentials) stops on
  SIGTTIN until ctx ends. Callers turn prompts off, as the git client does
  with `GIT_TERMINAL_PROMPT=0` and `SSH_ASKPASS_REQUIRE=force`.
- **Foreground** (026) is for one interactive command (`pic-sure compose --
  exec hpds sh`). The child stays in the CLI's process group, so it can
  read the terminal and gets Ctrl-C from it, and `Stream` hands it the
  writers as they are (an `*os.File` becomes its stdout), without line
  buffering. When ctx ends from SIGINT, the runner leaves the child alone:
  Ctrl-C has reached it from the terminal already, and a second signal
  would count as a second Ctrl-C (compose's force-kill). On any other
  cancellation it sends SIGTERM, then SIGKILL `WaitDelay` later. The call
  waits for the child, then returns ctx's error if ctx ended first.
  `cli.newForegroundRunner` builds one.
- **Environment.** A child gets only `PATH`, `HOME`, `TERM`,
  `SSH_AUTH_SOCK`, every `DOCKER_*` and `XDG_*` variable, and
  `HTTP_PROXY`/`HTTPS_PROXY`/`NO_PROXY`/`ALL_PROXY` in either case from the
  CLI's environment, then `Cmd.Env`, where a later entry wins. Anything else
  (`COMPOSE_PROJECT_NAME`, `LANG`, a token in the user's shell) has to be
  put in `Cmd.Env`.
- **Results.** Death by signal N is exit code 128+N. An error from a ctx
  that ended wraps `ctx.Err()` and the context's cause, so a signal's
  `exitcode.Signaled` survives. A program that can't start gives exit code
  -1 and an error.
- **Stream** writes whole lines (a line over 64 KiB arrives in pieces), and
  never calls its two writers at once, so they can be the same writer. A
  writer error ends the copy and is returned.
- **Logging.** Argv, dir and env names at debug level, then the exit code
  and duration. Never env values or stdin.

**WithTimeout** (`timeout.go`) wraps any `Runner` so each call is cancelled
after d (over `ExecRunner`, it returns up to `WaitDelay` later):
`docker.WithTimeout(d.Runner, 10*time.Second)` for `compose ps`, 5–10 s for
probes (§10.2). Long operations take no timeout and end only with their
context. A call that runs out of time returns a `*TimeoutError`
(`"ARGV timed out after 10s"`), which matches `context.DeadlineExceeded`; a
caller's own cancellation stays a plain context error.

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
  output, both streams, to one writer (`Logs` can send compose's stderr to
  `ComposeLogsOpts.Err` instead) (wrap the sink in
  `events.NewLogWriter`) and return an `*ExitError` carrying compose's
  message when it fails. `Down` always passes `--remove-orphans`.
- `ComposeRunOpts.Env` (032) sets container variables as bare `-e NAME`,
  values in `Cmd.Env`; a name the stack's `Env()` sets is refused.
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
- `WorkTree(dir)` (031): a user's checkout, not a managed clone. Its HEAD
  commit, and `Dirty` when it has modified, staged or untracked (not
  ignored) files, whatever the user's status settings.
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

Ticket 019; 057 adds `Entries` and `RemoveEntry` for `cache list/prune`. `cache.DefaultRoot()` is
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
- `ResolveRef(ctx, component, ref)` (`ref.go`, 028): a tag, branch or
  sha to a full commit sha in `git/<repo>.git`, under the repo's fetch
  lock. It clones, or fetches every branch and tag first so a branch gives
  its current head; a full sha the clone already has skips the fetch.
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
- `SourceDir(component, sha)` (031) is where `EnsureSource` keeps that
  tree, without making it, for a step's `Check`.
- `FrontendBuildDir(tag)` (030) is `build/frontend-<tag>`, the frontend
  build's copy of its source, not created.
- `EnsureMavenVolume(ctx, d.Docker)` creates `MavenVolume` (`pic-sure-m2`).
  Mount the volume only under the reactor lock.
- `LockUse(ctx)` (057) takes the cache's use lock shared, and
  `LockPrune(ctx)` takes it exclusively. Any number of commands hold
  `LockUse`, but never alongside a prune. Hold it from before taking a
  source tree or image from the cache until state.json records it:
  `ImagesStep`'s Apply holds it for its whole run. Both wait up to
  `UseLockTimeout` (15 min).
- `Entries()` (057, `prune.go`) lists what prune may remove: source trees
  (`EntrySource`), `build/` contexts, `downloads/`, and `EntryTemp` for
  `tmp/` entries and the `*.tmp-*` siblings in `src/<repo>/`, `git/` and the
  root, with `Repo` set for those under a fetch lock. Clones, the
  release-control clone and `locks/` are never entries. `RemoveEntry(ctx,
  e)` removes one under the lock of whoever writes it (the fetch lock, the
  reactor lock, or the frontend image's lock for `build/frontend-<tag>`),
  and renames a source tree to `<sha>.tmp-prune` first, so a cut-short
  removal never leaves a partial tree that `EnsureSource` would trust.

## internal/release

Ticket 028; 060 adds the gate's self-update action. Release-control (§8)
is read in three calls that `init` and `update` make in order, before any
stack mutation:

- `Fetch(ctx, cache, git, sink, step, Options{Repo, Branch, Commit})`
  clones or fetches release-control into `cache.ReleaseControlDir()` under
  `LockRepo("release-control")`, takes the branch head or the
  `--release-commit` pin (full or abbreviated sha; a pin already in the
  clone needs no fetch; a re-fetch takes tags too, so a tag-only pin
  resolves), and parses `build-spec.json` from that commit. A
  malformed pin is exit 2; an unknown pin or branch is exit 3. The
  `Release` it returns carries the repo, branch, full commit and the
  `BuildSpec`.
- `ParseBuildSpec` reads `.application[] | {project_job_git_key,
  git_hash}`. Only the keys pic-sure reads (the catalog's component keys
  and `PSCLI`) are checked: non-empty `git_hash`, no duplicates. Other
  projects' entries are ignored. `Spec.Ref(key)` looks one up.
- `rel.Gate(ctx, GateOptions{...})` is the CLI compatibility gate (§8
  table). Missing `PSCLI` warns; equal proceeds; older warns, or is exit 5
  with `release.cli_compat: strict`; newer asks `Confirm` (set it only on a
  TTY) or needs `SelfUpdate` (`--self-update`), then calls
  `Updater.SelfUpdate(ctx, version)`, and is otherwise exit 5 naming the
  command to run. SelfUpdate re-executes on success; if it returns nil
  anyway, the gate is exit 5 so the old binary doesn't go on. A nil `Updater` is always exit 5 with
  instructions; `a.newSelfUpdater` (see internal/selfupdate) builds the real one. `IgnoreCLIVersion` turns any mismatch into a warning. A CLI
  version `CompareVersions` can't order (`dev`, a bare sha) is treated as
  equal with a warning, and so is a `PSCLI` that isn't a version.
- `rel.ResolveComponents(ctx, cache, sink, step, cfg.Components)` resolves
  each component's ref to a commit with `cache.ResolveRef` (see
  internal/cache), leaving out a component with a local `source`, and
  any not named in its optional `only` list (031):
  `components.<name>.ref` from pic-sure.yaml if set, else the build-spec's
  key, else `main` with a warning. An unknown ref is exit 3. The commits are
  then in the cache's clones, so `EnsureSource` doesn't fetch again.
- `rel.Record(state, components)` sets state.json's release and
  components; the caller saves the state.


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
  which browsers read as an IPv4 address. `CheckHostname` is that rule on
  its own; the config validator applies it to `network.hostname`. Pass
  `Deps.Rand` and `Deps.Clock.Now()`; a fixed `Rand` doesn't make the key
  deterministic.
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

Ticket 014. Statement builders, escaping, and the clients that run the
statements. Every value reaches the database inside SQL text on the
client's stdin, and the password reaches the client as `MYSQL_PWD` or
`PGPASSWORD` through `Cmd.Env` with a bare `-e`, so neither ever appears in
argv. The clients run through the `docker.Engine` (`Deps.Docker`): `Exec`
for a container, `Run` for a remote client. They take the Engine rather
than `ops.Deps` so that operations can import this package.

- **Running SQL.** `ExecMySQL(ctx, engine, target, statements...)` runs the
  statements in one `mysql --batch` session, which stops at the first
  error. A client that exits non-zero gives a `*docker.ExitError` whose
  argv is the client's own and whose stderr is the client's
  (`ERROR 1045 ... Access denied`, say); a docker failure gives the
  Engine's error. `QueryMySQL` does the same for one
  query and returns its rows as `[][]string`.
  - A `MySQLTarget` with `Container` runs
    `docker exec -i -e MYSQL_PWD <cid> mysql ...` in the stack's
    picsure-db. Adding `Host: "127.0.0.1"` makes the client connect over
    TCP, for the readiness probe.
  - Without `Container`, it runs
    `docker run -i --rm -e MYSQL_PWD mysql:8.0 mysql --host=... --port=...`
    against a remote server.
  - `User` defaults to root.
  - `ExecPostgres` runs psql in the dictionary-db container with
    `PGPASSWORD`, and with `ON_ERROR_STOP`, so that a failure exits
    non-zero. `QueryPostgres` (032) returns rows, tab-separated, so query
    only values without tabs or newlines.
  - A statement's trailing semicolon is optional.
- **Escaping.** `QuoteMySQL` doubles single quotes and backslash-escapes
  `\`, NUL, CR, LF and Ctrl-Z. `QuotePostgres` doubles single quotes and
  switches to `E'...'` when the value has a backslash. It fails with
  `ErrNUL` on a NUL, which Postgres text can't hold. Both escapers assume a
  UTF-8 connection, and the clients force one (`utf8mb4`,
  `PGCLIENTENCODING=UTF8`). The MySQL clients also drop
  `NO_BACKSLASH_ESCAPES` from the session's `sql_mode`, so a remote server
  that sets it stores the values unchanged.
- **Builders** return SQL text containing escaped secrets. Never log it.
  A server error can quote part of a statement, so treat an error's stderr
  with the same care.
  - Seed (033): `AppliedMigrationsQuery`, `CountUsersWithEmail`,
    `SeedAdminUser(email, id)` and `SetApplicationToken`. `SeedAdminUser`
    is one transaction that inserts the user only if the email is absent,
    and gives it the Top Admin and User roles only when it inserted it. A
    replay with the same id changes nothing.
  - Bootstrap (054): `Bootstrap(AppUsers(AppPasswords{...}), syncPasswords)`.
    It creates the databases and users with `IF NOT EXISTS`, plus the
    grants. With `syncPasswords` it adds an `ALTER USER` for each user.
  - Rotation (058): `AlterUserPassword(Account{User, Host}, pw)` for MySQL
    and `AlterPostgresPassword(role, pw)` for Postgres. The local
    picsure-db has both `root@localhost` and `root@%`, so rotating root
    needs a statement for each account.
- `integration_test.go` (build tag `integration`) runs all of this against
  real `mysql:8.0` and `postgres:16-alpine` containers, and skips when
  there is no Docker daemon:
  `go test -tags integration ./internal/sql/`.

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
  plain HTTP to the proxy, so `ParseURL` refuses an `https://` proxy URL
  (025): validation fails with a hint to write `http://`.
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

Ticket 060. `selfupdate.Updater` replaces the running binary with a GitHub
release (§8, D12). The cli builds it with `a.newSelfUpdater(proxyURL, sink,
step)` (`internal/cli/selfupdate.go`); `init` and `update` (034, 036) are
to build it with their config's `*netproxy.Proxy` and set it as
`release.GateOptions.Updater`. The `self-update` command uses the proxy of
the stack it runs in. Without a stack, or when the stack sets no proxy, it
uses the environment's (`HTTPS_PROXY` and so on). `PIC_SURE_RELEASE_API` replaces the
GitHub API root (mirrors, tests).

- `Install(ctx, version)` (the command): refuse a binary it mustn't replace
  before any download (exit 3 with what to do instead): one whose real path
  (symlinks resolved) is in a Homebrew `Cellar`/`Caskroom` or a system
  prefix (`/usr/bin`, `/nix/store`, ...), or whose directory the user can't
  write. Then look the release up: without `--to`, the highest `v2.X.Y`
  in the paged release list that is neither a draft nor a prerelease (the
  rule `install.sh` uses; GitHub's `releases/latest` is just the last one
  published), and exit 3 if there is none; with `--to`, `releases/tags/vX`
  (`--to 2.1.0` means `v2.1.0`; an unknown tag is exit 3). Download
  `checksums.txt`, its cosign bundle if the release has one, and `pic-sure_<os>_<arch>.tar.gz`, check the archive's SHA-256, extract
  `pic-sure` from the archive's root into a temp file beside the binary
  (keeping its mode) and rename it over the binary. The running version,
  or a newest release older than it, is a no-op; an explicit older `--to`
  downgrades.
- Signatures: every v2 release carries `checksums.txt.sigstore.json`
  (`.goreleaser.yaml`), so a release without it is refused (exit 1). With
  cosign on PATH, `CosignVerifier` runs `cosign verify-blob`, accepting only
  a keyless signature from the repo's `release.yml` for the release's own
  tag, so an older release's signed assets can't pass for a newer one;
  failure is exit 1. Without cosign the update warns and relies on the
  checksum, as `install.sh` does; `RequireSignature` makes that an error
  too (exit 3).
- `SelfUpdate(ctx, version)` (`release.SelfUpdater`, the gate's action):
  `Install`, then `syscall.Exec` the new binary with the process's argv and
  environment plus `PIC_SURE_SELF_UPDATED=<version>`, so it doesn't
  return on success. Exec keeps the pid and drops close-on-exec files (the
  stack lock) but skips deferred cleanups, so call the gate before taking
  anything else that needs one. A refusal is exit 5 here. A second
  `SelfUpdate` in a re-executed process is exit 5 rather than a loop.
- HTTP errors name the asset or release, never a URL, so proxy credentials
  can't reach a message. A request that receives nothing for a minute is
  abandoned. Cancellation is checked again before the rename and the exec.

## internal/events

Ticket 001 defines the event types and `Sink`; ticket 004 adds the plain
and NDJSON renderers. The TUI renderer (038) is in `internal/progress`.

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

## internal/progress

Ticket 038. The TUI renderer for an operation's events (§10.3).

- `Model` is a Bubble Tea v2 model fed `EventMsg{Event}` and ended with
  `DoneMsg{OK, LogPath}`. It shows each step with a spinner (a static `•`
  without `Options.Animations`), `✓`, `-` (skipped) or `✗`, the step's
  progress text and warnings, the running step's last `LiveTail` (8) log
  lines, and a failed step's last `FailTail` (20). Event text loses escape
  sequences, control characters (C1 too) and, in log lines, everything
  before a `\r`. A progress or
  log event for an ID that isn't a running step goes under the last running
  step, or at the bottom when none runs; a warning no running step owns gets
  its own row. Ctrl-C asks first ("Press Ctrl-C again", withdrawn after 5 s
  or by another key); the second press calls `Options.Interrupt` once. In
  scrollback mode one more Ctrl-C while it stops sets `Forced` and quits,
  as a second SIGINT kills the process.
- With `Options.Scrollback` (the inline program), finished leading rows are
  printed above the program with `tea.Println`, one print in flight at a
  time so order holds, and the live area keeps only what still runs. On
  `DoneMsg` it prints every remaining row (so a tall final frame can't clip
  one), then quits; the final frame holds only `Log file: <path>` after a
  failure. With `Options.NoColor` the prints are stripped of color, which
  Bubble Tea doesn't do for them. `Init` doesn't query the terminal's
  background: a short run could exit before the reply arrives. Without it (a screen embedding the
  model, tickets 039/040/047), `View` keeps every row and `DoneMsg` doesn't
  quit; `Done()` reports it.
- `Renderer` is an `events.Sink` that runs the model as an inline program
  (not the alt-screen) on the given terminal, with Bubble Tea's signal
  handler off. It starts with the first event, so a command that emits none
  never touches the terminal. A `Result` ends it and waits for the final
  frame, so what the caller prints next goes below it; later events are
  dropped. `Close` ends it as a success and hands the terminal back; a
  later event starts a new program. `Force` is called, after the terminal
  is restored, when the user forces a quit. `Write` is an `io.Writer` that
  prints whole lines above the frame while it runs and straight to the
  output otherwise, after any final frame. A program that fails to start leaves the run without a
  display; the operation still runs.

**Wiring (`internal/cli`).** `execute` wraps the command's context so the
renderer's `Interrupt` cancels it with `exitcode.Signaled(os.Interrupt)`,
and its `Force` exits 130 at once: while the TUI runs, the terminal is in raw mode and Ctrl-C arrives as a key,
not SIGINT, and the run still exits 130 with the step to resume named.
SIGTERM still cancels through `main`. The renderer draws on stderr, so
stdout keeps only the command's summary; `finish` and `printReport` end it
before writing that, and so do `config edit` and `compose` before handing
the terminal to the editor or compose (a `--wait-lock` wait can start it). The run log's stderr records go through `Write`
(`logStderr`), and `openRunLog` keeps the file's path for the failure
line. The hidden `smoke-steps` command (`smokesteps.go`, build tag
`smoketest`, registered through `extraCommands`) emits steps without
Docker for the PTY tests in `smoke/progress_pty_test.go`; the smoke tests
build the binary with that tag.

## internal/log

Ticket 005. Debug logging that is safe to attach to a bug report (§6.1,
§6.3).

- **A run.** `log.New(Options{Level, Stderr, File})` starts one command's
  logging; `Run.Logger()` is what `Deps.Log` holds. Records at the
  `--log-level` go to stderr as text. With `File`, every record down to
  debug also goes, as JSON lines, to `<stack>/.pic-sure/logs/cli-<UTC
  ts>-<pid>.log` (0600, directories 0700), once `Run.OpenFile(store, now)`
  is called. The pid keeps runs that start in the same millisecond from
  racing for a name. The cli's `log.Store` is the stack, so the log files
  and their directory are recorded in the manifest, and pruning goes through
  `Stack.Remove`; pruning ignores a log the manifest doesn't list. Records
  logged before `OpenFile` are held in memory (up to 1 MiB) and written first.
  `OpenFile` then prunes the stack's run logs to the newest that fit in both
  50 files and 50 MiB, always keeping the current one. Failing to open or
  prune is never a command failure. `OpenFile` returns the file's path.
- **Secret values.** `log.RegisterSecrets(values...)` adds to a
  process-wide registry. Every byte a run writes, to stderr or the file,
  passes through it, so a registered value is replaced with `[REDACTED]`
  wherever it appears: message, attr, error, struct, or an attr added with
  `Logger.With` before the value was registered. Its JSON-escaped and
  Go-quoted forms are caught too. Register a secret as soon as it is read
  or generated (008 does it when secrets load; whoever reads one from stdin
  does it there). `log.Redact(s)` applies the registry to any string, for
  `support-bundle` (059). Values shorter than 4 bytes aren't registered:
  they would match all through unrelated text. The registry also scrubs the
  userinfo of every URL (`http://user:pw@host` becomes
  `http://[REDACTED]@host`), so a proxy or Git password reaches no log
  even unregistered. The cli registers the stack's secrets through
  `stack.SetSecretRegistrar(log.RegisterSecrets)`, and redacts the error
  message it prints and puts in the `--json` result the same way.
- **Secret names.** An attr whose name ends in a secret word (`password`,
  `secret`, `token`, `salt`, `api_key`, `encryption_key` and so on; see
  `IsSecretName`) has its value replaced, as has every attr in a group with
  such a name. `token_expiry` and `password_file` aren't secret names.
- `log.ParseLevel` accepts `debug`, `info`, `warn` and `error`, in any
  case.

**Wiring (`internal/cli/logging.go`).** `markRunning` starts the run's
logging when a command's `RunE` starts, and `App.Run` closes it, writing
the exit code and error as the last record. The first record has the
version, OS, command, flags and arguments. `a.openStack` calls
`a.openRunLog(st)` once the stack passes the version gate; `init` (034)
must call it once `.pic-sure/` exists. Until something
calls it, nothing is written to disk. The read-only commands (`status`, `ps`,
`logs`, `doctor`, `config show/get`, `version`) get a file only at
`--log-level debug`, so polling never fills the directory. A bad
`--log-level` is a usage error.

## internal/tui

The v1 TUI shell: landing, setup wizard host, activity screen and load
wizard. `tui.Run` takes the command's context and turns off Bubble Tea's
signal handler, so SIGINT and SIGTERM end the TUI through the context and
the CLI exits 128+N. Ticket 001 removed its script layer: every action fails
to start with "not implemented in v2 yet (ticket NNN)", the release-branch
and dev-overlay lookups return nothing, and the archive lister fails. Tickets
039 and 047 rewire it onto in-process operations, showing their progress
with an embedded `progress.Model` (038).

It runs on the Charm v2 modules (`charm.land/bubbletea/v2`, `bubbles/v2`,
`huh/v2`, `lipgloss/v2`; ticket 002). The root model's `View` returns a
`tea.View` with `AltScreen` set; embedded models (the dashboard) leave
terminal modes to it. `Init` sends `tea.RequestBackgroundColor`, and the
reply is passed to `styles.SetDarkBackground`. Any non-empty `NO_COLOR`
forces `colorprofile.Ascii`, because Bubble Tea itself honours only values
that parse as true. Every embedded huh form is sized with `dialog.Fit`, never
`WithWidth`. huh v2 still freezes group viewports once a width is set, and
still ships esc disabled, so the screens handle esc themselves.

## internal/dashboard

The v1 dashboard. Its polls, log follower and actions report "not
implemented in v2 yet" until ticket 040 rewires them. `pollCmd` and
`TestPollCmdNotWedgedByOrphanGrandchild` stay only as the model for ticket
003's grandchild handling; 040 deletes them.

## internal/wizard

The v1 setup form over `.env` keys. It no longer writes anything; ticket
039 moves it onto the config schema (006).

## internal/filebrowser

The file picker used by the load wizard, on Bubble Tea v2.

## internal/styles

The shared palette and styles, on lipgloss v2. Lip Gloss v2 has no adaptive
colors, so `styles.AdaptiveColor` picks its light or dark variant at render
time from the flag `SetDarkBackground` sets (dark until the terminal
answers). Use it only for hex colors. The status colors are plain ANSI 1–3,
so terminal themes can remap them. Lip Gloss v2 always renders full color and
Bubble Tea downsamples it, so tests that check NO_COLOR output downsample with
`styles/stylestest.Downsample`. Lip Gloss v2 also counts the border in
`Width`/`Height`.

## internal/dialog

The huh dialogs shared by `tui` and `dashboard` (the reset form), and `Fit`,
which sizes an embedded form with a synthetic `WindowSizeMsg` and gives it
`Theme`: huh's Charm theme for the background `styles` reports, with v1's
option grays (huh v2.0.3 swaps their light and dark values).

## internal/exitcode

Ticket 001. The exit codes (§10.4), the `Error` type that carries one, the
constructors `Failed`, `Usage`, `Precondition`, `ConfirmRequired`,
`Incompatible` and `Signaled`, and `FromError`, which maps any error to its
code. The constructors take `fmt.Errorf` arguments, so `%w` wraps a cause.

## internal/phenoinput

Ticket 041. Turns the file given to `data load-phenotype --file` into a CSV
the loader can mount (§9.6), using only Go's archive libraries.

- **Formats**, detected by content, never by name: a plain CSV, a gzip of
  one CSV, a tar (gzipped or not) and a zip. An empty file, binary data
  (a NUL byte in the first 8 KiB, or the magic of bzip2, xz, zstd, lz4 or
  7-Zip), and an archive without `.csv` entries are rejected.
- **Entries** are an archive's regular files whose names end in `.csv` (any
  case), cleaned with `path.Clean`, so `./a.csv` is listed and matched as
  `a.csv`. macOS metadata (`._*`, `__MACOSX/`) doesn't count. An archive
  with a CSV entry outside its own directory (`../x.csv`, `/x.csv`) or two
  CSV entries of the same name is rejected outright.
- `Resolve(ctx, file, Options{Entry, MkdirTemp})` returns an `Input` (`CSV`,
  the absolute path to mount; `Format`; `Entry`; `Warnings`) and a cleanup
  func. A plain CSV is used in place, with no temp dir. Otherwise Resolve
  makes a per-run `phenotype-*` directory with `MkdirTemp` and writes the
  gzip's content or the archive entry to `allConcepts.csv` there (0644 in
  the 0700 directory, so a loader container running as another user can
  read the bind mount). The entry's own name never reaches the path. The
  cleanup func removes that directory; on error Resolve removes it itself.
  `MkdirTemp` is required, so extraction can't fall back to `$TMPDIR`: pass
  the cache's `TempDir` method, which also prunes run directories a killed
  run left behind.
- One CSV entry is selected automatically. Several need `--entry`, and a
  missing or unknown `--entry` is an `*EntryError` listing the entries.
  `--entry` for a non-archive becomes a warning in `Input.Warnings` for the
  caller to emit. Reads stop when `ctx` ends, with its cause as the error.
- Gzip input may hold several members and trailing zero padding, as GNU
  tar accepts. A gzipped tar is read to the end of the stream when listed,
  so a bad checksum after the tar's end marker is still caught.
- `ListCSVEntries(ctx, file)` is the read-only lister for the TUI load
  wizard: the sorted entries of an archive, nil for a plain CSV or gzip,
  and an error for any input Resolve would reject before extracting.

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

## internal/testfixtures/genomic

Ticket 048. Generates the synthetic genomic fixture checked into
`testdata/genomic`: two single-contig VCFs (one BGZF, one plain),
`vcfIndex.tsv`, a phenotype CSV for the same patients, and
`expected.json`, the patients each documented genomic query returns.
`Write(dir, indexDir)` writes it; `indexDir` prefixes the VCF names in the
index, because the loaders open those paths inside their container. The
`genomic-fixture -abs DIR` command writes a loadable copy.
`testdata/genomic/README.md` records the input format HPDS expects.
`go generate ./internal/testfixtures/genomic` refreshes the checked-in copy,
and a test fails when it's stale. `TestFixtureLoadsInHPDS` runs the real
loaders over it when `PICSURE_HPDS_ETL_IMAGE` names a pic-sure-hpds-etl
image.

## tools/templatedrift

The drift check behind `.github/workflows/template-drift.yml` (ticket 066,
spec §14). It reads the AIO commit and the template → AIO source table from
`internal/render/templates/README.md` (so that table's format is its input),
diffs an AIO git checkout between that commit and `-to`, and prints a
Markdown report: exit 0 for no drift, 1 for drift, 2 for an error. It only
reads the AIO repository. The workflow fetches the AIO branch, runs it, and
writes the run summary and the one tracking issue; the repository, branch
and commit are dispatch inputs.

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
