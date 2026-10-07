package cli

import (
	"strings"
	"testing"

	"github.com/JamesPeck/pic-sure-cli/internal/ops"
)

func TestWriteUpdatePlan(t *testing.T) {
	p := &ops.UpdatePlan{
		Stack:   "demo",
		DryRun:  true,
		Config:  ops.ConfigPlan{From: 1, To: 2, Migrations: []string{"1→2: rename a key"}},
		Release: ops.ReleaseChange{From: "5555555555555555555555555555555555555555", To: "6666666666666666666666666666666666666666"},
		Components: []ops.ComponentChange{
			{Name: "frontend", FromRef: "v2", FromCommit: "0123456789abcdef0123456789abcdef01234567",
				ToRef: "v2.1", ToCommit: "7777777777777777777777777777777777777777", Changed: true},
			{Name: "migrations", ToRef: "main", ToCommit: "3333333333333333333333333333333333333333"},
		},
		Images:     []ops.ImageChange{{Name: "pic-sure-httpd", Action: ops.ImageBuild}, {Name: "dictionary-etl", Action: ops.ImageUpToDate}},
		Migrations: ops.MigrationsPlan{Status: ops.MigrationsStatusPending, StartedDB: true},
		Token:      ops.TokenPlan{Renew: true},
		Restarts:   []ops.RestartPlan{{Service: "httpd", Action: ops.RestartRecreate, Reasons: []string{"its image is rebuilt"}}},
	}
	var b strings.Builder
	if err := writeUpdatePlan(&b, p); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"Update plan for stack demo (dry run: nothing in the stack was changed)",
		"config:      schema 1 → 2, backed up first\n                1→2: rename a key",
		"release:     555555555555 → 666666666666",
		"frontend        v2 (0123456789ab) → v2.1 (777777777777)",
		"migrations      main (333333333333) (unchanged)",
		"build:          pic-sure-httpd",
		"up_to_date:     dictionary-etl",
		"migrations:  pending; started the database to check",
		"token:       renewed",
		"httpd           recreate: its image is rebuilt",
	} {
		if !strings.Contains(b.String(), want) {
			t.Errorf("plan text lacks %q:\n%s", want, b.String())
		}
	}
}
