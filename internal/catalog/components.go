package catalog

import "strings"

// Component names: the keys under components: in pic-sure.yaml.
const (
	PicSure       = "pic-sure"
	Frontend      = "frontend"
	Migrations    = "migrations"
	DictionaryETL = "dictionary-etl"
)

// CLISpecKey is the build-spec key naming the CLI release that was validated
// with a build (D32). It isn't a component: the compatibility gate reads it
// (§8).
const CLISpecKey = "PSCLI"

// A Component is a source repo the stack is built from. Release-control's
// build-spec pins each one to a git ref (§8).
type Component struct {
	Name    string // key under components: in pic-sure.yaml
	Repo    string // GitHub owner/name
	SpecKey string // build-spec.json project_job_git_key
}

// RepoName is the repo's name without its owner. The host cache keys its
// bare clones and source trees by it: git/<RepoName>.git and
// src/<RepoName>/<sha> (§7.1).
func (c Component) RepoName() string {
	_, name, _ := strings.Cut(c.Repo, "/")
	return name
}

// CloneURL is the repo's HTTPS clone URL.
func (c Component) CloneURL() string {
	return "https://github.com/" + c.Repo + ".git"
}

// Components returns the components in the order the bash lists them.
func Components() []Component {
	return []Component{
		{Name: PicSure, Repo: "hms-dbmi/pic-sure", SpecKey: "PSA"},
		{Name: Frontend, Repo: "hms-dbmi/PIC-SURE-Frontend", SpecKey: "PSF"},
		{Name: Migrations, Repo: "hms-dbmi/PIC-SURE-Migrations", SpecKey: "PSM"},
		// The bash ignores this key and builds the checkout it finds; v2 honours
		// it (D31).
		{Name: DictionaryETL, Repo: "hms-dbmi/picsure-dictionary-etl", SpecKey: "DICTIONARY_ETL"},
	}
}

// LookupComponent returns the component with the given name.
func LookupComponent(name string) (Component, bool) {
	return find(Components(), func(c Component) bool { return c.Name == name })
}

// ComponentBySpecKey returns the component a build-spec key pins.
func ComponentBySpecKey(key string) (Component, bool) {
	return find(Components(), func(c Component) bool { return c.SpecKey == key })
}

func find[T any](items []T, match func(T) bool) (T, bool) {
	for _, it := range items {
		if match(it) {
			return it, true
		}
	}
	var zero T
	return zero, false
}
