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
layer. Global flags are in `a.Global`. Then turn the command's testscript
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
	f.On(fakerunner.Glob("docker compose * run --rm flyway-init"))
	var rec events.Recorder
	d := &ops.Deps{Runner: f, Sink: &rec, Clock: ops.FixedClock(t0), /* ... */}

	if err := ops.Up(ctx, d, st, ops.UpOptions{}); err != nil {
		t.Fatal(err)
	}
	f.AssertOrder(
		fakerunner.Glob("docker compose * up -d --wait picsure-db"),
		fakerunner.Glob("docker compose * run --rm flyway-init"),
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
- `root.go`: registers every command. It wraps each `RunE` so that any
  error raised before a `RunE` starts is reported as a usage error. A
  `PreRunE` that fails for any other reason must return an
  `*exitcode.Error`.
- `helpers.go`: `notImplemented(ticket)`, `newGroup`, and the `help`
  command. A group run without a subcommand, or with an unknown one, is a
  usage error, and so is `help` with an unknown topic.
- `deps.go`: `newDeps` assembles `ops.Deps`. Each field comes from a
  constructor in its owner's file: `runner.go` (003), `output.go` (004, which
  also reports errors), `logging.go` (005), `engine.go` (016) and
  `gitclient.go` (018, landed). Until the others land, the runner fails
  every call, the sink discards events, the logger discards logs, and
  Docker is nil.

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

_Tickets 006 (config schema), 007 (stack directory, state, manifest, lock),
008 (secrets) and 009 (version gate, config migrations) fill this in. File
ownership is in the package doc._ `ops` imports `stack`, so `stack` must not
import `ops`: secret generation takes the `io.Reader` as an argument.

## internal/render

_Tickets 020 (templates) and 021 (render and goldens) fill this in._

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
`docker.Engine` and `docker.Composer` are empty placeholder interfaces
until tickets 016 and 017 add their methods, in their own packages, so that
neither ticket has to edit `Deps`. Ticket 018 filled in `git.Client` the
same way.

## internal/steps

Ticket 001 defines `Step` and `Run`'s signature; ticket 011 implements the
engine.

A `Step` has a stable kebab-case `ID` (users pass it to `--skip-step`), a
`Title`, a `Check` that reports whether the step is already done without
changing anything (nil means always apply), and an `Apply` that does the
work, reports through the sink, and is safe to re-run after a failure.
`steps.Run(ctx, sink, steps, steps.Options{Skip: ...})` emits
`StepStarted`/`StepDone` and stops at the first failure.

_Ticket 011 documents the engine here._

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

_Tickets 003, 016 and 017 document their parts here._

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

_Ticket 019 fills this in; 057 adds `cache list/prune`._

## internal/release

_Ticket 028 fills this in; 060 adds the gate's self-update action._

## internal/pki

_Ticket 012 fills this in._

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

_Ticket 015 fills this in._

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

_Ticket 004 documents the renderers here._

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
