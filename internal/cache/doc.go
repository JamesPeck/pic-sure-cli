// Package cache is the host cache shared by every stack, at
// $XDG_CACHE_HOME/pic-sure (spec §7.1): bare clones, immutable per-commit
// source trees, the release-control clone, downloads, transient build
// contexts, per-run temporary directories, the locks (per-repo fetch, the
// global reactor lock, per image tag), and the name of the pic-sure-m2
// Maven volume.
//
// Ticket 019 wrote cache.go (root and layout), source.go (EnsureSource),
// lock.go and maven.go. Ticket 057 adds `cache list` and `cache prune`.
package cache
