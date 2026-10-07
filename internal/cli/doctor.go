package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/JamesPeck/pic-sure-cli/internal/cache"
	"github.com/JamesPeck/pic-sure-cli/internal/exitcode"
	"github.com/JamesPeck/pic-sure-cli/internal/log"
	"github.com/JamesPeck/pic-sure-cli/internal/ops"
	"github.com/JamesPeck/pic-sure-cli/internal/stack"
	"github.com/spf13/cobra"
)

func newDoctorCmd(a *App) *cobra.Command {
	var network bool
	c := &cobra.Command{
		Use:   "doctor",
		Short: "Check the host, Docker, the stack and the network",
		Long: `Check the host, Docker, the stack and the network.

Without a stack (no --stack, and none at or above the current directory)
only the host and Docker are checked. Exits 1 if any check fails.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return a.doctor(cmd, network)
		},
	}
	c.Flags().BoolVar(&network, "network", false, "also check reachability through the proxy")
	return c
}

func (a *App) doctor(cmd *cobra.Command, network bool) error {
	d := a.newDeps()
	opts := ops.DoctorOptions{Host: systemHost{}, Network: network}
	opts.CacheDir, opts.CacheErr = cache.DefaultRoot()

	st, err := a.openStack(cmd)
	switch {
	case err == nil:
		defer func() { _ = st.Close() }()
		opts.Stack = st
		if c, err := a.stackCompose(cmd, d.Runner, st); err == nil {
			d.Compose = c
		} else {
			opts.ComposeErr = err
		}
	case errors.Is(err, stack.ErrNotFound) && a.Global.Stack == "":
		// No stack here: check the host only.
	default:
		return err
	}

	report := ops.Doctor(cmd.Context(), d, opts)
	// Compose had the secrets, and its error can quote one.
	for i := range report.Checks {
		report.Checks[i].Message = log.Redact(report.Checks[i].Message)
		report.Checks[i].Detail = log.Redact(report.Checks[i].Detail)
	}
	if err := a.printReport(report, func(w io.Writer) error { return writeDoctorText(w, report) }); err != nil {
		return err
	}
	if report.Failed() {
		return exitcode.Failed("doctor: %d check(s) failed", report.Count(ops.CheckFail))
	}
	return nil
}

var doctorMarks = map[ops.CheckStatus]string{ops.CheckOK: "[ OK ]", ops.CheckWarn: "[WARN]", ops.CheckFail: "[FAIL]"}

func writeDoctorText(w io.Writer, r *ops.DoctorReport) error {
	var b strings.Builder
	if r.Stack != "" {
		fmt.Fprintf(&b, "Stack: %s\n", r.Stack)
	}
	for _, c := range r.Checks {
		fmt.Fprintf(&b, "%s %-24s %s\n", doctorMarks[c.Status], c.Name, c.Message)
		for line := range strings.Lines(c.Detail) {
			fmt.Fprintf(&b, "       %s", line)
			if !strings.HasSuffix(line, "\n") {
				b.WriteByte('\n')
			}
		}
	}
	fmt.Fprintf(&b, "%d ok, %d warnings, %d failed\n",
		r.Count(ops.CheckOK), r.Count(ops.CheckWarn), r.Count(ops.CheckFail))
	_, err := io.WriteString(w, b.String())
	return err
}

// systemHost is the real ops.Host.
type systemHost struct{}

func (systemHost) LookPath(name string) (string, error) { return exec.LookPath(name) }

// DiskFree measures the nearest existing directory at or above path, so a
// cache root that hasn't been created yet is measured where it will be.
func (systemHost) DiskFree(path string) (uint64, error) {
	for {
		if _, err := os.Stat(path); err == nil {
			return diskFree(path)
		}
		parent := filepath.Dir(path)
		if parent == path {
			return 0, fmt.Errorf("no existing directory above %s", path)
		}
		path = parent
	}
}

// PortFree reports a port busy only when binding it says so. Binding the
// wildcard address alone misses a loopback listener on macOS, so it tries
// 127.0.0.1 too. Any other error, such as EACCES for a port below 1024 on
// Linux, which Docker can still publish, counts as free.
func (systemHost) PortFree(port int) bool {
	for _, host := range []string{"", "127.0.0.1"} {
		l, err := net.Listen("tcp", net.JoinHostPort(host, strconv.Itoa(port)))
		if errors.Is(err, syscall.EADDRINUSE) {
			return false
		}
		if err == nil {
			_ = l.Close()
		}
	}
	return true
}

func (systemHost) Reach(ctx context.Context, rawURL string, proxy func(*http.Request) (*url.URL, error)) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, rawURL, nil)
	if err != nil {
		return 0, err
	}
	tr := &http.Transport{Proxy: proxy}
	defer tr.CloseIdleConnections()
	client := &http.Client{
		Transport: tr,
		// The first answer is enough to show the host was reached.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	_ = resp.Body.Close()
	return resp.StatusCode, nil
}
