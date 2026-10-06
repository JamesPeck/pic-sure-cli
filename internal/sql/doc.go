// Package sql builds the MySQL and Postgres statements the CLI runs (seed,
// remote bootstrap, password rotation), escapes values into them, and runs
// them on stdin through `docker exec` or `docker run`, with the password in
// the environment and never in argv. Ticket 014 implements it.
package sql
