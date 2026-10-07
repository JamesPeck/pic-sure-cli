// Package render turns a stack's config, secret names and state into the
// files under .pic-sure/render/: one merged compose.yaml and the static files
// (httpd vhost, Flyway and DB-init scripts, Vite dev config, Maven
// settings.xml), plus the per-call compose environment (spec §6.4). Render
// is a pure function and golden-tested.
//
// The embedded templates, ported from AIO, are in templates/ (ticket 020; its
// README maps each one to its AIO source). Ticket 021 implements rendering
// and the goldens.
package render
