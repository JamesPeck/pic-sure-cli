// Package events is how operations report progress: the event types (spec
// §10.3), the Sink interface they are emitted to, a Recorder for tests, and
// LogWriter, which turns subprocess output into Log events.
//
// The plain (plain.go: timestamped stderr) and NDJSON (ndjson.go: --json
// stdout) renderers are here, with WriteReport (ndjson.go), which prints a
// read-only command's single JSON object. The TUI renderer is in
// internal/progress.
//
// Who emits what: the step engine (internal/steps) emits StepStarted and
// StepDone; an operation's steps emit Progress, Log and Warning; the cli
// layer emits the single Result, last, after the operation returns.
package events
