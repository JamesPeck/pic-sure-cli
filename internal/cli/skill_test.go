package cli

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// skillDir is the agent skill for using pic-sure (skills/pic-sure), whose
// command lines must match the real command tree.
const skillDir = "../../skills/pic-sure"

func TestSkillCommandsExist(t *testing.T) {
	files := skillFiles(t)
	allFlags := flagNames(newDocRoot())
	checked := 0
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		name, _ := filepath.Rel(skillDir, f)
		for _, line := range skillCommandLines(string(data)) {
			for _, args := range picSureInvocations(line) {
				checked++
				if err := checkInvocation(args); err != nil {
					t.Errorf("%s: %q: %v", name, line, err)
				}
			}
		}
		// Flags in prose or in partial commands still have to exist somewhere.
		for _, m := range regexp.MustCompile(`(^|[\s(\x60])--([a-z][a-z0-9-]*)`).FindAllStringSubmatch(string(data), -1) {
			if !allFlags[m[2]] {
				t.Errorf("%s: no command has the flag --%s", name, m[2])
			}
		}
	}
	// Guards against the extraction silently finding nothing.
	if checked < 20 {
		t.Errorf("found only %d pic-sure commands in the skill", checked)
	}
	t.Logf("checked %d pic-sure commands", checked)
}

func TestSkillHasNoEmDash(t *testing.T) {
	for _, f := range skillFiles(t) {
		data, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for i, line := range strings.Split(string(data), "\n") {
			if strings.ContainsRune(line, '—') {
				t.Errorf("%s:%d has an em dash", f, i+1)
			}
		}
	}
}

func TestSkillCheckerCatchesMistakes(t *testing.T) {
	for _, line := range []string{
		"pic-sure stauts --json",
		"pic-sure data demo --hep 1024",
		"pic-sure --stack DIR destroy --yes --prune",
		"pic-sure init DIR --json --name x --auth-mode",
	} {
		args := picSureInvocations(line)
		if len(args) != 1 {
			t.Fatalf("%q: %d invocations", line, len(args))
		}
		if checkInvocation(args[0]) == nil {
			t.Errorf("%q passed", line)
		}
	}
	for _, line := range []string{
		"pic-sure --stack DIR data demo nhanes --json > demo.ndjson",
		"pic-sure config set hpds.java_opts -Xmx4g && pic-sure up --json",
		"printf '%s\\n' \"$S\" | pic-sure --yes --json secrets rotate auth0-client-secret",
		"pic-sure compose -- exec hpds sh",
		"pic-sure COMMAND --help",
	} {
		for _, args := range picSureInvocations(line) {
			if err := checkInvocation(args); err != nil {
				t.Errorf("%q: %v", line, err)
			}
		}
	}
}

func skillFiles(t *testing.T) []string {
	t.Helper()
	var files []string
	err := filepath.WalkDir(skillDir, func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && strings.HasSuffix(p, ".md") {
			files = append(files, p)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatalf("no skill files in %s", skillDir)
	}
	return files
}

// newDocRoot is the command tree with cobra's help and version flags, which
// it otherwise adds only at execute time.
func newDocRoot() *cobra.Command {
	root := newRootCmd(NewApp(BuildInfo{}))
	root.InitDefaultHelpFlag()
	root.InitDefaultVersionFlag()
	return root
}

func flagNames(cmd *cobra.Command) map[string]bool {
	names := map[string]bool{}
	var walk func(*cobra.Command)
	walk = func(c *cobra.Command) {
		c.InitDefaultHelpFlag()
		c.Flags().VisitAll(func(f *pflag.Flag) { names[f.Name] = true })
		c.PersistentFlags().VisitAll(func(f *pflag.Flag) { names[f.Name] = true })
		for _, sub := range c.Commands() {
			walk(sub)
		}
	}
	walk(cmd)
	return names
}

var (
	fenceRe      = regexp.MustCompile("(?s)```[a-z]*\n(.*?)```")
	inlineCodeRe = regexp.MustCompile("`([^`]+)`")
)

// skillCommandLines returns the lines of fenced code blocks, with
// backslash continuations joined, and every inline code span.
func skillCommandLines(md string) []string {
	var out []string
	for _, m := range fenceRe.FindAllStringSubmatch(md, -1) {
		block := strings.ReplaceAll(m[1], "\\\n", " ")
		out = append(out, strings.Split(block, "\n")...)
	}
	prose := fenceRe.ReplaceAllString(md, "")
	// Inline spans may wrap across lines in Markdown.
	prose = strings.ReplaceAll(prose, "\n", " ")
	for _, m := range inlineCodeRe.FindAllStringSubmatch(prose, -1) {
		out = append(out, m[1])
	}
	return out
}

// picSureInvocations splits a shell line into words, roughly as sh would
// (quotes, pipes, redirections, && and ||), and returns the arguments of
// every pic-sure command on it.
func picSureInvocations(line string) [][]string {
	var (
		out     [][]string
		cur     []string
		inPS    bool
		atStart = true
	)
	flush := func() {
		if inPS {
			out = append(out, cur)
		}
		cur, inPS, atStart = nil, false, true
	}
	words := shellWords(line)
	for i := 0; i < len(words); i++ {
		w := words[i]
		switch {
		case w == "|" || w == "||" || w == "&&" || w == ";":
			flush()
		case w == "#":
			flush()
			i = len(words)
		case strings.HasPrefix(w, ">") || strings.HasPrefix(w, "2>") || strings.HasPrefix(w, "<"):
			// A redirection and its target end the command's arguments.
			if w == ">" || w == "2>" || w == "<" {
				i++
			}
			if inPS {
				out = append(out, cur)
				inPS = false
			}
		case atStart:
			atStart = false
			inPS = w == "pic-sure"
		case inPS:
			cur = append(cur, w)
		}
	}
	flush()
	return out
}

func shellWords(line string) []string {
	var (
		words []string
		b     strings.Builder
		quote rune
		have  bool
	)
	for _, r := range line {
		switch {
		case quote != 0:
			if r == quote {
				quote = 0
			} else {
				b.WriteRune(r)
			}
		case r == '\'' || r == '"':
			quote, have = r, true
		case r == ' ' || r == '\t':
			if have {
				words = append(words, b.String())
				b.Reset()
				have = false
			}
		default:
			b.WriteRune(r)
			have = true
		}
	}
	if have {
		words = append(words, b.String())
	}
	return words
}

// placeholderRe matches an argument standing for any command, such as
// COMMAND in `pic-sure COMMAND --help`.
var placeholderRe = regexp.MustCompile(`^[A-Z][A-Z_]*$`)

// checkInvocation resolves args against the command tree as cobra would
// and parses their flags, failing on an unknown command or flag.
func checkInvocation(args []string) error {
	cmd, rest, err := newDocRoot().Find(args)
	if err == nil {
		cmd.InitDefaultHelpFlag()
		err = cmd.ParseFlags(rest)
	}
	if err != nil {
		return err
	}
	pos := cmd.Flags().Args()
	if len(pos) > 0 && placeholderRe.MatchString(pos[0]) && cmd.HasAvailableSubCommands() {
		return nil
	}
	if len(pos) > 0 && cmd.HasAvailableSubCommands() {
		return fmt.Errorf("%s has no subcommand %q", cmd.CommandPath(), pos[0])
	}
	return cmd.ValidateArgs(pos)
}
