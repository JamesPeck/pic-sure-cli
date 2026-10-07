package docker_test

import (
	"reflect"
	"testing"

	"github.com/JamesPeck/pic-sure-cli/internal/docker"
)

// psLines is compose ≥ 2.21 output: one object per line, with fields the
// parser doesn't model and nulls where compose has no value.
const psLines = `{"Command":"\"/entrypoint.sh\"","CreatedAt":"2026-10-06 10:00:00 -0400 EDT","ExitCode":0,"Health":"healthy","ID":"a1","Image":"hms-dbmi/pic-sure-hpds:abc123","Labels":"com.docker.compose.project=demo","LocalVolumes":"2","Mounts":"demo_hpds-data","Name":"demo-hpds-1","Names":"demo-hpds-1","Networks":"demo_data","Ports":"","Project":"demo","Publishers":null,"RunningFor":"2 minutes ago","Service":"hpds","Size":"0B","State":"running","Status":"Up 2 minutes (healthy)"}
{"ExitCode":null,"Health":null,"ID":"b2","Image":"httpd:2.4","Name":"demo-httpd-1","Project":"demo","Publishers":[{"URL":"0.0.0.0","TargetPort":443,"PublishedPort":8443,"Protocol":"tcp"}],"Service":"httpd","State":"running","Status":"Up 2 minutes"}
{"ExitCode":137,"Health":"unhealthy","ID":"c3","Image":"mysql:8.0","Name":"demo-picsure-db-1","Project":"demo","Service":"picsure-db","State":"exited","Status":"Exited (137) 5 seconds ago"}
`

// psArray is the same three containers as compose < 2.21 printed them.
const psArray = `[
  {"ID":"a1","Name":"demo-hpds-1","Project":"demo","Service":"hpds","Image":"hms-dbmi/pic-sure-hpds:abc123","Labels":"com.docker.compose.project=demo","State":"running","Status":"Up 2 minutes (healthy)","Health":"healthy","ExitCode":0,"Publishers":null,"Created":1791295200},
  {"ID":"b2","Name":"demo-httpd-1","Project":"demo","Service":"httpd","Image":"httpd:2.4","State":"running","Status":"Up 2 minutes","Health":null,"ExitCode":null,"Publishers":[{"URL":"0.0.0.0","TargetPort":443,"PublishedPort":8443,"Protocol":"tcp"}]},
  {"ID":"c3","Name":"demo-picsure-db-1","Project":"demo","Service":"picsure-db","Image":"mysql:8.0","State":"exited","Status":"Exited (137) 5 seconds ago","Health":"unhealthy","ExitCode":137}
]
`

var psWant = []docker.ComposeService{
	{ID: "a1", Name: "demo-hpds-1", Project: "demo", Service: "hpds", Image: "hms-dbmi/pic-sure-hpds:abc123",
		State: "running", Status: "Up 2 minutes (healthy)", Health: "healthy", Labels: "com.docker.compose.project=demo"},
	{ID: "b2", Name: "demo-httpd-1", Project: "demo", Service: "httpd", Image: "httpd:2.4",
		State: "running", Status: "Up 2 minutes",
		Publishers: []docker.ComposePublisher{{URL: "0.0.0.0", TargetPort: 443, PublishedPort: 8443, Protocol: "tcp"}}},
	{ID: "c3", Name: "demo-picsure-db-1", Project: "demo", Service: "picsure-db", Image: "mysql:8.0",
		State: "exited", Status: "Exited (137) 5 seconds ago", Health: "unhealthy", ExitCode: 137},
}

func TestParseComposePsBothShapes(t *testing.T) {
	for name, input := range map[string]string{"lines": psLines, "array": psArray} {
		t.Run(name, func(t *testing.T) {
			got, err := docker.ParseComposePs([]byte(input))
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, psWant) {
				t.Errorf("got  %+v\nwant %+v", got, psWant)
			}
		})
	}
}

func TestComposeServiceLabel(t *testing.T) {
	s := docker.ComposeService{Labels: "com.docker.compose.project=demo,com.docker.compose.config-hash=abc,empty="}
	for key, want := range map[string]string{
		"com.docker.compose.project": "demo", docker.ConfigHashLabel: "abc", "empty": "", "missing": "", "com.docker.compose": "",
	} {
		if got := s.Label(key); got != want {
			t.Errorf("Label(%q) = %q, want %q", key, got, want)
		}
	}
}

func TestParseComposePsNoContainers(t *testing.T) {
	for _, input := range []string{"", "\n", "  \n ", "[]", "[]\n", "null\n"} {
		got, err := docker.ParseComposePs([]byte(input))
		if err != nil || len(got) != 0 {
			t.Errorf("input %q: %+v, %v; want no containers", input, got, err)
		}
	}
}

func TestParseComposePsRejectsMalformedOutput(t *testing.T) {
	for _, input := range []string{
		"not json",
		`{"Service":"hpds"`,
		`{"Service":"hpds"}` + "\nWARN something\n",
		`"a string"`,
		`[1, 2]`,
		`{"Service":"hpds","ExitCode":"zero"}`,
	} {
		if got, err := docker.ParseComposePs([]byte(input)); err == nil {
			t.Errorf("input %q: parsed as %+v, want an error", input, got)
		}
	}
}
