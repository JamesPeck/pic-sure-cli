package cli

import (
	"fmt"
	"io"

	"github.com/spf13/cobra"
)

func newVersionCmd(a *App) *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the CLI version",
		Args:  cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			report := versionReport{Version: a.Info.Version, Commit: a.Info.Commit, Date: a.Info.Date}
			return a.printReport(report, func(w io.Writer) error {
				_, err := fmt.Fprintln(w, versionString(a.Info))
				return err
			})
		},
	}
}

// versionReport is `version --json`.
type versionReport struct {
	Version string `json:"version"`
	Commit  string `json:"commit"`
	Date    string `json:"date"`
}

// versionString names the product line ("v2 (native)", which tells it apart
// from the v1 script wrapper of the same name; spec §14) and the build.
func versionString(info BuildInfo) string {
	return fmt.Sprintf("pic-sure v2 (native)\nversion %s, commit %s, built %s", info.Version, info.Commit, info.Date)
}
