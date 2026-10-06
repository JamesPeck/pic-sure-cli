package fakerunner

import (
	"regexp"
	"slices"
	"strings"

	"github.com/JamesPeck/pic-sure-cli/internal/docker"
)

// Matcher selects calls by argv. Argv includes the program name, e.g.
// ["docker", "volume", "ls"].
type Matcher interface {
	Match(argv []string) bool
	String() string
}

// Exact matches argv exactly, element by element.
func Exact(argv ...string) Matcher { return exact(argv) }

type exact []string

func (m exact) Match(argv []string) bool { return slices.Equal(argv, m) }
func (m exact) String() string           { return docker.FormatArgv(m) }

// Glob matches the whole argv joined with single spaces against pattern, in
// which * matches any run of characters (spaces included) and ? matches any
// one character. Everything else is literal; there is no escape for * or ?.
// For example, Glob("docker compose * up -d --wait *") matches an
// `up -d --wait` of one or more services, whatever the -f files before it.
func Glob(pattern string) Matcher {
	var b strings.Builder
	b.WriteString(`(?s)^`)
	for _, r := range pattern {
		switch r {
		case '*':
			b.WriteString(`.*`)
		case '?':
			b.WriteString(`.`)
		default:
			b.WriteString(regexp.QuoteMeta(string(r)))
		}
	}
	b.WriteString(`$`)
	return &joined{source: pattern, re: regexp.MustCompile(b.String())}
}

// Regex matches the whole argv joined with single spaces against expr,
// which is unanchored unless it says otherwise. It panics if expr does not
// compile.
func Regex(expr string) Matcher {
	return &joined{source: "re:" + expr, re: regexp.MustCompile(expr)}
}

type joined struct {
	source string
	re     *regexp.Regexp
}

func (m *joined) Match(argv []string) bool { return m.re.MatchString(strings.Join(argv, " ")) }
func (m *joined) String() string           { return m.source }
