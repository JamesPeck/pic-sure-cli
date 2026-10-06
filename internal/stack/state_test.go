package stack

import (
	"errors"
	"io/fs"
	"os"
	"reflect"
	"testing"
	"time"
)

func TestStateRoundTrip(t *testing.T) {
	s := newStack(t)
	if _, err := s.LoadState(); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("LoadState before any save: err = %v, want ErrNotExist", err)
	}

	t0 := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	st := &State{
		CLIVersion:    "v2.0.0",
		SchemaVersion: 1,
		Release:       Release{Repo: "https://github.com/hms-dbmi/pic-sure-baseline-release-control", Branch: "main", Commit: "49be6ec"},
		Components: map[string]Component{
			"pic-sure": {Ref: "v3.1.0", Commit: "0123456789abcdef0123456789abcdef01234567"},
		},
		Images: map[string]string{"pic-sure-hpds": "0123456789ab"},
	}
	st.StartOperation("init", t0)
	if err := s.SaveState(st); err != nil {
		t.Fatal(err)
	}
	st.FinishOperation(nil, t0.Add(time.Minute))
	if err := s.SaveState(st); err != nil {
		t.Fatal(err)
	}
	wantMode(t, s.Path(StateFile), 0o644)

	got, err := s.LoadState()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, st) {
		t.Errorf("LoadState = %+v, want %+v", got, st)
	}
	want := &Operation{Name: "init", Status: OperationOK, StartedAt: t0, FinishedAt: t0.Add(time.Minute)}
	if !reflect.DeepEqual(got.LastOperation, want) {
		t.Errorf("LastOperation = %+v, want %+v", got.LastOperation, want)
	}
	if !got.CreatedAt.Equal(t0) || !got.UpdatedAt.Equal(t0.Add(time.Minute)) {
		t.Errorf("CreatedAt %v, UpdatedAt %v", got.CreatedAt, got.UpdatedAt)
	}
	if m, _ := s.Manifest(); !m.Has(StateFile) {
		t.Error("state.json not recorded in the manifest")
	}
}

func TestStateOperationStatus(t *testing.T) {
	t0 := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	var st State
	st.FinishOperation(nil, t0) // nothing started: no-op
	if st.LastOperation != nil {
		t.Fatalf("LastOperation = %+v", st.LastOperation)
	}
	st.StartOperation("up", t0)
	if st.LastOperation.Status != OperationRunning {
		t.Errorf("status = %q, want running", st.LastOperation.Status)
	}
	st.FinishOperation(errors.New("compose up failed"), t0.Add(time.Second))
	if st.LastOperation.Status != OperationFailed {
		t.Errorf("status = %q, want failed", st.LastOperation.Status)
	}
	if !st.CreatedAt.Equal(t0) {
		t.Errorf("CreatedAt = %v, want the first operation's start", st.CreatedAt)
	}
}

// A newer CLI's state still loads: unknown fields are ignored.
func TestStateIgnoresUnknownFields(t *testing.T) {
	s := newStack(t)
	data := `{"cli_version": "v2.3.0", "schema_version": 2, "release": {"commit": "abc"}, "from_the_future": {"x": 1}}`
	if err := os.WriteFile(s.Path(StateFile), []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
	st, err := s.LoadState()
	if err != nil {
		t.Fatal(err)
	}
	if st.CLIVersion != "v2.3.0" || st.SchemaVersion != 2 || st.Release.Commit != "abc" {
		t.Errorf("LoadState = %+v", st)
	}
}

func TestStateCorrupt(t *testing.T) {
	s := newStack(t)
	if err := os.WriteFile(s.Path(StateFile), []byte("{"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := s.LoadState(); err == nil {
		t.Error("LoadState of a corrupt file succeeded")
	}
}
