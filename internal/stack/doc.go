// Package stack is the on-disk model of one stack (spec §6): finding it, its
// pic-sure.yaml config, its secrets, state.json, manifest.json, the lock, and
// the version gate. Its files:
//
//   - config*.go: the pic-sure.yaml schema: types, defaults, validation,
//     load and save, the field metadata table, the derived auth-mode
//     flags, and which keys are private.
//   - dir.go, write.go, state.go, manifest.go, lock.go, owner.go and
//     remove.go: discovery from --stack or the cwd, the directory owner
//     check, confined atomic writes through os.Root, state.json,
//     manifest.json, the per-stack lock, and removal for destroy.
//   - labels.go, ownership.go and volume.go: the stack labels, whose a
//     Docker resource is by those labels, and creating a labelled stack
//     volume before compose does.
//   - secrets*.go: generation from an io.Reader the caller passes in
//     (ops.Deps.Rand), secrets.yaml, the HPDS key file, and redaction
//     registration.
//   - gate.go and migrate.go: the version gate by command class and the
//     config migration framework.
//
// ops imports stack, so stack must not import ops.
package stack
