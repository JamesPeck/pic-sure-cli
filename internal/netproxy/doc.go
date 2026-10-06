// Package netproxy turns the config's proxy block into what each egress
// path needs (spec §9.10): an http.Transport proxy func, env vars for git
// and containers, docker build args, a Maven settings.xml and JVM options,
// all with the effective no-proxy list. Ticket 015 implements it.
package netproxy
