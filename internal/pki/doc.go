// Package pki generates the httpd server certificate with crypto/x509, and
// validates operator-provided certificates in tls.mode provided (spec §9.1
// step 6). Ticket 012 implements it.
package pki
