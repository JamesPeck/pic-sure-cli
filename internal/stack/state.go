package stack

import (
	"encoding/json"
	"fmt"
	"time"
)

// State is .pic-sure/state.json: what the CLI last did to the stack. It holds
// no secrets. Unknown fields are ignored on load, so a read-only command of
// an older CLI can still read a newer CLI's state (§10.6).
type State struct {
	// CLIVersion is the version of the CLI that last rendered the stack.
	CLIVersion string `json:"cli_version"`
	// SchemaVersion is the pic-sure.yaml schema the stack was last
	// rendered with (§6.2).
	SchemaVersion int `json:"schema_version"`
	// Release is the release-control commit the component commits came
	// from (§8).
	Release Release `json:"release"`
	// Components maps each component (pic-sure, frontend, migrations,
	// dictionary-etl) to the commit the stack runs.
	Components map[string]Component `json:"components,omitempty"`
	// Images maps each image name to the tag the stack runs.
	Images map[string]string `json:"images,omitempty"`
	// LastOperation is the most recent mutating command.
	LastOperation *Operation `json:"last_operation,omitempty"`
	// CreatedAt is when init first saved the state.
	CreatedAt time.Time `json:"created_at,omitzero"`
	// UpdatedAt is when the state last changed.
	UpdatedAt time.Time `json:"updated_at,omitzero"`
}

// Release is a release-control commit.
type Release struct {
	Repo   string `json:"repo,omitempty"`
	Branch string `json:"branch,omitempty"`
	Commit string `json:"commit,omitempty"`
}

// Component is the source commit of one component.
type Component struct {
	// Ref is the tag or branch it was resolved from: build-spec's git_hash,
	// or the ref in pic-sure.yaml.
	Ref string `json:"ref,omitempty"`
	// Commit is the full commit SHA.
	Commit string `json:"commit"`
}

// Operation is one run of a mutating command.
type Operation struct {
	// Name is the command, such as "up" or "update".
	Name       string          `json:"name"`
	Status     OperationStatus `json:"status"`
	StartedAt  time.Time       `json:"started_at"`
	FinishedAt time.Time       `json:"finished_at,omitzero"`
}

// OperationStatus is how far an operation got.
type OperationStatus string

// Operation statuses. An operation still "running" in a stack nobody holds
// the lock on was interrupted.
const (
	OperationRunning OperationStatus = "running"
	OperationOK      OperationStatus = "ok"
	OperationFailed  OperationStatus = "failed"
)

// StartOperation records that the command name started at now.
func (st *State) StartOperation(name string, now time.Time) {
	st.LastOperation = &Operation{Name: name, Status: OperationRunning, StartedAt: now}
	st.touch(now)
}

// FinishOperation records how the operation StartOperation began ended: ok
// when err is nil, failed otherwise. Without one it does nothing.
func (st *State) FinishOperation(err error, now time.Time) {
	if st.LastOperation == nil {
		return
	}
	st.LastOperation.Status = OperationOK
	if err != nil {
		st.LastOperation.Status = OperationFailed
	}
	st.LastOperation.FinishedAt = now
	st.touch(now)
}

func (st *State) touch(now time.Time) {
	if st.CreatedAt.IsZero() {
		st.CreatedAt = now
	}
	st.UpdatedAt = now
}

// LoadState reads state.json. Before init first saves it, the error wraps
// fs.ErrNotExist.
func (s *Stack) LoadState() (*State, error) {
	data, err := s.root.ReadFile(StateFile)
	if err != nil {
		return nil, err
	}
	var st State
	if err := json.Unmarshal(data, &st); err != nil {
		return nil, fmt.Errorf("reading %s: %w", s.Path(StateFile), err)
	}
	return &st, nil
}

// SaveState atomically writes state.json.
func (s *Stack) SaveState(st *State) error {
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	return s.WriteFile(StateFile, append(data, '\n'), 0o644)
}
