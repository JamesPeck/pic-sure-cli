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
// refuses, with exit 3, a volume the ownership rule (Owner) calls another
// stack's, rather than overwrite what it holds.
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
	if Owner(s.ID(), s.Dir, v.Labels) == Foreign {
		return docker.Volume{}, exitcode.Precondition("volume %s belongs to %s, not this one, so pic-sure won't write to it",
			vol, OwnerName(v.Labels))
	}
	return v, nil
}
