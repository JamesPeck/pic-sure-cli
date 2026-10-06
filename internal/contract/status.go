package contract

// Status mirrors `status.sh --json` (schema_version 1).
type Status struct {
	SchemaVersion  int            `json:"schema_version"`
	Command        string         `json:"command"`
	Env            StatusEnv      `json:"env"`
	ReleaseControl ReleaseControl `json:"release_control"`
	Repos          []Repo         `json:"repos"`
	Docker         Docker         `json:"docker"`
	Services       []Service      `json:"services"`
	Health         Health         `json:"health"`
	Database       Database       `json:"database"`
	Migrations     Migrations     `json:"migrations"`
}

type StatusEnv struct {
	FilePresent        bool    `json:"file_present"`
	FileValid          *bool   `json:"file_valid"`
	ComposeProjectName string  `json:"compose_project_name"`
	DBMode             string  `json:"db_mode"`
	DBHost             *string `json:"db_host"`
	DBPort             *string `json:"db_port"`
	AuthMode           string  `json:"auth_mode"`
	PicsureImageTag    string  `json:"picsure_image_tag"`
}

type ReleaseControl struct {
	Repo   string            `json:"repo"`
	Branch string            `json:"branch"`
	Commit *string           `json:"commit"`
	Refs   map[string]string `json:"refs"`
}

type Repo struct {
	Name    string  `json:"name"`
	Present bool    `json:"present"`
	Current *string `json:"current"`
	Target  string  `json:"target"`
	State   string  `json:"state"` // clean | dirty | missing
}

type Docker struct {
	CLIPresent         bool  `json:"cli_present"`
	ComposeAvailable   bool  `json:"compose_available"`
	DaemonReachable    bool  `json:"daemon_reachable"`
	ComposeConfigValid *bool `json:"compose_config_valid"`
}

type Service struct {
	Name     string  `json:"name"`
	State    *string `json:"state"`
	Health   *string `json:"health"`
	ExitCode *int    `json:"exit_code"`
}

// Health is the gateway's deep, cross-service probe (`/system/status`). It is
// only populated when `status.sh --deep-health` was passed: without the flag
// Checked is false and Healthy/Status are nil, with Message carrying the
// reason the probe was skipped.
type Health struct {
	Checked bool    `json:"checked"`
	Healthy *bool   `json:"healthy"`
	Status  *string `json:"status"`
	Message string  `json:"message"`
}

type Database struct {
	Mode    string  `json:"mode"`
	Service *string `json:"service"`
	Host    *string `json:"host"`
	Port    *string `json:"port"`
}

type Migrations struct {
	Checked bool   `json:"checked"`
	Ready   *bool  `json:"ready"`
	Message string `json:"message"`
}
