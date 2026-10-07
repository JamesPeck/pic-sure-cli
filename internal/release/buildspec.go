package release

import (
	"encoding/json"
	"fmt"

	"github.com/JamesPeck/pic-sure-cli/internal/catalog"
)

// BuildSpecFile is the file in release-control that pins the components.
const BuildSpecFile = "build-spec.json"

// A BuildSpec is release-control's build-spec.json: one ref per build-spec
// key. Only `.application[] | {project_job_git_key, git_hash}` is read.
type BuildSpec struct {
	refs map[string]string
}

type buildSpecFile struct {
	Application []struct {
		Key *string `json:"project_job_git_key"`
		Ref *string `json:"git_hash"`
	} `json:"application"`
}

// ParseBuildSpec parses build-spec.json. An entry for a key pic-sure reads
// (a component's, or catalog.CLISpecKey) needs a non-empty git_hash and
// mustn't appear twice; entries for other projects are ignored.
func ParseBuildSpec(data []byte) (*BuildSpec, error) {
	var f buildSpecFile
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", BuildSpecFile, err)
	}
	if f.Application == nil {
		return nil, fmt.Errorf("parsing %s: no application list", BuildSpecFile)
	}
	refs := map[string]string{}
	for _, app := range f.Application {
		if app.Key == nil || !readsKey(*app.Key) {
			continue
		}
		key := *app.Key
		if app.Ref == nil || *app.Ref == "" {
			return nil, fmt.Errorf("parsing %s: %s has no git_hash", BuildSpecFile, key)
		}
		if _, dup := refs[key]; dup {
			return nil, fmt.Errorf("parsing %s: %s appears twice", BuildSpecFile, key)
		}
		refs[key] = *app.Ref
	}
	return &BuildSpec{refs: refs}, nil
}

func readsKey(key string) bool {
	_, ok := catalog.ComponentBySpecKey(key)
	return ok || key == catalog.CLISpecKey
}

// Ref returns the git_hash the build-spec gives key, such as "PSA" or
// catalog.CLISpecKey.
func (s *BuildSpec) Ref(key string) (string, bool) {
	ref, ok := s.refs[key]
	return ref, ok
}
