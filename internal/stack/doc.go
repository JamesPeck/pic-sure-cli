// Package stack is the on-disk model of one stack (spec §6): finding it, its
// pic-sure.yaml config, its secrets, state.json, manifest.json, the lock, and
// the version gate. Four tickets share it, each in its own files:
//
//   - 006: the pic-sure.yaml schema (config*.go): types, defaults,
//     validation, load and save, the field metadata table, and the derived
//     auth-mode flags.
//   - 007: the stack directory (dir.go, state.go, manifest.go, lock.go and
//     friends): discovery from --stack or the cwd, confined atomic writes
//     through os.Root, state.json, manifest.json, the per-stack lock, and
//     the stack labels.
//   - 008: secrets (secrets*.go): generation through ops.Deps.Rand,
//     secrets.yaml, the HPDS key file, and redaction registration.
//   - 009: the version gate by command class and the config migration
//     framework (gate*.go, migrate*.go).
package stack
