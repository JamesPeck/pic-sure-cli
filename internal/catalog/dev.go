package catalog

// Dev ports are a block of DevPortSpan ports per stack starting at
// network.dev_ports.base (§6.5): debug ports at offsets 0–5, the HMR port
// at offset 6.
const DevPortSpan = 7

// NoPort marks a dev variant that publishes no port.
const NoPort = -1

// A DevVariant is what `dev on NAME` switches on: services run from a local
// checkout instead of the release images (§7.3). A variant names every
// service it replaces, so turning one on never leaves half of a component on
// the release image.
type DevVariant struct {
	Name string // what dev on/off and dev.services take
	// Component is the component whose local source
	// (components.<c>.source) the variant needs.
	Component string
	// Services are the compose services it replaces. Each one's image is
	// built from the local source, with the build context and Dockerfile
	// of its Images entry.
	Services []string
	// Port is the offset from network.dev_ports.base of the port the
	// variant publishes on 127.0.0.1: JDWP for a Java service, the Vite
	// server for httpd-hmr. NoPort if it publishes none.
	Port int
	// Image, when set, is the image the variant runs instead of building
	// its services' own images.
	Image string
	// Networks are networks the variant joins beyond its services' own.
	// An internal network can't publish ports, so hpds joins app for its
	// debug port.
	Networks []string
	// Volumes are volumes the variant mounts beyond its services' own.
	Volumes []string
}

// DevVariants returns the dev variants. httpd and httpd-hmr both replace
// httpd, so a stack can have at most one of them on.
func DevVariants() []DevVariant {
	return []DevVariant{
		// Ports keep the bash's numbering, offset from 5005, which left 5006
		// (offset 1) unused.
		{Name: "psama", Component: PicSure, Services: []string{"psama"}, Port: 0},
		{Name: "hpds", Component: PicSure, Services: []string{"hpds"}, Port: 2, Networks: []string{"app"}},
		{Name: "gateway", Component: PicSure, Services: []string{"gateway"}, Port: 3},
		{Name: "operations", Component: PicSure, Services: []string{"pic-sure-operations-service"}, Port: 4},
		{Name: "query", Component: PicSure, Services: []string{"pic-sure-hpds-query-service"}, Port: 5},
		{Name: "visualization", Component: PicSure, Services: []string{"visualization"}, Port: NoPort},
		// The bash's dictionary overlay rebuilt only dictionary-api (§13).
		{Name: "dictionary", Component: PicSure, Services: []string{"dictionary-api", "dictionary-dump"}, Port: NoPort},
		{Name: "httpd", Component: Frontend, Services: []string{"httpd"}, Port: NoPort},
		{Name: "httpd-hmr", Component: Frontend, Services: []string{"httpd"}, Port: 6, Image: "node", Volumes: []string{"frontend-node-modules"}},
	}
}

// LookupDevVariant returns the dev variant with the given name.
func LookupDevVariant(name string) (DevVariant, bool) {
	return find(DevVariants(), func(d DevVariant) bool { return d.Name == name })
}
