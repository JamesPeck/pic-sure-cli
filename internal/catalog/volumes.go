package catalog

import "strings"

// Scope says who owns a volume.
type Scope int

const (
	// StackScoped volumes belong to one stack. Compose creates them as
	// <project>_<name> with the stack labels, and only that stack's
	// teardown removes them.
	StackScoped Scope = iota
	// SharedData volumes hold a published HPDS data set (§9.7). They are
	// external to every stack: shared-data publish creates them as
	// <set>_<name without "shared-">, and stack teardown never touches them.
	SharedData
	// HostScoped volumes are external and shared by every stack on the host,
	// under a fixed name. Only cache prune removes them.
	HostScoped
)

// Kind says what sort of thing a volume holds, which decides what reset
// clears (§9.8).
type Kind int

const (
	KindDatabase Kind = iota + 1 // the MySQL auth and picsure schemas
	KindData                     // HPDS and dictionary data, staging, query results
	KindTLS                      // TLS material the CLI generates and copies in
	KindLogs                     // service logs
	KindCache                    // rebuildable caches
)

// A Volume is a named Docker volume.
type Volume struct {
	Name  string // logical name: the key under the compose file's volumes:
	Scope Scope
	Kind  Kind
	When  Condition // which stacks have it
	Holds string    // what it holds, for humans
}

// DockerName is the volume's name in Docker. owner is the stack's compose
// project name for a stack volume and the data set's name for a shared data
// volume; host volumes ignore it.
func (v Volume) DockerName(owner string) string {
	switch v.Scope {
	case SharedData:
		return owner + "_" + strings.TrimPrefix(v.Name, "shared-")
	case HostScoped:
		return v.Name
	default:
		return owner + "_" + v.Name
	}
}

// Volumes returns every volume.
func Volumes() []Volume {
	return []Volume{
		{Name: "picsure-db-data", Kind: KindDatabase, When: LocalDBOnly, Holds: "MySQL data: the auth and picsure schemas"},
		{Name: "dictionary-db-data", Kind: KindData, Holds: "Postgres data: the dictionary schema"},
		{Name: "hpds-data", Kind: KindData, When: LocalHPDSOnly, Holds: "the HPDS phenotype store and its encryption key"},
		{Name: "hpds-genomic", Kind: KindData, When: LocalHPDSOnly, Holds: "the HPDS genomic store"},
		{Name: "hpds-csv", Kind: KindData, Holds: "HPDS CSV exports"},
		{Name: "hpds-query-results", Kind: KindData, Holds: "query result files for GIC file sharing"},
		{Name: "genomic-staging", Kind: KindData, Holds: "VCFs and loader output staged by data load-genomic"},
		{Name: "shared-hpds-data", Scope: SharedData, Kind: KindData, When: SharedHPDSOnly, Holds: "a published phenotype store, mounted read-only"},
		{Name: "shared-hpds-genomic", Scope: SharedData, Kind: KindData, When: SharedHPDSOnly, Holds: "a published genomic store, the source of each stack's copy"},
		{Name: "hpds-genomic-copy", Kind: KindData, When: SharedHPDSOnly, Holds: "this stack's copy of the shared genomic store, where HPDS writes its indexes"},
		{Name: "certs", Kind: KindTLS, Holds: "httpd's server key, cert and chain, owned 2:2"},
		{Name: "truststore", Kind: KindTLS, When: CustomTrustOnly, Holds: "psama's cacerts plus the operator's CA certs"},
		{Name: "httpd-logs", Kind: KindLogs, Holds: "httpd logs"},
		{Name: "psama-logs", Kind: KindLogs, Holds: "psama logs"},
		{Name: "hpds-logs", Kind: KindLogs, Holds: "HPDS logs"},
		{Name: "visualization-logs", Kind: KindLogs, Holds: "visualization logs"},
		{Name: "dictionary-logs", Kind: KindLogs, Holds: "dictionary-api and dictionary-dump logs"},
		{Name: "logging-logs", Kind: KindLogs, Holds: "the audit event log"},
		{Name: "frontend-node-modules", Kind: KindCache, Holds: "node_modules for the httpd-hmr dev variant"},
		{Name: "pic-sure-m2", Scope: HostScoped, Kind: KindCache, Holds: "the Maven repository every reactor build uses (§7.1)"},
	}
}

// LookupVolume returns the volume with the given logical name.
func LookupVolume(name string) (Volume, bool) {
	return find(Volumes(), func(v Volume) bool { return v.Name == name })
}
