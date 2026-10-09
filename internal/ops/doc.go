// Package ops holds the use cases: init, up, update, build, migrate, seed,
// data loading, shared data, dev, teardown, status, doctor and support
// (spec §9). An operation is a function that takes a context, *Deps and its
// options struct, does its work through Deps, and returns an error, plus its
// report if it has one. Long operations are a list of steps.Step run by
// steps.Run.
//
// Deps is in deps.go, and each operation lives in its own file (init.go,
// up.go, update.go, build.go and so on). Put helpers that several
// operations need in the lower package they wrap (for example the
// compose adapter), not in a shared file here. See docs/architecture.md for
// the pattern.
package ops
