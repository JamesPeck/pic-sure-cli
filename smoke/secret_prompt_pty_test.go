package smoke

import (
	"path/filepath"
	"strings"
	"testing"
)

// initSecretArgs run init with --auth0-client-secret-stdin. A secret under
// 32 bytes fails before anything is created, which shows what was read.
func initSecretArgs(t *testing.T, extra ...string) []string {
	return append([]string{"init", filepath.Join(t.TempDir(), "stack"), "--name", "smoke93",
		"--admin-email", "admin@example.com", "--auth0-client-id", "client-id",
		"--auth0-client-secret-stdin"}, extra...)
}

// On a terminal, a --*-stdin flag asks for the secret on one line, with
// echo off, instead of waiting silently for Ctrl-D.
func TestSecretPromptHidesInputUnderPTY(t *testing.T) {
	skipUnlessPTYAllowed(t)
	s := startPTY(t, t.TempDir(), initSecretArgs(t)...)
	s.waitFor("Paste the Auth0 client secret and press Enter (input is hidden):")
	s.send("Typed0Short\r")
	s.waitExit(2)
	out := s.text()
	if !strings.Contains(out, "the client secret is 11 bytes") {
		t.Errorf("the typed line wasn't read; output:\n%s", out)
	}
	if strings.Contains(out, "Typed0Short") {
		t.Errorf("the secret was echoed; output:\n%s", out)
	}
}

// --non-interactive can't prompt, so a terminal stdin is a usage error.
func TestSecretPromptRefusedNonInteractiveUnderPTY(t *testing.T) {
	skipUnlessPTYAllowed(t)
	s := startPTY(t, t.TempDir(), initSecretArgs(t, "--non-interactive")...)
	s.waitExit(2)
	if out := s.text(); !strings.Contains(out, "stdin is a terminal, and --non-interactive forbids prompting; pipe the secret in") {
		t.Errorf("output:\n%s", out)
	}
}
