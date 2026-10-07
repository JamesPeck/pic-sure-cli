package cli

import (
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/JamesPeck/pic-sure-cli/internal/docker"
	"github.com/JamesPeck/pic-sure-cli/internal/ops"
)

func newStatusCmd(a *App) *cobra.Command {
	c := &cobra.Command{
		Use:   "status",
		Short: "Report the stack's config, versions, images, services and migrations",
		Long: `Report on the stack without changing it: config validity, the version
gate, the release and component commits, which images are present, the
services compose reports, the database mode, migrations, the introspection
token's expiry, and the URLs to register in Auth0. It exits 0 whenever it
finds the stack, even if parts of it couldn't be read.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if deep, _ := cmd.Flags().GetBool("deep"); deep {
				return notImplemented("037")(cmd, nil)
			}
			return a.status(cmd)
		},
	}
	c.Flags().Bool("deep", false, "also probe inside the containers: gateway, HPDS data, frontend CSP (ticket 037)")
	return c
}

func (a *App) status(cmd *cobra.Command) error {
	st, err := a.openStack(cmd)
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()
	d := a.newDeps()
	opts := ops.StatusOptions{CLIVersion: a.Info.Version, Migrations: a.configMigrations()}
	if c, err := a.stackCompose(cmd, d.Runner, st); err == nil {
		d.Compose = c
	} else if !errors.Is(err, docker.ErrNotRendered) {
		opts.ComposeErr = err
	}
	report := ops.Status(cmd.Context(), d, st, opts)
	return a.printReport(report, func(w io.Writer) error { return writeStatus(w, report) })
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
