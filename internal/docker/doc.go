// Package docker runs subprocesses and drives the user's docker CLI (spec
// §10.2, D10). There is no Docker SDK: everything goes through Runner, which
// tests replace with fakerunner.
//
// Files:
//   - runner.go: Cmd, Result, the Runner interface, ExitError and
//     RunChecked.
//   - exec.go: ExecRunner, the production Runner (process groups,
//     cancellation, the base environment).
//   - timeout.go: WithTimeout, per-call timeouts for any Runner.
//   - engine*.go: the Engine interface and its implementation over
//     the docker CLI. engine.go holds the interface and the shared error
//     handling; engine_system.go (version, info), engine_images.go,
//     engine_volumes.go, engine_containers.go (run, create, start, exec,
//     cp, rm, inspect) and engine_logs.go hold the rest.
//   - compose.go: the Composer interface and the Compose adapter, the
//     only code that builds `docker compose` argv; composeps.go
//     parses `compose ps --format json`.
//   - fakerunner/: the fake Runner for unit tests.
//
// git (internal/git) runs through the same Runner.
package docker
