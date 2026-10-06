package stack

// Labels on every container, volume and network of a stack (§6.1). Anything
// destructive selects by these, never by a name prefix (§6.5).
const (
	LabelStack    = "org.hms-dbmi.picsure.stack"
	LabelStackDir = "org.hms-dbmi.picsure.stack-dir"
)

// Labels returns the labels that mark a resource as belonging to this stack,
// whose pic-sure.yaml name is name.
func (s *Stack) Labels(name string) map[string]string {
	return map[string]string{LabelStack: name, LabelStackDir: s.Dir}
}
