// Package sql builds the MySQL and Postgres statements the CLI runs (seed,
// remote bootstrap, password rotation), escapes values into them, and runs
// them on stdin through `docker exec` or `docker run`, with the password in
// the environment and never in argv (spec §6.3).
//
// Files:
//   - escape.go: string-literal and identifier quoting for both databases.
//   - exec.go: the targets, and ExecMySQL, QueryMySQL and ExecPostgres.
//   - statements.go: the statement builders.
package sql
