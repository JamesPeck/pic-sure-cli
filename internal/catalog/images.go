package catalog

import "strings"

// Namespace is the repository namespace built images are tagged under:
// hms-dbmi/<image>:<tag>. Pull mode puts a registry in front of it (D33).
const Namespace = "hms-dbmi"

// An Image is a container image that a stack runs or that a build or helper
// container uses.
type Image struct {
	// Name is the image's short name. A built image's repository is
	// hms-dbmi/<Name>; the build picks its tag (§7.2).
	Name string
	// Component is the component whose source the image is built from:
	// pic-sure for the Maven reactor's images, otherwise frontend or
	// dictionary-etl. It is empty for a third-party image, which is pulled.
	Component string
	// Context and Dockerfile locate a built image's docker build. Both are
	// relative to the reactor's build directory for pic-sure images, and to
	// the component's source tree for the others. The Dockerfile is always
	// inside the context, so copying the context out of the reactor
	// container is enough to build it.
	Context    string
	Dockerfile string
	// Ref is a third-party image's reference, pinned to a tag, except
	// node's: httpd-hmr takes its tag from the frontend's .nvmrc.
	Ref string
}

// Built reports whether the CLI builds the image from source.
func (i Image) Built() bool { return i.Component != "" }

// Repository is the image's repository: hms-dbmi/<Name> for a built image,
// and Ref without its tag for a third-party one.
func (i Image) Repository() string {
	if !i.Built() {
		repo, _, _ := strings.Cut(i.Ref, ":")
		return repo
	}
	return Namespace + "/" + i.Name
}

// Images returns every image. The pic-sure images come first, in the order
// the bash builds them.
func Images() []Image {
	return []Image{
		// The reactor images: AIO's MONOREPO_IMAGES, which aio_test.go checks
		// this list against.
		reactor("pic-sure-gateway", "services/pic-sure-gateway", "services/pic-sure-gateway/Dockerfile"),
		reactor("pic-sure-operations-service", "services/pic-sure-operations-service", "services/pic-sure-operations-service/Dockerfile"),
		reactor("pic-sure-hpds-query-service", "services/pic-sure-hpds-query-service", "services/pic-sure-hpds-query-service/Dockerfile"),
		reactor("pic-sure-psama", "services/pic-sure-auth-microapp", "services/pic-sure-auth-microapp/pic-sure-auth-services/Dockerfile"),
		reactor("pic-sure-hpds", "services/pic-sure-hpds", "services/pic-sure-hpds/docker/pic-sure-hpds/Dockerfile"),
		// The phenotype and genomic loaders (§9.6) run it; no service does.
		reactor("pic-sure-hpds-etl", "services/pic-sure-hpds", "services/pic-sure-hpds/docker/pic-sure-hpds-etl/Dockerfile"),
		reactor("pic-sure-visualization", "services/pic-sure-visualization-service", "services/pic-sure-visualization-service/Dockerfile"),
		reactor("pic-sure-logging", "services/pic-sure-logging", "services/pic-sure-logging/Dockerfile"),
		reactor("pic-sure-dictionary-api", "services/picsure-dictionary", "services/picsure-dictionary/Dockerfile"),
		reactor("pic-sure-dictionary-dump", "services/picsure-dictionary/aggregate", "services/picsure-dictionary/aggregate/Dockerfile"),
		// The dictionary weights step runs it; no service does.
		reactor("dictionary-weights", "services/picsure-dictionary/dictionaryweights", "services/picsure-dictionary/dictionaryweights/Dockerfile"),

		{Name: "pic-sure-httpd", Component: Frontend, Context: ".", Dockerfile: "Dockerfile"},
		// Dictionary hydrate and data loads run it; no service does.
		{Name: "dictionary-etl", Component: DictionaryETL, Context: ".", Dockerfile: "Dockerfile"},

		{Name: "mysql", Ref: "mysql:8.0"},
		{Name: "postgres", Ref: "postgres:16-alpine"},
		{Name: "flyway", Ref: "flyway/flyway:10"},
		// Helper containers that work on volumes.
		{Name: "alpine", Ref: "alpine:3.23"},
		// The reactor build container (§7.2 step 2).
		{Name: "maven", Ref: "maven:3-amazoncorretto-25"},
		// httpd-hmr's Vite server, tagged <.nvmrc version>-alpine3.23.
		{Name: "node", Ref: "node"},
	}
}

func reactor(name, context, dockerfile string) Image {
	return Image{Name: name, Component: PicSure, Context: context, Dockerfile: dockerfile}
}

// LookupImage returns the image with the given name.
func LookupImage(name string) (Image, bool) {
	return find(Images(), func(i Image) bool { return i.Name == name })
}

// ImagesBuiltFrom returns the images built from a component's source, in
// build order. For pic-sure these are the reactor images.
func ImagesBuiltFrom(component string) []Image {
	var out []Image
	for _, i := range Images() {
		if i.Component == component {
			out = append(out, i)
		}
	}
	return out
}
