// Package steps runs an operation as an ordered list of idempotent steps
// (spec §9): for each step, Check whether it is already done and Apply it if
// not, emitting StepStarted and StepDone events, honouring --skip-step, and
// stopping at the first failure. Plan reports what Run would do without
// applying anything, for dry runs.
package steps
