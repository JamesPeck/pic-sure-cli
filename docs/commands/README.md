# Command reference

Generated from the CLI's help by `make docs`; don't edit by hand.

| Command | Does |
|---|---|
| [`pic-sure`](pic-sure.md) | Install and operate PIC-SURE All-in-One stacks |
| [`pic-sure build`](pic-sure_build.md) | Build the stack's images (default: every component) |
| [`pic-sure cache`](pic-sure_cache.md) | Inspect and prune the host cache |
| [`pic-sure cache list`](pic-sure_cache_list.md) | List cached sources, images and downloads, and what uses them |
| [`pic-sure cache prune`](pic-sure_cache_prune.md) | Remove cache entries no stack uses |
| [`pic-sure compose`](pic-sure_compose.md) | Run docker compose against the rendered stack (escape hatch) |
| [`pic-sure config`](pic-sure_config.md) | Show and change pic-sure.yaml |
| [`pic-sure config edit`](pic-sure_config_edit.md) | Edit the config in $EDITOR, validating on save |
| [`pic-sure config get`](pic-sure_config_get.md) | Print one config value or section |
| [`pic-sure config set`](pic-sure_config_set.md) | Validate and set one config value |
| [`pic-sure config show`](pic-sure_config_show.md) | Print the stack's config, defaults included |
| [`pic-sure data`](pic-sure_data.md) | Load data into HPDS and the dictionary |
| [`pic-sure data demo`](pic-sure_data_demo.md) | Load a demo dataset (default: nhanes) |
| [`pic-sure data load-genomic`](pic-sure_data_load-genomic.md) | Load VCF data into a genomic partition |
| [`pic-sure data load-phenotype`](pic-sure_data_load-phenotype.md) | Load phenotype data into HPDS, then the dictionary |
| [`pic-sure db`](pic-sure_db.md) | Manage a remote MySQL (db.mode remote only) |
| [`pic-sure db bootstrap`](pic-sure_db_bootstrap.md) | Create the databases, users and grants on a remote MySQL |
| [`pic-sure destroy`](pic-sure_destroy.md) | Remove everything the CLI created for this stack |
| [`pic-sure dev`](pic-sure_dev.md) | Run services from local source with debug ports |
| [`pic-sure dev list`](pic-sure_dev_list.md) | List the services that have a dev mode |
| [`pic-sure dev off`](pic-sure_dev_off.md) | Remove SERVICE's dev variant and debug port |
| [`pic-sure dev on`](pic-sure_dev_on.md) | Run SERVICE from local source |
| [`pic-sure dictionary`](pic-sure_dictionary.md) | Rebuild and load the search dictionary |
| [`pic-sure dictionary hydrate`](pic-sure_dictionary_hydrate.md) | Build the dictionary from the loaded HPDS data |
| [`pic-sure dictionary load-csv`](pic-sure_dictionary_load-csv.md) | Load a custom dictionary from CSV |
| [`pic-sure dictionary load-facets`](pic-sure_dictionary_load-facets.md) | Load facet categories, facets and facet concepts |
| [`pic-sure dictionary weights`](pic-sure_dictionary_weights.md) | Recompute the search weights |
| [`pic-sure doctor`](pic-sure_doctor.md) | Check the host, Docker, the stack and the network |
| [`pic-sure down`](pic-sure_down.md) | Stop the stack's containers (volumes are kept) |
| [`pic-sure init`](pic-sure_init.md) | Create a stack in DIR (default: the current directory) and bring it up |
| [`pic-sure logs`](pic-sure_logs.md) | Show service logs |
| [`pic-sure migrate`](pic-sure_migrate.md) | Run the database migrations |
| [`pic-sure ps`](pic-sure_ps.md) | List the stack's containers |
| [`pic-sure reset`](pic-sure_reset.md) | Remove the stack's containers and data volumes; keep its config |
| [`pic-sure restart`](pic-sure_restart.md) | Restart services (default: all) |
| [`pic-sure secrets`](pic-sure_secrets.md) | Manage the stack's secrets |
| [`pic-sure secrets rotate`](pic-sure_secrets_rotate.md) | Rotate a secret everywhere it is used |
| [`pic-sure self-update`](pic-sure_self-update.md) | Replace this binary with a verified release |
| [`pic-sure shared-data`](pic-sure_shared-data.md) | Publish HPDS data for other stacks to mount read-only |
| [`pic-sure shared-data list`](pic-sure_shared-data_list.md) | List the published data sets |
| [`pic-sure shared-data publish`](pic-sure_shared-data_publish.md) | Publish this stack's HPDS data as an immutable data set |
| [`pic-sure shared-data remove`](pic-sure_shared-data_remove.md) | Remove a data set no container uses |
| [`pic-sure status`](pic-sure_status.md) | Report the stack's config, versions, images, services and migrations |
| [`pic-sure support-bundle`](pic-sure_support-bundle.md) | Write a redacted diagnostics archive to attach to an issue |
| [`pic-sure up`](pic-sure_up.md) | Converge an existing stack to running |
| [`pic-sure update`](pic-sure_update.md) | Update the stack to the current release: config, images, migrations |
| [`pic-sure version`](pic-sure_version.md) | Print the CLI version |
