// Command templatedrift reports how the bash All-in-One's compose and config
// files changed since the commit the embedded stack templates were ported
// from (spec §14). It reads that commit and the template → AIO
// source table from internal/render/templates/README.md, diffs the AIO
// checkout between the commit and a later revision, and prints a Markdown
// report.
//
// Exit status: 0 when nothing watched changed, 1 when something did, 2 on
// an error. It only reads the AIO repository.
//
//	go run ./tools/templatedrift -aio ../pic-sure-all-in-one -to aio-compose
package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"regexp"
	"slices"
	"strings"
)

func main() {
	os.Exit(run(context.Background(), os.Args[1:], os.Stdout, os.Stderr))
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fl := flag.NewFlagSet("templatedrift", flag.ContinueOnError)
	fl.SetOutput(stderr)
	aio := fl.String("aio", "", "AIO git checkout or bare clone to read (required)")
	readme := fl.String("readme", "internal/render/templates/README.md", "templates README")
	from := fl.String("from", "", "AIO revision the templates match (default: the README's AIO commit)")
	to := fl.String("to", "HEAD", "AIO revision to compare with")
	name := fl.String("name", "AIO", "name of the upstream, for the report")
	maxBytes := fl.Int("max-bytes", 0, "leave out diffs that would make the report longer than this (0: no limit)")
	if err := fl.Parse(args); err != nil {
		return 2
	}
	if *aio == "" || fl.NArg() > 0 {
		_, _ = fmt.Fprintln(stderr, "usage: templatedrift -aio DIR [-from REV] [-to REV] [-readme FILE] [-name NAME] [-max-bytes N]")
		return 2
	}
	d, err := drift(ctx, *aio, *readme, *from, *to)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "templatedrift:", err)
		return 2
	}
	_, _ = io.WriteString(stdout, d.Report(*name, *maxBytes))
	if d.Changed() {
		return 1
	}
	return 0
}

func drift(ctx context.Context, aio, readme, from, to string) (*Drift, error) {
	data, err := os.ReadFile(readme)
	if err != nil {
		return nil, err
	}
	r, err := ParseReadme(data)
	if err != nil {
		return nil, err
	}
	if from == "" {
		from = r.Commit
	}
	return Diff(ctx, aio, from, to, r.Sources)
}

// Readme is what the templates README records about the port.
type Readme struct {
	Commit string
	// Sources maps each AIO source path to the templates ported from it.
	Sources map[string][]string
}

var (
	commitLine = regexp.MustCompile("(?m)^AIO commit: `([0-9a-f]{7,40})`$")
	codeSpan   = regexp.MustCompile("`([^`]+)`")
)

// ParseReadme reads the AIO commit line and the "Templates and their AIO
// sources" table, whose rows are "| `template` | `source`, ... |" or
// "| `template` | none |".
func ParseReadme(data []byte) (Readme, error) {
	m := commitLine.FindSubmatch(data)
	if m == nil {
		return Readme{}, errors.New("README has no \"AIO commit: `<sha>`\" line")
	}
	r := Readme{Commit: string(m[1]), Sources: map[string][]string{}}
	for line := range strings.SplitSeq(string(data), "\n") {
		cells := strings.Split(line, "|")
		if len(cells) != 4 || !strings.HasPrefix(strings.TrimSpace(cells[1]), "`") {
			continue
		}
		m := codeSpan.FindStringSubmatch(cells[1])
		if m == nil {
			return Readme{}, fmt.Errorf("README: table row %q has no template", strings.TrimSpace(line))
		}
		tmpl := m[1]
		srcs := codeSpan.FindAllStringSubmatch(cells[2], -1)
		if len(srcs) == 0 && strings.TrimSpace(cells[2]) != "none" {
			return Readme{}, fmt.Errorf("README: %s has neither AIO sources nor \"none\"", tmpl)
		}
		for _, s := range srcs {
			r.Sources[s[1]] = append(r.Sources[s[1]], tmpl)
		}
	}
	if len(r.Sources) == 0 {
		return Readme{}, errors.New("README maps no template to an AIO source")
	}
	return r, nil
}

// watched reports whether a change to an AIO file the templates weren't
// ported from still matters: a compose file or anything under config/, such
// as a new dev overlay.
func watched(p string) bool {
	base := path.Base(p)
	return p == base && strings.HasPrefix(base, "docker-compose") && (strings.HasSuffix(base, ".yml") || strings.HasSuffix(base, ".yaml")) ||
		strings.HasPrefix(p, "config/") || p == ".env.example"
}

// Change is one changed AIO file.
type Change struct {
	Path      string
	Status    string   // added, modified, deleted, ...
	Templates []string // templates ported from it; none for a watched file
	Diff      string
}

// Drift is what changed in the AIO between two revisions.
type Drift struct {
	From, To string // full commit IDs
	Sources  int    // AIO sources the templates were ported from
	Ported   []Change
	Other    []Change
}

// Changed reports whether anything watched changed.
func (d *Drift) Changed() bool { return len(d.Ported)+len(d.Other) > 0 }

var statusNames = map[byte]string{'A': "added", 'M': "modified", 'D': "deleted", 'T': "type changed"}

// Diff compares the AIO repository at dir between from and to.
func Diff(ctx context.Context, dir, from, to string, sources map[string][]string) (*Drift, error) {
	g := func(args ...string) (string, error) {
		cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
		// Never write to the AIO repository, not even an index refresh.
		cmd.Env = append(os.Environ(), "GIT_OPTIONAL_LOCKS=0")
		var out, errb bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &errb
		if err := cmd.Run(); err != nil {
			return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(errb.String()))
		}
		return out.String(), nil
	}
	resolve := func(rev string) (string, error) {
		out, err := g("rev-parse", "--verify", "--quiet", "--end-of-options", rev+"^{commit}")
		if err != nil {
			return "", fmt.Errorf("AIO revision %q not found in %s", rev, dir)
		}
		return strings.TrimSpace(out), nil
	}
	d := &Drift{Sources: len(sources)}
	var err error
	if d.From, err = resolve(from); err != nil {
		return nil, err
	}
	if d.To, err = resolve(to); err != nil {
		return nil, err
	}
	out, err := g("diff", "--name-status", "--no-renames", "-z", d.From, d.To)
	if err != nil {
		return nil, err
	}
	fields := strings.Split(strings.TrimSuffix(out, "\x00"), "\x00")
	for i := 0; i+1 < len(fields); i += 2 {
		c := Change{Path: fields[i+1], Status: statusNames[fields[i][0]], Templates: sources[fields[i+1]]}
		if c.Status == "" {
			c.Status = fields[i]
		}
		if c.Templates == nil && !watched(c.Path) {
			continue
		}
		if c.Diff, err = g("diff", "--no-color", "--no-ext-diff", "--no-renames", d.From, d.To, "--", c.Path); err != nil {
			return nil, err
		}
		if c.Templates != nil {
			d.Ported = append(d.Ported, c)
		} else {
			d.Other = append(d.Other, c)
		}
	}
	return d, nil
}

// Report renders the drift as Markdown. With maxBytes > 0 it leaves out the
// diffs that don't fit and says so.
func (d *Drift) Report(name string, maxBytes int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Compared %s at `%s` with `%s`, the commit the templates were ported from.\n\n", name, d.To[:12], d.From[:12])
	if !d.Changed() {
		fmt.Fprintf(&b, "None of its %d template sources, compose files or config files changed.\n", d.Sources)
		return b.String()
	}
	if len(d.Ported) > 0 {
		fmt.Fprintf(&b, "%d of the %d AIO files the templates were ported from changed:\n\n", len(d.Ported), d.Sources)
		b.WriteString("| AIO file | Change | Templates |\n|---|---|---|\n")
		for _, c := range d.Ported {
			fmt.Fprintf(&b, "| `%s` | %s | %s |\n", c.Path, c.Status, codeList(c.Templates))
		}
		b.WriteString("\n")
	}
	if len(d.Other) > 0 {
		b.WriteString("Compose or config files no template was ported from changed:\n\n")
		b.WriteString("| AIO file | Change |\n|---|---|\n")
		for _, c := range d.Other {
			fmt.Fprintf(&b, "| `%s` | %s |\n", c.Path, c.Status)
		}
		b.WriteString("\n")
	}
	b.WriteString("### Diffs\n")
	const omittedNote = "\n%d diffs left out to fit; the full report is in the workflow run's summary.\n"
	omitted := 0
	for _, c := range slices.Concat(d.Ported, d.Other) {
		fence := strings.Repeat("`", max(3, longestRun(c.Diff, '`')+1))
		block := fmt.Sprintf("\n<details><summary><code>%s</code></summary>\n\n%sdiff\n%s%s\n\n</details>\n", c.Path, fence, c.Diff, fence)
		if maxBytes > 0 && b.Len()+len(block)+len(omittedNote)+8 > maxBytes {
			omitted++
			continue
		}
		b.WriteString(block)
	}
	if omitted > 0 {
		fmt.Fprintf(&b, omittedNote, omitted)
	}
	return b.String()
}

func codeList(items []string) string {
	quoted := make([]string, len(items))
	for i, s := range items {
		quoted[i] = "`" + s + "`"
	}
	return strings.Join(quoted, ", ")
}

func longestRun(s string, c byte) int {
	longest, n := 0, 0
	for i := range len(s) {
		if s[i] == c {
			n++
			longest = max(longest, n)
		} else {
			n = 0
		}
	}
	return longest
}
