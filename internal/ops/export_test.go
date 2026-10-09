package ops

// MigrationsCheck lets the plan tests replace MigrationsUpToDate.
var MigrationsCheck = &migrationsCheck

// DockerProbeTimeout and BundleLogsTimeout let tests shorten doctor's and
// support-bundle's Docker timeouts.
var (
	DockerProbeTimeout = &dockerProbeTimeout
	BundleLogsTimeout  = &bundleLogsTimeout
)
