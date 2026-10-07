# pic-sure

Install and operate PIC-SURE All-in-One stacks

```text
pic-sure installs, runs, updates, loads data into, and tears down
PIC-SURE All-in-One stacks on macOS and Linux. It needs only docker (with
the compose plugin) and git.

Run with no arguments on a terminal to open the TUI.
```

```
pic-sure [flags]
```

## Subcommands

- [`pic-sure build`](pic-sure_build.md): Build the stack's images (default: every component)
- [`pic-sure cache`](pic-sure_cache.md): Inspect and prune the host cache
- [`pic-sure compose`](pic-sure_compose.md): Run docker compose against the rendered stack (escape hatch)
- [`pic-sure config`](pic-sure_config.md): Show and change pic-sure.yaml
- [`pic-sure data`](pic-sure_data.md): Load data into HPDS and the dictionary
- [`pic-sure db`](pic-sure_db.md): Manage a remote MySQL (db.mode remote only)
- [`pic-sure destroy`](pic-sure_destroy.md): Remove everything the CLI created for this stack
- [`pic-sure dev`](pic-sure_dev.md): Run services from local source with debug ports
- [`pic-sure dictionary`](pic-sure_dictionary.md): Rebuild and load the search dictionary
- [`pic-sure doctor`](pic-sure_doctor.md): Check the host, Docker, the stack and the network
- [`pic-sure down`](pic-sure_down.md): Stop the stack's containers (volumes are kept)
- [`pic-sure init`](pic-sure_init.md): Create a stack in DIR (default: the current directory) and bring it up
- [`pic-sure logs`](pic-sure_logs.md): Show service logs
- [`pic-sure migrate`](pic-sure_migrate.md): Run the database migrations
- [`pic-sure ps`](pic-sure_ps.md): List the stack's containers
- [`pic-sure reset`](pic-sure_reset.md): Remove the stack's containers and data volumes; keep its config
- [`pic-sure restart`](pic-sure_restart.md): Restart services (default: all)
- [`pic-sure secrets`](pic-sure_secrets.md): Manage the stack's secrets
- [`pic-sure self-update`](pic-sure_self-update.md): Replace this binary with a verified release
- [`pic-sure shared-data`](pic-sure_shared-data.md): Publish HPDS data for other stacks to mount read-only
- [`pic-sure status`](pic-sure_status.md): Report the stack's config, versions, images, services and migrations
- [`pic-sure support-bundle`](pic-sure_support-bundle.md): Write a redacted diagnostics archive to attach to an issue
- [`pic-sure up`](pic-sure_up.md): Converge an existing stack to running
- [`pic-sure update`](pic-sure_update.md): Update the stack to the current release: config, images, migrations
- [`pic-sure version`](pic-sure_version.md): Print the CLI version

## Flags

```
  -h, --help              help for pic-sure
      --json              machine-readable JSON on stdout; implies --non-interactive
      --log-level LEVEL   stderr log LEVEL: debug, info, warn or error (default info)
      --no-animations     static TUI, without animation
      --non-interactive   never prompt; fail when an answer is needed
      --plain             plain timestamped output instead of the TUI
      --skip-step ID      skip the step with this ID (repeatable; converging commands only)
      --stack DIR         act on the stack in DIR (default: the stack containing the current directory)
  -v, --version           version for pic-sure
      --wait-lock         if another pic-sure command is changing the stack, wait for it instead of failing
      --yes               answer yes to every confirmation, including destructive ones
```
