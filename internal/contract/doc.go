// Package contract holds the JSON types of the v1 script contract
// (status.sh --json, preflight.sh --json, compose ps). The v1 parsers are
// gone; the types stay only because the TUI dashboard (internal/dashboard)
// still renders from them. Ticket 040 moves the dashboard onto the v2 status
// report (ticket 027) and the compose adapter's ps parser (ticket 017), and
// then this package can be deleted.
package contract
