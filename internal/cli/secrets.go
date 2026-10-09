package cli

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/JamesPeck/pic-sure-cli/internal/docker"
	"github.com/JamesPeck/pic-sure-cli/internal/exitcode"
	"github.com/JamesPeck/pic-sure-cli/internal/ops"
	"github.com/JamesPeck/pic-sure-cli/internal/stack"
)

func newSecretsCmd(a *App) *cobra.Command {
	rotate := &cobra.Command{
		Use:   "rotate NAME",
		Short: "Rotate a secret everywhere it is used",
		Long: `Replace one secret: first in the database that checks it, then in
secrets.yaml, then in the running services whose compose config uses it,
which are recreated. NAME is one of:

  db-root, db-picsure, db-auth, db-airflow   MySQL passwords (ALTER USER)
  dictionary-db                              the dictionary Postgres password
  introspection-token                        PSAMA's token, re-issued
  query-service-token, application-token,
  logging-key, obfuscation-salt              read by the services only
  hpds-key                                   the HPDS encryption key
  auth0-client-secret                        the Auth0 application's secret

New values are generated, except the Auth0 client secret and, with a remote
database, the root password: those are read from stdin and need --yes. The
Auth0 client secret must be at least 32 bytes; supplying it re-issues the
introspection token, and it is how a stack leaves open mode: set
auth.auth0.client_id, then auth.mode, rotate auth0-client-secret, then run ` + "`pic-sure up`" + `. With a remote database,
db-root records a password the DBA has already changed, after checking it
logs in.

hpds-key refuses while HPDS has data loaded, since the new key can't read
it: --discard-data --yes deletes the data first. Load it again afterwards.

The database must be running for a database secret. On a terminal you
confirm; otherwise pass --yes.`,
		Args:      cobra.ExactArgs(1),
		ValidArgs: ops.RotateNames(),
		RunE:      a.rotateSecret,
	}
	rotate.Flags().Bool("discard-data", false, "with hpds-key: delete the loaded HPDS data, which the new key can't read (needs --yes)")
	return newGroup("secrets", "Manage the stack's secrets", rotate)
}

func (a *App) rotateSecret(cmd *cobra.Command, args []string) error {
	name := args[0]
	if !slices.Contains(ops.RotateNames(), name) {
		return withUsageHint(exitcode.Usage("unknown secret %q; secrets rotate takes %s", name, strings.Join(ops.RotateNames(), ", ")))
	}
	discard, _ := cmd.Flags().GetBool("discard-data")
	if discard && name != ops.SecretHPDSKey {
		return exitcode.Usage("--discard-data is only for hpds-key")
	}
	if discard && !a.Global.Yes {
		return exitcode.ConfirmRequired("--discard-data deletes the loaded HPDS data; pass --yes with it. Nothing was changed.")
	}
	st, err := a.openStackUnlogged(cmd)
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()
	cfg, err := st.LoadConfig()
	if err != nil {
		return configError(err)
	}
	opts := ops.RotateOptions{Name: name, DiscardData: discard}
	stdin := ops.RotateReadsStdin(cfg, name)
	if stdin {
		// Stdin holds the secret, so it can't hold the answer too.
		if !a.Global.Yes {
			return exitcode.ConfirmRequired("secrets rotate %s reads the new value from stdin, so it needs --yes. Nothing was changed.", name)
		}
		what := secretPromptNames["auth.auth0.client_secret"]
		if name == ops.SecretDBRoot {
			what = secretPromptNames["db.remote.root_password"]
		}
		if opts.Value, err = a.readUserSecret(cmd.Context(), what, "stdin"); err != nil {
			return err
		}
	} else if err := a.confirmYes(cmd, fmt.Sprintf("This replaces stack %s's %s and recreates the running services that use it.", cfg.Name, name)); err != nil {
		return err
	}
	if err := ops.CheckRotateOptions(cfg, opts); err != nil {
		return err
	}
	a.openRunLog(st)

	d := a.newDeps()
	lock, err := a.lockStack(cmd.Context(), cmd, st, d.Sink)
	if err != nil {
		return err
	}
	defer func() { _ = lock.Unlock() }()
	// Under the lock: the config may have changed while we waited.
	if cfg, err = st.LoadConfig(); err != nil {
		return configError(err)
	}
	if ops.RotateReadsStdin(cfg, name) != stdin {
		return exitcode.Precondition("db.mode changed while waiting for the stack lock; run the command again")
	}
	state, err := st.LoadState()
	if errors.Is(err, fs.ErrNotExist) || err == nil && state.InitializedAt.IsZero() {
		return exitcode.Precondition("the stack in %s isn't initialised; run `pic-sure init %s` to finish it", st.Dir, st.Dir)
	}
	if err != nil {
		return err
	}
	sec, err := st.LoadSecrets()
	if errors.Is(err, fs.ErrNotExist) {
		return exitcode.Precondition("the stack in %s has no %s; run `pic-sure init %s` to finish it", st.Dir, stack.SecretsFile, st.Dir)
	}
	if err != nil {
		return err
	}
	if d.Compose, err = a.upCompose(d, st, cfg, sec)(); err != nil {
		if errors.Is(err, docker.ErrNotRendered) {
			return exitcode.Precondition("%w; run `pic-sure up`", err)
		}
		return err
	}

	if err := checkOwned(cmd, d, st, cfg); err != nil {
		return err
	}
	state.StartOperation("secrets rotate", d.Clock.Now())
	if err := st.SaveState(state); err != nil {
		return err
	}
	report, err := ops.RotateSecret(cmd.Context(), d, st, cfg, sec, opts)
	if ferr := finishUp(d, st, err); err == nil {
		err = ferr
	}
	if err != nil {
		return err
	}
	return a.finish(report, func(w io.Writer) error { return writeRotateSummary(w, report) })
}

// confirmYes asks the user to confirm with y. --yes answers for them.
// Without terminals to ask on (canConfirm), it is exit 4, before anything
// has changed.
func (a *App) confirmYes(cmd *cobra.Command, what string) error {
	if a.Global.Yes {
		return nil
	}
	if !a.canConfirm() {
		return exitcode.ConfirmRequired("%s needs confirmation: pass --yes, or run it on a terminal. Nothing was changed.", cmd.CommandPath())
	}
	yes, err := a.askYesNo(cmd.Context(), what+"\nContinue?")
	if err != nil {
		return err
	}
	if !yes {
		return exitcode.ConfirmRequired("not confirmed; nothing was changed")
	}
	return nil
}

func writeRotateSummary(w io.Writer, r *ops.RotateReport) error {
	var b strings.Builder
	fmt.Fprintf(&b, "Rotated %s for stack %s.\n", r.Secret, r.Stack)
	if r.DiscardedData {
		b.WriteString("The HPDS data was deleted; load it again.\n")
	}
	if r.TokenExpiry != nil {
		fmt.Fprintf(&b, "The new introspection token is valid until %s.\n", r.TokenExpiry.Format(time.DateOnly))
	}
	if len(r.Restarted) > 0 {
		fmt.Fprintf(&b, "Restarted %s.\n", strings.Join(r.Restarted, ", "))
	}
	_, err := io.WriteString(w, b.String())
	return err
}
