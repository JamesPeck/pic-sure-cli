// Package git runs the user's git through docker.Runner, so credential
// helpers and SSH config work as they do in a terminal (spec D24): bare
// clones, fetch, ref resolution, ls-remote, and `git archive` unpacked in
// Go. Ticket 018 implements it.
package git
