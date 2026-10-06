// Package log sets up log/slog for a command run: the stderr level from
// --log-level, the per-run JSON file under .pic-sure/logs/ with retention,
// and the handler that redacts registered secret values (spec §6.1, §6.3).
// Ticket 005 implements it.
package log
