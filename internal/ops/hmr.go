package ops

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/JamesPeck/pic-sure-cli/internal/catalog"
	"github.com/JamesPeck/pic-sure-cli/internal/docker"
	"github.com/JamesPeck/pic-sure-cli/internal/events"
	"github.com/JamesPeck/pic-sure-cli/internal/exitcode"
	"github.com/JamesPeck/pic-sure-cli/internal/stack"
	"github.com/JamesPeck/pic-sure-cli/internal/steps"
)

// httpd-hmr's step IDs.
const (
	NodeImageStepID = "node-image"
	HMRVolumeStepID = "hmr-volume"
)

const (
	hmrVariant = "httpd-hmr"
	// nodeTagSuffix pins the node image's Alpine release, as AIO does.
	nodeTagSuffix = "-alpine3.23"
)

// nvmrcVersion is a concrete Node version; AIO refuses aliases such as lts/*
// and partial versions such as 24 too, since the tag would move.
var nvmrcVersion = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+$`)

// hmrOn reports whether httpd-hmr is in dev.services.
func hmrOn(cfg *stack.Config) bool { return slices.Contains(cfg.Dev.Services, hmrVariant) }

// NodeTag is the node image tag httpd-hmr runs: the frontend source's
// .nvmrc version on Alpine. A missing source, a missing .nvmrc or one
// without a concrete x.y.z version is exit 3.
func NodeTag(stackDir string, cfg *stack.Config) (string, error) {
	src := componentSource(cfg, catalog.Frontend)
	if src == "" {
		return "", exitcode.Precondition("dev %s needs components.frontend.source", hmrVariant)
	}
	if !filepath.IsAbs(src) {
		src = filepath.Join(stackDir, src)
	}
	path := filepath.Join(src, ".nvmrc")
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return "", exitcode.Precondition("dev %s takes the Node version from %s, which doesn't exist", hmrVariant, path)
	}
	if err != nil {
		return "", err
	}
	v := strings.TrimSpace(string(data))
	if !nvmrcVersion.MatchString(v) {
		return "", exitcode.Precondition("dev %s needs a concrete Node version (x.y.z) in %s, not %q", hmrVariant, path, v)
	}
	return v + nodeTagSuffix, nil
}

// NodeImageStep records httpd-hmr's node tag (NodeTag) in state.json's
// images, where render reads it, so a frontend that moves to another Node
// version gets it on the next up.
func NodeImageStep(st *stack.Stack, cfg *stack.Config, state *stack.State) steps.Step {
	return steps.Step{
		ID:    NodeImageStepID,
		Title: "Take the Node version from the frontend's .nvmrc",
		Check: func(context.Context) (bool, error) {
			tag, err := NodeTag(st.Dir, cfg)
			return err == nil && state.Images["node"] == tag, err
		},
		Apply: func(context.Context, events.Sink) error {
			tag, err := NodeTag(st.Dir, cfg)
			if err != nil {
				return err
			}
			if state.Images == nil {
				state.Images = map[string]string{}
			}
			state.Images["node"] = tag
			return st.SaveState(state)
		},
	}
}

// HostUser is the "UID:GID" of this process, which httpd-hmr's node
// container runs as, or "" where the platform has none (Windows).
func HostUser() string {
	uid, gid := os.Getuid(), os.Getgid()
	if uid < 0 || gid < 0 {
		return ""
	}
	return strconv.Itoa(uid) + ":" + strconv.Itoa(gid)
}

// hmrVolumeScript gives the node_modules volume to the user in $1 unless it
// already has it. Docker creates a volume root-owned, and httpd-hmr's
// pnpm, running as that user, couldn't write it.
const hmrVolumeScript = `[ "$(stat -c %u:%g /v)" = "$1" ] || chown -R "$1" /v`

// HMRVolumeStep makes sure httpd-hmr's node_modules volume exists and
// belongs to user (HostUser) before the container starts as that user.
func HMRVolumeStep(d *Deps, st *stack.Stack, cfg *stack.Config, user string) steps.Step {
	return steps.Step{
		ID:    HMRVolumeStepID,
		Title: "Give the node_modules volume to " + user,
		Apply: func(ctx context.Context, sink events.Sink) error {
			v, _ := catalog.LookupVolume("frontend-node-modules")
			vol, err := st.EnsureVolume(ctx, d.Docker, cfg.Name, v.DockerName(cfg.Name), v.Name)
			if err != nil {
				return err
			}
			name, err := docker.UniqueName(cfg.Name+"-hmr", d.Rand)
			if err != nil {
				return err
			}
			alpine, _ := catalog.LookupImage("alpine")
			var stderr bytes.Buffer
			code, err := d.Docker.Run(ctx, docker.RunOpts{
				Image:   alpine.Ref,
				Name:    name,
				Remove:  true,
				Network: "none",
				Labels:  st.Labels(cfg.Name),
				Mounts:  []docker.Mount{{Source: vol.Name, Target: "/v"}},
				Args:    []string{"sh", "-c", hmrVolumeScript, "sh", user},
				Stderr:  &stderr,
			})
			if err == nil && code != 0 {
				err = fmt.Errorf("the helper container exited %d: %s", code, strings.TrimSpace(stderr.String()))
			}
			if err != nil {
				return fmt.Errorf("giving volume %s to %s: %w", vol.Name, user, err)
			}
			return nil
		},
	}
}
