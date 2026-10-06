// Package jwt generates the PSAMA introspection token: an HS256 JWT keyed
// by the first line of the Auth0 client secret (spec §9.4), byte for byte as
// jwt-creator v1.0.0 makes it.
package jwt
