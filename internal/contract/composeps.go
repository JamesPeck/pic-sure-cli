package contract

// ComposeService is one row of `scripts/compose.sh ps --format json`. Only
// the fields the dashboard needs are modeled; compose emits many more.
type ComposeService struct {
	Service  string `json:"Service"`
	State    string `json:"State"`
	Health   string `json:"Health"`
	ExitCode int    `json:"ExitCode"`
}
