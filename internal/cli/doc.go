// Package cli is the cobra command tree (spec §5): global flags, one file
// per command group, and the mapping from a command's error to the process
// exit code.
//
// root.go registers every command's constructor. Each command group has
// its own file (init.go, up.go, composeverbs.go, status.go, and so on), and
// each dependency its own wiring file:
//
//   - runner.go: the process runner.
//   - output.go: the output mode, its event sink, and how a command ends:
//     finish, printReport, or an error that reportError reports.
//   - logging.go: the slog logger.
//   - engine.go: the docker Engine.
//   - gitclient.go: the git Client.
//   - stack.go: finding, opening and locking the stack a command acts on.
//
// deps.go assembles those into an ops.Deps for a command run.
package cli
