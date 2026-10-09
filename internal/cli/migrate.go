package cli

import (
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	"github.com/JamesPeck/pic-sure-cli/internal/exitcode"
	"github.com/JamesPeck/pic-sure-cli/internal/ops"
)

func newMigrateCmd(a *App) *cobra.Command {
	c := &cobra.Command{
		Use:   "migrate",
		Short: "Run the database migrations",
		Long: `Start the database if it isn't running, then run the Flyway migrations:
the auth and picsure schemas, the migrations project's, and the dictionary's.
A database that is already migrated is left alone. After a migration, psama
and dictionary-api are restarted if they are running.

--check verifies the migrations' inputs (the SQL directories, the dictionary
schema, the project UUIDs, the remote database settings and the compose
config) without touching the database. --repair runs Flyway repair, which
removes failed entries from the history so a fixed migration can run again.`,
		Args: cobra.NoArgs,
		RunE: a.migrate,
	}
	c.Flags().Bool("check", false, "verify the migration inputs without running them")
	c.Flags().Bool("repair", false, "run Flyway repair")
	c.MarkFlagsMutuallyExclusive("check", "repair")
	return skippable(c)
}

func (a *App) migrate(cmd *cobra.Command, _ []string) error {
	check, _ := cmd.Flags().GetBool("check")
	repair, _ := cmd.Flags().GetBool("repair")
	if check && len(a.Global.SkipSteps) > 0 {
		return exitcode.Usage("--skip-step can't be used with --check, which runs no steps")
	}
	st, err := a.openStackToCheckSkips(cmd)
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()
	if len(a.Global.SkipSteps) > 0 {
		// The plan depends on db.mode, so this reads the config before the
		// lock to check the IDs.
		cfg, err := st.LoadConfig()
		if err != nil {
			return configError(err)
		}
		if err := a.checkStackSkipSteps(cmd, st, ops.MigrateStepIDs(cfg)); err != nil {
			return err
		}
	}
	d := a.newDeps()
	if !check {
		lock, err := a.lockStack(cmd.Context(), cmd, st, d.Sink)
		if err != nil {
			return err
		}
		defer func() { _ = lock.Unlock() }()
	}
	c, cfg, sec, err := a.stackComposeConfig(cmd, d.Runner, st)
	if err != nil {
		return err
	}
	d.Compose = c

	if check {
		report := ops.MigrateCheck(cmd.Context(), d, cfg, sec)
		if err := a.printReport(report, func(w io.Writer) error { return writeMigrateCheck(w, report) }); err != nil {
			return err
		}
		if !report.OK {
			return exitcode.Precondition("migration check failed")
		}
		return nil
	}
	opts := ops.MigrateOptions{Action: ops.FlywayMigrate}
	if repair {
		opts.Action = ops.FlywayRepair
	}
	if err := ops.Migrate(cmd.Context(), d, cfg, sec, opts, a.Global.SkipSteps); err != nil {
		return err
	}
	return a.finish(map[string]any{"action": opts.Action}, nil)
}

func writeMigrateCheck(w io.Writer, r *ops.MigrateCheckReport) error {
	var b strings.Builder
	for _, in := range r.Inputs {
		mark := "ok"
		if !in.OK {
			mark = "FAIL"
		}
		fmt.Fprintf(&b, "%-4s %s", mark, in.Name)
		if in.Path != "" {
			fmt.Fprintf(&b, ": %s", in.Path)
		}
		if in.Problem != "" {
			fmt.Fprintf(&b, " (%s)", in.Problem)
		}
		b.WriteString("\n")
	}
	for _, warn := range r.Warnings {
		fmt.Fprintf(&b, "warning: %s\n", warn)
	}
	if r.OK {
		b.WriteString("Migration inputs look valid.\n")
	}
	_, err := io.WriteString(w, b.String())
	return err
}
