package stack

import (
	"context"
	"errors"

	"github.com/JamesPeck/pic-sure-cli/internal/docker"
	"github.com/JamesPeck/pic-sure-cli/internal/exitcode"
)

// EnsureVolume returns the stack volume a helper container is about to
// write into: docker volume vol, compose key key, of the stack named name.
// It creates a missing one with VolumeLabels, so compose adopts it. It
// refuses, with exit 3, a volume labelled for another stack or not labelled
// at all, rather than overwrite what it holds.
func (s *Stack) EnsureVolume(ctx context.Context, e docker.Engine, name, vol, key string) (docker.Volume, error) {
	v, err := e.VolumeInspect(ctx, vol)
	if errors.Is(err, docker.ErrNotFound) {
		if err := e.VolumeCreate(ctx, vol, s.VolumeLabels(name, key)); err != nil {
			return docker.Volume{}, err
		}
		v, err = e.VolumeInspect(ctx, vol)
	}
	if err != nil {
		return docker.Volume{}, err
	}
	if owner := v.Labels[LabelStack]; owner != name {
		return docker.Volume{}, exitcode.Precondition("volume %s exists but isn't stack %s's (its %s label is %q), so pic-sure won't write to it",
			vol, name, LabelStack, owner)
	}
	return v, nil
}
