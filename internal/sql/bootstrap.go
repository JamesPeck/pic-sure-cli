package sql

import (
	"slices"
	"strings"
)

// dbPrivileges are the mysql.db columns that GRANT ALL PRIVILEGES ON db.*
// sets, in MySQL 5.7 and 8. GRANT OPTION (Grant_priv) is not among them.
var dbPrivileges = []string{"Select", "Insert", "Update", "Delete", "Create", "Drop", "References",
	"Index", "Alter", "Create_tmp_table", "Lock_tables", "Create_view", "Show_view",
	"Create_routine", "Alter_routine", "Execute", "Event", "Trigger"}

// Databases lists the databases the users are granted, each once, in
// order.
func Databases(users []AppUser) []string {
	var dbs []string
	for _, u := range users {
		for _, db := range u.Databases {
			if !slices.Contains(dbs, db) {
				dbs = append(dbs, db)
			}
		}
	}
	return dbs
}

// BootstrapStateQuery returns a query for what Bootstrap(users, ...) has
// already created, as rows of two columns:
//
//   - ("version", the server's version);
//   - ("database", name) for each of the users' databases that exists;
//   - ("user", name) for each user that exists as name@'%';
//   - ("account", "name host") for each other account with a user's name,
//     which the server may match before name@'%';
//   - ("grant", "name db") for each user with all privileges on one of its
//     databases.
//
// It reads mysql.user and mysql.db, so the account running it needs SELECT
// on the mysql schema.
func BootstrapStateQuery(users []AppUser) string {
	var names, dbs []string
	for _, u := range users {
		names = append(names, QuoteMySQL(u.Name))
	}
	for _, db := range Databases(users) {
		dbs = append(dbs, QuoteMySQL(db))
	}
	userIn := "(" + strings.Join(names, ", ") + ")"
	all := make([]string, len(dbPrivileges))
	for i, p := range dbPrivileges {
		all[i] = p + "_priv = 'Y'"
	}
	return "SELECT 'version', VERSION()" +
		" UNION ALL SELECT 'database', SCHEMA_NAME FROM information_schema.SCHEMATA WHERE SCHEMA_NAME IN (" + strings.Join(dbs, ", ") + ")" +
		" UNION ALL SELECT 'user', User FROM mysql.user WHERE Host = '%' AND User IN " + userIn +
		" UNION ALL SELECT 'account', CONCAT(User, ' ', Host) FROM mysql.user WHERE Host <> '%' AND User IN " + userIn +
		" UNION ALL SELECT 'grant', CONCAT(User, ' ', Db) FROM mysql.db WHERE Host = '%' AND User IN " + userIn +
		" AND " + strings.Join(all, " AND ")
}

// UseDatabase returns the statement that makes db the session's default
// database. It fails on the server unless the account has a privilege on
// db, so running it as an application user checks that user's access.
func UseDatabase(db string) string { return "USE " + quoteMySQLIdent(db) }
