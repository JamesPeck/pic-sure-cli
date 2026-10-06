package events

// Event is one thing an operation reports while it runs. The concrete types
// below are the whole set; renderers switch on them.
type Event interface {
	// Type is the event's stable name: the "type" field of its NDJSON line.
	Type() string
}

// StepStarted reports that step ID began.
type StepStarted struct {
	ID    string `json:"id"`
	Title string `json:"title"`
}

// Progress reports how far step ID has got.
type Progress struct {
	ID   string   `json:"id"`
	Text string   `json:"text"`
	Pct  *float64 `json:"pct,omitempty"` // 0–100; nil when the total is unknown
}

// Log is one line of output from step ID, usually a subprocess's.
type Log struct {
	ID     string `json:"id"`
	Stream string `json:"stream"` // StreamStdout or StreamStderr
	Line   string `json:"line"`   // without the trailing newline
}

// Log streams.
const (
	StreamStdout = "stdout"
	StreamStderr = "stderr"
)

// Warning is something the user should know that doesn't fail the step.
type Warning struct {
	ID   string `json:"id,omitempty"` // empty when not tied to a step
	Text string `json:"text"`
}

// StepDone reports how step ID ended.
type StepDone struct {
	ID     string     `json:"id"`
	Status StepStatus `json:"status"`
}

// StepStatus is how a step ended.
type StepStatus string

// Step statuses.
const (
	StepOK      StepStatus = "ok"
	StepSkipped StepStatus = "skipped" // Check found it done, or --skip-step named it
	StepFailed  StepStatus = "failed"
)

// Result is the final event of a command, emitted once, by the cli layer,
// after the operation returns. Data is the command's report; Error is set
// when OK is false.
type Result struct {
	OK    bool       `json:"ok"`
	Data  any        `json:"data,omitempty"`
	Error *ErrorInfo `json:"error,omitempty"`
}

// ErrorInfo describes a failed command.
type ErrorInfo struct {
	ExitCode int    `json:"exit_code"`
	Message  string `json:"message"`
	Step     string `json:"step,omitempty"` // the failed step's ID, when a step failed
}

func (StepStarted) Type() string { return "step_started" }
func (Progress) Type() string    { return "progress" }
func (Log) Type() string         { return "log" }
func (Warning) Type() string     { return "warning" }
func (StepDone) Type() string    { return "step_done" }
func (Result) Type() string      { return "result" }
