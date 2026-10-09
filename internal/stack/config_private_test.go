package stack_test

import (
	"slices"
	"testing"

	"github.com/JamesPeck/pic-sure-cli/internal/stack"
)

func TestPrivateKey(t *testing.T) {
	for key, want := range map[string]bool{
		"auth.auth0.client_secret":                true,
		"db.remote.root_password":                 true,
		"email.password":                          true,
		"auth.admin_email":                        true,
		"auth.consent_authorization":              false, // a field, though secret-named
		"auth.mode":                               false,
		"network.https_port":                      false,
		"services.psama.env.API_TOKEN":            true, // a wildcard field
		"services.psama.env.LOG_LEVEL":            false,
		"stray.password":                          true, // not a field
		"stray.encryption_key":                    true,
		"stray.key":                               false,
		"stray.name":                              false,
		"services.psama.env.AUTHORIZATION_HEADER": false,
	} {
		if got := stack.PrivateKey(key); got != want {
			t.Errorf("PrivateKey(%q) = %v, want %v", key, got, want)
		}
	}
}

func TestPrivateFields(t *testing.T) {
	var got []string
	for _, f := range stack.Fields {
		if f.Private() {
			got = append(got, f.Key)
		}
	}
	want := []string{"auth.auth0.client_secret", "auth.admin_email", "db.remote.root_password", "email.password"}
	if !slices.Equal(got, want) {
		t.Errorf("private fields = %v, want %v", got, want)
	}
}
