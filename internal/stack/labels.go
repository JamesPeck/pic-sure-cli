package stack

// Labels on every container, volume and network of a stack (§6.1). Anything
// destructive selects by these, never by a name prefix (§6.5).
const (
	LabelStack    = "org.hms-dbmi.picsure.stack"
	LabelStackDir = "org.hms-dbmi.picsure.stack-dir"
	LabelStackID  = "org.hms-dbmi.picsure.stack-id"
)

// Labels returns the labels that mark a resource as belonging to this stack,
// whose pic-sure.yaml name is name.
func (s *Stack) Labels(name string) map[string]string {
	return StackLabels(name, s.Dir, s.ID())
}

// StackLabels returns the labels of stack name in dir with ID id. A stack
// without an ID yet gets no stack-id label.
func StackLabels(name, dir, id string) map[string]string {
	l := map[string]string{LabelStack: name, LabelStackDir: dir}
	if id != "" {
		l[LabelStackID] = id
	}
	return l
}

// Compose's labels on the volumes it creates. A volume without the project
// label makes compose warn, on every up, that it didn't create it.
const (
	LabelComposeProject = "com.docker.compose.project"
	LabelComposeVolume  = "com.docker.compose.volume"
)

// VolumeLabels returns the labels for a stack volume the CLI creates before
// compose does, such as the certs volume: the stack labels plus compose's
// own, so compose adopts the volume as one it made. key is the volume's
// name under the compose file's volumes:.
func (s *Stack) VolumeLabels(name, key string) map[string]string {
	l := s.Labels(name)
	l[LabelComposeProject] = name
	l[LabelComposeVolume] = key
	return l
}
