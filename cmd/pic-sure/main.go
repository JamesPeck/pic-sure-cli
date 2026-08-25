// Command pic-sure is a frontend for the PIC-SURE All-in-One deployment
// scripts. Every mutating operation is performed by exec'ing the bash
// scripts at the repository root; the binary never edits .env or invokes
// docker compose directly (see pic-sure-all-in-one/docs/cli-contract.md).
package main

import (
	"os"

	"github.com/JamesPeck/pic-sure-cli/internal/commands"
)

// Injected via -ldflags (see Makefile).
var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

func main() {
	os.Exit(commands.Execute(commands.BuildInfo{
		Version: version,
		Commit:  commit,
		Date:    date,
	}))
}
