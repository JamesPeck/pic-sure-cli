// Package wizard is the setup form for a new stack. It asks for the
// pic-sure.yaml fields init has flags for (spec §5), plus the HPDS heap,
// taking each field's kind, help, options, secrecy and requiredness from
// stack.Fields, and validates them as `pic-sure config set` does. It never
// writes anything: its host hands the confirmed config to init.
package wizard

import "github.com/JamesPeck/pic-sure-cli/internal/stack"

// Item is one field the wizard asks for, by its config key.
type Item struct {
	Key   string
	Title string
}

// Group is one page of the form.
type Group struct {
	Title       string
	Description string
	Items       []Item
	// Shown reports whether the group applies to what has been entered so
	// far; nil means always.
	Shown func(f *Form) bool
	// askProxy makes the group the "use a proxy?" question.
	askProxy bool
}

// Groups is the form, in order.
var Groups = []Group{
	{
		Title:       "Stack",
		Description: "The stack's name is fixed once it exists. The release-control branch picks the component versions.",
		Items: []Item{
			{Key: "name", Title: "Stack name"},
			{Key: "release.branch", Title: "Release-control branch"},
			{Key: "frontend.theme", Title: "Frontend theme"},
		},
	},
	{
		Title:       "Access",
		Description: "Who can reach the data, and the first administrator.",
		Items: []Item{
			{Key: "auth.mode", Title: "Auth mode"},
			{Key: "auth.admin_email", Title: "Admin email"},
		},
	},
	{
		Title:       "Auth0",
		Description: "From your Auth0 application: the tenant, the client ID and its secret.",
		Items: []Item{
			{Key: "auth.auth0.tenant", Title: "Auth0 tenant"},
			{Key: "auth.auth0.client_id", Title: "Auth0 client ID"},
			{Key: "auth.auth0.client_secret", Title: "Auth0 client secret"},
		},
		Shown: func(f *Form) bool { return f.Value("auth.mode") != string(stack.AuthOpen) },
	},
	{
		Title:       "Ports",
		Description: "Host ports the stack listens on. These are free now.",
		Items: []Item{
			{Key: "network.http_port", Title: "HTTP port"},
			{Key: "network.https_port", Title: "HTTPS port"},
		},
	},
	{
		Title:       "Database",
		Description: "local runs a bundled MySQL; remote uses your own server.",
		Items:       []Item{{Key: "db.mode", Title: "Database"}},
	},
	{
		Title:       "Remote database",
		Description: "Where to reach your MySQL, and an admin account that can create databases and users.",
		Items: []Item{
			{Key: "db.remote.host", Title: "Host"},
			{Key: "db.remote.port", Title: "Port"},
			{Key: "db.remote.root_user", Title: "Admin user"},
			{Key: "db.remote.root_password", Title: "Admin password"},
		},
		Shown: func(f *Form) bool { return f.Value("db.mode") == string(stack.DBRemote) },
	},
	{
		Title:       "HPDS",
		Description: "The query engine's JVM options. Lower -Xmx on a small Docker VM.",
		Items:       []Item{{Key: "hpds.java_opts", Title: "HPDS JVM options"}},
	},
	{
		Title:       "Proxy",
		Description: "Does this host reach the internet through a proxy?",
		askProxy:    true,
	},
	{
		Title:       "Proxy settings",
		Description: "The HTTPS proxy starts as the HTTP one; clear it if HTTPS goes direct.",
		Items: []Item{
			{Key: "proxy.http", Title: "HTTP proxy"},
			{Key: "proxy.https", Title: "HTTPS proxy"},
			{Key: "proxy.no_proxy", Title: "No proxy for"},
		},
		Shown: func(f *Form) bool { return f.useProxy },
	},
}
