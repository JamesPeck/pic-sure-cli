package cache

import "context"

// MavenVolume is the Docker volume that holds the Maven repository every
// reactor build shares (§7.1). Use it only while holding LockReactor.
const MavenVolume = "pic-sure-m2"

// VolumeCreator creates a named Docker volume, succeeding if it exists.
// docker.Engine (ticket 016) is one.
type VolumeCreator interface {
	VolumeCreate(ctx context.Context, name string, labels map[string]string) error
}

// EnsureMavenVolume creates MavenVolume if it doesn't exist and returns its
// name.
func EnsureMavenVolume(ctx context.Context, d VolumeCreator) (string, error) {
	return MavenVolume, d.VolumeCreate(ctx, MavenVolume, nil)
}
