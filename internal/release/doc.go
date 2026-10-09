// Package release reads release-control: it fetches the repo into the
// cache, parses build-spec.json, resolves component commits (honouring
// --release-commit), and runs the CLI compatibility gate (spec §8).
package release
