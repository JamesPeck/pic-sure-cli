package ops

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"strings"

	"github.com/JamesPeck/pic-sure-cli/internal/catalog"
	"github.com/JamesPeck/pic-sure-cli/internal/docker"
	"github.com/JamesPeck/pic-sure-cli/internal/events"
	"github.com/JamesPeck/pic-sure-cli/internal/stack"
	"github.com/JamesPeck/pic-sure-cli/internal/steps"
)

// HPDSKeyStepID is the HPDS key step's ID, for --skip-step.
const HPDSKeyStepID = "hpds-key"

const hpdsDataVolume = "hpds-data"

// hpdsKeyScript runs in the helper container, with the hpds-data volume at
// /data and the key on stdin. It writes the key beside its final place and
// renames it over the old one, so hpds never reads half a key.
const hpdsKeyScript = `set -eu
umask 077
cat > /data/.encryption_key.incoming
mv -f /data/.encryption_key.incoming /data/encryption_key
`

// HPDSKeyStep is §9.1 step 11, ID "hpds-key", for init, up and update to
// run before hpds starts: it copies the stack's HPDS key file into the
// hpds-data volume as encryption_key (0600, root's, as hpds runs as root),
// through an alpine helper that gets the key on stdin. It creates the
// volume with the stack's labels if compose hasn't, and refuses one
// labelled for another stack.
//
// The step is done when state.json records the same key copied into the
// same volume, so a volume re-created by `reset` is keyed again. With
// hpds.data: shared there is nothing to do: the shared data set carries the
// key its data was encrypted with.
func HPDSKeyStep(d *Deps, st *stack.Stack, cfg *stack.Config) steps.Step {
	k := &hpdsKeyStep{d: d, st: st, cfg: cfg}
	return steps.Step{
		ID:    HPDSKeyStepID,
		Title: "Install the HPDS encryption key",
		Check: k.check,
		Apply: k.apply,
	}
}

type hpdsKeyStep struct {
	d   *Deps
	st  *stack.Stack
	cfg *stack.Config
}

func (k *hpdsKeyStep) volume() string {
	v, _ := catalog.LookupVolume(hpdsDataVolume)
	return v.DockerName(k.cfg.Name)
}

// key returns the key file's contents, as hpds reads them, and their hash.
func (k *hpdsKeyStep) key() ([]byte, string, error) {
	key, err := k.st.LoadHPDSKey()
	if errors.Is(err, fs.ErrNotExist) {
		return nil, "", fmt.Errorf("the stack has no HPDS key file (%s): %w", k.st.Path(stack.HPDSKeyFile), err)
	}
	if err != nil {
		return nil, "", err
	}
	data := []byte(string(key) + "\n")
	sum := sha256.Sum256(data)
	return data, "sha256:" + hex.EncodeToString(sum[:]), nil
}

func (k *hpdsKeyStep) check(ctx context.Context) (bool, error) {
	if k.cfg.HPDS.Data != stack.HPDSLocal {
		return true, nil
	}
	state, err := k.st.LoadState()
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil || state.HPDSKey == nil {
		return false, err
	}
	_, hash, err := k.key()
	if err != nil {
		return false, err
	}
	if hash != state.HPDSKey.Hash {
		return false, nil
	}
	vol, err := k.d.Docker.VolumeInspect(ctx, k.volume())
	if errors.Is(err, docker.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return vol.CreatedAt == state.HPDSKey.VolumeCreatedAt, nil
}

func (k *hpdsKeyStep) apply(ctx context.Context, sink events.Sink) error {
	if k.cfg.HPDS.Data != stack.HPDSLocal {
		return nil
	}
	data, hash, err := k.key()
	if err != nil {
		return err
	}
	vol, err := k.st.EnsureVolume(ctx, k.d.Docker, k.cfg.Name, k.volume(), hpdsDataVolume)
	if err != nil {
		return err
	}

	// Forget the old copy first, so an interrupted one isn't taken for it.
	state, err := k.st.LoadState()
	if errors.Is(err, fs.ErrNotExist) {
		state, err = &stack.State{}, nil
	}
	if err != nil {
		return err
	}
	if state.HPDSKey != nil {
		state.HPDSKey = nil
		if err := k.st.SaveState(state); err != nil {
			return err
		}
	}

	name, err := docker.UniqueName(k.cfg.Name+"-hpds-key", k.d.Rand)
	if err != nil {
		return err
	}
	alpine, _ := catalog.LookupImage("alpine")
	var stderr bytes.Buffer
	code, err := k.d.Docker.Run(ctx, docker.RunOpts{
		Image:   alpine.Ref,
		Name:    name,
		Remove:  true,
		Network: "none",
		Labels:  k.st.Labels(k.cfg.Name),
		Mounts:  []docker.Mount{{Source: vol.Name, Target: "/data"}},
		Args:    []string{"sh", "-c", hpdsKeyScript},
		Stdin:   bytes.NewReader(data),
		Stderr:  &stderr,
	})
	if err != nil {
		// docker run was interrupted or failed, but its container may
		// still be there, and writing the volume.
		_ = k.d.Docker.Rm(context.WithoutCancel(ctx), name, true)
	} else if code != 0 {
		err = fmt.Errorf("the helper container exited %d", code)
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			err = fmt.Errorf("%w: %s", err, msg[strings.LastIndexByte(msg, '\n')+1:])
		}
	}
	if err != nil {
		return fmt.Errorf("copying the HPDS key into volume %s: %w", vol.Name, err)
	}

	state.HPDSKey = &stack.VolumeCopy{Hash: hash, VolumeCreatedAt: vol.CreatedAt}
	return k.st.SaveState(state)
}
