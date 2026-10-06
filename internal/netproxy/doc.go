// Package netproxy turns the config's proxy block into what each egress
// path needs (spec §9.10): an http.Transport proxy func, env vars for git
// and containers, docker build args, a Maven settings.xml and JVM options,
// all with the effective no-proxy list.
//
// It imports nothing from this module but the catalog, so internal/stack
// can use ParseURL and ParseNoProxy to validate the config.
package netproxy
