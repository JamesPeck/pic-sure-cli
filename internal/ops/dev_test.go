package ops_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strconv"
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
	if err := x.st.SaveState(x.state); err != nil {
		t.Fatal(err)
	}
	x.saveConfig()
	tag := ops.DevTag("demo", localSHA, false)
	x.reactorFresh(tag, localSHA)
	// compose config fails, so render's changed files restart every
	// running service.
	x.f.On(fakerunner.Glob("docker compose * config *")).Exit(1)
	x.f.On(fakerunner.Glob("docker compose * restart *"))
	return x, tag
}

// running makes compose ps report services running, and compose up
// succeed.
func (x *buildFixture) running(services ...string) {
	x.f.On(fakerunner.Glob("docker compose * up *"))
	var ps strings.Builder
	for _, s := range services {
		ps.WriteString(`{"Service":"` + s + `","State":"running"}` + "\n")
	}
	x.f.On(fakerunner.Glob("docker compose * ps *")).Stdout(ps.String())
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
	x.running("picsure-db", "httpd", "gateway", "hpds")
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
	// The source rebuilt every reactor image, so the component's running
	// services go to compose up too, and nothing else.
	x.f.AssertCalled(fakerunner.Glob("docker compose * up -d --no-deps --wait --wait-timeout 900 gateway psama hpds"))
}

func TestDevOnAStoppedStackLeavesTheStartToUp(t *testing.T) {
	x, _ := devFixture(t)
	x.running()
	if err := x.dev("psama", true); err != nil {
		t.Fatal(err)
	}
	x.f.AssertNotCalled(fakerunner.Glob("docker compose * up *"))
	if c := x.compose(); !strings.Contains(c, "127.0.0.1:15000:5005") {
		t.Errorf("compose.yaml lacks psama's debug port:\n%s", c)
	}
}

// hmrFixture is devFixture with a frontend checkout whose .nvmrc holds
// nvmrc, and its node_modules volume already the stack's.
func hmrFixture(t *testing.T, nvmrc string) *buildFixture {
	t.Helper()
	x, _ := devFixture(t)
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, ".nvmrc"), []byte(nvmrc), 0o644); err != nil {
		t.Fatal(err)
	}
	x.cfg.Components.Frontend.Source = src
	x.saveConfig()
	vol, _ := json.Marshal([]map[string]any{{"Name": "demo_frontend-node-modules", "Labels": map[string]string{stack.LabelStack: "demo"}}})
	x.f.On(fakerunner.Glob("docker volume inspect demo_frontend-node-modules")).Stdout(string(vol))
	x.f.On(fakerunner.Glob("docker run * alpine:3.23 sh -c *"))
	return x
}

func TestDevOnHMRRunsNodeFromTheNvmrcWithoutABuild(t *testing.T) {
	x := hmrFixture(t, "24.19.0\n")
	x.running("httpd", "psama", "gateway")
	if err := x.dev("httpd-hmr", true); err != nil {
		t.Fatal(err)
	}
	state, err := x.st.LoadState()
	if err != nil {
		t.Fatal(err)
	}
	if state.Images["node"] != "24.19.0-alpine3.23" {
		t.Errorf("node image = %q", state.Images["node"])
	}
	c := x.compose()
	for _, want := range []string{"node:24.19.0-alpine3.23", `"VITE_ORIGIN": "http://127.0.0.1:3000"`, "127.0.0.1:15006:3000"} {
		if !strings.Contains(c, want) {
			t.Errorf("compose.yaml lacks %s:\n%s", want, c)
		}
	}
	if user := ops.HostUser(); user != "" {
		if !strings.Contains(c, "user: "+strconv.Quote(user)) {
			t.Errorf("compose.yaml doesn't run httpd as %s", user)
		}
		x.f.AssertCalled(fakerunner.Glob("docker run * -v demo_frontend-node-modules:/v alpine:3.23 sh -c * sh " + user))
	}
	if _, err := os.Stat(filepath.Join(x.cfg.Components.Frontend.Source, "node_modules")); ops.HostUser() != "" && err != nil {
		t.Errorf("node_modules mount point: %v", err)
	}
	x.f.AssertNotCalled(fakerunner.Glob("docker build *"))
	x.f.AssertNotCalled(fakerunner.Glob("docker buildx *"))
	// Nothing was built, so only httpd is recreated.
	x.f.AssertCalled(fakerunner.Glob("docker compose * up -d --no-deps --wait --wait-timeout 900 httpd"))
}

func TestNodeTag(t *testing.T) {
	for _, tc := range []struct{ nvmrc, want string }{
		{"24.19.0\n", "24.19.0-alpine3.23"},
		{" 22.1.10 ", "22.1.10-alpine3.23"},
		{"24\n", ""},
		{"lts/*", ""},
		{"v24.19.0", ""},
	} {
		x := hmrFixture(t, tc.nvmrc)
		got, err := ops.NodeTag(x.st.Dir, x.cfg)
		if got != tc.want || (tc.want == "") != (exitcode.FromError(err) == 3) {
			t.Errorf("%q: %q, %v", tc.nvmrc, got, err)
		}
	}
	x := hmrFixture(t, "24.19.0")
	x.cfg.Components.Frontend.Source = "fe"
	if _, err := ops.NodeTag(x.st.Dir, x.cfg); exitcode.FromError(err) != 3 || !strings.Contains(err.Error(), filepath.Join(x.st.Dir, "fe", ".nvmrc")) {
		t.Errorf("relative source without .nvmrc: %v", err)
	}
}

func TestUpWithHMRRefreshesTheNodeTag(t *testing.T) {
	x := hmrFixture(t, "24.19.0")
	x.cfg.Dev.Services = []string{"httpd-hmr"}
	x.state.Images["node"] = "22.0.0-alpine3.23"
	var ids []string
	for _, s := range ops.UpSteps(x.d, x.st, x.cfg, &stack.Secrets{}, x.state, ops.ConvergeOptions{}) {
		ids = append(ids, s.ID)
	}
	if !slices.Contains(ids, ops.NodeImageStepID) || ops.HostUser() != "" && !slices.Contains(ids, ops.HMRVolumeStepID) {
		t.Fatalf("up's steps: %v", ids)
	}
	if want := ops.UpStepIDs(x.cfg); !slices.Equal(ids, want) {
		t.Errorf("UpSteps %v, UpStepIDs %v", ids, want)
	}
	step := ops.NodeImageStep(x.st, x.cfg, x.state)
	if done, err := step.Check(context.Background()); done || err != nil {
		t.Fatalf("check = %v, %v", done, err)
	}
	if err := step.Apply(context.Background(), x.d.Sink); err != nil {
		t.Fatal(err)
	}
	if done, err := step.Check(context.Background()); !done || err != nil {
		t.Errorf("check after apply = %v, %v", done, err)
	}
	x.cfg.Dev.Services = nil
	for _, s := range ops.UpSteps(x.d, x.st, x.cfg, &stack.Secrets{}, x.state, ops.ConvergeOptions{}) {
		if s.ID == ops.NodeImageStepID || s.ID == ops.HMRVolumeStepID {
			t.Errorf("up without httpd-hmr runs %s", s.ID)
		}
	}
}

func TestDevOffKeepsTheSourceBuildWithoutTheDebugPort(t *testing.T) {
	x, tag := devFixture(t)
	x.running("psama", "gateway")
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

func TestDevOffSavesTheConfigLastSoAFailureCanBeRetried(t *testing.T) {
	x, _ := devFixture(t)
	x.cfg.Dev.Services = []string{"psama"}
	x.state.DevImages = map[string]string{"pic-sure-psama": "rel"}
	if err := x.st.SaveState(x.state); err != nil {
		t.Fatal(err)
	}
	x.saveConfig()
	x.f.On(fakerunner.Glob("docker compose * up *")).Exit(1)
	x.running("psama")
	var se *steps.Error
	if err := x.dev("psama", false); !errors.As(err, &se) || se.Step != ops.DevStartStepID {
		t.Fatalf("err = %v, want dev-start's", err)
	}
	saved, err := x.st.LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(saved.Dev.Services, []string{"psama"}) {
		t.Errorf("dev.services = %v after a failed dev off", saved.Dev.Services)
	}
}

func TestDevOnRefusals(t *testing.T) {
	cfg := stack.DefaultConfig()
	psama, _ := catalog.LookupDevVariant("psama")
	if err := ops.CheckDevOn("/stack", &cfg, psama); exitcode.FromError(err) != 3 || !strings.Contains(err.Error(), "config set components.pic-sure.source PATH") {
		t.Errorf("no source: %v", err)
	}
	cfg.Components.Frontend.Source = "/nonexistent/fe"
	hmr, _ := catalog.LookupDevVariant("httpd-hmr")
	if err := ops.CheckDevOn("/stack", &cfg, hmr); exitcode.FromError(err) != 3 || !strings.Contains(err.Error(), ".nvmrc") {
		t.Errorf("httpd-hmr without .nvmrc: %v", err)
	}
	cfg.Dev.Services = []string{"httpd-hmr"}
	httpd, _ := catalog.LookupDevVariant("httpd")
	if err := ops.CheckDevOn("/stack", &cfg, httpd); exitcode.FromError(err) != 3 || !strings.Contains(err.Error(), "dev off httpd-hmr") {
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
