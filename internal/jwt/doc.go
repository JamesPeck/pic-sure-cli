// Package jwt generates the PSAMA introspection token: an HS256 JWT keyed
// by the first line of the Auth0 client secret (spec §9.4). It reproduces
// jwt-creator v1.0.0, which v1 built and ran in a Maven container.
package jwt
