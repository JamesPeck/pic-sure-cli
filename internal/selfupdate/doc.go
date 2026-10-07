// Package selfupdate replaces the running binary with a GitHub release:
// download through the configured proxy, SHA-256 and cosign verification,
// atomic rename, and re-exec. It is never implicit (spec D12, §8).
//
//   - selfupdate.go: the Updater, Install (the self-update command) and
//     SelfUpdate (the compatibility gate's action, which re-executes).
//   - fetch.go: resolving the release and downloading its assets.
//   - target.go: finding the binary to replace and refusing package-managed
//     or unwritable ones.
package selfupdate
