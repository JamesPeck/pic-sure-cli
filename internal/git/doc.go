// Package git runs the user's git through docker.Runner, so credential
// helpers and SSH config work as they do in a terminal (spec D24). It keeps
// bare clones up to date (EnsureBare, Fetch), resolves tags, branches and
// shas to commits (ResolveRef), lists remote refs (LsRemote), and streams
// `git archive` (Archive) for Unpack to write out in Go.
//
// Files: client.go has the Client and its implementation over the Runner;
// unpack.go has Unpack.
package git
