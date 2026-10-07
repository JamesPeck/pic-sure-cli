package release_test

import (
	"strings"
	"testing"

	"github.com/JamesPeck/pic-sure-cli/internal/release"
)

func TestParseBuildSpec(t *testing.T) {
	// The shape of release-control's james_mono branch, plus an entry for a
	// project pic-sure doesn't build.
	spec, err := release.ParseBuildSpec([]byte(`{
  "application": [
    {"project_name": "PIC-SURE-API Build", "project_job_git_key": "PSA", "git_hash": "v4.0.0"},
    {"project_name": "PIC-SURE-Frontend Build", "project_job_git_key": "PSF", "git_hash": "main"},
    {"project_name": "Something else", "project_job_git_key": "OTHER", "git_hash": ""},
    {"project_name": "Unkeyed"},
    {"project_name": "PIC-SURE CLI", "project_job_git_key": "PSCLI", "git_hash": "v2.0.0"}
  ]
}`))
	if err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]string{"PSA": "v4.0.0", "PSF": "main", "PSCLI": "v2.0.0"} {
		if got, ok := spec.Ref(key); !ok || got != want {
			t.Errorf("Ref(%s) = %q, %v; want %q", key, got, ok, want)
		}
	}
	for _, key := range []string{"PSM", "DICTIONARY_ETL", "OTHER"} {
		if got, ok := spec.Ref(key); ok {
			t.Errorf("Ref(%s) = %q, want none", key, got)
		}
	}
}

func TestParseBuildSpecRejects(t *testing.T) {
	tests := map[string]struct{ json, want string }{
		"not JSON":       {`{`, "parsing build-spec.json"},
		"no list":        {`{"applications": []}`, "no application list"},
		"empty git_hash": {`{"application": [{"project_job_git_key": "PSM", "git_hash": ""}]}`, "PSM has no git_hash"},
		"no git_hash":    {`{"application": [{"project_job_git_key": "PSCLI"}]}`, "PSCLI has no git_hash"},
		"duplicate": {`{"application": [{"project_job_git_key": "PSA", "git_hash": "a"},
			{"project_job_git_key": "PSA", "git_hash": "b"}]}`, "PSA appears twice"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := release.ParseBuildSpec([]byte(tt.json))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("err = %v, want %q", err, tt.want)
			}
		})
	}
}
