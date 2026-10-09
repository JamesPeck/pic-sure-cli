package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/JamesPeck/pic-sure-cli/internal/catalog"
	"github.com/JamesPeck/pic-sure-cli/internal/exitcode"
	"github.com/JamesPeck/pic-sure-cli/internal/stack"
)

func TestWriteDev(t *testing.T) {
	psama, _ := catalog.LookupDevVariant("psama")
	dict, _ := catalog.LookupDevVariant("dictionary")
	hmr, _ := catalog.LookupDevVariant("httpd-hmr")
	for _, c := range []struct {
		name string
		r    devReport
		v    catalog.DevVariant
		want string
	}{
		{"on with a debug port", devReport{Service: "psama", On: true, Services: []string{"psama"}, Port: 15000, Source: "/src/ps"}, psama,
			"dev psama is on: psama runs from /src/ps; attach a debugger (JDWP) to 127.0.0.1:15000.\n"},
		{"on without one", devReport{Service: "dictionary", On: true, Services: dict.Services, Source: "/src/ps"}, dict,
			"dev dictionary is on: dictionary-api, dictionary-dump run from /src/ps.\n"},
		{"on with a dev server", devReport{Service: "httpd-hmr", On: true, Services: []string{"httpd"}, Port: 15006, Source: "/src/fe"}, hmr,
			"dev httpd-hmr is on: httpd runs from /src/fe; browse to http://localhost:15006.\n"},
		{"off without a source build", devReport{Service: "httpd-hmr", Services: []string{"httpd"}, Source: "/src/fe"}, hmr,
			"dev httpd-hmr is off: httpd no longer runs the node dev server.\n"},
		{"off with the source built", devReport{Service: "psama", Services: []string{"psama"}, Source: "/src/ps", built: true}, psama,
			"dev psama is off: psama has no debug port.\n" +
				"psama still runs the build of components.pic-sure.source (/src/ps), which applies to the whole component.\n" +
				"To return to the release images: pic-sure config set components.pic-sure.source '' && pic-sure up\n"},
	} {
		var b strings.Builder
		if err := writeDev(&b, &c.r, c.v); err != nil {
			t.Fatal(err)
		}
		if b.String() != c.want {
			t.Errorf("%s:\n%s\nwant\n%s", c.name, b.String(), c.want)
		}
	}
}

// dev list is read-only (§10.6), so on a pic-sure.yaml in a schema this
// pic-sure can't decode it warns and reads the keys it needs as written.
func TestDevListOnANewerSchema(t *testing.T) {
	dir := t.TempDir()
	config := fmt.Sprintf("schema: %d\nname: demo\ndev: {services: [hpds]}\nnetwork: {dev_ports: {base: 16000}}\ncomponents: {pic-sure: {source: ../pic-sure}}\n", stack.ConfigSchema+1)
	if err := os.MkdirAll(filepath.Join(dir, ".pic-sure"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, stack.ConfigFile), []byte(config), 0o644); err != nil {
		t.Fatal(err)
	}
	a, stdout, stderr := testApp(t)
	if code := a.Run(context.Background(), []string{"dev", "list", "--stack", dir}); code != exitcode.CodeOK {
		t.Fatalf("exit = %d; stderr %q", code, stderr)
	}
	if !strings.Contains(stderr.String(), "pic-sure: warning: this pic-sure can't decode pic-sure.yaml schema") {
		t.Errorf("stderr = %q", stderr)
	}
	want := regexp.MustCompile(`(?m)^hpds +on +127\.0\.0\.1:16002 +pic-sure +\.\./pic-sure$`)
	if out := stdout.String(); !want.MatchString(out) || !regexp.MustCompile(`(?m)^gateway +off +127\.0\.0\.1:16003 `).MatchString(out) {
		t.Errorf("stdout:\n%s", out)
	}
}
