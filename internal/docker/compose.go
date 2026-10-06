package docker

// Composer runs `docker compose` against one rendered stack: it owns the -f
// list and the per-call environment (spec §6.4), so callers never assemble
// compose argv themselves. It is a placeholder: ticket 017 adds the methods
// and the Compose adapter that implements them, and ops.Deps.Compose holds
// one for the stack a command acts on.
type Composer interface{}
