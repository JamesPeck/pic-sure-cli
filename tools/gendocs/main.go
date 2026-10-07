// Command gendocs writes the Markdown command reference from the CLI's
// cobra help (`make docs`).
//
//	go run ./tools/gendocs docs/commands
package main

import (
	"fmt"
	"os"

	"github.com/JamesPeck/pic-sure-cli/internal/cli"
)

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: gendocs DIR")
		os.Exit(2)
	}
	if err := cli.WriteCommandDocs(os.Args[1]); err != nil {
		fmt.Fprintln(os.Stderr, "gendocs:", err)
		os.Exit(1)
	}
}
