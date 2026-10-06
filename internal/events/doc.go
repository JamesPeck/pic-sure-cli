// Package events is how operations report progress: the event types (spec
// §10.3), the Sink interface they are emitted to, a Recorder for tests, and
// LogWriter, which turns subprocess output into Log events.
//
// Ticket 001 owns the types and the Sink contract. Ticket 004 adds the plain
// (timestamped stderr) and NDJSON (--json stdout) renderers as Sinks, in
// their own files; ticket 038 adds the TUI renderer in internal/tui.
//
// Who emits what: the step engine (internal/steps) emits StepStarted and
// StepDone; an operation's steps emit Progress, Log and Warning; the cli
// layer emits the single Result, last, after the operation returns.
package events
