// Package release reads release-control: it fetches the repo into the
// cache, parses build-spec.json, resolves component commits (honouring
// --release-commit), and runs the CLI compatibility gate (spec §8). Ticket
// 028 implements it; ticket 060 implements the gate's self-update action.
package release
