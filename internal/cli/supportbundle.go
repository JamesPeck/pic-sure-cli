package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/JamesPeck/pic-sure-cli/internal/cache"
	"github.com/JamesPeck/pic-sure-cli/internal/docker"
	"github.com/JamesPeck/pic-sure-cli/internal/exitcode"
	"github.com/JamesPeck/pic-sure-cli/internal/log"
	"github.com/JamesPeck/pic-sure-cli/internal/ops"
	"github.com/JamesPeck/pic-sure-cli/internal/stack"
)

func newSupportBundleCmd(a *App) *cobra.Command {
	var output string
	c := &cobra.Command{
		Use:   "support-bundle",
		Short: "Write a redacted diagnostics archive to attach to an issue",
		Long: `Write a gzipped tar of diagnostics to attach to an issue: status --deep
--json, doctor --json, the newest ` + fmt.Sprint(ops.BundleRunLogs) + ` run logs, compose ps, each service's last
` + fmt.Sprint(ops.BundleLogTail) + ` log lines, and pic-sure.yaml, state.json and manifest.json. Every value
in the stack's secrets.yaml and HPDS key file is replaced with [REDACTED]
in every file. README.txt in the archive lists what couldn't be collected.

Without a stack (no --stack, and none at or above the current directory)
the archive has the host checks alone. The archive goes to
./pic-sure-support-NAME-TIMESTAMP.tar.gz unless -o names a file, and its
path is printed. It exits 0 once the archive is written, even if parts of
it couldn't be collected.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return a.supportBundle(cmd, output)
		},
	}
	c.Flags().StringVarP(&output, "output", "o", "", "write the archive to `FILE`")
	return c
}

func (a *App) supportBundle(cmd *cobra.Command, output string) error {
	d := a.newDeps()
	opts := ops.SupportBundleOptions{Doctor: ops.DoctorOptions{Host: systemHost{}}}
	opts.Doctor.CacheDir, opts.Doctor.CacheErr = cache.DefaultRoot()
	name := "host"

	st, err := a.openStack(cmd)
	switch {
	case err == nil:
		defer func() { _ = st.Close() }()
		opts.Stack, opts.Doctor.Stack = st, st
		opts.Status = ops.StatusOptions{CLIVersion: a.Info.Version, Migrations: a.configMigrations(), Deep: true}
		if c, err := a.stackCompose(cmd, d.Runner, st); err == nil {
			d.Compose = c
		} else {
			opts.Doctor.ComposeErr = err
			if !errors.Is(err, docker.ErrNotRendered) {
				opts.Status.ComposeErr = err
			}
		}
		name = filepath.Base(st.Dir)
		if cfg, err := st.LoadConfig(); err == nil {
			name = cfg.Name
		}
	case errors.Is(err, stack.ErrNotFound) && a.Global.Stack == "":
	default:
		return err
	}

	if output == "" {
		output = fmt.Sprintf("pic-sure-support-%s-%s.tar.gz", name, d.Clock.Now().UTC().Format("20060102T150405Z"))
	}
	path, err := filepath.Abs(output)
	if err != nil {
		return exitcode.Failed("%w", err)
	}
	opts.Prefix = strings.TrimSuffix(strings.TrimSuffix(filepath.Base(path), ".tar.gz"), ".tgz")
	if strings.Trim(opts.Prefix, ".") == "" {
		opts.Prefix = "pic-sure-support"
	}

	report, err := writeBundle(path, func(w io.Writer) (*ops.SupportBundleReport, error) {
		return ops.SupportBundle(cmd.Context(), d, w, opts)
	})
	if cause := context.Cause(cmd.Context()); cause != nil {
		return cause
	}
	if err != nil {
		return exitcode.Failed("writing the support bundle: %w", err)
	}
	report.Path = path
	return a.printReport(report, func(w io.Writer) error { return writeSupportBundle(w, report) })
}

// writeBundle writes the archive next to path under a temporary name, mode
// 0600, and renames it into place once it is complete. The temporary file
// is removed whatever happens, a panic included.
func writeBundle(path string, write func(io.Writer) (*ops.SupportBundleReport, error)) (*ops.SupportBundleReport, error) {
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return nil, err
	}
	defer func() { _ = os.Remove(f.Name()) }()
	report, err := write(f)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(f.Name(), path)
	}
	if err != nil {
		return nil, err
	}
	return report, nil
}

func writeSupportBundle(w io.Writer, r *ops.SupportBundleReport) error {
	var b strings.Builder
	fmt.Fprintf(&b, "Wrote %s (%d files)\n", r.Path, len(r.Files))
	if r.ShortSecrets > 0 {
		fmt.Fprintf(&b, "%d secret(s) are shorter than %d bytes and are redacted only where they stand alone; check the archive before sharing it\n", r.ShortSecrets, log.MinSecret)
	}
	if len(r.Problems) > 0 {
		b.WriteString("Not collected:\n")
		for _, p := range r.Problems {
			fmt.Fprintf(&b, "  - %s\n", p)
		}
	}
	_, err := io.WriteString(w, b.String())
	return err
}
