// Package catalog holds the one Go table per concept that the bash AIO kept
// in several drifting copies (spec §10.1): the components with their repos
// and build-spec keys, the images (name, context, Dockerfile), the services,
// networks and volumes of a stack, and the dev variants. Secrets are not
// here: the Secrets type in package stack (ticket 008) is their table.
//
// The package is data and lookups only, and imports nothing from this
// module, so any package may import it. Every table is returned by a
// function that builds a fresh copy, so a caller can sort or append to what
// it gets without changing what other callers see.
package catalog
