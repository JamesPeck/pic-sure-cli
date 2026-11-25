package ops

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/JamesPeck/pic-sure-cli/internal/catalog"
	"github.com/JamesPeck/pic-sure-cli/internal/docker"
	"github.com/JamesPeck/pic-sure-cli/internal/events"
	"github.com/JamesPeck/pic-sure-cli/internal/stack"
	"github.com/JamesPeck/pic-sure-cli/internal/steps"
)

// LoggingVolumeStepID is the ID of the step that gives logging-logs to the
// user pic-sure-logging runs as.
const LoggingVolumeStepID = "logging-volume"

const (
	loggingImage  = "pic-sure-logging"
	loggingVolume = "logging-logs"
	loggingLogs   = "/app/logs"
)

// LoggingVolumeStep gives the logging-logs volume to the user the built
// pic-sure-logging image runs as, before start recreates the service on it.
// pic-sure-logging runs as a non-root user from pic-sure e4aefc9 on, and a
// volume an older, root image wrote is root's, so the new one can't open
// audit.log. As AIO's start-picsure.sh does (pic-sure-all-in-one 8a7aaf7),
// it runs chown -R in the image itself, as root, so a user name resolves
// as the service would resolve it.
//
// The step is done when the image runs as root or the stack records no
// built image, which covers every release before e4aefc9.
func LoggingVolumeStep(d *Deps, st *stack.Stack, cfg *stack.Config, state *stack.State) steps.Step {
	user := func(ctx context.Context) (ref, user string, err error) {
		img, _ := catalog.LookupImage(loggingImage)
		tag := state.DevImages[loggingImage]
		if tag == "" {
			tag = state.Images[loggingImage]
		}
		if tag == "" {
			return "", "", nil
		}
		ref = img.Repository() + ":" + tag
		user, err = d.Docker.ImageUser(ctx, ref)
		if errors.Is(err, docker.ErrNotFound) {
			return ref, "", nil
		}
		if err != nil || runsAsRoot(user) {
			return ref, "", err
		}
		return ref, user, nil
	}
	return steps.Step{
		ID:    LoggingVolumeStepID,
		Title: "Give the logging-logs volume to the logging service's user",
		Check: func(ctx context.Context) (bool, error) {
			_, u, err := user(ctx)
			return u == "", err
		},
		Apply: func(ctx context.Context, _ events.Sink) error {
			ref, u, err := user(ctx)
			if err != nil || u == "" {
				return err
			}
			v, _ := catalog.LookupVolume(loggingVolume)
			vol, err := st.EnsureVolume(ctx, d.Docker, cfg.Name, v.DockerName(cfg.Name), v.Name)
			if err != nil {
				return err
			}
			name, err := docker.UniqueName(cfg.Name+"-logging-chown", d.Rand)
			if err != nil {
				return err
			}
			var stderr bytes.Buffer
			code, err := d.Docker.Run(ctx, docker.RunOpts{
				Image:      ref,
				Name:       name,
				PullNever:  true,
				Remove:     true,
				User:       "0",
				Entrypoint: "chown",
				Network:    "none",
				Labels:     st.Labels(cfg.Name),
				Mounts:     []docker.Mount{{Source: vol.Name, Target: loggingLogs}},
				Args:       []string{"-R", u, loggingLogs},
				Stderr:     &stderr,
			})
			if err != nil {
				// The container may still be there, and chowning the volume.
				_ = d.Docker.Rm(context.WithoutCancel(ctx), name, true)
			} else if code != 0 {
				err = fmt.Errorf("the helper container exited %d: %s", code, strings.TrimSpace(stderr.String()))
			}
			if err != nil {
				return fmt.Errorf("giving volume %s to %s: %w", vol.Name, u, err)
			}
			return nil
		},
	}
}

// runsAsRoot reports whether a Dockerfile USER value ("", "0", "root",
// "0:0", "root:staff" ...) runs the container as root.
func runsAsRoot(user string) bool {
	uid, _, _ := strings.Cut(user, ":")
	return uid == "" || uid == "0" || uid == "root"
}
