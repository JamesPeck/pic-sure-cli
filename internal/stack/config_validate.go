package stack

import (
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"net"
	"net/mail"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"

	"github.com/JamesPeck/pic-sure-cli/internal/netproxy"
	"github.com/JamesPeck/pic-sure-cli/internal/pki"
)

// Problem is one thing wrong with a config, at a key path.
type Problem struct {
	Path string // dotted key path such as network.http_port; empty for the whole file
	Line int    // line in pic-sure.yaml, or 0 when unknown
	Msg  string
}

func (p Problem) String() string {
	switch {
	case p.Path == "" && p.Line == 0:
		return p.Msg
	case p.Path == "":
		return fmt.Sprintf("line %d: %s", p.Line, p.Msg)
	case p.Line == 0:
		return p.Path + ": " + p.Msg
	default:
		return fmt.Sprintf("%s (line %d): %s", p.Path, p.Line, p.Msg)
	}
}

// ConfigError is an invalid config: every problem found, in key order.
type ConfigError struct {
	Problems []Problem
}

func (e *ConfigError) Error() string {
	if len(e.Problems) == 1 {
		return "invalid " + ConfigFile + ": " + e.Problems[0].String()
	}
	var b strings.Builder
	b.WriteString("invalid " + ConfigFile + ":")
	for _, p := range e.Problems {
		b.WriteString("\n  " + p.String())
	}
	return b.String()
}

var (
	// Compose project names, also used for service names.
	nameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)
	// Docker volume names, which shared data set names become part of.
	sharedNameRE  = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]*$`)
	hostLabelRE   = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9-]*[A-Za-z0-9])?$`)
	envNameRE     = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	projectNameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*$`)
)

// Validate checks c without touching the filesystem and returns a
// *ConfigError listing every problem, or nil.
func (c *Config) Validate() error {
	v := &validator{c: c}

	if c.Schema != ConfigSchema {
		v.add("schema", "must be %d, got %d", ConfigSchema, c.Schema)
	}
	v.required()
	v.enums()
	v.noControlChars(reflect.ValueOf(c).Elem(), "")

	if c.Name != "" && !nameRE.MatchString(c.Name) {
		v.add("name", "must be lowercase letters, digits, - and _, starting with a letter or digit; got %q", c.Name)
	}
	if v.hostname("network.hostname", c.Network.Hostname) && c.Network.Hostname != "" {
		// The TLS step must be able to put it in a certificate.
		if err := pki.CheckHostname(c.Network.Hostname); err != nil {
			v.add("network.hostname", "%v", err)
		}
	}
	v.ports()

	if c.Auth.AdminEmail != "" {
		if a, err := mail.ParseAddress(c.Auth.AdminEmail); err != nil || a.Address != c.Auth.AdminEmail {
			v.add("auth.admin_email", "want an email address like admin@example.org, got %q", c.Auth.AdminEmail)
		}
	}
	v.hostname("auth.auth0.tenant", c.Auth.Auth0.Tenant)

	if c.DB.Mode == DBRemote {
		v.hostname("db.remote.host", c.DB.Remote.Host)
		v.port("db.remote.port", c.DB.Remote.Port)
	}
	if c.HPDS.Data == HPDSShared && c.HPDS.SharedName != "" && !sharedNameRE.MatchString(c.HPDS.SharedName) {
		v.add("hpds.shared_name", "must be letters, digits, '.', - and _, starting with a letter or digit; got %q", c.HPDS.SharedName)
	}

	// git takes these as arguments.
	v.notOption("release.repo", c.Release.Repo)
	v.notOption("release.branch", c.Release.Branch)
	v.notOption("components.pic-sure.ref", c.Components.PicSure.Ref)
	v.notOption("components.frontend.ref", c.Components.Frontend.Ref)
	v.notOption("components.migrations.ref", c.Components.Migrations.Ref)
	v.notOption("components.dictionary-etl.ref", c.Components.DictionaryETL.Ref)
	if p := c.Components.Migrations.Project; p != "" && (!projectNameRE.MatchString(p) || p == "..") {
		v.add("components.migrations.project", "must be one directory name, got %q", p)
	}

	v.serviceNames()
	if _, err := netproxy.ParseURL(c.Proxy.HTTP); err != nil {
		v.add("proxy.http", "%v", err)
	}
	if _, err := netproxy.ParseURL(c.Proxy.HTTPS); err != nil {
		v.add("proxy.https", "%v", err)
	}
	if _, err := netproxy.ParseNoProxy(c.Proxy.NoProxy); err != nil {
		v.add("proxy.no_proxy", "%v", err)
	}

	return v.err()
}

// CheckFiles checks that the files and directories c names exist. Relative
// paths are relative to the stack directory dir. It returns a *ConfigError
// or nil.
func (c *Config) CheckFiles(dir string) error {
	v := &validator{c: c}
	if c.TLS.Mode == TLSProvided {
		v.exists("tls.cert_file", dir, c.TLS.CertFile, false)
		v.exists("tls.key_file", dir, c.TLS.KeyFile, false)
		v.exists("tls.chain_file", dir, c.TLS.ChainFile, false)
	}
	v.exists("components.pic-sure.source", dir, c.Components.PicSure.Source, true)
	v.exists("components.frontend.source", dir, c.Components.Frontend.Source, true)
	v.exists("components.migrations.source", dir, c.Components.Migrations.Source, true)
	v.exists("components.dictionary-etl.source", dir, c.Components.DictionaryETL.Source, true)
	return v.err()
}

type validator struct {
	c        *Config
	problems []Problem
}

func (v *validator) add(path, format string, args ...any) {
	v.problems = append(v.problems, Problem{Path: path, Msg: fmt.Sprintf(format, args...)})
}

func (v *validator) err() error {
	if len(v.problems) == 0 {
		return nil
	}
	slices.SortStableFunc(v.problems, func(a, b Problem) int { return strings.Compare(a.Path, b.Path) })
	return &ConfigError{Problems: v.problems}
}

// required checks the Fields whose RequiredWhen holds.
func (v *validator) required() {
	for _, f := range Fields {
		if f.Secret || !f.Required(v.c) {
			continue
		}
		val, err := v.c.Get(f.Key)
		if err != nil || !reflect.ValueOf(val).IsZero() {
			continue
		}
		if f.RequiredWhen.Desc == always.Desc {
			v.add(f.Key, "required")
		} else {
			v.add(f.Key, "required when %s", f.RequiredWhen.Desc)
		}
	}
}

// enums checks the Fields that have Options.
func (v *validator) enums() {
	for _, f := range Fields {
		if len(f.Options) == 0 {
			continue
		}
		val, err := v.c.Get(f.Key)
		if err != nil {
			continue
		}
		s := reflect.ValueOf(val).String()
		if !slices.Contains(f.Options, s) {
			v.add(f.Key, "must be one of %s; got %q", strings.Join(f.Options, ", "), s)
		}
	}
}

// noControlChars rejects control characters in any string, since values
// end up in environment variables, SQL and file names.
func (v *validator) noControlChars(val reflect.Value, path string) {
	switch val.Kind() {
	case reflect.String:
		if strings.ContainsFunc(val.String(), func(r rune) bool { return r < 0x20 || r == 0x7f }) {
			v.add(path, "must not contain control characters such as newlines")
		}
	case reflect.Struct:
		t := val.Type()
		for i := range t.NumField() {
			key, inline := yamlKey(t.Field(i))
			if inline {
				v.noControlChars(val.Field(i), path)
			} else {
				v.noControlChars(val.Field(i), joinKey(path, key))
			}
		}
	case reflect.Slice:
		for i := range val.Len() {
			v.noControlChars(val.Index(i), path)
		}
	case reflect.Map:
		iter := val.MapRange()
		for iter.Next() {
			v.noControlChars(iter.Value(), joinKey(path, iter.Key().String()))
		}
	}
}

func (v *validator) port(path string, p int) {
	if p < 1 || p > 65535 {
		v.add(path, "must be a port from 1 to 65535, got %d", p)
	}
}

func (v *validator) ports() {
	n := v.c.Network
	v.port("network.http_port", n.HTTPPort)
	v.port("network.https_port", n.HTTPSPort)
	if n.HTTPPort == n.HTTPSPort && n.HTTPPort >= 1 && n.HTTPPort <= 65535 {
		v.add("network.https_port", "must differ from network.http_port (both are %d)", n.HTTPPort)
	}
	base, top := n.DevPorts.Base, n.DevPorts.Base+DevPortCount-1
	if base < 1 || top > 65535 {
		v.add("network.dev_ports.base", "must be from 1 to %d, so the %d dev ports fit; got %d", 65535-DevPortCount+1, DevPortCount, base)
		return
	}
	for _, p := range []struct {
		key  string
		port int
	}{{"network.http_port", n.HTTPPort}, {"network.https_port", n.HTTPSPort}} {
		if p.port >= base && p.port <= top {
			v.add("network.dev_ports.base", "the dev ports %d-%d include %s (%d)", base, top, p.key, p.port)
		}
	}
}

// hostname checks that a non-empty s is a DNS name or an IP address, and
// reports whether it is (or is empty).
func (v *validator) hostname(path, s string) bool {
	if s == "" || net.ParseIP(s) != nil {
		return true
	}
	ok := len(s) <= 253
	for label := range strings.SplitSeq(s, ".") {
		ok = ok && hostLabelRE.MatchString(label) && len(label) <= 63
	}
	if !ok {
		v.add(path, "want a host name or IP address, got %q", s)
	}
	return ok
}

func (v *validator) notOption(path, s string) {
	if strings.HasPrefix(s, "-") {
		v.add(path, "must not start with -, got %q", s)
	}
}

func (v *validator) serviceNames() {
	seen := map[string]bool{}
	for _, s := range v.c.Dev.Services {
		switch {
		case !nameRE.MatchString(s):
			v.add("dev.services", "want service names, got %q", s)
		case seen[s]:
			v.add("dev.services", "%q is listed twice", s)
		}
		seen[s] = true
	}
	for _, name := range slices.Sorted(maps.Keys(v.c.Services)) {
		if !nameRE.MatchString(name) {
			v.add("services."+name, "%q is not a service name", name)
		}
		for _, env := range slices.Sorted(maps.Keys(v.c.Services[name].Env)) {
			if !envNameRE.MatchString(env) {
				v.add("services."+name+".env."+env, "%q is not an environment variable name", env)
			}
		}
	}
}

func (v *validator) exists(path, dir, p string, wantDir bool) {
	if p == "" {
		return
	}
	if !filepath.IsAbs(p) {
		p = filepath.Join(dir, p)
	}
	fi, err := os.Stat(p)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		v.add(path, "%s does not exist", p)
	case err != nil:
		v.add(path, "%v", err)
	case wantDir && !fi.IsDir():
		v.add(path, "%s is not a directory", p)
	case !wantDir && !fi.Mode().IsRegular():
		v.add(path, "%s is not a file", p)
	}
}
