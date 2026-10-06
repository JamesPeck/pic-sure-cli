package catalog

import (
	"slices"
	"strings"
	"testing"
)

func names[T any](items []T, name func(T) string) []string {
	out := make([]string, len(items))
	for i, it := range items {
		out[i] = name(it)
	}
	return out
}

func assertUnique(t *testing.T, what string, values []string) {
	t.Helper()
	seen := map[string]bool{}
	for _, v := range values {
		if v == "" {
			t.Errorf("%s: empty value", what)
		}
		if seen[v] {
			t.Errorf("%s: %q appears twice", what, v)
		}
		seen[v] = true
	}
}

func TestNamesAreUnique(t *testing.T) {
	cs := Components()
	assertUnique(t, "component names", names(cs, func(c Component) string { return c.Name }))
	assertUnique(t, "component repos", names(cs, func(c Component) string { return c.Repo }))
	assertUnique(t, "build-spec keys", append(names(cs, func(c Component) string { return c.SpecKey }), CLISpecKey))
	assertUnique(t, "image names", names(Images(), func(i Image) string { return i.Name }))
	assertUnique(t, "service names", names(Services(), func(s Service) string { return s.Name }))
	assertUnique(t, "network names", names(Networks(), func(n Network) string { return n.Name }))
	assertUnique(t, "volume names", names(Volumes(), func(v Volume) string { return v.Name }))
	assertUnique(t, "dev variant names", names(DevVariants(), func(d DevVariant) string { return d.Name }))
}

func TestImagesAreWellFormed(t *testing.T) {
	for _, i := range Images() {
		if !i.Built() {
			if i.Ref == "" || i.Context != "" || i.Dockerfile != "" {
				t.Errorf("third-party image %s: want only a Ref, got %+v", i.Name, i)
			}
			continue
		}
		if _, ok := LookupComponent(i.Component); !ok {
			t.Errorf("image %s: unknown component %q", i.Name, i.Component)
		}
		if i.Ref != "" {
			t.Errorf("built image %s has a Ref", i.Name)
		}
		if i.Context == "" || i.Dockerfile == "" {
			t.Errorf("built image %s: missing context or Dockerfile", i.Name)
		}
		if i.Context != "." && !strings.HasPrefix(i.Dockerfile, i.Context+"/") {
			t.Errorf("image %s: Dockerfile %s is outside its context %s", i.Name, i.Dockerfile, i.Context)
		}
	}
	// §7.2 and the bash both build eleven images from the reactor.
	if n := len(ImagesBuiltFrom(PicSure)); n != 11 {
		t.Errorf("reactor images: got %d, want 11", n)
	}
}

func TestServicesReferToKnownEntries(t *testing.T) {
	services := Services()
	if !slices.IsSortedFunc(services, func(a, b Service) int { return int(a.Phase - b.Phase) }) {
		t.Error("Services() isn't ordered by phase")
	}
	for _, s := range services {
		if _, ok := LookupImage(s.Image); !ok {
			t.Errorf("service %s: unknown image %q", s.Name, s.Image)
		}
		for _, n := range s.Networks {
			if _, ok := LookupNetwork(n); !ok {
				t.Errorf("service %s: unknown network %q", s.Name, n)
			}
		}
		for _, v := range s.Volumes {
			if _, ok := LookupVolume(v); !ok {
				t.Errorf("service %s: unknown volume %q", s.Name, v)
			}
		}
		if s.Phase == PhaseMigrate && !s.OneShot {
			t.Errorf("service %s: migrations run with compose run --rm, so it must be one-shot", s.Name)
		}
	}
}

func TestDevVariantsReferToKnownEntries(t *testing.T) {
	ports := map[int]string{}
	for _, d := range DevVariants() {
		if _, ok := LookupComponent(d.Component); !ok {
			t.Errorf("dev variant %s: unknown component %q", d.Name, d.Component)
		}
		if len(d.Services) == 0 {
			t.Errorf("dev variant %s: no services", d.Name)
		}
		for _, name := range d.Services {
			s, ok := LookupService(name)
			if !ok {
				t.Errorf("dev variant %s: unknown service %q", d.Name, name)
				continue
			}
			if s.OneShot || s.Phase != PhaseApp {
				t.Errorf("dev variant %s: %s isn't a long-running app service", d.Name, name)
			}
			// Without its own image, a variant builds each service's image
			// from its component's source, so that must be where it comes from.
			if img, _ := LookupImage(s.Image); d.Image == "" && img.Component != d.Component {
				t.Errorf("dev variant %s: %s's image is built from %q, not %q", d.Name, name, img.Component, d.Component)
			}
		}
		if d.Image != "" {
			if _, ok := LookupImage(d.Image); !ok {
				t.Errorf("dev variant %s: unknown image %q", d.Name, d.Image)
			}
		}
		for _, n := range d.Networks {
			if _, ok := LookupNetwork(n); !ok {
				t.Errorf("dev variant %s: unknown network %q", d.Name, n)
			}
		}
		for _, v := range d.Volumes {
			if _, ok := LookupVolume(v); !ok {
				t.Errorf("dev variant %s: unknown volume %q", d.Name, v)
			}
		}
		if d.Port == NoPort {
			continue
		}
		if d.Port < 0 || d.Port >= DevPortSpan {
			t.Errorf("dev variant %s: port offset %d is outside 0..%d", d.Name, d.Port, DevPortSpan-1)
		}
		if other, dup := ports[d.Port]; dup {
			t.Errorf("dev variants %s and %s share port offset %d", other, d.Name, d.Port)
		}
		ports[d.Port] = d.Name
	}
}

func TestSpecKeysMapToComponents(t *testing.T) {
	for key, want := range map[string]string{
		"PSA": PicSure, "PSF": Frontend, "PSM": Migrations, "DICTIONARY_ETL": DictionaryETL,
	} {
		c, ok := ComponentBySpecKey(key)
		if !ok || c.Name != want {
			t.Errorf("ComponentBySpecKey(%q) = %q, %v; want %q", key, c.Name, ok, want)
		}
	}
	if c, ok := ComponentBySpecKey(CLISpecKey); ok {
		t.Errorf("%s maps to component %s; it names the CLI", CLISpecKey, c.Name)
	}
}

func TestComponentRepoNames(t *testing.T) {
	c, _ := LookupComponent(Frontend)
	if got := c.RepoName(); got != "PIC-SURE-Frontend" {
		t.Errorf("RepoName() = %q", got)
	}
	if got := c.CloneURL(); got != "https://github.com/hms-dbmi/PIC-SURE-Frontend.git" {
		t.Errorf("CloneURL() = %q", got)
	}
}

func TestVolumeDockerNames(t *testing.T) {
	for _, tc := range []struct{ volume, owner, want string }{
		{"hpds-data", "mystack", "mystack_hpds-data"},
		{"shared-hpds-data", "picsure-demo", "picsure-demo_hpds-data"},
		{"shared-hpds-genomic", "picsure-demo", "picsure-demo_hpds-genomic"},
		{"pic-sure-m2", "mystack", "pic-sure-m2"},
	} {
		v, ok := LookupVolume(tc.volume)
		if !ok {
			t.Fatalf("no volume %s", tc.volume)
		}
		if got := v.DockerName(tc.owner); got != tc.want {
			t.Errorf("%s.DockerName(%q) = %q, want %q", tc.volume, tc.owner, got, tc.want)
		}
	}
	for _, v := range Volumes() {
		if v.Scope == SharedData && !strings.HasPrefix(v.Name, "shared-") {
			t.Errorf("shared data volume %s: DockerName needs the shared- prefix", v.Name)
		}
	}
}

func TestModeSelectsServicesAndVolumes(t *testing.T) {
	serviceNames := func(m Mode) []string {
		return names(ServicesIn(m), func(s Service) string { return s.Name })
	}
	if got := serviceNames(Mode{}); !slices.Contains(got, "picsure-db") || slices.Contains(got, "hpds-genomic-seed") {
		t.Errorf("local stack services: %v", got)
	}
	if got := serviceNames(Mode{RemoteDB: true}); slices.Contains(got, "picsure-db") {
		t.Errorf("remote-db stack runs picsure-db: %v", got)
	}
	if got := serviceNames(Mode{SharedHPDS: true}); !slices.Contains(got, "hpds-genomic-seed") {
		t.Errorf("shared stack has no genomic seed: %v", got)
	}

	hpds, _ := LookupService("hpds")
	if got, want := hpds.VolumesIn(Mode{}), []string{"hpds-data", "hpds-genomic", "hpds-csv", "hpds-query-results", "hpds-logs"}; !slices.Equal(got, want) {
		t.Errorf("hpds volumes, local: %v, want %v", got, want)
	}
	if got, want := hpds.VolumesIn(Mode{SharedHPDS: true}), []string{"shared-hpds-data", "hpds-genomic-copy", "hpds-csv", "hpds-query-results", "hpds-logs"}; !slices.Equal(got, want) {
		t.Errorf("hpds volumes, shared: %v, want %v", got, want)
	}
	psama, _ := LookupService("psama")
	if got := psama.VolumesIn(Mode{}); slices.Contains(got, "truststore") {
		t.Errorf("psama mounts a truststore without custom certs: %v", got)
	}
	if got := psama.VolumesIn(Mode{CustomTrust: true}); !slices.Contains(got, "truststore") {
		t.Errorf("psama has no truststore with custom certs: %v", got)
	}
}

func TestRestartAfterMigrate(t *testing.T) {
	var got []string
	for _, s := range Services() {
		if s.RestartAfterMigrate {
			got = append(got, s.Name)
		}
	}
	if want := []string{"psama", "dictionary-api"}; !slices.Equal(got, want) {
		t.Errorf("restart-after-migrate services: %v, want %v", got, want)
	}
}

func TestServicesUsing(t *testing.T) {
	if got, want := ServicesUsing("flyway"), []string{"flyway-init", "flyway-dictionary-init"}; !slices.Equal(got, want) {
		t.Errorf("ServicesUsing(flyway) = %v, want %v", got, want)
	}
	if got := ServicesUsing("pic-sure-hpds-etl"); len(got) != 0 {
		t.Errorf("ServicesUsing(pic-sure-hpds-etl) = %v; the loaders run it, not a service", got)
	}
}

func TestTablesAreFreshCopies(t *testing.T) {
	s := Services()
	s[0].Name = "changed"
	s[0].Networks[0] = "changed"
	if again := Services(); again[0].Name == "changed" || again[0].Networks[0] == "changed" {
		t.Error("Services() shares its backing arrays between calls")
	}
}
