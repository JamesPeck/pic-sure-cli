package ops

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/JamesPeck/pic-sure-cli/internal/docker"
	"github.com/JamesPeck/pic-sure-cli/internal/render"
)

// StatusDeep is what `status --deep` adds: probes run inside the running
// containers (spec §9.9). Compose's healthchecks are deliberately shallow,
// so an empty HPDS doesn't mark its container unhealthy; these answer
// whether the stack works.
type StatusDeep struct {
	Gateway StatusGateway `json:"gateway"`
	Data    StatusData    `json:"data"`
	HTTP    StatusHTTP    `json:"http"`
}

// StatusGateway is the gateway's /system/status, which folds in every
// downstream service.
type StatusGateway struct {
	// Checked is false when the probe couldn't run, such as when the
	// gateway isn't running; Message says why.
	Checked bool `json:"checked"`
	// Healthy is null when not checked.
	Healthy *bool `json:"healthy"`
	// Status is the gateway's answer, such as RUNNING or ONE OR MORE
	// COMPONENTS DEGRADED; empty when it gave none.
	Status  string `json:"status"`
	Message string `json:"message"`
}

// StatusData is whether HPDS has data loaded and can answer a COUNT.
type StatusData struct {
	Checked bool `json:"checked"`
	// Ready is null when unknown: not checked, or an answer the probe
	// doesn't recognize.
	Ready   *bool  `json:"ready"`
	Message string `json:"message"`
}

// CSP classifications of the frontend's HTML response.
const (
	CSPFrontend = "frontend"
	CSPFloor    = "floor"
	CSPBoth     = "both"
	CSPNone     = "none"
	CSPUnknown  = "unknown"
)

// StatusHTTP is the frontend's HTML response through httpd's TLS ingress.
type StatusHTTP struct {
	Checked bool `json:"checked"`
	// CSP is which Content-Security-Policy the HTML carries: frontend (one
	// policy with a nonce, from SvelteKit), floor (only the vhost's
	// restrictive fallback, so the frontend build sets none), both (more
	// than one policy, which browsers intersect), none, or unknown.
	CSP     string `json:"csp"`
	Message string `json:"message"`
}

// wget's own timeouts, in seconds (§9.9: 5–10 s).
const (
	gatewayWgetTimeout = 10
	deepWgetTimeout    = 5
)

// deepExecBound bounds a probe's whole compose exec: wget's timeout plus
// time for compose to start the command.
var deepExecBound = func(wgetTimeout int) time.Duration {
	return time.Duration(wgetTimeout)*time.Second + 15*time.Second
}

// statusDeep runs the probes in the containers r.Services reports
// running.
func statusDeep(ctx context.Context, d *Deps, r *StatusReport) *StatusDeep {
	deep := &StatusDeep{HTTP: StatusHTTP{CSP: CSPUnknown}}
	running := map[string]bool{}
	for _, s := range r.Services {
		if s.State == "running" {
			running[s.Service] = true
		}
	}
	skip := func(service string) string {
		switch {
		case r.ServicesError != "":
			return "services unknown: " + r.ServicesError
		case !running[service]:
			return service + " is not running"
		}
		return ""
	}

	if why := skip("gateway"); why != "" {
		deep.Gateway.Message = why
	} else {
		deep.Gateway = probeGateway(ctx, d.Compose)
	}
	if why := skip("hpds"); why != "" {
		deep.Data.Message = why + "; data readiness unknown"
	} else {
		deep.Data = probeData(ctx, d.Compose)
	}
	if why := skip("httpd"); why != "" {
		deep.HTTP.Message = why + "; CSP unknown"
	} else {
		deep.HTTP = probeHTTP(ctx, d.Compose)
	}
	return deep
}

// wgetResult is one busybox wget run inside a container: the body from
// stdout, and the -S response headers and wget's own messages from stderr.
type wgetResult struct {
	code    int
	body    string
	headers string
	// err is set when wget didn't run, such as when the container stopped
	// after compose ps; the probe then counts as not checked.
	err error
	// timeout is set when wget ran but didn't finish within it.
	timeout time.Duration
}

// wget runs busybox wget in service. The Spring images (corretto-alpine)
// and httpd have wget but no curl.
func wget(ctx context.Context, c docker.Composer, service string, timeout int, args ...string) wgetResult {
	bound := deepExecBound(timeout)
	ectx, cancel := context.WithTimeout(ctx, bound)
	defer cancel()
	argv := append([]string{"wget", "-q", "-S", "-O", "-", "-T", strconv.Itoa(timeout)}, args...)
	var out, errOut bytes.Buffer
	code, err := c.Exec(ectx, docker.ComposeExecOpts{Service: service, Args: argv, Stdout: &out, Stderr: &errOut})
	res := wgetResult{code: code, body: out.String(), headers: errOut.String(), err: err}
	if ctx.Err() == nil && ectx.Err() != nil {
		res.err, res.timeout = nil, bound
	}
	return res
}

var statusLineRE = regexp.MustCompile(`(?m)^\s*HTTP/[0-9.]+ ([0-9]{3})`)

// statusCodes is every HTTP status line in wget's -S output, in order:
// more than one means it followed a redirect.
func (w wgetResult) statusCodes() []int {
	var codes []int
	for _, m := range statusLineRE.FindAllStringSubmatch(w.headers, -1) {
		n, _ := strconv.Atoi(m[1])
		codes = append(codes, n)
	}
	return codes
}

// lastStatus is the final response's status code, or 0 without one.
func (w wgetResult) lastStatus() int {
	codes := w.statusCodes()
	if len(codes) == 0 {
		return 0
	}
	return codes[len(codes)-1]
}

// header is every value of the response header name, case-insensitively.
func (w wgetResult) header(name string) []string {
	var values []string
	for _, line := range strings.Split(w.headers, "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(line), ":")
		if ok && strings.EqualFold(k, name) {
			values = append(values, strings.TrimSpace(v))
		}
	}
	return values
}

// why is the probe failure's detail, for a message.
func (w wgetResult) why() string {
	switch {
	case w.timeout != 0:
		return fmt.Sprintf(": no answer within %s", w.timeout)
	case w.lastStatus() != 0:
		return fmt.Sprintf(" (HTTP %d)", w.lastStatus())
	}
	if line := lastNonEmptyLine(w.headers); line != "" {
		return ": " + line
	}
	return fmt.Sprintf(" (wget exit %d)", w.code)
}

func lastNonEmptyLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}

// notRun is the message for a probe whose wget didn't run.
func notRun(service string, err error) string {
	return fmt.Sprintf("couldn't run wget in %s: %v", service, err)
}

func probeGateway(ctx context.Context, c docker.Composer) StatusGateway {
	res := wget(ctx, c, "gateway", gatewayWgetTimeout, "http://localhost:8080/system/status")
	if res.err != nil {
		return StatusGateway{Message: notRun("gateway", res.err)}
	}
	g := StatusGateway{Checked: true}
	if res.timeout != 0 || res.code != 0 {
		g.Healthy = new(false)
		if status := res.lastStatus(); res.timeout == 0 && status != 0 {
			g.Message = fmt.Sprintf("gateway /system/status answered HTTP %d", status)
		} else {
			g.Message = "gateway /system/status did not respond" + res.why()
		}
		return g
	}
	g.Status = strings.TrimSpace(res.body)
	g.Healthy = new(g.Status == "RUNNING")
	if *g.Healthy {
		g.Message = "all gateway downstreams are up"
	} else {
		g.Message = "gateway reports " + strconv.Quote(g.Status)
	}
	return g
}

var countRE = regexp.MustCompile(`^[0-9]+$`)

// probeData asks HPDS for a COUNT. HPDS has no authentication filter, and
// its v3 handler refuses with 403 while the encryption key isn't loaded.
// init installs the key, so a fresh stack answers 0; the actuator then
// tells, since it is DOWN without loaded metadata (it never checks the key).
func probeData(ctx context.Context, c docker.Composer) StatusData {
	res := wget(ctx, c, "hpds", deepWgetTimeout,
		"--header=Content-Type: application/json",
		`--post-data={"query":{"expectedResultType":"COUNT"}}`,
		"http://localhost:8080/PIC-SURE/v3/query/sync")
	if res.err != nil {
		return StatusData{Message: notRun("hpds", res.err) + "; data readiness unknown"}
	}
	dr := StatusData{Checked: true}
	switch code := res.lastStatus(); {
	case res.timeout != 0:
		dr.Message = "HPDS query unavailable" + res.why() + "; data readiness unknown"
		return dr
	case code == 403:
		dr.Ready = new(false)
		dr.Message = "HPDS refused the query (HTTP 403): its encryption key isn't loaded; load data with pic-sure data demo or pic-sure data load-phenotype"
		return dr
	case code != 200:
		dr.Message = "HPDS query unavailable" + res.why() + "; data readiness unknown"
		return dr
	case !countRE.MatchString(strings.TrimSpace(res.body)):
		dr.Message = "unexpected answer to HPDS's COUNT; data readiness unknown"
		return dr
	}

	health := wget(ctx, c, "hpds", deepWgetTimeout, "http://localhost:8080/actuator/health")
	status := ""
	switch {
	case health.err != nil || health.timeout != 0:
	case health.lastStatus() == 503:
		status = "DOWN"
	case health.lastStatus() == 200:
		var h struct {
			Status string `json:"status"`
		}
		if json.Unmarshal([]byte(health.body), &h) == nil {
			status = h.Status
		}
	}
	switch status {
	case "UP":
		dr.Ready = new(true)
		dr.Message = "HPDS answers COUNT queries and its data is healthy"
	case "DOWN", "OUT_OF_SERVICE":
		dr.Ready = new(false)
		dr.Message = "HPDS's health is " + status + ": no data loaded, or a broken load; load data with pic-sure data demo or pic-sure data load-phenotype"
	case "":
		why := health.why()
		if health.err != nil {
			why = ": " + health.err.Error()
		}
		dr.Message = "HPDS answered the COUNT, but its health is unknown" + why
	default:
		dr.Message = "HPDS answered the COUNT, but its health is " + status
	}
	return dr
}

// probeHTTP fetches the frontend through httpd's TLS ingress and classifies
// its CSP. Only a single 200 text/html response counts, so that a redirect
// or an Apache error page, which carry the floor, isn't taken for the
// frontend's HTML.
func probeHTTP(ctx context.Context, c docker.Composer) StatusHTTP {
	res := wget(ctx, c, "httpd", deepWgetTimeout, "--no-check-certificate", "https://127.0.0.1/")
	if res.err != nil {
		return StatusHTTP{CSP: CSPUnknown, Message: notRun("httpd", res.err) + "; CSP unknown"}
	}
	h := StatusHTTP{Checked: true, CSP: CSPUnknown}
	codes := res.statusCodes()
	contentType := strings.Join(res.header("Content-Type"), ",")
	switch {
	case res.timeout != 0 || len(codes) == 0:
		h.Message = "the frontend's HTML is unavailable" + res.why() + "; CSP unknown"
	case len(codes) > 1:
		h.Message = fmt.Sprintf("https://127.0.0.1/ redirects (HTTP %s), so it isn't the frontend's HTML; CSP unknown", strings.Trim(fmt.Sprint(codes), "[]"))
	case codes[0] != 200:
		h.Message = "the frontend's HTML is unavailable" + res.why() + "; CSP unknown"
	case !isHTML(contentType):
		h.Message = fmt.Sprintf("https://127.0.0.1/ answered %q, not HTML; CSP unknown", contentType)
	default:
		h.CSP, h.Message = classifyCSP(res.header("Content-Security-Policy"))
	}
	return h
}

func isHTML(contentType string) bool {
	mediaType, _, _ := strings.Cut(contentType, ";")
	return strings.EqualFold(strings.TrimSpace(mediaType), "text/html")
}

func classifyCSP(policies []string) (csp, message string) {
	switch {
	case len(policies) == 0:
		return CSPNone, "the HTML has no CSP"
	case len(policies) > 1:
		return CSPBoth, "the HTML has more than one CSP, which browsers intersect; check the httpd vhost"
	case policies[0] == render.CSPFloorPolicy:
		return CSPFloor, "the HTML has only httpd's CSP floor, so the frontend sets none; update the frontend and rebuild"
	case strings.Contains(policies[0], "'nonce-"):
		return CSPFrontend, "the HTML has the frontend's nonce CSP"
	}
	return CSPUnknown, "the HTML's CSP is unrecognized"
}
