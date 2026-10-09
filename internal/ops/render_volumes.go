package ops

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/JamesPeck/pic-sure-cli/internal/docker"
	"github.com/JamesPeck/pic-sure-cli/internal/exitcode"
	"github.com/JamesPeck/pic-sure-cli/internal/stack"
)

// existingVolumeLabels returns render.Input.ExistingVolumeLabels: for each
// volume compose made or adopted for this stack's project that is the
// stack's own under the ownership rule (§6.1), its key and labels without
// compose's. Another stack's volumes are left to the ownership check.
func existingVolumeLabels(ctx context.Context, d *Deps, st *stack.Stack, name string) (map[string]map[string]string, error) {
	vols, err := ownVolumes(ctx, d, st, name)
	if err != nil {
		return nil, err
	}
	out := map[string]map[string]string{}
	for key, v := range vols {
		out[key] = userLabels(v.Labels)
	}
	return out, nil
}

// ownVolumes returns the stack's volumes in its compose project, by key.
func ownVolumes(ctx context.Context, d *Deps, st *stack.Stack, name string) (map[string]docker.Volume, error) {
	vols, err := d.Docker.VolumeList(ctx, stack.LabelComposeProject+"="+name)
	if err != nil {
		return nil, err
	}
	dir := CanonicalDir(st.Dir)
	out := map[string]docker.Volume{}
	for _, v := range vols {
		if key := v.Labels[stack.LabelComposeVolume]; key != "" && stack.Owner(st.ID(), dir, v.Labels) != stack.Foreign {
			out[key] = v
		}
	}
	return out, nil
}

// userLabels returns labels without compose's own.
func userLabels(labels map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range labels {
		if !strings.HasPrefix(k, stack.LabelComposePrefix) {
			out[k] = v
		}
	}
	return out
}

// CheckVolumeLabels refuses, with exit 3, a compose config (merged, not
// interpolated) that gives one of the stack's existing volumes labels it
// doesn't have: compose would offer to recreate the volume, deleting its
// data. Only volumes compose made carry the config hash it compares, and a
// label set from a ${VAR} can't be compared uninterpolated, so those are
// skipped. A render by pic-sure up always matches; one by an older pic-sure
// may not.
func CheckVolumeLabels(ctx context.Context, d *Deps, st *stack.Stack, name string, config []byte) error {
	var f struct {
		Volumes map[string]struct {
			Labels composeLabels
		}
	}
	if err := yaml.Unmarshal(config, &f); err != nil {
		return fmt.Errorf("reading the compose config: %w", err)
	}
	vols, err := ownVolumes(ctx, d, st, name)
	if err != nil {
		return err
	}
	var differ []string
	for key, vol := range vols {
		v, ok := f.Volumes[key]
		if !ok || vol.Labels[docker.ConfigHashLabel] == "" {
			continue
		}
		want := map[string]string{}
		for k, val := range v.Labels {
			if strings.Contains(strings.ReplaceAll(val, "$$", ""), "$") {
				want = nil
				break
			}
			want[k] = strings.ReplaceAll(val, "$$", "$")
		}
		if want != nil && !maps.Equal(userLabels(vol.Labels), want) {
			differ = append(differ, vol.Name)
		}
	}
	if len(differ) > 0 {
		slices.Sort(differ)
		return exitcode.Precondition("the rendered compose file doesn't match the labels of volume(s) %s, so compose would offer to recreate them, deleting their data; run `pic-sure up` to re-render first", strings.Join(differ, ", "))
	}
	return nil
}

// composeLabels reads compose labels in either form compose config prints:
// a mapping or a list of "key=value".
type composeLabels map[string]string

func (l *composeLabels) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind == yaml.MappingNode {
		return n.Decode((*map[string]string)(l))
	}
	var list []string
	if err := n.Decode(&list); err != nil {
		return err
	}
	*l = composeLabels{}
	for _, e := range list {
		k, v, _ := strings.Cut(e, "=")
		(*l)[k] = v
	}
	return nil
}
