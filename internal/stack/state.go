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
	// StackID is the random ID init gave the stack, in the stack-id label
	// of its Docker resources (§6.1). A copy of the directory has the same
	// one, which is how the ownership rule tells a copy from a move.
	StackID string `json:"stack_id,omitempty"`
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
	// DevImages maps the image of each service in an enabled dev variant to
	// the tag of its local build (§7.3), dev-<stack>-<sha12>[-dirty].
	DevImages map[string]string `json:"dev_images,omitempty"`
	// TLS is what the TLS step last copied into the certs volume.
	TLS *TLSInstall `json:"tls,omitempty"`
	// Truststore is what the truststore step last built in the truststore
	// volume.
	Truststore *TruststoreBuild `json:"truststore,omitempty"`
	// HPDSKey is the HPDS key the hpds-key step last copied into the
	// hpds-data volume.
	HPDSKey *VolumeCopy `json:"hpds_key,omitempty"`
	// PendingRestarts are the services a converging step changed the files
	// or volumes of while they ran, which must restart to read them. It is
	// kept until they have, so a failed run's re-run still restarts them.
	PendingRestarts []string `json:"pending_restarts,omitempty"`
	// InitializedAt is when init finished; a stack without it is still
	// being created, and init resumes it.
	InitializedAt time.Time `json:"initialized_at,omitzero"`
	// LastOperation is the most recent mutating command.
	LastOperation *Operation `json:"last_operation,omitempty"`
	// CreatedAt is when the first operation started.
	CreatedAt time.Time `json:"created_at,omitzero"`
	// UpdatedAt is when an operation last started or finished.
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
	// Source is the local checkout (components.<name>.source, absolute)
	// the commit was read from, when the component builds from one (§7.3);
	// Ref is then empty.
	Source string `json:"source,omitempty"`
	// Dirty says the local checkout had uncommitted changes.
	Dirty bool `json:"dirty,omitempty"`
}

// TLSInstall identifies the files the TLS step copied into the certs
// volume, so a re-run can tell whether the volume already holds them.
type TLSInstall struct {
	// Hash is the hex SHA-256 of what was copied: the file names and
	// contents, and how they were installed.
	Hash string `json:"hash"`
	// VolumeCreatedAt is the volume's creation time, as docker reports it.
	// A volume re-created since then, say by compose after a `down -v`, is
	// empty whatever the hash says.
	VolumeCreatedAt string `json:"volume_created_at"`
}

// TruststoreBuild identifies what the truststore step built psama's
// truststore from, so a re-run can tell whether the volume already holds it.
type TruststoreBuild struct {
	// Hash covers the custom certs, the build script and the psama image ID.
	Hash string `json:"hash"`
	// VolumeCreatedAt is the volume's creation time, as docker reports it.
	// A volume re-created since then is empty whatever the hash says.
	VolumeCreatedAt string `json:"volume_created_at"`
}

// VolumeCopy identifies a file a step copied into a stack volume, so a
// re-run can tell whether the volume still holds it.
type VolumeCopy struct {
	// Hash is the hex SHA-256 of what was copied.
	Hash string `json:"hash"`
	// VolumeCreatedAt is the volume's creation time, as docker reports it.
	// A volume re-created since then is empty whatever the hash says.
	VolumeCreatedAt string `json:"volume_created_at"`
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

// SaveState atomically writes state.json. A State without a StackID keeps
// the one state.json has, so a command that loaded state before the ID was
// given can't drop it.
func (s *Stack) SaveState(st *State) error {
	if st.StackID == "" {
		if cur, err := s.LoadState(); err == nil {
			st.StackID = cur.StackID
		}
	}
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	return s.WriteFile(StateFile, append(data, '\n'), 0o644)
}
