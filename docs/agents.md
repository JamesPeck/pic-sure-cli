# pic-sure for agents and automation

How to drive pic-sure from scripts, CI and AI agents. Every command runs
without a terminal; this page covers how to keep it from prompting, what to
parse and what the exit codes mean. The [README](../README.md) explains
what the commands do, and the [command reference](commands/README.md) lists
every flag. For a coding agent, the [pic-sure skill](../skills/pic-sure/SKILL.md)
turns this page into a task-oriented guide; the README's
[For AI agents](../README.md#for-ai-agents) section says how to install it.

## Rules

1. **Pass `--json`** to every command you parse. It implies
   `--non-interactive`, so pic-sure never waits for input. Without `--json`,
   pass `--non-interactive` (or run without a terminal on stdin and stdout).
2. **`--yes` is separate consent.** `--json` and `--non-interactive` forbid
   prompts but never consent: `reset`, `destroy` and `secrets rotate` exit 4
   without `--yes`. `--yes` never replaces the pic-sure binary; that takes
   `--self-update`.
3. **Parse only JSON.** Human output, plain-mode lines and stderr change
   freely. The JSON shapes are in [json-schemas.md](json-schemas.md) and
   change additively within `schema_version` 2: ignore fields you don't know.
4. **Decide on the exit code**, then read the JSON for detail. The one
   exception is `pic-sure compose -- ARGS`, which exits with compose's own
   code, so 2–5 there don't mean what the table below says.
5. **Secrets go on stdin**, never in arguments: `--auth0-client-secret-stdin`,
   `--db-root-password-stdin`, and `secrets rotate auth0-client-secret`
   (and `db-root` with a remote database). One trailing newline is
   stripped; any other CR or LF is a usage error. With both init flags,
   give one secret per line, the client secret first. Pipe the secret in:
   when stdin is a terminal, pic-sure asks for each secret on stderr and
   reads one line with echo off, and with `--json` or `--non-interactive`
   a terminal stdin is exit 2.
6. **Name the stack.** Commands act on the stack containing the working
   directory. Pass `--stack DIR` when you aren't inside it; `init` takes
   DIR as its argument.
7. **Don't run raw `docker compose`** against a stack. It has no `.env`,
   so compose won't have the secrets. Use `pic-sure compose -- ARGS`.
8. **Don't edit `.pic-sure/`.** Change config with `pic-sure config set
   KEY VALUE` (validated), then `pic-sure up`.

## Global flags

| Flag | Meaning |
|---|---|
| `--stack DIR` | Act on the stack in DIR. Default: the stack containing the current directory. |
| `--json` | NDJSON events, or one report object, on stdout. Implies `--non-interactive`. Not with `--plain`. |
| `--plain` | Timestamped human lines on stderr instead of the TUI progress view. Chosen automatically without a terminal or when `CI` is set (`CI=false` and `CI=0` don't count). |
| `--non-interactive` | Never prompt; fail when an answer is needed (exit 4 for a missing confirmation). |
| `--yes` | Answer yes to every confirmation, including destructive ones. |
| `--wait-lock` | If another pic-sure command is changing the stack, wait for it instead of failing with exit 1. |
| `--skip-step ID` | Skip a step (repeatable). Only init, up, update, build, migrate, db bootstrap and the dictionary commands take it; every other command is exit 2 before it does anything. The IDs are the `id`s of the command's `step_started` events; an unknown ID is exit 2 before anything changes. |
| `--log-level LEVEL` | stderr log level: `debug`, `info`, `warn` or `error`. The per-run file log under `.pic-sure/logs/` is always at debug. |
| `--no-animations` | Static TUI. |

Flags, global or not, can go before or after a command's arguments, with
two exceptions: everything after `compose --` goes to compose, and
`config set` takes its flags before KEY (so a VALUE like `-Xmx4g` needs no
quoting). `--json` overrides the automatic choice of plain output; only an
explicit `--plain` conflicts with it. `NO_COLOR` is honoured.

## What prompts, and how to answer

| Command | Prompt on a terminal | Without one |
|---|---|---|
| `reset`, `destroy` | type the stack name | `--yes`, else exit 4, nothing changed |
| `secrets rotate NAME` | `[y/N]` | `--yes`, else exit 4. Names that read stdin always need `--yes`. |
| `init`, `update` when the release needs a newer pic-sure | `[y/N]` to self-update | `--self-update` installs it and continues; else exit 5. `--ignore-cli-version` goes on with this one. |
| `pic-sure` with no arguments | opens the TUI | prints help |

Nothing else prompts. `init` never opens its wizard from the command line:
a missing required flag is exit 2 naming it. Its required flags are
`--name` and `--admin-email`, plus `--auth0-client-id` and
`--auth0-client-secret-stdin` unless `--auth-mode open`.

## Exit codes

| Code | Meaning | Examples |
|---|---|---|
| 0 | Success | Also: `status` whenever it finds the stack; `init` on a stack that is already complete; `support-bundle` once the archive is written, even with parts missing. |
| 1 | The operation failed | A step failed (`result.error.step` names it); `doctor` with a failing check (doctor reports a missing Docker as a check, so it's 1, not 3); the stack lock is held (use `--wait-lock`); loading data into a stack that mounts shared data. |
| 2 | Usage error | Unknown command or flag; missing required init flag; invalid config value or `pic-sure.yaml`; an `https://` proxy URL; a secret with a line break or (Auth0) under 32 bytes; a resumed `init` with a flag that would change the config. |
| 3 | Precondition unmet | Docker or the compose plugin missing, or the daemon unreachable (any command that calls Docker; the message says to install or start Docker); compose too old (`init`); no stack found, or the one found without `--stack` belongs to another user (pass `--stack DIR` to use it); stack not initialised or not rendered (run `init` or `up`); ports busy; a generated open-mode client secret outside open mode (run `secrets rotate auth0-client-secret`); the stack's name in use by another stack's Docker resources, as in a copy of a stack directory (the message names the stack directory they belong to; `status --json` lists each as `foreign`), or by those a stack deleted without `destroy` left behind (`init` prints the `docker` commands that remove them). |
| 4 | Confirmation required | A destructive command with no terminal and no `--yes`. Nothing was changed. |
| 5 | Incompatible | The stack was rendered by a newer pic-sure (mutating commands); pending config migrations (run `update`); `release.cli_compat: strict` and the release names another pic-sure; the release needs a newer pic-sure and there is no terminal or `--self-update`. |
| 130, 143, 128+N | Interrupted by signal N | Ctrl-C is 130, SIGTERM 143; the command ran its cleanups first. 141 when stdout or stderr is a closed pipe (`pic-sure up --json \| head -1` stops `up`). |

On a non-zero exit, stderr's last line is `pic-sure: MESSAGE`. With
`--json`, stdout's last line is then the failed `result` with the same
message and the exit code, unless the command already printed its report:
`doctor` (exit 1), `migrate --check` and `db bootstrap --check` (exit 3)
print their report with nothing after it. A report
command that fails before it has a report (`status` with no stack, exit 3)
prints the failed `result` instead. Re-running a failed converging command (`init`, `up`,
`update`) resumes it: completed steps are skipped.

## JSON output

- **Reports.** `status`, `doctor`, `ps`, `version`, `support-bundle`,
  `cache list`, `dev list`, `shared-data list`, `migrate --check` and
  `db bootstrap --check` print one object with `schema_version: 2` first,
  and no `result`. `config show --json` prints the config and `config get
  KEY --json` prints `{"key", "value"}`, without `schema_version`.
- **Event streams.** Every other command prints NDJSON events and ends
  with one `result` line, whose `data` holds the command's report.
  `compose` refuses `--json`. `logs --json` emits each log line as a `log`
  event of step `logs`.

Both are specified in [json-schemas.md](json-schemas.md).

## Recipe: a demo stack from nothing

Needs `jq` besides pic-sure's own requirements.

```sh
set -eu
dir="$HOME/picsure/demo"

# Print the failed result (exit code, step, message) and stop.
fail() { tail -n1 "$1" | jq -c '.error'; exit 1; }

pic-sure doctor --json > doctor.json || {
  jq -r '.checks[] | select(.status == "fail") | "\(.name): \(.message)"' doctor.json
  exit 1
}

# Open mode needs no Auth0 credentials; init generates the client secret.
# The first init builds every image from source: allow an hour, and give
# the command that long a timeout. If it's killed (exit 130/143 after its
# cleanups), re-running the same command resumes it.
pic-sure init "$dir" --json --name demo --auth-mode open \
  --admin-email admin@example.com --auto-ports \
  --set hpds.java_opts=-Xmx2g > init.ndjson || fail init.ndjson
url="$(tail -n1 init.ndjson | jq -r '.data.url')"

# Returns once HPDS is healthy with the new data.
pic-sure --stack "$dir" data demo nhanes --json > demo.ndjson || fail demo.ndjson

pic-sure --stack "$dir" status --deep --json > status.json
jq -e '.deep.data.ready == true' status.json
jq -r '.services[] | "\(.service) \(.state) \(.health)"' status.json
echo "PIC-SURE is at $url"
```

If init exits 5, the release was validated with a newer pic-sure: run
`pic-sure self-update` (or add `--self-update` to init, except with a
`--*-stdin` flag) and re-run init. A failed init resumes when re-run with
the same flags.

`status --json` has the stack's URL as `.auth0.web_origin`, in every auth
mode. With Auth0 instead of open mode, drop `--auth-mode open` and add
`--auth0-client-id ID --auth0-client-secret-stdin`, with the secret on
stdin. `.data.auth0` in init's result (and `.auth0` in `status --json`)
has the URLs to register in the Auth0 application.

Teardown, without prompts:

```sh
pic-sure --stack "$dir" destroy --yes --json
pic-sure cache prune --json    # optional: drop images no stack uses
```

## Other recipes

```sh
pic-sure update --dry-run --json | tail -n1 | jq '.data'   # the plan, no changes
pic-sure update --json --self-update                        # unattended update
pic-sure config set hpds.java_opts -Xmx8g && pic-sure up --json
pic-sure data load-phenotype --file allConcepts.csv.gz --json
pic-sure reset --keep-db --yes --json && pic-sure up --json  # empty HPDS, same DB
pic-sure support-bundle --json | jq -r .path
```

## Changes from pic-sure v1

pic-sure v1 wrapped the bash All-in-One scripts; v2 replaces them, so
scripts written for v1 need updating.

| v1 | v2 |
|---|---|
| Run inside a `pic-sure-all-in-one` checkout, or `--root DIR` | Run inside a stack directory, or `--stack DIR`. No checkout is needed. |
| `--yes` and `--non-interactive` were the same flag | `--non-interactive` forbids prompts; `--yes` consents. `--json` implies the first, never the second. |
| Arguments passed through to scripts; `--` reached the script | No passthrough, except `compose -- ARGS`. Every flag is pic-sure's own. |
| `init --auth0-client-secret VALUE`, `--db-root-password VALUE` | `--auth0-client-secret-stdin`, `--db-root-password-stdin` |
| `init --skip-auth` | `init --auth-mode open` (a client secret is generated) |
| `init --wizard` | `pic-sure` with no arguments on a terminal |
| `init --release-control-branch` | `init --release-branch`; later, `config set release.branch` and `update` |
| `init` options without a flag (heap and so on) | `init --set KEY=VALUE` for any non-secret key |
| `preflight [--network]`, exit 1 on failure | `doctor [--network]`, exit 1 on failure |
| `status --json` with `schema_version: 1` | `status --json` with `schema_version: 2`; a different document (json-schemas.md) |
| `etl load-phenotype`, `etl load-genomic`, other `etl` subcommands | `data load-phenotype`, `data load-genomic`, `dictionary hydrate/load-csv/load-facets/weights` |
| `demo-data [DATASET] [--all]` | `data demo [nhanes\|synthea\|1000genomes\|all]` |
| `seed-db` | Part of `up` and `init` |
| `migrate --check --repair --no-restart --bootstrap-remote-db` | `migrate [--check \| --repair]`; `db bootstrap` for a remote database |
| `release-control` | `update` (moves to the branch head) and `update --release-commit SHA` |
| `dev up OVERLAY`, `dev off NAME` | `dev on SERVICE`, `dev off SERVICE` |
| `reset --all`, `reset --repos` | `reset` (removes the database too; `--keep-db` keeps it). There are no sibling repos. |
| `uninstall --yes` | `destroy --yes` |
| Exit 2 for every failure where no script ran | 2 for usage only; 3 precondition, 4 confirmation, 5 incompatible |
| Script exit codes passed through | 1 for a failed operation |
