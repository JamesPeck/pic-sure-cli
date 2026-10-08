package cli

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
)

// WriteCommandDocs replaces the Markdown command reference in dir with one
// page per visible command, plus README.md listing them (`make docs`).
// The pages hold only what cobra's help shows, so they are
// the same on every machine.
func WriteCommandDocs(dir string) error {
	root := newDocRoot()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	old, err := filepath.Glob(filepath.Join(dir, "*.md"))
	if err != nil {
		return err
	}
	for _, f := range old {
		if err := os.Remove(f); err != nil {
			return err
		}
	}

	var index bytes.Buffer
	index.WriteString("# Command reference\n\n")
	index.WriteString("Generated from the CLI's help by `make docs`; don't edit by hand.\n\n")
	index.WriteString("| Command | Does |\n|---|---|\n")
	for _, c := range docCommands(root) {
		fmt.Fprintf(&index, "| [`%s`](%s) | %s |\n", c.CommandPath(), docFile(c), c.Short)
		if err := os.WriteFile(filepath.Join(dir, docFile(c)), commandDoc(c), 0o644); err != nil {
			return err
		}
	}
	return os.WriteFile(filepath.Join(dir, "README.md"), index.Bytes(), 0o644)
}

// newDocRoot is the command tree with cobra's help and version flags,
// which cobra otherwise adds only at execute time.
func newDocRoot() *cobra.Command {
	root := newRootCmd(NewApp(BuildInfo{}))
	root.InitDefaultHelpFlag()
	root.InitDefaultVersionFlag()
	return root
}

// docCommands returns cmd and its visible descendants, depth first in
// cobra's (alphabetical) order.
func docCommands(cmd *cobra.Command) []*cobra.Command {
	out := []*cobra.Command{cmd}
	for _, c := range cmd.Commands() {
		if documented(c) {
			out = append(out, docCommands(c)...)
		}
	}
	return out
}

// documented reports whether c gets a page: help and completion are
// cobra's own.
func documented(c *cobra.Command) bool {
	return !c.Hidden && c.Name() != "help" && c.Name() != "completion"
}

func docFile(c *cobra.Command) string {
	return strings.ReplaceAll(c.CommandPath(), " ", "_") + ".md"
}

func commandDoc(c *cobra.Command) []byte {
	var b bytes.Buffer
	fmt.Fprintf(&b, "# %s\n\n%s\n\n", c.CommandPath(), c.Short)
	// Help text is plain text, with <placeholders> and indented tables that
	// Markdown would swallow, so it goes in a code block as --help shows it.
	if c.Long != "" {
		fmt.Fprintf(&b, "```text\n%s\n```\n\n", strings.TrimRight(c.Long, "\n"))
	}
	// A group's RunE only rejects a missing subcommand; the root's opens
	// the TUI.
	if c.Runnable() && (!c.HasParent() || !c.HasAvailableSubCommands()) {
		fmt.Fprintf(&b, "```\n%s\n```\n\n", c.UseLine())
	}
	if len(c.Aliases) > 0 {
		fmt.Fprintf(&b, "Aliases: %s\n\n", strings.Join(c.Aliases, ", "))
	}
	if c.Example != "" {
		fmt.Fprintf(&b, "## Examples\n\n```\n%s\n```\n\n", c.Example)
	}
	if c.HasAvailableSubCommands() {
		b.WriteString("## Subcommands\n\n")
		for _, s := range c.Commands() {
			if documented(s) {
				fmt.Fprintf(&b, "- [`%s`](%s): %s\n", s.CommandPath(), docFile(s), s.Short)
			}
		}
		b.WriteString("\n")
	}
	if fl := c.NonInheritedFlags(); fl.HasAvailableFlags() {
		fmt.Fprintf(&b, "## Flags\n\n```\n%s```\n\n", fl.FlagUsages())
	}
	if c.HasParent() {
		fmt.Fprintf(&b, "The [global flags](%s#flags) apply too.\n\n", docFile(c.Root()))
	}
	if p := c.Parent(); p != nil {
		fmt.Fprintf(&b, "See also: [`%s`](%s)\n", p.CommandPath(), docFile(p))
	}
	return append(bytes.TrimRight(b.Bytes(), "\n"), '\n')
}
