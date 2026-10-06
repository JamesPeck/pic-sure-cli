// Package catalog holds the one Go table per concept that the bash AIO kept
// in several drifting copies: components with their repos and build-spec
// keys, the images (name, context, Dockerfile), the services, the volumes
// and the secrets. Ticket 010 implements it.
package catalog
