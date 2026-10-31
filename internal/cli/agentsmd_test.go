package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The root AGENTS.md points agents at the docs and copies docs/agents.md's
// exit-code table. These tests keep both from going stale.
const (
	repoRoot     = "../.."
	agentsMDName = "AGENTS.md"
	agentsDoc    = "docs/agents.md"
)

func readRepoFile(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(repoRoot, name))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestAgentsMDIsShort(t *testing.T) {
	if n := strings.Count(readRepoFile(t, agentsMDName), "\n"); n >= 60 {
		t.Errorf("%s has %d lines; keep it under 60 and link to the docs instead", agentsMDName, n)
	}
}

func TestReadmeLinksToAgentsMD(t *testing.T) {
	if !strings.Contains(readRepoFile(t, "README.md"), "](AGENTS.md)") {
		t.Errorf("README.md doesn't link to %s", agentsMDName)
	}
}

var (
	mdLinkRe   = regexp.MustCompile(`\]\(([^)\s]+)\)`)
	codePathRe = regexp.MustCompile("`([A-Za-z0-9_.-]+(?:/[A-Za-z0-9_.-]+)+/?)`")
)

func TestAgentsMDPathsExist(t *testing.T) {
	// The exit-code table is a verbatim copy of docs/agents.md's, whose
	// links are relative to docs/ and whose paths may be inside a stack.
	var lines []string
	for _, line := range strings.Split(readRepoFile(t, agentsMDName), "\n") {
		if !strings.HasPrefix(line, "|") {
			lines = append(lines, line)
		}
	}
	md := strings.Join(lines, "\n")
	var paths []string
	for _, m := range mdLinkRe.FindAllStringSubmatch(md, -1) {
		if !strings.Contains(m[1], "://") {
			paths = append(paths, m[1])
		}
	}
	for _, m := range codePathRe.FindAllStringSubmatch(md, -1) {
		paths = append(paths, m[1])
	}
	// Guards against the extraction silently finding nothing.
	if len(paths) < 5 {
		t.Fatalf("found only %d paths in %s", len(paths), agentsMDName)
	}
	for _, p := range paths {
		file, anchor, _ := strings.Cut(p, "#")
		if _, err := os.Stat(filepath.Join(repoRoot, file)); err != nil {
			t.Errorf("%s names %s, which doesn't exist", agentsMDName, file)
			continue
		}
		if anchor != "" && !hasHeadingAnchor(readRepoFile(t, file), anchor) {
			t.Errorf("%s links to %s, but %s has no heading #%s", agentsMDName, p, file, anchor)
		}
	}
}

func TestAgentsMDFlagsExist(t *testing.T) {
	allFlags := flagNames(newDocRoot())
	for _, m := range flagMentionRe.FindAllStringSubmatch(readRepoFile(t, agentsMDName), -1) {
		if !allFlags[m[2]] {
			t.Errorf("%s: no command has the flag --%s", agentsMDName, m[2])
		}
	}
}

// hasHeadingAnchor reports whether md has a heading whose GitHub anchor is
// anchor.
func hasHeadingAnchor(md, anchor string) bool {
	for _, line := range strings.Split(fenceRe.ReplaceAllString(md, ""), "\n") {
		if !strings.HasPrefix(line, "#") {
			continue
		}
		title := strings.ToLower(strings.TrimSpace(strings.TrimLeft(line, "#")))
		slug := strings.Map(func(r rune) rune {
			switch {
			case r == ' ':
				return '-'
			case r == '-' || r == '_' || r >= 'a' && r <= 'z' || r >= '0' && r <= '9':
				return r
			}
			return -1
		}, title)
		if slug == anchor {
			return true
		}
	}
	return false
}

func TestAgentsMDExitCodesMatchAgentsDoc(t *testing.T) {
	src := exitCodeTable(readRepoFile(t, agentsDoc))
	if len(src) < 5 {
		t.Fatalf("found only %d rows under %s's \"## Exit codes\" heading", len(src), agentsDoc)
	}
	if diffs := exitCodeTableDiffs(src, exitCodeTable(readRepoFile(t, agentsMDName))); len(diffs) > 0 {
		t.Errorf("%s's exit-code table is out of step with %s's.\n"+
			"%s is the source: copy the table under its \"## Exit codes\" heading into %s verbatim.\n"+
			"(If you meant to change %s, make the change in %s and copy it back.)\n%s",
			agentsMDName, agentsDoc, agentsDoc, agentsMDName, agentsMDName, agentsDoc, strings.Join(diffs, "\n"))
	}
}

func TestExitCodeTableDiffsCatchesDrift(t *testing.T) {
	src := exitCodeTable(readRepoFile(t, agentsDoc))
	if len(exitCodeTableDiffs(src, src)) != 0 {
		t.Fatal("a table differs from itself")
	}
	edited := append([]string(nil), src...)
	edited[3] += " more"
	for name, copied := range map[string][]string{
		"edited row":  edited,
		"missing row": src[:len(src)-1],
		"extra row":   append(append([]string(nil), src...), "| 6 | New | x |"),
		"reordered":   append([]string{src[3]}, append(append([]string(nil), src[:3]...), src[4:]...)...),
	} {
		if len(exitCodeTableDiffs(src, copied)) == 0 {
			t.Errorf("%s: no difference reported", name)
		}
	}
}

// exitCodeTable returns the rows, header included, of the first table
// under md's "## Exit codes" heading.
func exitCodeTable(md string) []string {
	_, after, ok := strings.Cut(md, "\n## Exit codes\n")
	if !ok {
		return nil
	}
	var rows []string
	for _, line := range strings.Split(after, "\n") {
		if strings.HasPrefix(line, "|") {
			rows = append(rows, strings.TrimRight(line, " "))
		} else if len(rows) > 0 || strings.HasPrefix(line, "#") {
			break
		}
	}
	return rows
}

// exitCodeTableDiffs describes, row by row, how copied differs from src.
func exitCodeTableDiffs(src, copied []string) []string {
	var diffs []string
	for i := 0; i < max(len(src), len(copied)); i++ {
		switch {
		case i >= len(copied):
			diffs = append(diffs, fmt.Sprintf("row %d is missing from %s:\n  %s: %s", i+1, agentsMDName, agentsDoc, src[i]))
		case i >= len(src):
			diffs = append(diffs, fmt.Sprintf("row %d is only in %s:\n  %s: %s", i+1, agentsMDName, agentsMDName, copied[i]))
		case src[i] != copied[i]:
			diffs = append(diffs, fmt.Sprintf("row %d differs:\n  %s: %s\n  %s: %s", i+1, agentsDoc, src[i], agentsMDName, copied[i]))
		}
	}
	return diffs
}
