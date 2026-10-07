// Package ops holds the use cases: init, up, update, build, migrate, seed,
// data loading, shared data, dev, teardown, status, doctor and support
// (spec §9). An operation is a function that takes a context, *Deps and its
// options struct, does its work through Deps, and returns an error, plus its
// report if it has one. Long operations are a list of steps.Step run by
// steps.Run.
//
// Ticket 001 owns Deps (deps.go). Each operation lives in its own file,
// owned by the ticket that implements it: init.go (034), up.go (035),
// update.go (036), build.go (031), migrate.go (032), seed.go (033),
// tls.go (024), doctor.go (025), status.go (027, 037), loader and data
// files (042–046, 049), shareddata.go (050), dev.go (052), db.go (054),
// teardown.go (056), cache.go (057), secrets.go (058), support.go (059).
// Put helpers that several operations need in the lower package they wrap
// (for example the compose adapter), not in a shared file here. See
// docs/architecture.md for the pattern.
package ops
