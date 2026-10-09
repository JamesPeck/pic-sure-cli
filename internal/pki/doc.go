// Package pki generates the httpd server certificate with crypto/x509, and
// validates operator-provided certificates in tls.mode provided (spec §9.1
// step 6). It works on PEM bytes and does no file I/O: the caller reads and
// writes the files and puts them in the certs volume.
package pki
