// Package render turns a stack's config, secret names and state into the
// files under .pic-sure/render/: one merged compose.yaml and the static files
// (httpd vhost, Flyway and DB-init scripts, Vite dev config, Maven
// settings.xml), plus the per-call compose environment (spec §6.4). Render
// is a pure function and golden-tested.
//
// Ticket 020 ports the embedded templates from AIO into
// internal/render/templates/. Ticket 021 implements rendering and the
// goldens; ticket 051 adds the shared-HPDS fragment.
package render
