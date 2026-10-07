package ops_test

import (
	"context"
	"errors"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/JamesPeck/pic-sure-cli/internal/catalog"
	"github.com/JamesPeck/pic-sure-cli/internal/docker"
	"github.com/JamesPeck/pic-sure-cli/internal/docker/fakerunner"
	"github.com/JamesPeck/pic-sure-cli/internal/exitcode"
	"github.com/JamesPeck/pic-sure-cli/internal/ops"
	"github.com/JamesPeck/pic-sure-cli/internal/render"
	"github.com/JamesPeck/pic-sure-cli/internal/stack"
	"github.com/JamesPeck/pic-sure-cli/internal/steps"
)

// devFixture is an initialised stack built from release images, with a
// clean local pic-sure checkout configured and its reactor images built.
func devFixture(t *testing.T) (*buildFixture, string) {
	t.Helper()
	x := newBuildFixture(t)
	x.cfg.Components.PicSure.Source = x.checkout(false)
	x.state.Images = map[string]string{}
	for _, img := range catalog.Images() {
		if img.Component != "" {
			x.state.Images[img.Name] = "rel"
		}
	}
	x.state.Images["node"] = "22.0.0"
	if err := x.st.SaveState(x.state); err != nil {
		t.Fatal(err)
	}
	x.saveConfig()
	tag := ops.DevTag("demo", localSHA, false)
	x.reactorFresh(tag, localSHA)
	// No service is running, and compose config can't say what reads the
	// rendered files, so nothing is restarted.
	x.f.On(fakerunner.Glob("docker compose * ps *"))
	x.f.On(fakerunner.Glob("docker compose * config *")).Exit(1)
	x.f.On(fakerunner.Glob("docker compose * up *"))
	return x, tag
}

func (x *buildFixture) saveConfig() {
	x.cfg.Auth.Mode = stack.AuthOpen
	x.cfg.Auth.AdminEmail = "admin@example.com"
	doc, err := stack.NewConfigDoc(x.cfg)
	if err != nil {
		x.t.Fatal(err)
	}
	data, err := doc.Bytes()
	if err != nil {
		x.t.Fatal(err)
	}
	if err := x.st.WriteConfig(data); err != nil {
		x.t.Fatal(err)
	}
}

func (x *buildFixture) dev(name string, on bool) error {
	x.t.Helper()
	doc, err := x.st.ReadConfigDoc()
	if err != nil {
		x.t.Fatal(err)
	}
	cfg, err := doc.Config()
	if err != nil {
		x.t.Fatal(err)
	}
	x.cfg = cfg
	plan, err := ops.DevSteps(x.d, x.st, doc, cfg, x.state, ops.DevOptions{
		ConvergeOptions: ops.ConvergeOptions{
			Cache:   x.cache,
			Compose: func() (docker.Composer, error) { return docker.NewCompose(x.f, x.st.Dir, nil) },
		},
		Variant: name,
		On:      on,
	})
	if err != nil {
		return err
	}
	return steps.Run(context.Background(), x.d.Sink, plan, steps.Options{})
}

func (x *buildFixture) compose() string {
	data, err := os.ReadFile(x.st.Path(render.ComposeFile))
	if err != nil {
		x.t.Fatal(err)
	}
	return string(data)
}

func TestDevOnBuildsRendersAndRecreatesOnlyItsComponent(t *testing.T) {
	x, tag := devFixture(t)
	if err := x.dev("psama", true); err != nil {
		t.Fatal(err)
	}
	saved, err := x.st.LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(saved.Dev.Services, []string{"psama"}) {
		t.Errorf("dev.services = %v", saved.Dev.Services)
	}
	state, err := x.st.LoadState()
	if err != nil {
		t.Fatal(err)
	}
	if state.DevImages["pic-sure-psama"] != tag || state.Images["pic-sure-psama"] != tag {
		t.Errorf("images %v, dev images %v", state.Images, state.DevImages)
	}
	if c := x.compose(); !strings.Contains(c, "127.0.0.1:15000:5005") || !strings.Contains(c, "jdwp") {
		t.Errorf("compose.yaml lacks psama's debug port:\n%s", c)
	}
	// The source replaced every reactor image, so every service of the
	// component is recreated, and nothing else.
	x.f.AssertCalled(fakerunner.Glob("docker compose * up -d --no-deps --wait --wait-timeout 900 gateway * psama *pic-sure-logging"))
	x.f.AssertNotCalled(fakerunner.Glob("docker compose * up * httpd*"))
	x.f.AssertNotCalled(fakerunner.Glob("docker compose * up * picsure-db*"))
}

func TestDevOffKeepsTheSourceBuildWithoutTheDebugPort(t *testing.T) {
	x, tag := devFixture(t)
	if err := x.dev("psama", true); err != nil {
		t.Fatal(err)
	}
	if err := x.dev("psama", false); err != nil {
		t.Fatal(err)
	}
	saved, err := x.st.LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if len(saved.Dev.Services) != 0 {
		t.Errorf("dev.services = %v", saved.Dev.Services)
	}
	state, err := x.st.LoadState()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := state.DevImages["pic-sure-psama"]; ok || state.Images["pic-sure-psama"] != tag {
		t.Errorf("images %v, dev images %v", state.Images, state.DevImages)
	}
	c := x.compose()
	if strings.Contains(c, "15000") || strings.Contains(c, "jdwp") {
		t.Errorf("compose.yaml still has the debug port:\n%s", c)
	}
	if !strings.Contains(c, "hms-dbmi/pic-sure-psama:"+tag) {
		t.Errorf("psama doesn't run the source build:\n%s", c)
	}
	calls := x.f.Calls()
	last := calls[len(calls)-1].String()
	if !strings.HasSuffix(last, "up -d --no-deps --wait --wait-timeout 900 psama") {
		t.Errorf("last call %q, want psama alone recreated", last)
	}
}

func TestDevOnRefusals(t *testing.T) {
	cfg := stack.DefaultConfig()
	psama, _ := catalog.LookupDevVariant("psama")
	if err := ops.CheckDevOn(&cfg, psama); exitcode.FromError(err) != 3 || !strings.Contains(err.Error(), "config set components.pic-sure.source PATH") {
		t.Errorf("no source: %v", err)
	}
	cfg.Components.Frontend.Source = "/src/fe"
	cfg.Dev.Services = []string{"httpd-hmr"}
	httpd, _ := catalog.LookupDevVariant("httpd")
	if err := ops.CheckDevOn(&cfg, httpd); exitcode.FromError(err) != 3 || !strings.Contains(err.Error(), "dev off httpd-hmr") {
		t.Errorf("httpd beside httpd-hmr: %v", err)
	}
	if _, err := ops.LookupDev("nope"); exitcode.FromError(err) != 2 {
		t.Errorf("unknown variant: %v", err)
	}
}

func TestDevOnFailedBuildSavesNothing(t *testing.T) {
	x := newBuildFixture(t)
	x.cfg.Components.PicSure.Source = x.checkout(true)
	x.saveConfig()
	if err := x.st.SaveState(x.state); err != nil {
		t.Fatal(err)
	}
	x.f.On(fakerunner.Glob("docker *")).Exit(1).Stderr("boom\n")
	err := x.dev("psama", true)
	var se *steps.Error
	if !errors.As(err, &se) || se.Step != ops.ImagesStepID {
		t.Fatalf("err = %v, want the images step's", err)
	}
	saved, err := x.st.LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if len(saved.Dev.Services) != 0 {
		t.Errorf("dev.services = %v after a failed build", saved.Dev.Services)
	}
}

func TestDevList(t *testing.T) {
	cfg := stack.DefaultConfig()
	cfg.Network.DevPorts.Base = 15010
	cfg.Components.PicSure.Source = "../pic-sure"
	cfg.Dev.Services = []string{"hpds"}
	got := map[string]ops.DevVariantInfo{}
	for _, v := range ops.DevList(&cfg) {
		got[v.Name] = v
	}
	if len(got) != len(catalog.DevVariants()) {
		t.Errorf("%d variants, want %d", len(got), len(catalog.DevVariants()))
	}
	if h := got["hpds"]; !h.On || h.Port != 15012 || h.Source != "../pic-sure" {
		t.Errorf("hpds %+v", h)
	}
	if v := got["visualization"]; v.On || v.Port != 0 {
		t.Errorf("visualization %+v", v)
	}
	if h := got["httpd-hmr"]; h.Port != 15016 || h.Source != "" {
		t.Errorf("httpd-hmr %+v", h)
	}
}
