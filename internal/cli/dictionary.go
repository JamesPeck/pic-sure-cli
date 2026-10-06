package cli

import "github.com/spf13/cobra"

func newDictionaryCmd(a *App) *cobra.Command {
	sub := func(use, short string) *cobra.Command {
		return &cobra.Command{Use: use, Short: short, Args: cobra.NoArgs, RunE: notImplemented("044")}
	}
	return newGroup("dictionary", "Rebuild and load the search dictionary",
		sub("hydrate", "Build the dictionary from the loaded HPDS data"),
		sub("load-csv", "Load a custom dictionary from CSV"),
		sub("load-facets", "Load facet categories, facets and facet concepts"),
		sub("weights", "Recompute the search weights"),
	)
}
