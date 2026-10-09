package ops

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/JamesPeck/pic-sure-cli/internal/exitcode"
	"github.com/JamesPeck/pic-sure-cli/internal/stack"
)

// existingVolumeLabels returns render.Input.ExistingVolumeLabels: for each
// volume compose made or adopted for this stack's project that is the
// stack's own under the ownership rule (§6.1), its key and labels without
// compose's. Another stack's volumes are left to the ownership check.
func existingVolumeLabels(ctx context.Context, d *Deps, st *stack.Stack, name string) (map[string]map[string]string, error) {
	vols, err := d.Docker.VolumeList(ctx, stack.LabelComposeProject+"="+name)
	if err != nil {
		return nil, err
	}
	dir := CanonicalDir(st.Dir)
	out := map[string]map[string]string{}
	for _, v := range vols {
		key := v.Labels[stack.LabelComposeVolume]
		if key == "" || stack.Owner(st.ID(), dir, v.Labels) == stack.Foreign {
			continue
		}
		labels := map[string]string{}
		for k, val := range v.Labels {
			if !strings.HasPrefix(k, stack.LabelComposePrefix) {
				labels[k] = val
			}
		}
		out[key] = labels
	}
	return out, nil
}

// CheckVolumeLabels refuses, with exit 3, a compose config (merged, not
// interpolated) that gives one of the stack's existing volumes labels it
// doesn't have: compose would offer to recreate the volume, deleting its
// data. A render made by pic-sure up never does; one made before render
// kept existing volumes' labels can.
func CheckVolumeLabels(ctx context.Context, d *Deps, st *stack.Stack, name string, config []byte) error {
	var f struct {
		Volumes map[string]struct {
			Labels composeLabels
		}
	}
	if err := yaml.Unmarshal(config, &f); err != nil {
		return fmt.Errorf("reading the compose config: %w", err)
	}
	existing, err := existingVolumeLabels(ctx, d, st, name)
	if err != nil {
		return err
	}
	var differ []string
	for key, have := range existing {
		v, ok := f.Volumes[key]
		if !ok {
			continue
		}
		want := map[string]string{}
		for k, val := range v.Labels {
			want[k] = strings.ReplaceAll(val, "$$", "$")
		}
		if !maps.Equal(have, want) {
			differ = append(differ, name+"_"+key)
		}
	}
	if len(differ) > 0 {
		slices.Sort(differ)
		return exitcode.Precondition("the rendered compose file doesn't match the labels of volume(s) %s, so compose would offer to recreate them, deleting their data; run `pic-sure up` to re-render first", strings.Join(differ, ", "))
	}
	return nil
}

// composeLabels reads compose labels in either form: a mapping, or the
// list of "key=value" that compose config prints.
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
