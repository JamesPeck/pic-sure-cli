package cache

import (
	"context"

	"github.com/JamesPeck/pic-sure-cli/internal/docker"
)

// MavenVolume is the Docker volume that holds the Maven repository every
// reactor build shares (§7.1). Use it only while holding LockReactor.
const MavenVolume = "pic-sure-m2"

// VolumeCreator creates a named Docker volume, succeeding if it exists.
// docker.Engine is one.
type VolumeCreator interface {
	VolumeCreate(ctx context.Context, name string, labels map[string]string) error
}

var _ VolumeCreator = docker.Engine(nil)

// EnsureMavenVolume creates MavenVolume if it doesn't exist.
func EnsureMavenVolume(ctx context.Context, d VolumeCreator) error {
	return d.VolumeCreate(ctx, MavenVolume, nil)
}
