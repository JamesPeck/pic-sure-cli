package cli

import (
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/JamesPeck/pic-sure-cli/internal/docker"
	"github.com/JamesPeck/pic-sure-cli/internal/log"
	"github.com/JamesPeck/pic-sure-cli/internal/ops"
	"github.com/JamesPeck/pic-sure-cli/internal/stack"
)

func newStatusCmd(a *App) *cobra.Command {
	c := &cobra.Command{
		Use:   "status",
		Short: "Report the stack's config, versions, images, services and migrations",
		Long: `Report on the stack without changing it: config validity, the version
gate, the release and component commits, which images are present, the
services compose reports, the database mode, migrations, the introspection
token's expiry, and the URLs to register in Auth0. It exits 0 whenever it
finds the stack, even if parts of it couldn't be read.

--deep also runs probes inside the running containers, which take a few
seconds: the gateway's /system/status, a COUNT query that tells whether
HPDS has data loaded, and which Content-Security-Policy the frontend's
HTML gets.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			deep, _ := cmd.Flags().GetBool("deep")
			return a.status(cmd, deep)
		},
	}
	c.Flags().Bool("deep", false, "also probe inside the running containers: the gateway's health, whether HPDS has data, and the frontend's CSP")
	return c
}

func (a *App) status(cmd *cobra.Command, deep bool) error {
	st, err := a.openStack(cmd)
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()
	report := a.statusReport(cmd, st, deep)
	return a.printReport(report, func(w io.Writer) error { return writeStatus(w, report) })
}

// statusReport is status's report on st, its messages redacted.
func (a *App) statusReport(cmd *cobra.Command, st *stack.Stack, deep bool) *ops.StatusReport {
	d := a.newDeps()
	opts := ops.StatusOptions{CLIVersion: a.Info.Version, Migrations: a.configMigrations(), Deep: deep}
	if c, err := a.stackCompose(cmd, d.Runner, st); err == nil {
		d.Compose = c
	} else if !errors.Is(err, docker.ErrNotRendered) {
		opts.ComposeErr = err
	}
	report := ops.Status(cmd.Context(), d, st, opts)
	// Compose had the secrets, and its error can quote one.
	report.ServicesError = log.Redact(report.ServicesError)
	if dp := report.Deep; dp != nil {
		dp.Gateway.Message = log.Redact(dp.Gateway.Message)
		dp.Data.Message = log.Redact(dp.Data.Message)
		dp.HTTP.Message = log.Redact(dp.HTTP.Message)
	}
	return report
}

// writeStatus is the human form of the report.
func writeStatus(w io.Writer, r *ops.StatusReport) error {
	var b strings.Builder
	line := func(label, format string, args ...any) {
		fmt.Fprintf(&b, "%-12s "+format+"\n", append([]any{label + ":"}, args...)...)
	}
	orNone := func(s string) string {
		if s == "" {
			return "(not recorded)"
		}
		return s
	}

	line("Stack", "%s (%s)", orNone(r.Stack.Name), r.Stack.Dir)
	switch {
	case r.Config.Error != "":
		line("Config", "unreadable: %s", r.Config.Error)
	case r.Config.Valid:
		line("Config", "valid")
	default:
		line("Config", "invalid")
		for _, p := range r.Config.Problems {
			loc := p.Path
			if p.Line > 0 {
				loc = fmt.Sprintf("%s (line %d)", p.Path, p.Line)
			}
			fmt.Fprintf(&b, "  - %s: %s\n", loc, p.Message)
		}
	}
	v := r.Versions
	line("CLI", "%s (config schema %d)", v.CLI, v.Schema)
	switch {
	case v.Error != "":
		line("Stack CLI", "unknown: %s", v.Error)
	case v.StackCLI == "" && v.StackSchema == 0:
		line("Stack CLI", "(not rendered yet)")
	default:
		line("Stack CLI", "%s (rendered schema %d, config schema %d)", orNone(v.StackCLI), v.StackSchema, v.ConfigSchema)
	}
	line("Gate", "%s", gateText(v))
	if r.StateError != "" {
		line("State", "unreadable: %s", r.StateError)
	}

	if r.Release.Commit == "" {
		line("Release", "(not recorded)")
	} else {
		line("Release", "%s %s %s", r.Release.Repo, r.Release.Branch, shortSHA(r.Release.Commit))
	}
	for _, c := range r.Components {
		ref := ""
		if c.Ref != "" {
			ref = " (" + c.Ref + ")"
		}
		fmt.Fprintf(&b, "  %-16s %s%s\n", c.Name, orNone(shortSHA(c.Commit)), ref)
	}

	b.WriteString("Images:\n")
	for _, img := range r.Images {
		state := "unknown"
		switch {
		case img.Ref == "":
			state = "no tag recorded"
		case img.Present != nil && *img.Present:
			state = "present"
		case img.Present != nil:
			state = "missing"
		}
		switch {
		case img.Dev && img.Ref == "":
			state = "replaced by a dev variant"
		case img.Dev:
			state += " (dev build)"
		}
		fmt.Fprintf(&b, "  %-28s %s\n", img.Name, state)
	}
	if r.ImagesError != "" {
		fmt.Fprintf(&b, "  (can't check: %s)\n", r.ImagesError)
	}

	b.WriteString("Services:\n")
	if len(r.Foreign) > 0 {
		fmt.Fprintf(&b, "  the stack name %s is in use by another stack's Docker resources, so these may be theirs:\n", r.Stack.Name)
		for line := range strings.Lines(ops.ResourceList(r.Foreign)) {
			b.WriteString("  " + line)
		}
		b.WriteString("\n")
	}
	switch {
	case r.ServicesError != "":
		fmt.Fprintf(&b, "  unknown: %s\n", r.ServicesError)
	case len(r.Services) == 0:
		b.WriteString("  no containers\n")
	}
	for _, s := range r.Services {
		state := s.State
		if s.Health != "" {
			state += " (" + s.Health + ")"
		}
		if s.State == "exited" {
			state += fmt.Sprintf(" (exit %d)", s.ExitCode)
		}
		fmt.Fprintf(&b, "  %-28s %s\n", s.Service, state)
	}

	switch {
	case r.DB == nil:
		line("Database", "unknown (config unreadable)")
	case r.DB.Host != "":
		line("Database", "%s (%s:%d)", r.DB.Mode, r.DB.Host, r.DB.Port)
	default:
		line("Database", "%s", r.DB.Mode)
	}
	line("Migrations", "%s", r.Migrations.Status)
	switch t := r.Token; {
	case t.Error != "":
		line("Token", "unknown: %s", t.Error)
	case t.ExpiresAt == nil:
		line("Token", "not issued")
	case t.Expired:
		line("Token", "expired %s", t.ExpiresAt.Format(time.RFC3339))
	default:
		line("Token", "expires %s", t.ExpiresAt.Format(time.RFC3339))
	}
	if op := r.LastOperation; op != nil {
		line("Last op", "%s, %s, started %s", op.Name, op.Status, op.StartedAt.Format(time.RFC3339))
	}

	if dp := r.Deep; dp != nil {
		b.WriteString("Deep checks:\n")
		fmt.Fprintf(&b, "  %-12s %s: %s\n", "Gateway", deepState(dp.Gateway.Checked, dp.Gateway.Healthy, "healthy", "unhealthy"), dp.Gateway.Message)
		fmt.Fprintf(&b, "  %-12s %s: %s\n", "HPDS data", deepState(dp.Data.Checked, dp.Data.Ready, "ready", "not ready"), dp.Data.Message)
		csp := dp.HTTP.CSP
		if !dp.HTTP.Checked {
			csp = "not checked"
		}
		fmt.Fprintf(&b, "  %-12s %s: %s\n", "Frontend CSP", csp, dp.HTTP.Message)
	}

	if au := r.Auth0; au != nil {
		if au.Needed {
			b.WriteString("Auth0 (register these in the application):\n")
		} else {
			b.WriteString("Auth0 (not needed in open mode; register these to allow login):\n")
		}
		fmt.Fprintf(&b, "  Callback URL: %s\n  Logout URL:   %s\n  Web origin:   %s\n", au.CallbackURL, au.LogoutURL, au.WebOrigin)
		if au.DevWebOrigin != "" {
			b.WriteString("Auth0 for httpd-hmr (dev):\n")
			fmt.Fprintf(&b, "  Callback URL: %s\n  Logout URL:   %s\n  Web origin:   %s\n", au.DevCallbackURL, au.DevLogoutURL, au.DevWebOrigin)
		}
	}
	_, err := io.WriteString(w, b.String())
	return err
}

// deepState is a probe's verdict: yes or no, or unknown when the probe
// didn't run or couldn't tell.
func deepState(checked bool, ok *bool, yes, no string) string {
	switch {
	case !checked:
		return "not checked"
	case ok == nil:
		return "unknown"
	case *ok:
		return yes
	}
	return no
}

func gateText(v ops.StatusVersions) string {
	switch v.Gate {
	case ops.GateStackNewer:
		return "the stack was rendered by a newer pic-sure; mutating commands are refused"
	case ops.GateUnsupportedSchema:
		return "this pic-sure can't migrate the config's schema; mutating commands are refused"
	case ops.GateMigrationsPending:
		return fmt.Sprintf("%d config migration(s) pending; run pic-sure update", len(v.PendingMigrations))
	case ops.GateOK:
		return "ok"
	}
	return "unknown"
}

func shortSHA(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}
