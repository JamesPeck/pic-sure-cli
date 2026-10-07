//go:build integration

package ops_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/JamesPeck/pic-sure-cli/internal/events"
	"github.com/JamesPeck/pic-sure-cli/internal/ops"
	"github.com/JamesPeck/pic-sure-cli/internal/sql"
	"github.com/JamesPeck/pic-sure-cli/internal/stack"
	"github.com/JamesPeck/pic-sure-cli/internal/steps"
)

// TestSeedAgainstDocker migrates a real stack and seeds it: the first seed
// creates the admin user and issues the token, the second skips, and after
// the token is cleared in auth.application a third writes secrets.yaml's
// token back without issuing another. The compose project is
// PICSURE_IT_PROJECT (default ws-v2-033).
//
//	go test -tags integration -run TestSeedAgainstDocker -timeout 30m ./internal/ops/
func TestSeedAgainstDocker(t *testing.T) {
	project := os.Getenv("PICSURE_IT_PROJECT")
	if project == "" {
		project = "ws-v2-033"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Minute)
	defer cancel()
	x := newMigrateIT(ctx, t, project)
	plan := []steps.Step{
		ops.DBStep(x.d, x.cfg, x.sec, ops.DBOptions{}),
		ops.MigrateStep(x.d, x.cfg, x.sec, ops.MigrateOptions{}),
		ops.SeedStep(x.d, x.st, x.cfg, x.sec),
	}
	run := func(want events.StepStatus) {
		t.Helper()
		*x.rec = events.Recorder{}
		if err := steps.Run(ctx, x.d.Sink, plan, steps.Options{}); err != nil {
			t.Fatal(err)
		}
		if got := stepStatus(x.rec)[ops.StepSeed]; got != want {
			t.Fatalf("seed %s, want %s", got, want)
		}
	}
	db := func() sql.MySQLTarget {
		t.Helper()
		svcs, err := x.d.Compose.Ps(ctx, "picsure-db")
		if err != nil || len(svcs) != 1 {
			t.Fatalf("picsure-db: %v, %v", svcs, err)
		}
		return sql.MySQLTarget{Container: svcs[0].ID, Password: string(x.sec.DBRootPassword)}
	}
	query := func(q string) string {
		t.Helper()
		rows, err := sql.QueryMySQL(ctx, x.d.Docker, db(), q)
		if err != nil || len(rows) != 1 || len(rows[0]) != 1 {
			t.Fatalf("%s: %q, %v", q, rows, err)
		}
		return rows[0][0]
	}
	checkSeeded := func() {
		t.Helper()
		if n := query(sql.CountUsersWithEmail(x.cfg.Auth.AdminEmail)); n != "1" {
			t.Errorf("%s admin users, want 1", n)
		}
		if n := query("SELECT COUNT(*) FROM auth.user_role ur JOIN auth.user u ON u.uuid = ur.user_id WHERE u.email = " +
			sql.QuoteMySQL(x.cfg.Auth.AdminEmail)); n != "2" {
			t.Errorf("admin has %s roles, want 2", n)
		}
		saved, err := x.st.LoadSecrets()
		if err != nil {
			t.Fatal(err)
		}
		if query("SELECT token FROM auth.application WHERE name = 'PICSURE'") != string(saved.IntrospectionToken) || saved.IntrospectionToken == "" {
			t.Error("auth.application's token isn't secrets.yaml's")
		}
	}

	run(events.StepOK)
	checkSeeded()
	token := x.sec.IntrospectionToken

	run(events.StepSkipped)
	checkSeeded()

	if err := sql.ExecMySQL(ctx, x.d.Docker, db(), "UPDATE auth.application SET token = NULL WHERE name = 'PICSURE'"); err != nil {
		t.Fatal(err)
	}
	run(events.StepOK)
	checkSeeded()
	if x.sec.IntrospectionToken != token {
		t.Error("re-syncing issued a new token")
	}
	if saved, _ := x.st.LoadSecrets(); saved.IntrospectionToken != stack.Secret(token) {
		t.Error("secrets.yaml's token changed")
	}
}
