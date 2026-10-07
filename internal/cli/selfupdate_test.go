package cli

import (
	"testing"

	"github.com/JamesPeck/pic-sure-cli/internal/events"
	"github.com/JamesPeck/pic-sure-cli/internal/stack"
)

const validConfig = "schema: 1\nname: demo\nauth: {admin_email: admin@example.org, auth0: {client_id: abc}}\n"

func TestSelfUpdateProxy(t *testing.T) {
	withConfig := func(t *testing.T, config string) string {
		dir := newTestStack(t)
		st, err := stack.Open(dir)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = st.Close() }()
		if err := st.WriteFile(stack.ConfigFile, []byte(config), 0o644); err != nil {
			t.Fatal(err)
		}
		return dir
	}
	tests := []struct {
		name    string
		dir     func(t *testing.T) string
		enabled bool
		warn    bool
	}{
		{name: "outside a stack", dir: func(t *testing.T) string { return t.TempDir() }},
		{name: "a stack without a proxy", dir: func(t *testing.T) string { return withConfig(t, validConfig) }},
		{name: "a stack with a proxy", enabled: true, dir: func(t *testing.T) string {
			return withConfig(t, validConfig+"proxy:\n  https: http://me:s3cret@proxy.example:3128\n")
		}},
		{name: "a stack whose config doesn't load", warn: true, dir: func(t *testing.T) string {
			return withConfig(t, "schema: 99\n")
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Chdir(tt.dir(t))
			a, _, _ := testApp(t)
			var rec events.Recorder
			p := a.selfUpdateProxy(&rec)
			if got := p != nil && p.Enabled(); got != tt.enabled {
				t.Errorf("proxy enabled = %v, want %v", got, tt.enabled)
			}
			if got := len(rec.Events()) > 0; got != tt.warn {
				t.Errorf("events = %v, want a warning: %v", rec.Events(), tt.warn)
			}
		})
	}
}
