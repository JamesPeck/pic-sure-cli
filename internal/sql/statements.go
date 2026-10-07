package sql

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"strings"
)

// The builders return SQL text with values already escaped into it. Pass
// the text to ExecMySQL, ExecPostgres or QueryMySQL, which send it on
// stdin. Never log it: it can hold passwords, tokens and email addresses.

// Role UUIDs that the Baseline auth migrations create
// (V2__CONFIGURE_PIC_SURE_APPLICATION.sql).
const (
	topAdminRoleID = "002DC366B0D8420F998F885D0ED797FD" // PIC-SURE Top Admin
	userRoleID     = "797FD002DC366B0D8420F998F885D0ED" // PIC-SURE User
)

// AppliedMigrationsQuery counts the migrations applied in the auth and
// picsure custom Flyway histories and returns the smaller count, so 0 means
// one of the two passes has applied nothing. The custom passes run with
// baselineOnMigrate, so a pass that failed on its first real migration and
// was then repaired leaves a successful BASELINE row behind; that row
// proves nothing and is not counted.
const AppliedMigrationsQuery = `SELECT LEAST(` +
	`(SELECT COUNT(*) FROM auth.flyway_custom_schema_history WHERE success = 1 AND version IS NOT NULL AND type <> 'BASELINE'), ` +
	`(SELECT COUNT(*) FROM picsure.flyway_custom_schema_history WHERE success = 1 AND version IS NOT NULL AND type <> 'BASELINE'))`

// CountUsersWithEmail returns a query for the number of auth users with the
// email.
func CountUsersWithEmail(email string) string {
	return "SELECT COUNT(*) FROM auth.user WHERE email = " + QuoteMySQL(email)
}

// SeedAdminUser returns the statements that create the admin user, with id
// as its UUID, and give it the PIC-SURE Top Admin and PIC-SURE User roles.
// The user is linked to the Google connection, as the bash seed does. If a
// user with the email already exists, the statements change nothing, even
// when replayed with the same id, and they run as one transaction, so a failure part-way never leaves a user
// without its roles.
func SeedAdminUser(email string, id [16]byte) []string {
	e := QuoteMySQL(email)
	uuid := "UNHEX('" + strings.ToUpper(hex.EncodeToString(id[:])) + "')"
	// Without HTML escaping, the metadata matches the bash seed's bytes for
	// an email with <, > or &. Encoding a struct of strings cannot fail.
	var meta bytes.Buffer
	enc := json.NewEncoder(&meta)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(struct {
		Email string `json:"email"`
	}{email})
	// The roles are granted only when this session's insert added the
	// user, so neither an existing user nor a replay with the same id gets
	// back a role an admin has since revoked.
	grant := func(role string) string {
		return "INSERT INTO auth.user_role (user_id, role_id) SELECT " + uuid + ", UNHEX('" + role + "') FROM DUAL WHERE @picsure_seeded_admin = 1"
	}
	return []string{
		"START TRANSACTION",
		"INSERT INTO auth.user (uuid, auth0_metadata, general_metadata, acceptedTOS, connectionId, email, matched, subject, is_active, long_term_token) " +
			"SELECT " + uuid + ", NULL, " + QuoteMySQL(strings.TrimSuffix(meta.String(), "\n")) + ", NULL, (SELECT uuid FROM auth.connection WHERE label = 'Google'), " + e + ", 0, NULL, 1, NULL " +
			"FROM DUAL WHERE NOT EXISTS (SELECT 1 FROM auth.user WHERE email = " + e + ")",
		"SET @picsure_seeded_admin = ROW_COUNT()",
		grant(topAdminRoleID),
		grant(userRoleID),
		"COMMIT",
	}
}

// SetApplicationToken returns the statement that stores the introspection
// token for the PICSURE application.
func SetApplicationToken(token string) string {
	return "UPDATE auth.application SET token = " + QuoteMySQL(token) + " WHERE name = 'PICSURE'"
}

// Account is a MySQL account, 'User'@'Host'.
type Account struct {
	User string
	Host string
}

func (a Account) quoted() string { return QuoteMySQL(a.User) + "@" + QuoteMySQL(a.Host) }

// AlterUserPassword returns the statement that sets the account's password.
// It fails on the server if the account does not exist.
func AlterUserPassword(a Account, password string) string {
	return "ALTER USER " + a.quoted() + " IDENTIFIED BY " + QuoteMySQL(password)
}

// AppUser is an application account on the picsure-db server. It connects
// from any host ('%') and has all privileges on its databases.
type AppUser struct {
	Name      string
	Password  string
	Databases []string
}

// AppPasswords holds the application accounts' passwords.
type AppPasswords struct {
	Picsure string
	Auth    string
	Airflow string
}

// AppUsers returns the picsure, auth and airflow accounts with their
// grants: the same accounts the local picsure-db creates on first start.
func AppUsers(p AppPasswords) []AppUser {
	return []AppUser{
		{Name: "picsure", Password: p.Picsure, Databases: []string{"picsure"}},
		{Name: "auth", Password: p.Auth, Databases: []string{"auth"}},
		{Name: "airflow", Password: p.Airflow, Databases: []string{"auth", "picsure"}},
	}
}

// Bootstrap returns the statements that prepare a remote server: create
// every database the users are granted, create each user, and grant it all
// privileges on its databases. Databases and users that already exist are
// left alone, passwords included; with syncPasswords, each user's password
// is also set to its Password.
func Bootstrap(users []AppUser, syncPasswords bool) []string {
	var stmts []string
	created := map[string]bool{}
	for _, u := range users {
		for _, db := range u.Databases {
			if !created[db] {
				created[db] = true
				stmts = append(stmts, "CREATE DATABASE IF NOT EXISTS "+quoteMySQLIdent(db))
			}
		}
	}
	for _, u := range users {
		a := Account{User: u.Name, Host: "%"}
		stmts = append(stmts, "CREATE USER IF NOT EXISTS "+a.quoted()+" IDENTIFIED BY "+QuoteMySQL(u.Password))
		if syncPasswords {
			stmts = append(stmts, AlterUserPassword(a, u.Password))
		}
		for _, db := range u.Databases {
			stmts = append(stmts, "GRANT ALL PRIVILEGES ON "+quoteMySQLIdent(db)+".* TO "+a.quoted())
		}
	}
	return stmts
}

// AlterPostgresPassword returns the statement that sets the role's
// password. It fails with ErrNUL if either value holds a NUL byte.
func AlterPostgresPassword(role, password string) (string, error) {
	r, err := quotePostgresIdent(role)
	if err != nil {
		return "", err
	}
	p, err := QuotePostgres(password)
	if err != nil {
		return "", err
	}
	return "ALTER ROLE " + r + " WITH PASSWORD " + p, nil
}
