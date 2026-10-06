// Package cli is the cobra command tree (spec §5): global flags, one file
// per command group, and the mapping from a command's error to the process
// exit code.
//
// root.go registers every command's constructor, so tickets never need to
// edit it. Each ticket edits only its own command file (init.go, up.go,
// composeverbs.go, status.go, and so on) and its own wiring file:
//
//   - runner.go (003): the process runner.
//   - output.go (004): the event sink for the output mode, and how a failed
//     command is reported.
//   - logging.go (005): the slog logger.
//   - engine.go (016): the docker Engine.
//   - gitclient.go (018): the git Client.
//   - stack.go (007): finding, opening and locking the stack a command acts
//     on.
//
// deps.go assembles those into an ops.Deps for a command run.
package cli
