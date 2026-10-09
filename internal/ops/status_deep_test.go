package ops_test

import (
	"context"
	"strings"
	"testing"

	"github.com/JamesPeck/pic-sure-cli/internal/docker/fakerunner"
	"github.com/JamesPeck/pic-sure-cli/internal/ops"
	"github.com/JamesPeck/pic-sure-cli/internal/render"
)

const (
	execGateway = "docker compose * exec -T gateway wget -q -S -O - -T 10 http://localhost:8080/system/status"
	execCount   = "docker compose * exec -T hpds wget -q -S -O - -T 5 --header=Content-Type: application/json --post-data=* http://localhost:8080/PIC-SURE/v3/query/sync"
	execHealth  = "docker compose * exec -T hpds wget -q -S -O - -T 5 http://localhost:8080/actuator/health"
	execHTML    = "docker compose * exec -T httpd wget -q -S -O - -T 5 --no-check-certificate https://127.0.0.1/"
)

// deepRunner is a fake for a stack whose gateway, hpds and httpd all run.
// Each test adds its probe answers.
func deepRunner(t *testing.T, running ...string) *fakerunner.Runner {
	t.Helper()
	f := fakerunner.New(t)
	f.On(fakerunner.Glob("docker image inspect *")).Stdout(`[{"Id":"sha256:1"}]`)
	ps := ""
	for _, s := range running {
		ps += `{"Name":"demo-` + s + `-1","Service":"` + s + `","State":"running"}` + "\n"
	}
	f.On(fakerunner.Glob("docker compose * ps --all --format json")).Stdout(ps)
	return f
}

func deepStatus(t *testing.T, f *fakerunner.Runner) *ops.StatusDeep {
	t.Helper()
	st := newStatusStack(t, statusConfig)
	saveStatusState(t, st)
	opts := statusOpts()
	opts.Deep = true
	r := ops.Status(context.Background(), statusDeps(t, f, st), st, opts)
	if r.Deep == nil {
		t.Fatal("no deep section")
	}
	return r.Deep
}

// headers is wget -S's response headers as busybox prints them.
func headers(lines ...string) string {
	return "  " + strings.Join(lines, "\n  ") + "\n"
}

func htmlHeaders(csp ...string) string {
	lines := []string{"HTTP/1.1 200 OK", "Content-Type: text/html; charset=utf-8"}
	for _, p := range csp {
		lines = append(lines, "Content-Security-Policy: "+p)
	}
	return headers(lines...)
}

func TestStatusDeepAllReady(t *testing.T) {
	f := deepRunner(t, "gateway", "hpds", "httpd")
	f.On(fakerunner.Glob(execGateway)).Stdout("RUNNING").Stderr(headers("HTTP/1.1 200 OK"))
	f.On(fakerunner.Glob(execCount)).Stdout("42").Stderr(headers("HTTP/1.1 200 OK"))
	f.On(fakerunner.Glob(execHealth)).Stdout(`{"status":"UP","groups":["liveness"]}`).Stderr(headers("HTTP/1.1 200 OK"))
	f.On(fakerunner.Glob(execHTML)).Stderr(htmlHeaders("default-src 'self'; script-src 'self' 'nonce-abc123'"))

	d := deepStatus(t, f)
	if g := d.Gateway; !g.Checked || g.Healthy == nil || !*g.Healthy || g.Status != "RUNNING" {
		t.Errorf("gateway %+v", g)
	}
	if dr := d.Data; !dr.Checked || dr.Ready == nil || !*dr.Ready {
		t.Errorf("data %+v", dr)
	}
	if h := d.HTTP; !h.Checked || h.CSP != ops.CSPFrontend {
		t.Errorf("http %+v", h)
	}
}

func TestStatusDeepDataNotReady(t *testing.T) {
	for name, tc := range map[string]struct {
		count, health func(*fakerunner.Rule)
		ready         *bool
		message       string
	}{
		"403 without the key": {
			count: func(r *fakerunner.Rule) {
				r.Exit(1).Stderr(headers("HTTP/1.1 403 Forbidden") + "wget: server returned error: HTTP/1.1 403 Forbidden\n")
			},
			ready: new(false), message: "HTTP 403",
		},
		"actuator 503": {
			count: func(r *fakerunner.Rule) { r.Stdout("0").Stderr(headers("HTTP/1.1 200 OK")) },
			health: func(r *fakerunner.Rule) {
				r.Exit(1).Stderr(headers("HTTP/1.1 503 Service Unavailable") + "wget: server returned error: HTTP/1.1 503 Service Unavailable\n")
			},
			ready: new(false), message: "health is DOWN: no data loaded",
		},
		"actuator unreadable": {
			count:  func(r *fakerunner.Rule) { r.Stdout("0").Stderr(headers("HTTP/1.1 200 OK")) },
			health: func(r *fakerunner.Rule) { r.Stdout("not json").Stderr(headers("HTTP/1.1 200 OK")) },
			ready:  nil, message: "health is unknown",
		},
		"non-numeric COUNT": {
			count: func(r *fakerunner.Rule) { r.Stdout(`{"error":"x"}`).Stderr(headers("HTTP/1.1 200 OK")) },
			ready: nil, message: "unexpected answer",
		},
		"HPDS down": {
			count: func(r *fakerunner.Rule) {
				r.Exit(1).Stderr("wget: can't connect to remote host (127.0.0.1): Connection refused\n")
			},
			ready: nil, message: "Connection refused",
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := deepRunner(t, "hpds")
			tc.count(f.On(fakerunner.Glob(execCount)))
			if tc.health != nil {
				tc.health(f.On(fakerunner.Glob(execHealth)))
			}
			dr := deepStatus(t, f).Data
			if !dr.Checked || !equalBoolPtr(dr.Ready, tc.ready) || !strings.Contains(dr.Message, tc.message) {
				t.Errorf("data %+v (ready %v), want ready %v and %q", dr, deref(dr.Ready), deref(tc.ready), tc.message)
			}
		})
	}
}

func TestStatusDeepCSP(t *testing.T) {
	for name, tc := range map[string]struct {
		stderr  string
		exit    int
		want    string
		message string
	}{
		"frontend": {stderr: htmlHeaders("script-src 'nonce-xyz'"), want: ops.CSPFrontend},
		"floor":    {stderr: htmlHeaders(render.CSPFloorPolicy), want: ops.CSPFloor},
		"both":     {stderr: htmlHeaders("script-src 'nonce-xyz'", render.CSPFloorPolicy), want: ops.CSPBoth},
		"none":     {stderr: htmlHeaders(), want: ops.CSPNone},
		"unrecognized": {
			stderr: htmlHeaders("default-src 'self'"), want: ops.CSPUnknown,
		},
		"lower-case header names": {
			stderr: headers("HTTP/1.1 200 OK", "content-type: TEXT/HTML", "content-security-policy: script-src 'nonce-a'"),
			want:   ops.CSPFrontend,
		},
		"a redirect": {
			stderr:  headers("HTTP/1.1 302 Found", "Location: /x") + htmlHeaders("script-src 'nonce-xyz'"),
			want:    ops.CSPUnknown,
			message: "redirects (HTTP 302 200)",
		},
		"not HTML": {
			stderr:  headers("HTTP/1.1 200 OK", "Content-Type: application/json", "Content-Security-Policy: "+render.CSPFloorPolicy),
			want:    ops.CSPUnknown,
			message: `answered "application/json", not HTML`,
		},
		"an error page": {
			stderr:  headers("HTTP/1.1 503 Service Unavailable", "Content-Type: text/html", "Content-Security-Policy: "+render.CSPFloorPolicy),
			exit:    1,
			want:    ops.CSPUnknown,
			message: "unavailable (HTTP 503)",
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := deepRunner(t, "httpd")
			f.On(fakerunner.Glob(execHTML)).Stderr(tc.stderr).Exit(tc.exit)
			h := deepStatus(t, f).HTTP
			if !h.Checked || h.CSP != tc.want || h.Message == "" || !strings.Contains(h.Message, tc.message) {
				t.Errorf("http %+v, want csp %s and %q", h, tc.want, tc.message)
			}
		})
	}
}

func TestStatusDeepGatewayDegraded(t *testing.T) {
	f := deepRunner(t, "gateway")
	f.On(fakerunner.Glob(execGateway)).Stdout("ONE OR MORE COMPONENTS DEGRADED\n").Stderr(headers("HTTP/1.1 200 OK"))
	g := deepStatus(t, f).Gateway
	if !g.Checked || g.Healthy == nil || *g.Healthy || g.Status != "ONE OR MORE COMPONENTS DEGRADED" {
		t.Errorf("gateway %+v", g)
	}
}

func TestStatusDeepGatewayAnswersAnError(t *testing.T) {
	f := deepRunner(t, "gateway")
	f.On(fakerunner.Glob(execGateway)).Exit(1).Stderr(headers("HTTP/1.1 502 Bad Gateway") + "wget: server returned error: HTTP/1.1 502 Bad Gateway\n")
	g := deepStatus(t, f).Gateway
	if !g.Checked || g.Healthy == nil || *g.Healthy || g.Message != "gateway /system/status answered HTTP 502" {
		t.Errorf("gateway %+v", g)
	}
}

func TestStatusDeepGatewayNoAnswer(t *testing.T) {
	f := deepRunner(t, "gateway")
	f.On(fakerunner.Glob(execGateway)).Exit(1).Stderr("wget: can't connect to remote host (127.0.0.1): Connection refused\n")
	g := deepStatus(t, f).Gateway
	if !g.Checked || g.Healthy == nil || *g.Healthy || g.Message != "gateway /system/status did not respond: wget: can't connect to remote host (127.0.0.1): Connection refused" {
		t.Errorf("gateway %+v", g)
	}
}

// A container that stops between compose ps and compose exec: wget never
// runs, so no probe counts as checked.
func TestStatusDeepExecFails(t *testing.T) {
	f := deepRunner(t, "gateway", "hpds", "httpd")
	f.On(fakerunner.Glob("docker compose * exec -T *")).Exit(1).Stderr("service \"x\" is not running\n")
	d := deepStatus(t, f)
	if d.Gateway.Checked || d.Gateway.Healthy != nil || !strings.HasPrefix(d.Gateway.Message, "couldn't run wget in gateway: ") {
		t.Errorf("gateway %+v", d.Gateway)
	}
	if d.Data.Checked || d.Data.Ready != nil || !strings.HasPrefix(d.Data.Message, "couldn't run wget in hpds: ") {
		t.Errorf("data %+v", d.Data)
	}
	if d.HTTP.Checked || d.HTTP.CSP != ops.CSPUnknown || !strings.HasPrefix(d.HTTP.Message, "couldn't run wget in httpd: ") {
		t.Errorf("http %+v", d.HTTP)
	}
}

// Nothing running: no probe runs (the fake fails any exec), and each
// section says why.
func TestStatusDeepNothingRunning(t *testing.T) {
	d := deepStatus(t, deepRunner(t))
	if d.Gateway.Checked || d.Gateway.Healthy != nil || d.Gateway.Message != "gateway is not running" {
		t.Errorf("gateway %+v", d.Gateway)
	}
	if d.Data.Checked || d.Data.Ready != nil || !strings.HasPrefix(d.Data.Message, "hpds is not running") {
		t.Errorf("data %+v", d.Data)
	}
	if d.HTTP.Checked || d.HTTP.CSP != ops.CSPUnknown || !strings.HasPrefix(d.HTTP.Message, "httpd is not running") {
		t.Errorf("http %+v", d.HTTP)
	}
}

func TestStatusWithoutDeepHasNoDeepSection(t *testing.T) {
	st := newStatusStack(t, statusConfig)
	saveStatusState(t, st)
	r := ops.Status(context.Background(), statusDeps(t, deepRunner(t, "gateway"), st), st, statusOpts())
	if r.Deep != nil {
		t.Errorf("deep %+v without Deep", r.Deep)
	}
}

func equalBoolPtr(a, b *bool) bool {
	return a == nil && b == nil || a != nil && b != nil && *a == *b
}

func deref(b *bool) any {
	if b == nil {
		return nil
	}
	return *b
}
