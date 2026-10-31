# AGENTS.md

`pic-sure` installs and runs a PIC-SURE stack with Docker. If you are an
agent driving it, read these before you run anything:

- [skills/pic-sure/SKILL.md](skills/pic-sure/SKILL.md): the task-oriented
  guide. Install, run, load data, check, update and tear down a stack, and
  what to do for each exit code.
- [docs/agents.md](docs/agents.md): the automation contract. Rules,
  global flags, what prompts and how to answer, exit codes and JSON output.
- [docs/json-schemas.md](docs/json-schemas.md): the shape of every JSON
  event and report.
- [docs/commands/README.md](docs/commands/README.md): every command and
  flag, generated from the CLI.

Working on pic-sure itself: the package layout is in
[docs/architecture.md](docs/architecture.md); run `make check` and
`make docs-check` before a PR.

## Three rules

1. **Pass `--json` and parse only stdout.** It implies `--non-interactive`.
   Human output and stderr change freely.
2. **`--yes` is separate consent from `--non-interactive`.** `--json` and
   `--non-interactive` forbid prompts but never consent: `reset`, `destroy`
   and `secrets rotate` exit 4 without `--yes`.
3. **Branch on the exit code first, then read the JSON** for detail.
   `pic-sure compose -- ARGS` is the exception: it exits with compose's code.
   A failed `result`'s `error.code` refines the exit code; treat a code you
   don't know as its exit code.

## Exit codes

Copied from [docs/agents.md](docs/agents.md#exit-codes), which is the
source; a test fails if the two differ.

| Code | Meaning | Examples |
|---|---|---|
| 0 | Success | Also: `status` whenever it finds the stack; `init` on a stack that is already complete; `support-bundle` once the archive is written, even with parts missing. |
| 1 | The operation failed | A step failed (`step_failed`; `result.error.step` names it); `doctor` with a failing check (doctor reports a missing Docker as a check, so it's 1, not 3); the stack lock is held (`stack_locked`; use `--wait-lock`); loading data into a stack that mounts shared data. Otherwise `failed`. |
| 2 | Usage error | Unknown command or flag; missing required init flag; invalid config value or `pic-sure.yaml`, or an `https://` proxy URL (`invalid_config`); a secret with a line break or (Auth0) under 32 bytes (`invalid_secret`); a resumed `init` with a flag that would change the config. Otherwise `usage`. |
| 3 | Precondition unmet | Docker or the compose plugin missing, or the daemon unreachable (`docker_unavailable`, `compose_not_installed`; any command that calls Docker; the message says to install or start Docker); compose too old (`init`); no stack found (`no_stack`), or the one found without `--stack` belongs to another user (`wrong_owner`; pass `--stack DIR` to use it); stack not initialised or not rendered (`stack_not_initialised`, `stack_not_rendered`; run `init` or `up`); ports busy (`ports_busy`); a generated open-mode client secret outside open mode (`generated_open_secret`; run `secrets rotate auth0-client-secret`); the stack's name in use by another stack's Docker resources, as in a copy of a stack directory (the message names the stack directory they belong to; `status --json` lists each as `foreign`), or by those a stack deleted without `destroy` left behind (`foreign_resources`; `init` prints the `docker` commands that remove them). Otherwise `precondition`. |
| 4 | Confirmation required | A destructive command without `--yes`, where it can't ask: stdin or stderr isn't a terminal, or `--non-interactive` or `--json` is set (`confirmation_required`). Nothing was changed. |
| 5 | Incompatible | The stack was rendered by a newer pic-sure (`cli_too_old`; mutating commands); pending config migrations (`config_migrations_pending`; run `update`); `release.cli_compat: strict` and the release was validated with an older pic-sure; the release needs a newer pic-sure and there is no terminal or `--self-update` (`cli_too_old`). Otherwise `incompatible`. |
| 130, 143, 128+N | Interrupted by signal N | Ctrl-C is 130, SIGTERM 143; the command ran its cleanups first. 141 when stdout or stderr is a closed pipe (`pic-sure up --json \| head -1` stops `up`). All are `interrupted`. |
