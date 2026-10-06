// Package selfupdate replaces the running binary with a GitHub release:
// download, SHA-256 and cosign verification, atomic rename, and re-exec. It
// is never implicit (spec D12, §8). Ticket 060 implements it.
package selfupdate
