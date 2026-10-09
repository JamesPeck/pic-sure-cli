package ops

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/JamesPeck/pic-sure-cli/internal/catalog"
	"github.com/JamesPeck/pic-sure-cli/internal/docker"
	"github.com/JamesPeck/pic-sure-cli/internal/events"
	"github.com/JamesPeck/pic-sure-cli/internal/exitcode"
	"github.com/JamesPeck/pic-sure-cli/internal/stack"
)

// ResourceRef is a Docker resource as reports and messages name it.
type ResourceRef struct {
	// Kind is "container", "volume", "network" or "image".
	Kind string `json:"kind"`
	Name string `json:"name"`
	// Stack and StackDir are its stack and stack-dir labels.
	Stack    string `json:"stack,omitempty"`
	StackDir string `json:"stack_dir,omitempty"`
}

func (r ResourceRef) String() string { return r.Kind + " " + r.Name }

// Owner describes the stack the resource belongs to, for messages.
func (r ResourceRef) Owner() string {
	return stack.OwnerName(map[string]string{stack.LabelStack: r.Stack, stack.LabelStackDir: r.StackDir})
}

// Resource is a container, volume or network that a stack's name selects.
type Resource struct {
	ResourceRef
	Labels map[string]string
	Claim  stack.Claim
}

// Refs returns the resources' refs.
func Refs(rs []Resource) []ResourceRef {
	refs := make([]ResourceRef, len(rs))
	for i, r := range rs {
		refs[i] = r.ResourceRef
	}
	return refs
}

// Ownership is what a stack's name selects in Docker, each resource
// classified by the ownership rule (§6.1).
type Ownership struct {
	Resources []Resource
	// Published holds the host ports the stack's own containers publish.
	Published map[int]bool
}

// Foreign returns the resources that belong to another stack.
func (o *Ownership) Foreign() []Resource { return o.with(stack.Foreign) }

// Moved returns this stack's resources labelled with the directory it was
// moved from.
func (o *Ownership) Moved() []Resource { return o.with(stack.Moved) }

func (o *Ownership) with(c stack.Claim) []Resource {
	var rs []Resource
	for _, r := range o.Resources {
		if r.Claim == c {
			rs = append(rs, r)
		}
	}
	return rs
}

// Err is exit 3 naming the foreign resources and whose they are, or nil
// when there are none.
func (o *Ownership) Err(name string) error {
	f := o.Foreign()
	if len(f) == 0 {
		return nil
	}
	return exitcode.Precondition("the stack name %s is in use by another stack's Docker resources, which pic-sure won't touch:\n%s",
		name, ResourceList(Refs(f)))
}

// ResourceList lists resources by owner, one indented line each: up to
// three by name, more by count, so a whole stack's take a line.
func ResourceList(rs []ResourceRef) string {
	var owners []string
	byOwner := map[string][]ResourceRef{}
	for _, r := range rs {
		o := r.Owner()
		if byOwner[o] == nil {
			owners = append(owners, o)
		}
		byOwner[o] = append(byOwner[o], r)
	}
	lines := make([]string, len(owners))
	for i, o := range owners {
		var parts []string
		if group := byOwner[o]; len(group) <= 3 {
			for _, r := range group {
				parts = append(parts, r.String())
			}
		} else {
			for _, kind := range []string{"container", "volume", "network", "image"} {
				if n := len(slices.DeleteFunc(slices.Clone(group), func(r ResourceRef) bool { return r.Kind != kind })); n > 0 {
					parts = append(parts, fmt.Sprintf("%d %ss", n, kind))
				}
			}
		}
		lines[i] = fmt.Sprintf("  %s: %s", o, strings.Join(parts, ", "))
	}
	return strings.Join(lines, "\n")
}

// StackResources lists the containers and networks of compose project
// name and the volumes of that project or labelled for stack name, and
// classifies each for the stack with ID id in dir. An empty dir, for a
// stack init hasn't made yet, owns nothing.
func StackResources(ctx context.Context, d *Deps, name, id, dir string) (*Ownership, error) {
	if dir != "" {
		dir = CanonicalDir(dir)
	}
	claim := func(labels map[string]string) stack.Claim {
		if dir == "" {
			return stack.Foreign
		}
		return stack.Owner(id, dir, labels)
	}
	o := &Ownership{Published: map[int]bool{}}

	cs, err := composeContainers(ctx, d, name)
	if err != nil {
		return nil, err
	}
	for _, c := range cs {
		r := newResource("container", c.Names, c.Labels, claim)
		o.Resources = append(o.Resources, r)
		if r.Claim == stack.Foreign {
			continue
		}
		for _, m := range publishedPort.FindAllStringSubmatch(c.Ports, -1) {
			p, _ := strconv.Atoi(m[1])
			o.Published[p] = true
		}
	}

	seen := map[string]bool{}
	for _, filter := range []string{stack.LabelComposeProject + "=" + name, stack.LabelStack + "=" + name} {
		vols, err := d.Docker.VolumeList(ctx, filter)
		if err != nil {
			return nil, err
		}
		for _, v := range vols {
			if !seen[v.Name] {
				seen[v.Name] = true
				o.Resources = append(o.Resources, newResource("volume", v.Name, v.Labels, claim))
			}
		}
	}

	nets, err := d.Docker.NetworkList(ctx, stack.LabelComposeProject+"="+name)
	if err != nil {
		return nil, err
	}
	for _, n := range nets {
		o.Resources = append(o.Resources, newResource("network", n.Name, n.Labels, claim))
	}
	return o, nil
}

func newResource(kind, name string, labels map[string]string, claim func(map[string]string) stack.Claim) Resource {
	ref := ResourceRef{Kind: kind, Name: name, Stack: labels[stack.LabelStack], StackDir: labels[stack.LabelStackDir]}
	return Resource{ResourceRef: ref, Labels: labels, Claim: claim(labels)}
}

// CheckOwnership is the ownership check every command that changes a
// stack's Docker resources makes (§6.1), holding the stack lock: exit 3 if
// the stack's name selects another stack's resources. Resources still labelled
// with the directory the stack moved from are adopted, with a note.
func CheckOwnership(ctx context.Context, d *Deps, st *stack.Stack, name string) (*Ownership, error) {
	o, err := StackResources(ctx, d, name, st.ID(), st.Dir)
	if err != nil {
		return nil, err
	}
	NoteMoved(d.Sink, o)
	return o, o.Err(name)
}

// NoteMoved emits a note for each directory the stack moved from whose
// resources it adopts, until the stack runs in its new directory: volumes
// keep the old directory for good, so once a container or network carries
// the new one the note would only repeat on every command.
func NoteMoved(sink events.Sink, o *Ownership) {
	if slices.ContainsFunc(o.Resources, func(r Resource) bool { return r.Claim == stack.Own && r.Kind != "volume" }) {
		return
	}
	var dirs []string
	for _, r := range o.Moved() {
		if dir := r.StackDir; !slices.Contains(dirs, dir) {
			dirs = append(dirs, dir)
			sink.Emit(events.Warning{Text: "stack moved from " + dir + "; adopting its resources"})
		}
	}
}

// psContainer is a docker ps row with the labels the ownership rule reads.
type psContainer struct {
	Names  string
	Ports  string
	Labels map[string]string
}

// psFormat has docker ps print each container's name, ports and stack
// labels exactly: its {{.Labels}} column joins them with commas, which
// cuts a directory with a comma short.
var psFormat = `{"Names":{{json .Names}},"Ports":{{json .Ports}},"Labels":{` +
	`"` + stack.LabelStack + `":{{json (.Label "` + stack.LabelStack + `")}},` +
	`"` + stack.LabelStackDir + `":{{json (.Label "` + stack.LabelStackDir + `")}},` +
	`"` + stack.LabelStackID + `":{{json (.Label "` + stack.LabelStackID + `")}}}}`

// composeContainers returns every container, running or not, of compose
// project name.
func composeContainers(ctx context.Context, d *Deps, name string) ([]psContainer, error) {
	res, err := docker.RunChecked(ctx, docker.WithTimeout(d.Runner, docker.PsTimeout), docker.Cmd{Argv: []string{
		"docker", "ps", "--all", "--no-trunc",
		"--filter", "label=" + stack.LabelComposeProject + "=" + name,
		"--format", psFormat,
	}})
	if err != nil {
		return nil, err
	}
	var cs []psContainer
	for line := range strings.Lines(string(res.Stdout)) {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var c psContainer
		if err := json.Unmarshal([]byte(line), &c); err != nil {
			return nil, fmt.Errorf("parsing docker ps: %w", err)
		}
		// docker prints a missing label as "".
		for k, v := range c.Labels {
			if v == "" {
				delete(c.Labels, k)
			}
		}
		cs = append(cs, c)
	}
	return cs, nil
}

// publishedPort is a host port in docker ps's Ports column, such as the
// 8443 of "0.0.0.0:8443->443/tcp".
var publishedPort = regexp.MustCompile(`:(\d+)->`)

// checkImageOwner refuses, with exit 3, to build ref over an image the
// ownership rule calls another stack's, for a build with stack labels. An
// image without stack labels predates them and is left to the build.
func checkImageOwner(ctx context.Context, d *Deps, ref string, labels map[string]string) error {
	dir := labels[stack.LabelStackDir]
	if dir == "" {
		return nil
	}
	have, err := d.Docker.ImageLabels(ctx, ref)
	if errors.Is(err, docker.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if have[stack.LabelStackDir] != "" && stack.Owner(labels[stack.LabelStackID], dir, have) == stack.Foreign {
		return exitcode.Precondition("image %s belongs to %s, so pic-sure won't build over it", ref, stack.OwnerName(have))
	}
	return nil
}

// withLabels returns labels with extra added, in a new map.
func withLabels(labels, extra map[string]string) map[string]string {
	l := maps.Clone(labels)
	maps.Copy(l, extra)
	return l
}

// DevImages returns the dev images of stack name (dev-<name>-* tags of the
// built catalog images) that carry stack labels, as foreign resources of
// kind "image": what init must refuse, since a build would tag over them.
func DevImages(ctx context.Context, d *Deps, name string) ([]Resource, error) {
	imgs, err := d.Docker.ImageList(ctx, catalog.Namespace+"/*")
	if err != nil {
		return nil, fmt.Errorf("listing images: %w", err)
	}
	var rs []Resource
	for _, img := range imgs {
		if devStack, ok := cachedImage(img.Ref); !ok || devStack != name || img.Labels[stack.LabelStackDir] == "" {
			continue
		}
		rs = append(rs, newResource("image", img.Ref, img.Labels, func(map[string]string) stack.Claim { return stack.Foreign }))
	}
	return rs, nil
}
