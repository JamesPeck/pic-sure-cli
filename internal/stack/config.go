package stack

// ConfigFile is the name of the config file in the stack directory.
const ConfigFile = "pic-sure.yaml"

// ConfigSchema is the pic-sure.yaml schema this CLI reads and writes.
const ConfigSchema = 1

// Config is pic-sure.yaml, schema 1 (spec §6.2). It is the one definition of
// a stack's configuration: validation, defaults, `pic-sure config`, the
// field table (and through it the wizard, init's flags and the docs) all
// derive from it. Secrets are not part of it; they live in secrets.yaml.
//
// The yaml tags are the file's keys, and the json tags mirror them for
// `config show --json`.
type Config struct {
	Schema     int                        `yaml:"schema" json:"schema"`
	Name       string                     `yaml:"name" json:"name"`
	Network    Network                    `yaml:"network" json:"network"`
	TLS        TLS                        `yaml:"tls" json:"tls"`
	Auth       Auth                       `yaml:"auth" json:"auth"`
	DB         DB                         `yaml:"db" json:"db"`
	HPDS       HPDS                       `yaml:"hpds" json:"hpds"`
	Frontend   Frontend                   `yaml:"frontend" json:"frontend"`
	Release    Release                    `yaml:"release" json:"release"`
	Components Components                 `yaml:"components" json:"components"`
	Images     Images                     `yaml:"images" json:"images"`
	Dev        Dev                        `yaml:"dev" json:"dev"`
	Proxy      Proxy                      `yaml:"proxy" json:"proxy"`
	Services   map[string]ServiceOverride `yaml:"services" json:"services"`
	Trust      Trust                      `yaml:"trust" json:"trust"`
	Email      Email                      `yaml:"email" json:"email"`
}

// Network is where the stack listens on the host.
type Network struct {
	Hostname  string   `yaml:"hostname" json:"hostname"`
	HTTPPort  int      `yaml:"http_port" json:"http_port"`
	HTTPSPort int      `yaml:"https_port" json:"https_port"`
	DevPorts  DevPorts `yaml:"dev_ports" json:"dev_ports"`
}

// DevPorts is the block of host ports dev overlays use: debug ports
// Base..Base+5 and HMR on Base+6.
type DevPorts struct {
	Base int `yaml:"base" json:"base"`
}

// DevPortCount is the size of the dev port block starting at DevPorts.Base.
const DevPortCount = 7

// TLSMode says where the server certificate comes from.
type TLSMode string

// TLS modes.
const (
	TLSGenerated TLSMode = "generated" // a self-signed leaf cert the CLI makes
	TLSProvided  TLSMode = "provided"  // the operator's files
)

// TLS is the HTTPS server certificate. The file paths are used only in
// provided mode, and a relative path is relative to the stack directory.
type TLS struct {
	Mode      TLSMode `yaml:"mode" json:"mode"`
	CertFile  string  `yaml:"cert_file" json:"cert_file"`
	KeyFile   string  `yaml:"key_file" json:"key_file"`
	ChainFile string  `yaml:"chain_file" json:"chain_file"`
}

// AuthMode is who can use PIC-SURE without logging in.
type AuthMode string

// Auth modes. See AuthFlags for what each one turns on.
const (
	AuthRequired AuthMode = "required" // no access without login
	AuthOpen     AuthMode = "open"     // Discover without login; no export or API
	AuthExplore  AuthMode = "explore"  // the query builder without login; export needs login
)

// Auth is login and access control. The Auth0 client secret lives in
// secrets.yaml.
type Auth struct {
	Mode                 AuthMode `yaml:"mode" json:"mode"`
	Auth0                Auth0    `yaml:"auth0" json:"auth0"`
	AdminEmail           string   `yaml:"admin_email" json:"admin_email"`
	TOS                  bool     `yaml:"tos" json:"tos"`
	ConsentAuthorization bool     `yaml:"consent_authorization" json:"consent_authorization"`
}

// Auth0 identifies the Auth0 application users log in through.
type Auth0 struct {
	Tenant   string `yaml:"tenant" json:"tenant"`
	ClientID string `yaml:"client_id" json:"client_id"`
}

// DBMode says whether the stack runs its own MySQL.
type DBMode string

// DB modes.
const (
	DBLocal  DBMode = "local"  // the bundled picsure-db container
	DBRemote DBMode = "remote" // an external MySQL such as RDS
)

// DB is the MySQL database. The remote root password lives in secrets.yaml.
type DB struct {
	Mode   DBMode   `yaml:"mode" json:"mode"`
	Remote RemoteDB `yaml:"remote" json:"remote"`
}

// RemoteDB is the external MySQL used when DB.Mode is remote.
type RemoteDB struct {
	Host     string `yaml:"host" json:"host"`
	Port     int    `yaml:"port" json:"port"`
	RootUser string `yaml:"root_user" json:"root_user"`
}

// HPDSData says where HPDS reads its data from.
type HPDSData string

// HPDS data modes.
const (
	HPDSLocal  HPDSData = "local"  // this stack's own data volumes
	HPDSShared HPDSData = "shared" // a published shared data set, read-only
)

// HPDS is the query engine.
type HPDS struct {
	Data       HPDSData `yaml:"data" json:"data"`
	SharedName string   `yaml:"shared_name" json:"shared_name"`
	// Profile is the Spring profile; empty means the shared data set's
	// recorded profile, or none for local data.
	Profile  string `yaml:"profile" json:"profile"`
	JavaOpts string `yaml:"java_opts" json:"java_opts"`
}

// Frontend is the web UI. Its values are baked into the frontend image at
// build time.
type Frontend struct {
	Theme       string    `yaml:"theme" json:"theme"`
	DocsEnabled bool      `yaml:"docs_enabled" json:"docs_enabled"`
	Analytics   Analytics `yaml:"analytics" json:"analytics"`
}

// Themes are the frontend themes (bash scripts/lib/config.sh).
var Themes = []string{"picsure", "bdc", "aim-ahead", "local"}

// Analytics holds the optional analytics IDs baked into the frontend.
type Analytics struct {
	GoogleAnalyticsID  string `yaml:"google_analytics_id,omitempty" json:"google_analytics_id,omitempty"`
	GoogleTagManagerID string `yaml:"google_tag_manager_id,omitempty" json:"google_tag_manager_id,omitempty"`
}

// CLICompat says what a CLI version mismatch with the release's build-spec
// does (D12).
type CLICompat string

// CLI compatibility settings.
const (
	CompatWarn   CLICompat = "warn"   // warn about an older CLI and carry on
	CompatStrict CLICompat = "strict" // refuse instead
)

// Release is the release-control repo whose build-spec pins the component
// versions.
type Release struct {
	Repo      string    `yaml:"repo" json:"repo"`
	Branch    string    `yaml:"branch" json:"branch"`
	CLICompat CLICompat `yaml:"cli_compat" json:"cli_compat"`
}

// Components are the source repos the images are built from, keyed in the
// file by component name.
type Components struct {
	PicSure       Component           `yaml:"pic-sure" json:"pic-sure"`
	Frontend      Component           `yaml:"frontend" json:"frontend"`
	Migrations    MigrationsComponent `yaml:"migrations" json:"migrations"`
	DictionaryETL Component           `yaml:"dictionary-etl" json:"dictionary-etl"`
}

// Component pins one source repo. An empty Ref means the release's
// build-spec decides; a non-empty Source is a local checkout that replaces
// the cached source tree (§7.3).
type Component struct {
	Ref    string `yaml:"ref" json:"ref"`
	Source string `yaml:"source" json:"source"`
}

// MigrationsComponent is the migrations repo, plus the project directory in
// it whose migrations run.
type MigrationsComponent struct {
	Component `yaml:",inline"`
	Project   string `yaml:"project" json:"project"`
}

// ImagesMode says how the stack gets its images.
type ImagesMode string

// Image modes.
const (
	ImagesBuild ImagesMode = "build" // build from source (the default, D6)
	ImagesPull  ImagesMode = "pull"  // pull published images (future, §7.4)
)

// Images is how the stack gets its images.
type Images struct {
	Mode ImagesMode `yaml:"mode" json:"mode"`
	// Registry is used in pull mode only; empty means the D33 default.
	Registry string `yaml:"registry" json:"registry"`
}

// Dev lists the services running from dev overlays.
type Dev struct {
	Services []string `yaml:"services" json:"services"`
}

// Proxy is the outbound HTTP proxy (§9.10). Empty fields mean no proxy.
type Proxy struct {
	HTTP  string `yaml:"http" json:"http"`
	HTTPS string `yaml:"https" json:"https"`
	// NoProxy is the user's comma-separated entries. The CLI always adds
	// the stack's service names, localhost and 127.0.0.1.
	NoProxy string `yaml:"no_proxy" json:"no_proxy"`
}

// ServiceOverride is an advanced per-service override, keyed in
// Config.Services by compose service name.
type ServiceOverride struct {
	JavaOpts string            `yaml:"java_opts,omitempty" json:"java_opts,omitempty"`
	Env      map[string]string `yaml:"env,omitempty" json:"env,omitempty"`
}

// Trust is where operator CA certs come from (§9.5).
type Trust struct {
	CustomCertsDir string `yaml:"custom_certs_dir" json:"custom_certs_dir"`
}

// Email is the outbound mail account. Its password lives in secrets.yaml.
type Email struct {
	User string `yaml:"user" json:"user"`
}

// DefaultConfig returns schema 1's defaults (spec §6.2). Name, the Auth0
// client ID and the admin email have no default.
func DefaultConfig() Config {
	return Config{
		Schema: ConfigSchema,
		Network: Network{
			Hostname:  "localhost",
			HTTPPort:  80,
			HTTPSPort: 443,
			DevPorts:  DevPorts{Base: 15000},
		},
		TLS: TLS{
			Mode:     TLSGenerated,
			CertFile: "certs/tls/server.crt",
			KeyFile:  "certs/tls/server.key",
		},
		Auth: Auth{
			Mode:  AuthRequired,
			Auth0: Auth0{Tenant: "avillachlab"},
		},
		DB: DB{
			Mode:   DBLocal,
			Remote: RemoteDB{Port: 3306, RootUser: "root"},
		},
		HPDS: HPDS{
			Data:     HPDSLocal,
			JavaOpts: "-XX:+UseParallelGC -XX:SurvivorRatio=250 -Xms1g -Xmx16g",
		},
		Frontend: Frontend{Theme: "picsure", DocsEnabled: true},
		Release: Release{
			Repo:      "https://github.com/hms-dbmi/pic-sure-baseline-release-control",
			Branch:    "main",
			CLICompat: CompatWarn,
		},
		Components: Components{
			Migrations: MigrationsComponent{Project: "Baseline"},
		},
		Images:   Images{Mode: ImagesBuild},
		Dev:      Dev{Services: []string{}},
		Services: map[string]ServiceOverride{},
		Trust:    Trust{CustomCertsDir: "certs/trust"},
	}
}
