package docker

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
)

// Volume is a named volume, from `docker volume inspect`.
type Volume struct {
	Name       string
	Driver     string
	Labels     map[string]string
	Mountpoint string
	// CreatedAt is as docker reports it, e.g. "2026-10-06T20:20:28Z".
	CreatedAt string
}

// Container is a container from `docker ps`.
type Container struct {
	ID   string
	Name string
	// Image is the reference the container was created from.
	Image string
	// State is e.g. "running", "exited" or "created".
	State string
}

func (e *cliEngine) VolumeCreate(ctx context.Context, name string, labels map[string]string) error {
	argv := append([]string{"docker", "volume", "create"}, labelArgs(labels)...)
	_, err := e.run(ctx, Cmd{Argv: append(argv, name)})
	return err
}

func (e *cliEngine) VolumeInspect(ctx context.Context, name string) (Volume, error) {
	vols, err := e.inspectVolumes(ctx, name)
	if err != nil {
		return Volume{}, err
	}
	if len(vols) != 1 {
		return Volume{}, fmt.Errorf("docker volume inspect %s: got %d volumes, want 1", name, len(vols))
	}
	return vols[0], nil
}

func (e *cliEngine) inspectVolumes(ctx context.Context, names ...string) ([]Volume, error) {
	res, err := e.run(ctx, Cmd{Argv: append([]string{"docker", "volume", "inspect"}, names...)})
	if err != nil {
		return nil, err
	}
	var vols []Volume
	if err := decodeJSON(res.Stdout, &vols); err != nil {
		return nil, fmt.Errorf("parsing docker volume inspect: %w", err)
	}
	return vols, nil
}

func (e *cliEngine) VolumeList(ctx context.Context, labels ...string) ([]Volume, error) {
	argv := []string{"docker", "volume", "ls", "-q"}
	for _, l := range labels {
		argv = append(argv, "--filter", "label="+l)
	}
	// ls gives only names; inspect gives the labels. A volume removed in
	// between fails the inspect, so list again once.
	for attempt := 1; ; attempt++ {
		res, err := e.run(ctx, Cmd{Argv: argv})
		if err != nil {
			return nil, err
		}
		names := strings.Fields(string(res.Stdout))
		if len(names) == 0 {
			return nil, nil
		}
		vols, err := e.inspectVolumes(ctx, names...)
		if errors.Is(err, ErrNotFound) && attempt < 2 {
			continue
		}
		if err != nil {
			return nil, err
		}
		slices.SortFunc(vols, func(a, b Volume) int { return strings.Compare(a.Name, b.Name) })
		return vols, nil
	}
}

func (e *cliEngine) VolumeRemove(ctx context.Context, name string) error {
	_, err := e.run(ctx, Cmd{Argv: []string{"docker", "volume", "rm", name}})
	return ignoreNotFound(err)
}

func (e *cliEngine) ContainersUsingVolume(ctx context.Context, name string) ([]Container, error) {
	res, err := e.run(ctx, Cmd{Argv: []string{
		"docker", "ps", "-a", "--no-trunc", "--filter", "volume=" + name, "--format", "{{json .}}",
	}})
	if err != nil {
		return nil, err
	}
	return parsePs(res.Stdout)
}

// ownName picks the container's own name from ps's comma-separated Names,
// which with --no-trunc also lists legacy link aliases ("other/alias").
func ownName(names string) string {
	all := strings.Split(names, ",")
	for _, n := range all {
		if !strings.Contains(n, "/") {
			return n
		}
	}
	return all[0]
}

// parsePs parses `docker ps --format '{{json .}}'`: one object per line.
func parsePs(out []byte) ([]Container, error) {
	var cs []Container
	sc := bufio.NewScanner(bytes.NewReader(out))
	sc.Buffer(nil, 1<<20)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var row struct{ ID, Names, Image, State string }
		if err := json.Unmarshal(line, &row); err != nil {
			return nil, fmt.Errorf("parsing docker ps: %w", err)
		}
		cs = append(cs, Container{ID: row.ID, Name: ownName(row.Names), Image: row.Image, State: row.State})
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("parsing docker ps: %w", err)
	}
	return cs, nil
}
