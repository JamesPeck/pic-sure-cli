package cli

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"strings"

	"github.com/spf13/cobra"

	"github.com/JamesPeck/pic-sure-cli/internal/exitcode"
	"github.com/JamesPeck/pic-sure-cli/internal/ops"
	"github.com/JamesPeck/pic-sure-cli/internal/stack"
)

func newDBCmd(a *App) *cobra.Command {
	bootstrap := &cobra.Command{
		Use:   "bootstrap",
		Short: "Create the databases, users and grants on a remote MySQL",
		Long: `For a stack with db.mode remote: connect to the remote MySQL as
db.remote.root_user and create the auth and picsure databases and the
picsure, auth and airflow users, with their passwords from secrets.yaml and
all privileges on their databases. What already exists is left alone, so an
existing user keeps its password; --sync-passwords sets each user's password
to the one in secrets.yaml. init, up, update and migrate run the same step.

--check reports the databases, users, grants and logins without changing
anything, and exits 3 when bootstrap has work to do.

The mysql client runs in a mysql:8.0 container, and the stack's services
connect from theirs, so db.remote.host must be reachable from a container:
localhost or 127.0.0.1 is the container itself. For a MySQL on the Docker
host, use host.docker.internal where the runtime provides it (Docker Desktop,
OrbStack).`,
		Args: cobra.NoArgs,
		RunE: a.dbBootstrap,
	}
	bootstrap.Flags().Bool("check", false, "report schemas, users and grants without changing anything")
	bootstrap.Flags().Bool("sync-passwords", false, "set the users' passwords to match secrets.yaml")
	bootstrap.MarkFlagsMutuallyExclusive("check", "sync-passwords")
	return newGroup("db", "Manage a remote MySQL (db.mode remote only)", bootstrap)
}

func (a *App) dbBootstrap(cmd *cobra.Command, _ []string) error {
	check, _ := cmd.Flags().GetBool("check")
	sync, _ := cmd.Flags().GetBool("sync-passwords")
	st, err := a.openStack(cmd)
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()
	d := a.newDeps()
	if !check {
		lock, err := a.lockStack(cmd.Context(), cmd, st, d.Sink)
		if err != nil {
			return err
		}
		defer func() { _ = lock.Unlock() }()
	}
	cfg, err := st.LoadConfig()
	if err != nil {
		return configError(err)
	}
	if cfg.DB.Mode != stack.DBRemote {
		return exitcode.Precondition("db bootstrap is for a remote database, and this stack's db.mode is %s; "+
			"its picsure-db creates the databases and users itself", cfg.DB.Mode)
	}
	sec, err := st.LoadSecrets()
	if errors.Is(err, fs.ErrNotExist) {
		return exitcode.Precondition("the stack has no secrets.yaml; run `pic-sure init` to finish creating it")
	}
	if err != nil {
		return err
	}
	if sec.DBRemoteRootPassword == "" {
		return exitcode.Precondition("secrets.yaml has no db_remote_root_password; " +
			"run `pic-sure init` with --db-root-password-stdin to store it")
	}

	if check {
		report, err := ops.CheckBootstrap(cmd.Context(), d, cfg, sec)
		if err != nil {
			return err
		}
		if err := a.printReport(report, func(w io.Writer) error { return writeBootstrapCheck(w, report) }); err != nil {
			return err
		}
		if report.NeedsSync() {
			return exitcode.Precondition("the remote database needs `pic-sure db bootstrap --sync-passwords`")
		}
		if !report.OK {
			return exitcode.Precondition("the remote database needs `pic-sure db bootstrap`")
		}
		return nil
	}
	opts := ops.BootstrapOptions{SyncPasswords: sync}
	if err := ops.Bootstrap(cmd.Context(), d, cfg, sec, opts, a.Global.SkipSteps); err != nil {
		return err
	}
	return a.finish(nil, func(w io.Writer) error {
		_, err := fmt.Fprintln(w, "The remote database is ready.")
		return err
	})
}

func writeBootstrapCheck(w io.Writer, r *ops.BootstrapReport) error {
	var b strings.Builder
	fmt.Fprintf(&b, "MySQL %s at %s\n", r.Version, r.Server)
	for _, c := range r.Checks {
		mark := "ok"
		if !c.OK {
			mark = "FAIL"
		}
		fmt.Fprintf(&b, "%-4s %s", mark, c.Name)
		if c.Problem != "" {
			fmt.Fprintf(&b, " (%s)", c.Problem)
		}
		b.WriteString("\n")
	}
	if r.OK {
		b.WriteString("The remote database is ready.\n")
	}
	_, err := io.WriteString(w, b.String())
	return err
}
