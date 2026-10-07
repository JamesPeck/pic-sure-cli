package sql_test

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/JamesPeck/pic-sure-cli/internal/sql"
)

func TestSeedAdminUser(t *testing.T) {
	id := [16]byte{0xab, 0xcd, 0xef, 0x01, 0x23, 0x45, 0x67, 0x89, 0xab, 0xcd, 0xef, 0x01, 0x23, 0x45, 0x67, 0x89}

	got := sql.SeedAdminUser("o'brien@example.org", id)

	uuid := "UNHEX('ABCDEF0123456789ABCDEF0123456789')"
	want := []string{
		"START TRANSACTION",
		"INSERT INTO auth.user (uuid, auth0_metadata, general_metadata, acceptedTOS, connectionId, email, matched, subject, is_active, long_term_token) " +
			"SELECT " + uuid + `, NULL, '{"email":"o''brien@example.org"}', NULL, (SELECT uuid FROM auth.connection WHERE label = 'Google'), 'o''brien@example.org', 0, NULL, 1, NULL ` +
			"FROM DUAL WHERE NOT EXISTS (SELECT 1 FROM auth.user WHERE email = 'o''brien@example.org')",
		"INSERT INTO auth.user_role (user_id, role_id) SELECT uuid, UNHEX('002DC366B0D8420F998F885D0ED797FD') FROM auth.user WHERE uuid = " + uuid,
		"INSERT INTO auth.user_role (user_id, role_id) SELECT uuid, UNHEX('797FD002DC366B0D8420F998F885D0ED') FROM auth.user WHERE uuid = " + uuid,
		"COMMIT",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("SeedAdminUser:\n got %q\nwant %q", got, want)
	}
}

// The metadata is JSON inside a SQL literal, so an email with a JSON
// special character is escaped for JSON first and then for SQL.
func TestSeedAdminUserMetadataIsValidJSON(t *testing.T) {
	got := sql.SeedAdminUser(`a"b\c@example.org`, [16]byte{})[1]

	if want := `'{"email":"a\\"b\\\\c@example.org"}'`; !strings.Contains(got, want) {
		t.Errorf("insert = %s\nwant metadata %s", got, want)
	}
}

func TestCountUsersWithEmail(t *testing.T) {
	got := sql.CountUsersWithEmail("o'brien@example.org")
	if want := "SELECT COUNT(*) FROM auth.user WHERE email = 'o''brien@example.org'"; got != want {
		t.Errorf("got %s, want %s", got, want)
	}
}

func TestSetApplicationToken(t *testing.T) {
	got := sql.SetApplicationToken("a.b'c")
	if want := "UPDATE auth.application SET token = 'a.b''c' WHERE name = 'PICSURE'"; got != want {
		t.Errorf("got %s, want %s", got, want)
	}
}

func TestAlterUserPassword(t *testing.T) {
	got := sql.AlterUserPassword(sql.Account{User: "root", Host: "localhost"}, `p'w\`)
	if want := `ALTER USER 'root'@'localhost' IDENTIFIED BY 'p''w\\'`; got != want {
		t.Errorf("got %s, want %s", got, want)
	}
}

func TestBootstrap(t *testing.T) {
	users := sql.AppUsers(sql.AppPasswords{Picsure: "pic'pw", Auth: "auth-pw", Airflow: `air\pw`})
	create := []string{
		"CREATE DATABASE IF NOT EXISTS `picsure`",
		"CREATE DATABASE IF NOT EXISTS `auth`",
	}
	picsure := "CREATE USER IF NOT EXISTS 'picsure'@'%' IDENTIFIED BY 'pic''pw'"
	picsureGrant := "GRANT ALL PRIVILEGES ON `picsure`.* TO 'picsure'@'%'"
	auth := "CREATE USER IF NOT EXISTS 'auth'@'%' IDENTIFIED BY 'auth-pw'"
	authGrant := "GRANT ALL PRIVILEGES ON `auth`.* TO 'auth'@'%'"
	airflow := `CREATE USER IF NOT EXISTS 'airflow'@'%' IDENTIFIED BY 'air\\pw'`
	airflowGrants := []string{
		"GRANT ALL PRIVILEGES ON `auth`.* TO 'airflow'@'%'",
		"GRANT ALL PRIVILEGES ON `picsure`.* TO 'airflow'@'%'",
	}

	t.Run("create only", func(t *testing.T) {
		want := append(append([]string{}, create...), picsure, picsureGrant, auth, authGrant, airflow)
		want = append(want, airflowGrants...)
		if got := sql.Bootstrap(users, false); !reflect.DeepEqual(got, want) {
			t.Errorf("Bootstrap:\n got %q\nwant %q", got, want)
		}
	})
	t.Run("sync passwords", func(t *testing.T) {
		want := append(append([]string{}, create...),
			picsure, "ALTER USER 'picsure'@'%' IDENTIFIED BY 'pic''pw'", picsureGrant,
			auth, "ALTER USER 'auth'@'%' IDENTIFIED BY 'auth-pw'", authGrant,
			airflow, `ALTER USER 'airflow'@'%' IDENTIFIED BY 'air\\pw'`)
		want = append(want, airflowGrants...)
		if got := sql.Bootstrap(users, true); !reflect.DeepEqual(got, want) {
			t.Errorf("Bootstrap:\n got %q\nwant %q", got, want)
		}
	})
}

func TestAlterPostgresPassword(t *testing.T) {
	got, err := sql.AlterPostgresPassword("picsure", `p'w\`)
	if err != nil {
		t.Fatal(err)
	}
	if want := `ALTER ROLE "picsure" WITH PASSWORD E'p''w\\'`; got != want {
		t.Errorf("got %s, want %s", got, want)
	}
	if _, err := sql.AlterPostgresPassword("picsure", "a\x00b"); !errors.Is(err, sql.ErrNUL) {
		t.Errorf("NUL in password: err = %v, want ErrNUL", err)
	}
	if _, err := sql.AlterPostgresPassword("pic\x00sure", "pw"); !errors.Is(err, sql.ErrNUL) {
		t.Errorf("NUL in role: err = %v, want ErrNUL", err)
	}
}
