// Package log sets up log/slog for one command run (spec §6.1, §6.3).
//
// A Run sends records at the --log-level to stderr as text and, once the
// command knows its stack, every record down to debug to a per-run JSON
// log file in the stack's .pic-sure/logs, pruning old ones. All output is
// redacted twice over: the value of any secret-named attr (IsSecretName)
// is replaced, and every secret value in the process-wide registry
// (RegisterSecrets) is scrubbed from the formatted output, wherever it
// appears.
package log
