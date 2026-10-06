package cli

import "github.com/spf13/cobra"

func newRootCmd(a *App) *cobra.Command {
	root := &cobra.Command{
		Use:   "pic-sure",
		Short: "Install and operate PIC-SURE All-in-One stacks",
		Long: `pic-sure installs, runs, updates, loads data into, and tears down
PIC-SURE All-in-One stacks on macOS and Linux. It needs only docker (with
the compose plugin) and git.

Run with no arguments on a terminal to open the TUI.`,
		Version:       versionString(a.Info),
		Args:          cobra.NoArgs,
		SilenceErrors: true, // App.Run reports errors
		SilenceUsage:  true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if a.canPrompt() && !a.Global.Plain {
				return a.startTUI()
			}
			return cmd.Help()
		},
	}
	root.SetVersionTemplate("{{.Version}}\n")
	a.Global.register(root)

	// Every command, in spec §5 order. Each constructor lives in its group's
	// file and is owned by the ticket that implements it.
	root.AddCommand(
		newInitCmd(a),
		newUpCmd(a),
		newDownCmd(a),
		newRestartCmd(a),
		newPsCmd(a),
		newLogsCmd(a),
		newComposeCmd(a),
		newStatusCmd(a),
		newDoctorCmd(a),
		newUpdateCmd(a),
		newBuildCmd(a),
		newMigrateCmd(a),
		newConfigCmd(a),
		newSecretsCmd(a),
		newDataCmd(a),
		newDictionaryCmd(a),
		newSharedDataCmd(a),
		newDevCmd(a),
		newDBCmd(a),
		newResetCmd(a),
		newDestroyCmd(a),
		newCacheCmd(a),
		newSelfUpdateCmd(a),
		newSupportBundleCmd(a),
		newVersionCmd(a),
	)
	markRunning(a, root)
	return root
}

// markRunning wraps the RunE of cmd and its descendants to set a.running.
// cobra validates required flags and flag groups after the PreRun hooks, so
// the start of RunE is the first point where the command line is known to
// be valid. A PreRunE that fails for a reason other than bad usage must
// return an *exitcode.Error to avoid being reported as a usage error.
func markRunning(a *App, cmd *cobra.Command) {
	if run := cmd.RunE; run != nil {
		cmd.RunE = func(c *cobra.Command, args []string) error {
			a.running = true
			return run(c, args)
		}
	}
	for _, sub := range cmd.Commands() {
		markRunning(a, sub)
	}
}
