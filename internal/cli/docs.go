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
// page per visible command, plus README.md listing them (ticket 065,
// `make docs`). The pages hold only what cobra's help shows, so they are
// the same on every machine.
func WriteCommandDocs(dir string) error {
	root := newRootCmd(NewApp(BuildInfo{}))
	root.DisableAutoGenTag = true
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
	index.WriteString("Generated from `pic-sure --help` by `make docs`; don't edit by hand.\n\n")
	index.WriteString("| Command | Does |\n|---|---|\n")
	for _, c := range docCommands(root) {
		fmt.Fprintf(&index, "| [`%s`](%s) | %s |\n", c.CommandPath(), docFile(c), c.Short)
		if err := os.WriteFile(filepath.Join(dir, docFile(c)), commandDoc(c), 0o644); err != nil {
			return err
		}
	}
	return os.WriteFile(filepath.Join(dir, "README.md"), index.Bytes(), 0o644)
}

// docCommands returns cmd and its visible descendants, depth first in
// cobra's (alphabetical) order. help and completion are cobra's own.
func docCommands(cmd *cobra.Command) []*cobra.Command {
	out := []*cobra.Command{cmd}
	for _, c := range cmd.Commands() {
		if c.Hidden || c.Name() == "help" || c.Name() == "completion" {
			continue
		}
		out = append(out, docCommands(c)...)
	}
	return out
}

func docFile(c *cobra.Command) string {
	return strings.ReplaceAll(c.CommandPath(), " ", "_") + ".md"
}

func commandDoc(c *cobra.Command) []byte {
	var b bytes.Buffer
	fmt.Fprintf(&b, "# %s\n\n%s\n\n", c.CommandPath(), c.Short)
	if c.Long != "" {
		b.WriteString(c.Long + "\n\n")
	}
	if c.Runnable() {
		fmt.Fprintf(&b, "```\n%s\n```\n\n", c.UseLine())
	}
	if len(c.Aliases) > 0 {
		fmt.Fprintf(&b, "Aliases: %s\n\n", strings.Join(c.Aliases, ", "))
	}
	if c.Example != "" {
		fmt.Fprintf(&b, "## Examples\n\n```\n%s\n```\n\n", c.Example)
	}
	if subs := docCommands(c)[1:]; len(subs) > 0 {
		b.WriteString("## Subcommands\n\n")
		for _, s := range subs {
			if s.Parent() == c {
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
