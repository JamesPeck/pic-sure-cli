package catalog

import (
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// The tests in this file compare the catalog with the bash AIO's files, as a
// drift signal. They need an AIO checkout: PICSURE_AIO_DIR, or else
// pic-sure-all-in-one beside this repo, as in the v2 workspace. Without one
// they skip.

func aioDir(t *testing.T) string {
	t.Helper()
	dir, set := os.LookupEnv("PICSURE_AIO_DIR")
	if !set {
		dir = filepath.Join("..", "..", "..", "pic-sure-all-in-one")
	}
	if _, err := os.Stat(filepath.Join(dir, "build-images.sh")); err != nil {
		if set {
			t.Fatalf("PICSURE_AIO_DIR: %v", err)
		}
		t.Skipf("no AIO checkout at %s; set PICSURE_AIO_DIR to compare the catalog with it", dir)
	}
	return dir
}

func TestReactorImagesMatchAIO(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(aioDir(t), "build-images.sh"))
	if err != nil {
		t.Fatal(err)
	}
	_, rest, ok := strings.Cut(string(data), "\nMONOREPO_IMAGES=(\n")
	body, _, ok2 := strings.Cut(rest, "\n)\n")
	if !ok || !ok2 {
		t.Fatal("build-images.sh: no MONOREPO_IMAGES=( ... ) block")
	}
	var aio []Image
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		entry, err := strconv.Unquote(line)
		fields := strings.Split(entry, "|")
		if err != nil || len(fields) != 3 {
			t.Fatalf("build-images.sh: unexpected MONOREPO_IMAGES entry %s", line)
		}
		aio = append(aio, Image{Name: fields[0], Component: PicSure, Context: fields[1], Dockerfile: fields[2]})
	}
	if got := ImagesBuiltFrom(PicSure); !slices.Equal(got, aio) {
		t.Errorf("reactor images differ from AIO's MONOREPO_IMAGES\n got: %+v\nwant: %+v", got, aio)
	}
}

func TestServicesMatchAIO(t *testing.T) {
	dir := aioDir(t)
	base := parseComposeServices(t, filepath.Join(dir, "docker-compose.yml"))
	shared := parseComposeServices(t, filepath.Join(dir, "docker-compose.shared-hpds.yml"))

	for name, aio := range base {
		s, ok := LookupService(name)
		if !ok {
			t.Errorf("AIO service %s isn't in the catalog", name)
			continue
		}
		img, _ := LookupImage(s.Image)
		if repo, _, built := strings.Cut(aio.image, ":${"); built {
			if repo != img.Repository() {
				t.Errorf("service %s: AIO image %s, catalog %s", name, aio.image, img.Repository())
			}
		} else if aio.image != img.Ref {
			t.Errorf("service %s: AIO image %s, catalog %s", name, aio.image, img.Ref)
		}
		if !sameSet(aio.networks, s.Networks) {
			t.Errorf("service %s: AIO networks %v, catalog %v", name, aio.networks, s.Networks)
		}
	}
	for _, s := range Services() {
		if base[s.Name] == nil && shared[s.Name] == nil {
			t.Errorf("catalog service %s is in neither AIO's base compose file nor its shared-hpds overlay", s.Name)
		}
	}
}

func TestDevVariantsMatchAIO(t *testing.T) {
	dir := aioDir(t)
	base := parseComposeServices(t, filepath.Join(dir, "docker-compose.yml"))
	overlays, err := filepath.Glob(filepath.Join(dir, "docker-compose.dev-*.yml"))
	if err != nil {
		t.Fatal(err)
	}
	var aioNames []string
	for _, path := range overlays {
		name := strings.TrimSuffix(strings.TrimPrefix(filepath.Base(path), "docker-compose.dev-"), ".yml")
		aioNames = append(aioNames, name)
		d, ok := LookupDevVariant(name)
		if !ok {
			continue
		}
		services := parseComposeServices(t, path)
		var svcNames []string
		for svc, aio := range services {
			svcNames = append(svcNames, svc)
			var extra []string
			for _, n := range aio.networks {
				if !slices.Contains(base[svc].networks, n) {
					extra = append(extra, n)
				}
			}
			if !sameSet(extra, d.Networks) {
				t.Errorf("dev variant %s: AIO adds networks %v, catalog %v", name, extra, d.Networks)
			}
			// httpd-hmr's port is Vite's 3000, not on the debug port numbering.
			if name == "httpd-hmr" {
				continue
			}
			want := NoPort
			for _, p := range aio.ports {
				if host, ok := strings.CutPrefix(p, "127.0.0.1:"); ok {
					port, _, _ := strings.Cut(host, ":")
					n, err := strconv.Atoi(port)
					if err != nil {
						t.Fatalf("dev overlay %s: port %q", name, p)
					}
					want = n - 5005
				}
			}
			if d.Port != want {
				t.Errorf("dev variant %s: port offset %d, AIO's port gives %d", name, d.Port, want)
			}
		}
		if !sameSet(svcNames, d.Services) {
			t.Errorf("dev variant %s: AIO overlay has services %v, catalog %v", name, svcNames, d.Services)
		}
	}
	if got := names(DevVariants(), func(d DevVariant) string { return d.Name }); !sameSet(got, aioNames) {
		t.Errorf("dev variants %v, AIO dev overlays %v", got, aioNames)
	}
}

type composeService struct {
	image    string
	networks []string
	ports    []string
}

// parseComposeServices reads the services in a compose file. It understands
// only the layout the AIO files use: two-space indents, block lists and
// trailing comments.
func parseComposeServices(t *testing.T, path string) map[string]*composeService {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	services := map[string]*composeService{}
	var section, key string
	var cur *composeService
	for _, line := range strings.Split(string(data), "\n") {
		if i := strings.Index(line, " #"); i >= 0 {
			line = line[:i]
		}
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		indent := len(line) - len(strings.TrimLeft(line, " "))
		switch {
		case indent == 0:
			section, _, _ = strings.Cut(trimmed, ":")
		case section != "services":
		case indent == 2:
			cur = &composeService{}
			services[strings.TrimSuffix(trimmed, ":")] = cur
		case indent == 4:
			var value string
			key, value, _ = strings.Cut(trimmed, ":")
			if key == "image" {
				cur.image = strings.TrimSpace(value)
			}
		case indent == 6 && strings.HasPrefix(trimmed, "- "):
			item := strings.TrimSpace(strings.TrimPrefix(trimmed, "- "))
			if unquoted, err := strconv.Unquote(item); err == nil {
				item = unquoted
			}
			switch key {
			case "networks":
				cur.networks = append(cur.networks, item)
			case "ports":
				cur.ports = append(cur.ports, item)
			}
		}
	}
	if len(services) == 0 {
		t.Fatalf("%s: no services", path)
	}
	return services
}

func sameSet(a, b []string) bool {
	a, b = slices.Clone(a), slices.Clone(b)
	slices.Sort(a)
	slices.Sort(b)
	return slices.Equal(a, b)
}
