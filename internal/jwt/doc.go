// Package jwt generates the PSAMA introspection token: an HS256 JWT keyed
// by the first line of the Auth0 client secret (spec §9.4). Ticket 013
// implements it.
package jwt
