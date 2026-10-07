package cli

import (
	"strings"
	"testing"

	"github.com/JamesPeck/pic-sure-cli/internal/ops"
)

func TestWriteStatusDeep(t *testing.T) {
	r := &ops.StatusReport{Deep: &ops.StatusDeep{
		Gateway: ops.StatusGateway{Checked: true, Healthy: new(true), Status: "RUNNING", Message: "all up"},
		Data:    ops.StatusData{Checked: true, Ready: new(false), Message: "no data"},
		HTTP:    ops.StatusHTTP{Checked: true, CSP: ops.CSPFrontend, Message: "nonce"},
	}}
	var b strings.Builder
	if err := writeStatus(&b, r); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"\n  Gateway      healthy: all up\n",
		"\n  HPDS data    not ready: no data\n",
		"\n  Frontend CSP frontend: nonce\n",
	} {
		if !strings.Contains(b.String(), want) {
			t.Errorf("no %q in\n%s", want, b.String())
		}
	}

	r.Deep.Gateway = ops.StatusGateway{Checked: true, Healthy: new(false), Message: "degraded"}
	r.Deep.Data = ops.StatusData{Checked: true, Message: "unknown health"}
	b.Reset()
	if err := writeStatus(&b, r); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"\n  Gateway      unhealthy: degraded\n", "\n  HPDS data    unknown: unknown health\n"} {
		if !strings.Contains(b.String(), want) {
			t.Errorf("no %q in\n%s", want, b.String())
		}
	}
}
