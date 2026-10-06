// Package docker runs subprocesses and drives the user's docker CLI (spec
// §10.2, D10). There is no Docker SDK: everything goes through Runner, which
// tests replace with fakerunner.
//
// Files and owners:
//   - runner.go (001): Cmd, Result, the Runner interface, ExitError and
//     RunChecked.
//   - The exec Runner (003): process groups, cancellation, timeouts and the
//     base environment, in its own file.
//   - engine.go (016): the Engine interface and its implementation over the
//     docker CLI (inspect, volumes, run/exec/cp/create, images, labels).
//   - compose.go (017): the Composer interface and the Compose adapter, the
//     only code that builds `docker compose` argv.
//   - fakerunner/ (001): the fake Runner for unit tests.
//
// git (internal/git) runs through the same Runner.
package docker
