package docker

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

// Image is one local image reference, from `docker image inspect`.
type Image struct {
	// Ref is the reference it was listed under, REPOSITORY:TAG.
	Ref string
	ID  string
	// RepoTags are every reference to the same image, Ref included.
	RepoTags []string
	// Size is the image's size in bytes, shared by all its references.
	Size    int64
	Created time.Time
	Labels  map[string]string
}

type imageListInspect struct {
	ID       string `json:"Id"`
	RepoTags []string
	Size     int64
	Created  time.Time
	Config   struct {
		Labels map[string]string
	}
}

func (e *cliEngine) ImageList(ctx context.Context, reference string) ([]Image, error) {
	argv := []string{"docker", "image", "ls", "--filter", "reference=" + reference, "--format", "{{.Repository}}:{{.Tag}}"}
	// ls gives only references; inspect gives the rest. An image removed in
	// between fails the inspect, so list again once.
	for attempt := 1; ; attempt++ {
		res, err := e.run(ctx, Cmd{Argv: argv})
		if err != nil {
			return nil, err
		}
		var refs []string
		for _, ref := range strings.Fields(string(res.Stdout)) {
			if !strings.HasSuffix(ref, ":<none>") && !strings.HasPrefix(ref, "<none>:") && !slices.Contains(refs, ref) {
				refs = append(refs, ref)
			}
		}
		if len(refs) == 0 {
			return nil, nil
		}
		imgs, err := e.inspectImages(ctx, refs)
		if errors.Is(err, ErrNotFound) && attempt < 2 {
			continue
		}
		if err != nil {
			return nil, err
		}
		slices.SortFunc(imgs, func(a, b Image) int { return strings.Compare(a.Ref, b.Ref) })
		return imgs, nil
	}
}

func (e *cliEngine) inspectImages(ctx context.Context, refs []string) ([]Image, error) {
	res, err := e.run(ctx, Cmd{Argv: append([]string{"docker", "image", "inspect"}, refs...)})
	if err != nil {
		return nil, err
	}
	var raw []imageListInspect
	if err := decodeJSON(res.Stdout, &raw); err != nil {
		return nil, fmt.Errorf("parsing docker image inspect: %w", err)
	}
	if len(raw) != len(refs) {
		return nil, fmt.Errorf("docker image inspect: got %d images for %d references", len(raw), len(refs))
	}
	imgs := make([]Image, len(raw))
	for i, r := range raw {
		imgs[i] = Image{Ref: refs[i], ID: r.ID, RepoTags: r.RepoTags, Size: r.Size, Created: r.Created, Labels: r.Config.Labels}
	}
	return imgs, nil
}

func (e *cliEngine) ContainerList(ctx context.Context) ([]ContainerInfo, error) {
	for attempt := 1; ; attempt++ {
		res, err := e.run(ctx, Cmd{Argv: []string{"docker", "ps", "-a", "-q", "--no-trunc"}})
		if err != nil {
			return nil, err
		}
		ids := strings.Fields(string(res.Stdout))
		if len(ids) == 0 {
			return nil, nil
		}
		cs, err := e.inspectContainers(ctx, ids)
		if errors.Is(err, ErrNotFound) && attempt < 2 {
			continue
		}
		if err != nil {
			return nil, err
		}
		slices.SortFunc(cs, func(a, b ContainerInfo) int { return strings.Compare(a.Name, b.Name) })
		return cs, nil
	}
}

// Network is a network, from `docker network inspect`.
type Network struct {
	Name   string
	Labels map[string]string
}

func (e *cliEngine) NetworkList(ctx context.Context, labels ...string) ([]Network, error) {
	argv := []string{"docker", "network", "ls", "-q", "--no-trunc"}
	for _, l := range labels {
		argv = append(argv, "--filter", "label="+l)
	}
	for attempt := 1; ; attempt++ {
		res, err := e.run(ctx, Cmd{Argv: argv})
		if err != nil {
			return nil, err
		}
		ids := strings.Fields(string(res.Stdout))
		if len(ids) == 0 {
			return nil, nil
		}
		res, err = e.run(ctx, Cmd{Argv: append([]string{"docker", "network", "inspect"}, ids...)})
		// docker says "network ID not found", not "No such network".
		var ee *ExitError
		if errors.As(err, &ee) && bytes.Contains(ee.Stderr, []byte(" not found")) && attempt < 2 {
			continue
		}
		if err != nil {
			return nil, err
		}
		var nets []Network
		if err := decodeJSON(res.Stdout, &nets); err != nil {
			return nil, fmt.Errorf("parsing docker network inspect: %w", err)
		}
		slices.SortFunc(nets, func(a, b Network) int { return strings.Compare(a.Name, b.Name) })
		return nets, nil
	}
}
