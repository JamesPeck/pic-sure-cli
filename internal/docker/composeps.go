package docker

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

// ComposeService is one container from `docker compose ps --format json`.
// Compose reports more fields than these; the rest are ignored.
type ComposeService struct {
	ID      string `json:"ID"`
	Name    string `json:"Name"` // the container name
	Project string `json:"Project"`
	Service string `json:"Service"`
	Image   string `json:"Image"`
	// State is the container state: created, running, restarting, paused,
	// exited, removing or dead.
	State string `json:"State"`
	// Status is compose's human-readable summary, e.g. "Up 2 minutes
	// (healthy)".
	Status string `json:"Status"`
	// Health is healthy, unhealthy or starting, and empty for a container
	// with no healthcheck. Compare it exactly: "unhealthy" contains
	// "healthy".
	Health string `json:"Health"`
	// ExitCode is the exit code of an exited container.
	ExitCode   int                `json:"ExitCode"`
	Publishers []ComposePublisher `json:"Publishers"`
	// Labels are the container's labels as compose prints them:
	// key=value pairs joined by commas. Read one with Label.
	Labels string `json:"Labels"`
}

// Label returns the value of the container label key, or "". A value that
// itself holds ",<key>=" can't be told apart from the next label.
func (s ComposeService) Label(key string) string {
	for _, kv := range strings.Split(s.Labels, ",") {
		if k, v, ok := strings.Cut(kv, "="); ok && k == key {
			return v
		}
	}
	return ""
}

// ComposePublisher is one port a container publishes.
type ComposePublisher struct {
	URL           string `json:"URL"`
	TargetPort    int    `json:"TargetPort"`
	PublishedPort int    `json:"PublishedPort"`
	Protocol      string `json:"Protocol"`
}

// ParseComposePs parses `docker compose ps --format json` output. Compose
// 2.21 and later print one JSON object per line; older releases print a
// single JSON array. Both are accepted, as is any mix of the two. Unknown
// fields are ignored, null fields read as their zero values, and empty
// output means no containers.
func ParseComposePs(data []byte) ([]ComposeService, error) {
	var services []ComposeService
	dec := json.NewDecoder(bytes.NewReader(data))
	for {
		var raw json.RawMessage
		err := dec.Decode(&raw)
		if errors.Is(err, io.EOF) {
			return services, nil
		}
		if err != nil {
			return nil, fmt.Errorf("parsing docker compose ps output: %w", err)
		}
		if raw[0] == '{' {
			var s ComposeService
			if err := json.Unmarshal(raw, &s); err != nil {
				return nil, fmt.Errorf("parsing docker compose ps output: %w", err)
			}
			services = append(services, s)
			continue
		}
		var batch []ComposeService // an array, or null
		if err := json.Unmarshal(raw, &batch); err != nil {
			return nil, fmt.Errorf("parsing docker compose ps output: %w", err)
		}
		services = append(services, batch...)
	}
}
