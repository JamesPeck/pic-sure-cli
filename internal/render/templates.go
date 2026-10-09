package render

import (
	"bytes"
	"embed"
	"fmt"
	"io/fs"
	"maps"
	"path"
	"slices"
	"strconv"
	"strings"
	"text/template"

	"go.yaml.in/yaml/v3"

	"github.com/JamesPeck/pic-sure-cli/internal/catalog"
)

// The embedded stack definition (D5), described in templates/README.md:
// compose fragments that render merges into the one compose.yaml, and the
// files it writes under render/files/.
//
//go:embed templates/compose templates/files
var embedded embed.FS

// templateFS returns the template tree, with paths relative to templates/.
func templateFS() fs.FS {
	sub, err := fs.Sub(embedded, "templates")
	if err != nil {
		panic(err) // the directory is embedded above
	}
	return sub
}

const (
	fragmentBase       = "compose/base.yaml.tmpl"
	fragmentLocalDB    = "compose/local-db.yaml.tmpl"
	fragmentLocalHPDS  = "compose/local-hpds.yaml.tmpl"
	fragmentSharedHPDS = "compose/shared-hpds.yaml.tmpl"
	fragmentTruststore = "compose/truststore.yaml.tmpl"
	fragmentServiceEnv = "compose/service-env.yaml.tmpl"
)

// devFragment is the fragment for the dev variant named variant.
func devFragment(variant string) string {
	return "compose/dev/" + variant + ".yaml.tmpl"
}

// composeFragments returns the fragments a stack's compose file merges, in
// merge order: base, one fragment per catalog Condition the stack meets, its
// dev variants in the order given, then the per-service environment
// overrides.
func composeFragments(m catalog.Mode, dev []string) []string {
	out := []string{fragmentBase}
	if catalog.LocalDBOnly.In(m) {
		out = append(out, fragmentLocalDB)
	}
	if catalog.SharedHPDSOnly.In(m) {
		out = append(out, fragmentSharedHPDS)
	} else {
		out = append(out, fragmentLocalHPDS)
	}
	if catalog.CustomTrustOnly.In(m) {
		out = append(out, fragmentTruststore)
	}
	for _, v := range dev {
		out = append(out, devFragment(v))
	}
	return append(out, fragmentServiceEnv)
}

// Options render adds to a service's JAVA_OPTS through templateData.JVMExtra.
const (
	// trustJavaOpts points psama at the truststore volume, which the
	// truststore fragment mounts at /truststore (§9.5).
	trustJavaOpts = "-Djavax.net.ssl.trustStore=/truststore/cacerts -Djavax.net.ssl.trustStorePassword=changeit"
	// debugJavaOpts is the JDWP agent of a dev variant. The dev fragments
	// publish its container port, 5005.
	debugJavaOpts = "-agentlib:jdwp=transport=dt_socket,server=y,suspend=n,address=*:5005"
)

// secretVars are the variables the compose fragments reference as ${NAME}.
// Each holds a value from secrets.yaml (§6.3), so the compose file never
// contains one; the Compose adapter sets them all on every call. Compose
// substitutes an empty string for an unset one.
var secretVars = []string{
	"DB_ROOT_PASSWORD",
	"DB_PICSURE_PASSWORD",
	"DB_AUTH_PASSWORD",
	"DB_AIRFLOW_PASSWORD",
	"DB_DICTIONARY_PASSWORD",
	"AUTH0_CLIENT_SECRET",
	"PICSURE_INTROSPECTION_TOKEN",
	"QUERY_SERVICE_INTERNAL_TOKEN",
	"PICSURE_APPLICATION_TOKEN",
	"LOGGING_API_KEY",
	"AGGREGATE_OBFUSCATION_SALT",
	"PICSURE_APPLICATION_ID",
	"PICSURE_RESOURCE_ID",
	"PICSURE_VIZ_RESOURCE_ID",
	"EMAIL_PASSWORD",
}

// proxyVars are the proxy variables the fragments reference as ${NAME} when
// templateData.Proxy is set. A proxy URL can carry credentials, so they stay
// out of the compose file like the secrets.
var proxyVars = []string{"HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY"}

// templateData is what every template is executed with. Render builds it
// from the stack's config, state and the catalog. It holds no secrets: see
// secretVars.
type templateData struct {
	Name string // the stack's name, which is its compose project name
	// Labels are the stack labels (§6.1) put on every service, volume and
	// network.
	Labels map[string]string
	// ExistingVolumeLabels is Input.ExistingVolumeLabels.
	ExistingVolumeLabels map[string]map[string]string
	// FilesDir is the absolute directory render writes files/ to
	// (.pic-sure/render/files). The compose file bind-mounts from it.
	FilesDir  string
	HTTPPort  int
	HTTPSPort int

	// Images maps catalog image names to the references the stack runs.
	Images map[string]string
	// DevImages maps the image of every service in an enabled dev variant
	// to its locally built reference (§7.3).
	DevImages map[string]string
	// DevPorts maps each enabled dev variant that publishes a port to its
	// host port: network.dev_ports.base plus the variant's catalog offset.
	DevPorts map[string]int

	// Absolute source trees, bind-mounted read-only: a host cache tree
	// (src/<repo>/<sha>) or the component's local source.
	PicSureSrc        string
	MigrationsSrc     string
	MigrationsProject string // components.migrations.project, e.g. Baseline
	FrontendSrc       string // used by httpd-hmr only
	HostUser          string // httpd-hmr's "UID:GID", or "" for the image's

	DB          dbTarget
	Auth        authSettings
	DocsEnabled bool   // frontend.docs_enabled
	EmailUser   string // email.user
	HPDSProfile string // the effective HPDS Spring profile, "" for none
	SharedHPDS  string // the shared data set's name, with hpds.data: shared

	// JavaOpts maps a service to its JAVA_OPTS from config (hpds.java_opts,
	// services.<name>.java_opts). A service without one gets its template's
	// default, which is AIO's.
	JavaOpts map[string]string
	// JVMExtra maps a service to options appended to its JAVA_OPTS: the
	// proxy properties (§9.10), trustJavaOpts for psama with custom certs,
	// and debugJavaOpts for a service in an enabled dev variant.
	JVMExtra map[string]string

	// Proxy says the stack has an outbound proxy. The services that call out
	// then get proxyVars in their environment.
	Proxy bool
	// FrontendEnv is the VITE_* environment of the httpd-hmr dev server.
	FrontendEnv map[string]string
	// ServiceEnv holds the services.<name>.env overrides, by service. Every
	// key must be a service the stack has: an override for any other would
	// add a service with no image.
	ServiceEnv map[string]map[string]string
}

// dbTarget is the MySQL that flyway-init, psama and the operations service
// connect to: picsure-db:3306 as root for db.mode: local, else db.remote.
type dbTarget struct {
	Host     string
	Port     int
	RootUser string
}

// authSettings are the auth switches the services read: the flags derived
// from auth.mode, plus auth.consent_authorization and auth.tos.
type authSettings struct {
	Auth0Tenant          string
	OpenIDP              bool // OPEN_IDP_PROVIDER_IS_ENABLED
	GatewayOpenAccess    bool // GATEWAY_OPEN_ACCESS_ENABLED
	PublicAccess         bool // ENABLE_PUBLIC_ACCESS
	ConsentAuthorization bool
	TOS                  bool
}

// Image returns the reference for the catalog image name.
func (d templateData) Image(name string) (string, error) {
	return lookup(d.Images, name, "image")
}

// DevImage returns the locally built reference for the catalog image name.
func (d templateData) DevImage(name string) (string, error) {
	return lookup(d.DevImages, name, "dev image")
}

// DevPort returns the host port of the dev variant.
func (d templateData) DevPort(variant string) (int, error) {
	return lookup(d.DevPorts, variant, "dev port for")
}

// Java returns the service's JAVA_OPTS: its configured options, or def when
// it has none, followed by its JVMExtra.
func (d templateData) Java(service, def string) string {
	opts := def
	if o := d.JavaOpts[service]; o != "" {
		opts = o
	}
	if x := d.JVMExtra[service]; x != "" {
		opts += " " + x
	}
	return opts
}

func lookup[V any](m map[string]V, key, what string) (V, error) {
	v, ok := m[key]
	if !ok {
		return v, fmt.Errorf("no %s %s", what, key)
	}
	return v, nil
}

var templateFuncs = template.FuncMap{
	"q":    quote,
	"flow": flow,
	// path joins host path elements with "/" (macOS and Linux only, D4).
	"path": path.Join,
}

// VolumeLabels returns the labels for the volume key: the ones its existing
// volume already has, if any, else Labels.
func (d templateData) VolumeLabels(key string) map[string]string {
	if l, ok := d.ExistingVolumeLabels[key]; ok {
		return l
	}
	return d.Labels
}

// quote renders v as a double-quoted YAML scalar that compose won't
// interpolate: each $ is doubled.
func quote(v any) string {
	return strconv.Quote(strings.ReplaceAll(fmt.Sprint(v), "$", "$$"))
}

// flow renders m as a one-line YAML mapping with sorted, quoted entries.
func flow(m map[string]string) string {
	var b strings.Builder
	b.WriteByte('{')
	for i, k := range slices.Sorted(maps.Keys(m)) {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(quote(k) + ": " + quote(m[k]))
	}
	b.WriteByte('}')
	return b.String()
}

// parseTemplate parses the template at name in the template tree. A missing
// map key is an execution error.
func parseTemplate(name string) (*template.Template, error) {
	src, err := fs.ReadFile(templateFS(), name)
	if err != nil {
		return nil, err
	}
	return template.New(name).Funcs(templateFuncs).Option("missingkey=error").Parse(string(src))
}

// executeTemplate parses and executes the template at name.
func executeTemplate(name string, d templateData) ([]byte, error) {
	t, err := parseTemplate(name)
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	if err := t.Execute(&buf, d); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// renderCompose executes the fragments and merges them, in order, into one
// compose file.
//
// Merging works on the YAML tree. A later fragment's mappings are merged into
// the earlier ones key by key, recursively. Any other value, a scalar or a
// sequence, replaces the earlier one whole, so a fragment that changes a list
// (a service's volumes, say) restates all of it. A fragment can't remove a
// key, which is why whatever only some stacks have lives in its own fragment
// and never in base (§6.4: nothing depends on compose's !reset), and a
// mapping can't be replaced by anything else, which would delete it.
//
// A replaced value takes the later fragment's comment, if any; a merged
// mapping keeps its comment unless the later fragment has one. A later
// fragment's header comment, separated from its first key by a blank line, is
// dropped.
func renderCompose(d templateData, fragments []string) ([]byte, error) {
	var out *yaml.Node
	for _, name := range fragments {
		src, err := executeTemplate(name, d)
		if err != nil {
			return nil, err
		}
		var doc yaml.Node
		if err := yaml.Unmarshal(src, &doc); err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		if len(doc.Content) == 0 {
			continue // only comments
		}
		root := doc.Content[0]
		if root.Kind != yaml.MappingNode {
			return nil, fmt.Errorf("%s: not a mapping", name)
		}
		if out == nil {
			out = &doc
			continue
		}
		if err := mergeMapping(out.Content[0], root, ""); err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
	}
	if out == nil {
		return nil, fmt.Errorf("no compose fragments")
	}
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(out); err != nil {
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// mergeMapping merges the mapping src into the mapping dst, as described on
// renderCompose. path is dst's key path, for errors.
func mergeMapping(dst, src *yaml.Node, path string) error {
	for i := 0; i+1 < len(src.Content); i += 2 {
		key, val := src.Content[i], src.Content[i+1]
		j := keyIndex(dst, key.Value)
		switch {
		case j < 0:
			dst.Content = append(dst.Content, key, val)
		case dst.Content[j+1].Kind == yaml.MappingNode:
			if val.Kind != yaml.MappingNode {
				return fmt.Errorf("%s%s: a mapping can only be merged with a mapping", path, key.Value)
			}
			if key.HeadComment != "" {
				dst.Content[j].HeadComment = key.HeadComment
			}
			if err := mergeMapping(dst.Content[j+1], val, path+key.Value+"."); err != nil {
				return err
			}
		default:
			// The key node carries the comments, which describe the new value.
			dst.Content[j], dst.Content[j+1] = key, val
		}
	}
	return nil
}

// keyIndex returns the index in the mapping m of the key named key, or -1.
func keyIndex(m *yaml.Node, key string) int {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return i
		}
	}
	return -1
}

// DemoFacetConfig returns the facet categories `data demo` loads through
// dictionary-etl's /api/facet/loader/load, AIO's
// demo-data/facet_loader_configuration.json.
func DemoFacetConfig() ([]byte, error) {
	return fs.ReadFile(templateFS(), "files/dictionary/facet_loader_configuration.json")
}
