---
name: pic-sure
description: Use the pic-sure CLI to install, run and look after a PIC-SURE All-in-One stack on macOS or Linux with Docker. Use when asked to set up or start a local PIC-SURE, load phenotype, genomic or demo (NHANES, Synthea, 1000 Genomes) data into it, check its health or status, change its config, update it, rotate its secrets, set up Auth0, collect a support bundle, troubleshoot it, or tear it down.
---

# pic-sure

`pic-sure` is one Go binary that installs and runs PIC-SURE All-in-One
stacks with the host's `docker` and `git`. A stack is a directory holding
`pic-sure.yaml`. Every command runs without a terminal, prints JSON with
`--json`, and has documented exit codes.

References, when this file isn't enough:
- `pic-sure COMMAND --help`: always there, always matches the binary.
- [docs/agents.md](https://github.com/JamesPeck/pic-sure-cli/blob/v2/docs/agents.md): rules, prompts and exit codes for automation.
- [docs/json-schemas.md](https://github.com/JamesPeck/pic-sure-cli/blob/v2/docs/json-schemas.md): every JSON shape.
- [README](https://github.com/JamesPeck/pic-sure-cli/blob/v2/README.md) and the [command reference](https://github.com/JamesPeck/pic-sure-cli/blob/v2/docs/commands/README.md).

## Rules

1. **Always pass a subcommand.** `pic-sure` with no arguments opens an
   interactive TUI on a terminal.
2. **Pass `--json` to every command** and parse only stdout's JSON. It
   implies `--non-interactive`, so nothing waits for input. Human output
   and stderr change freely; don't parse them. Commands that stream print
   NDJSON events and end with one `result` line, so read the last line
   (`tail -n1 out.ndjson | jq .`), and pass on any `warning` events'
   `text` to the user. Reports (`status`, `doctor`, `ps`,
   `version`, `support-bundle`) print one object.
3. **Decide on the exit code first**, then read the JSON for detail (see
   the table below).
4. **`--yes` is consent, not a formality.** Only add it when the user asked
   for that destructive action.
5. **Secrets go on stdin, never in arguments, files you write, or logs:**
   `--auth0-client-secret-stdin`, `--db-root-password-stdin`, and
   `secrets rotate auth0-client-secret`. Read them from the environment
   or ask the user to set one; don't echo them back.
6. **Name the stack.** Commands act on the stack containing the working
   directory. Elsewhere, pass `--stack DIR`. `init` takes DIR as its
   argument.
7. **Never run raw `docker compose`** on a stack: it lacks the secrets. Use
   `pic-sure compose -- ARGS` (e.g. `pic-sure compose -- exec hpds sh`).
   `compose` refuses `--json` and exits with compose's own code.
8. **Never edit `.pic-sure/`.** Change settings with `pic-sure config set
   KEY VALUE`, then `pic-sure up`.
9. **Long commands:** the first `init` builds every image from source and
   takes 30 to 60 minutes; later stacks reuse the images and take a few
   minutes. `data demo` takes a few minutes. If your shell tool has a
   shorter timeout than that, start the command in the background with
   its output in a file, then keep checking in the foreground until it has
   finished, e.g. `until grep -q '"type":"result"' init.ndjson; do sleep 20; done`
   in calls that fit your timeout. Don't end your turn or session while it
   runs: in a non-interactive session (`claude -p`, `codex exec`) that
   kills the command. A killed or failed `init`, `up` or `update` resumes
   when you re-run the same command.

## Safety with the user's stack

- Never run `destroy`, `reset`, `secrets rotate` or `self-update` unless
  the user asked for that. A stack you created yourself for a task the
  user gave you (for example "try it, then tear it down") counts as asked.
- Open mode (`--auth-mode open`) lets anyone who reaches the URL query the
  data, and its ports listen on every interface. Use it for local trials
  with demo data only.
- Demo data is synthetic. Real data may be in a stack: don't copy it out,
  and don't share a support bundle from such a stack without asking. The
  bundle is redacted, but check it before it leaves the machine.

## Set up a local stack with demo data

```sh
pic-sure version --json || curl -fsSL https://raw.githubusercontent.com/JamesPeck/pic-sure-cli/v2/install.sh | bash
pic-sure doctor --json > doctor.json
```

The installer puts the binary in `~/.local/bin`; add that to `PATH` if it
says so. Ask before installing anything else. If `doctor` exits 1, show
the user the failing checks
(`jq -r '.checks[] | select(.status == "fail") | "\(.name): \(.message)"' doctor.json`)
and fix them first. Warnings are fine.

Then create the stack. Pick a directory and a name (letters, digits, `-`
and `_`; the name can't change later):

```sh
pic-sure init ~/picsure/demo --json --name demo --auth-mode open \
  --admin-email admin@example.com --auto-ports \
  --set hpds.java_opts=-Xmx2g > init.ndjson 2> init.err
tail -n1 init.ndjson | jq '{ok, url: .data.url, error}'
```

- `--auto-ports` picks free ports from 8080/8443 up. Without it init wants
  80 and 443.
- `-Xmx2g` keeps HPDS small enough for a laptop. The default is 16 GB.
- Use the user's email for `--admin-email` if they gave one.

Load the NHANES demo data and confirm the stack is ready:

```sh
pic-sure --stack ~/picsure/demo data demo nhanes --json > demo.ndjson
pic-sure --stack ~/picsure/demo status --deep --json > status.json
jq '{ready: .deep.data.ready, gateway: .deep.gateway.healthy, url: .auth0.web_origin}' status.json
```

The stack is ready when `.deep.data.ready` is `true` and
`.deep.gateway.healthy` is `true`. Give the user the URL (`.data.url` in
init's result, or `.auth0.web_origin` from status). The certificate is
self-signed, so the browser warns once.

Other datasets: `pic-sure data demo synthea --json`, `1000genomes` or
`all`. Each replaces the stack's phenotype data.

## Exit codes

| Code | Meaning | Do this |
|---|---|---|
| 0 | Success | Read the JSON. `status` is 0 whenever it finds the stack, so check its fields. |
| 1 | The operation failed | Read `.error.step` and `.error.message` in the last `result` line. If the stack is locked by another pic-sure command, wait for it or re-run with `--wait-lock`. Otherwise look at `pic-sure logs --json SERVICE` or the run log in `.pic-sure/logs/`, fix the cause and re-run the same command (it resumes). If you can't fix it, run `support-bundle` and report. |
| 2 | Usage error | Fix the command line: see `pic-sure COMMAND --help`. On a resumed `init`, a flag that differs from the saved config is exit 2: re-run with the original flags and change the setting with `config set`. |
| 3 | Precondition unmet | Fix what the message names: start Docker, free the ports (or `--auto-ports` at init), run `init` (no stack) or `up` (not rendered), or supply the real Auth0 secret. |
| 4 | Confirmation required | Nothing changed. Ask the user; add `--yes` only if they agree. |
| 5 | Incompatible | Pending config migrations: run `update`. The release needs a newer pic-sure: run `self-update` (or add `--self-update`) only with the user's consent. A newer pic-sure rendered the stack: update this binary. |
| 130, 143 | Interrupted (Ctrl-C, SIGTERM) | Cleanups ran. Re-run the same command to resume. |

## Common tasks

Run these inside the stack directory, or add `--stack DIR`.

- **Health:** `pic-sure status --deep --json`, `pic-sure ps --json`,
  `pic-sure doctor --json` (add `--network` for connectivity and proxies).
- **Change config:** `pic-sure config set hpds.java_opts -Xmx4g`, then
  `pic-sure up --json`. `config show --json` prints the whole config;
  `config get KEY --json` one value. `config set` takes its flags before
  KEY.
- **Start and stop:** `pic-sure up --json` (safe to re-run; it converges),
  `pic-sure down --json` (keeps data), `pic-sure restart --json SERVICE`.
- **Logs:** `pic-sure logs --json SERVICE | tail -n 200 | jq -r .line`
  (no SERVICE means every service). Avoid `--follow` in a tool call; it
  doesn't return.
- **Update:** `pic-sure update --dry-run --json` shows the plan; then
  `pic-sure update --json`. If it exits 5 for a newer pic-sure, ask the
  user before adding `--self-update`.
- **Support bundle:** `pic-sure support-bundle --json | jq -r .path`.
  Tell the user where it is; don't upload it yourself.
- **Your own phenotype data:**
  `pic-sure data load-phenotype --json --file allConcepts.csv.gz` (plain,
  gzip, tar.gz or zip; `--entry NAME` picks a CSV in an archive), or
  `--input-dir DIR` for a directory of CSVs. It replaces the phenotype
  data and rebuilds the dictionary. `--heap MB` sets the loader's heap.
- **Genomic data:**
  `pic-sure data load-genomic --json --partition chr21 --vcf-index vcfIndex.tsv --promote --enable-profile`.
  Without `--promote` the partition is only staged.
- **Dictionary only:** `pic-sure dictionary hydrate --json` rebuilds it
  from the loaded data; `pic-sure dictionary weights --json` recomputes
  the search weights.
- **Two stacks on one machine:** give each its own directory and
  `--name`, and pass `--auto-ports` to both. Images are shared.
- **Auth0 instead of open mode:** drop `--auth-mode open` and pass
  `--auth0-client-id ID --auth0-client-secret-stdin`, with the secret
  (at least 32 bytes) on stdin, e.g.
  `printf '%s\n' "$AUTH0_CLIENT_SECRET" | pic-sure init DIR --json --name NAME --admin-email EMAIL --auth0-client-id "$AUTH0_CLIENT_ID" --auth0-client-secret-stdin --auto-ports`.
  The admin email must be a Google account. The URLs to register in the
  Auth0 application are in `.data.auth0` of init's result and `.auth0` of
  `status --json`: `callback_url`, `logout_url` and `web_origin`.
- **Open mode to Auth0 later:** `pic-sure config set auth.auth0.client_id ID`,
  then `pic-sure config set auth.mode required`, then
  `printf '%s\n' "$AUTH0_CLIENT_SECRET" | pic-sure --yes --json secrets rotate auth0-client-secret`,
  then `pic-sure up --json`.
- **Remote MySQL:** `pic-sure init DIR --db-mode remote --db-host HOST
  --db-port PORT --db-root-user USER --db-root-password-stdin` with the
  password on stdin, plus the usual init flags; see
  `pic-sure init --help` and `pic-sure db bootstrap --help`.
- **Proxy:** `pic-sure init DIR --https-proxy http://HOST:PORT` plus the
  usual flags (always `http://`, even for HTTPS), or
  `pic-sure config set proxy.https http://HOST:PORT` and `pic-sure up --json`. See the README's
  [Proxy](https://github.com/JamesPeck/pic-sure-cli/blob/v2/README.md#proxy)
  section; `doctor --network` explains the Docker daemon's own proxy.
- **Tear down** (only when asked): `pic-sure --stack DIR destroy --yes --json`
  removes the stack's containers, volumes and the files pic-sure created.
  `pic-sure reset --yes --json` empties the data but keeps the config.
  `pic-sure cache prune --json` frees images no stack uses.

## Troubleshooting

- **Ports busy** (exit 3, or "port is already allocated"): use
  `--auto-ports` at init, or `pic-sure config set network.https_port PORT`
  and `pic-sure config set network.http_port PORT`, then `pic-sure up --json`.
- **Not enough memory:** `doctor`'s `memory` check compares Docker's
  memory with the HPDS heap. Lower it (`--set hpds.java_opts=-Xmx2g` at
  init, or `pic-sure config set hpds.java_opts -Xmx2g` and `pic-sure up --json`) or ask the user to
  give Docker more (Docker Desktop: Settings, Resources). `data demo
  --heap 1024` lowers the loader's heap.
- **"all predefined address pools have been fully subnetted":** Docker
  has run out of network ranges. Each stack takes 4 networks, and stopped
  stacks keep theirs, so the default pools fill at roughly 7 stacks. Ask
  the user which old stacks to `destroy`; don't prune networks yourself.
  Then re-run the same command.
- **Docker Desktop file sharing:** a data file outside Docker's shared
  folders is copied into pic-sure's cache first, so expect a copy step.
  If even the copy can't be seen, the load exits 3: move the file under
  the user's home directory.
- **Stack locked** (exit 1, "locked by"): another pic-sure command is
  changing the stack. Wait for it, or re-run with `--wait-lock`.
- **"Data Sources: 0"** on the landing page with one dataset loaded is
  upstream dictionary behaviour (it counts datasets only when there are
  two or more), not a failed load. Check `status --deep` instead.
- **Is it ready?** Only `status --deep --json` says: `.deep.data.ready`
  is `true` when HPDS answers queries. `false` on a new stack means no
  data is loaded yet.
- **A step failed:** the failed `result` names `.error.step`. Re-running
  the same command skips the steps that already finished.
