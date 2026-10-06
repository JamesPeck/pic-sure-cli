package catalog

// Mode holds the config choices that decide which services and volumes a
// stack has. Render merges the matching fragments (§6.4).
type Mode struct {
	RemoteDB    bool // db.mode: remote
	SharedHPDS  bool // hpds.data: shared
	CustomTrust bool // trust.custom_certs_dir holds certs (§9.5)
}

// Condition says which stacks have a service or volume.
type Condition int

const (
	Always          Condition = iota
	LocalDBOnly               // db.mode: local
	LocalHPDSOnly             // hpds.data: local
	SharedHPDSOnly            // hpds.data: shared
	CustomTrustOnly           // the stack has custom CA certs
)

// In reports whether a stack in mode m meets the condition.
func (c Condition) In(m Mode) bool {
	switch c {
	case LocalDBOnly:
		return !m.RemoteDB
	case LocalHPDSOnly:
		return !m.SharedHPDS
	case SharedHPDSOnly:
		return m.SharedHPDS
	case CustomTrustOnly:
		return m.CustomTrust
	default:
		return true
	}
}

// Phase is when init and up bring a service up (§9.1 steps 8–12).
type Phase int

const (
	// PhaseDB services are the databases, started and probed first.
	PhaseDB Phase = iota + 1
	// PhaseMigrate services are the Flyway one-shots, run with
	// compose run --rm once the databases are up.
	PhaseMigrate
	// PhaseApp services are what compose up -d --wait starts last.
	PhaseApp
)

// A Network is one of a stack's compose networks.
type Network struct {
	Name     string
	Internal bool // no route off the host
}

// Networks returns the stack's networks.
func Networks() []Network {
	return []Network{
		{Name: "public"}, // only httpd joins it
		{Name: "app"},    // the app tier; psama reaches Auth0 through it
		{Name: "data", Internal: true},
		{Name: "query", Internal: true}, // isolates HPDS from the app tier
	}
}

// LookupNetwork returns the network with the given name.
func LookupNetwork(name string) (Network, bool) {
	return find(Networks(), func(n Network) bool { return n.Name == name })
}

// A Service is a compose service in the rendered stack.
type Service struct {
	Name     string
	Image    string   // name of its entry in Images
	Networks []string // none means network_mode: none
	// Volumes lists every volume the service may mount. Some exist only in
	// some modes; VolumesIn picks those a given stack has.
	Volumes []string
	Phase   Phase
	// OneShot services run to completion instead of staying up.
	OneShot bool
	// RestartAfterMigrate services cache what migrations and seeding
	// change, so they are restarted after them (§9.3 step 7).
	RestartAfterMigrate bool
	When                Condition // which stacks have it
}

// VolumesIn returns the volumes the service mounts in a stack in mode m.
func (s Service) VolumesIn(m Mode) []string {
	var out []string
	for _, name := range s.Volumes {
		if v, ok := LookupVolume(name); ok && v.When.In(m) {
			out = append(out, name)
		}
	}
	return out
}

// Services returns every service, by phase.
func Services() []Service {
	return []Service{
		{Name: "picsure-db", Image: "mysql", Networks: []string{"app"}, Volumes: []string{"picsure-db-data"}, Phase: PhaseDB, When: LocalDBOnly},
		{Name: "dictionary-db", Image: "postgres", Networks: []string{"data"}, Volumes: []string{"dictionary-db-data"}, Phase: PhaseDB},

		{Name: "flyway-init", Image: "flyway", Networks: []string{"app"}, Phase: PhaseMigrate, OneShot: true},
		{Name: "flyway-dictionary-init", Image: "flyway", Networks: []string{"data"}, Phase: PhaseMigrate, OneShot: true},

		{Name: "httpd", Image: "pic-sure-httpd", Networks: []string{"public", "app"}, Volumes: []string{"certs", "httpd-logs"}, Phase: PhaseApp},
		{Name: "gateway", Image: "pic-sure-gateway", Networks: []string{"app", "query"}, Phase: PhaseApp},
		{Name: "pic-sure-operations-service", Image: "pic-sure-operations-service", Networks: []string{"app"}, Phase: PhaseApp},
		{Name: "pic-sure-hpds-query-service", Image: "pic-sure-hpds-query-service", Networks: []string{"app", "query"}, Phase: PhaseApp},
		{Name: "psama", Image: "pic-sure-psama", Networks: []string{"app"}, Volumes: []string{"truststore", "psama-logs"}, Phase: PhaseApp, RestartAfterMigrate: true},
		// Copies the shared genomic store into hpds-genomic-copy before hpds
		// starts.
		{Name: "hpds-genomic-seed", Image: "alpine", Volumes: []string{"shared-hpds-genomic", "hpds-genomic-copy"}, Phase: PhaseApp, OneShot: true, When: SharedHPDSOnly},
		{Name: "hpds", Image: "pic-sure-hpds", Networks: []string{"query"}, Volumes: []string{
			"hpds-data", "hpds-genomic", "shared-hpds-data", "hpds-genomic-copy", "hpds-csv", "hpds-query-results", "hpds-logs",
		}, Phase: PhaseApp},
		{Name: "visualization", Image: "pic-sure-visualization", Networks: []string{"app"}, Volumes: []string{"visualization-logs"}, Phase: PhaseApp},
		{Name: "dictionary-api", Image: "pic-sure-dictionary-api", Networks: []string{"app", "data"}, Volumes: []string{"dictionary-logs"}, Phase: PhaseApp, RestartAfterMigrate: true},
		{Name: "dictionary-dump", Image: "pic-sure-dictionary-dump", Networks: []string{"data"}, Volumes: []string{"dictionary-logs"}, Phase: PhaseApp},
		// HPDS sends audit events to it over the query network.
		{Name: "pic-sure-logging", Image: "pic-sure-logging", Networks: []string{"app", "query"}, Volumes: []string{"logging-logs"}, Phase: PhaseApp},
	}
}

// ServicesIn returns the services a stack in mode m has, by phase.
func ServicesIn(m Mode) []Service {
	var out []Service
	for _, s := range Services() {
		if s.When.In(m) {
			out = append(out, s)
		}
	}
	return out
}

// LookupService returns the service with the given name.
func LookupService(name string) (Service, bool) {
	return find(Services(), func(s Service) bool { return s.Name == name })
}

// ServicesUsing returns the names of the services that run an image.
func ServicesUsing(image string) []string {
	var out []string
	for _, s := range Services() {
		if s.Image == image {
			out = append(out, s.Name)
		}
	}
	return out
}
