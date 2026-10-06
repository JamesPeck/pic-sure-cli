package docker

// Engine is the typed wrapper over the docker CLI that operations use for
// everything except compose: version and info, images, volumes, run, exec,
// cp, create, logs and labels. It is a placeholder: ticket 016 adds the
// methods and an implementation built on Runner, and ops.Deps.Docker holds
// one.
type Engine interface{}
