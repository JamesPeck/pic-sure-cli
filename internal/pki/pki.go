package pki

import (
	"bytes"
	"crypto"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"strings"
	"time"
)

// Validity is how long a generated certificate is valid.
const Validity = 365 * 24 * time.Hour

// maxCommonName is the RFC 5280 upper bound on a subject common name.
const maxCommonName = 64

var (
	// ErrKeyMismatch means the private key isn't the certificate's.
	ErrKeyMismatch = errors.New("the private key doesn't match the certificate")
	// ErrExpired means the certificate's validity period has ended.
	ErrExpired = errors.New("the certificate has expired")
	// ErrNotYetValid means the certificate's validity period hasn't begun.
	ErrNotYetValid = errors.New("the certificate isn't valid yet")
	// ErrEncryptedKey means the private key is passphrase-protected, which
	// httpd can't unlock without prompting.
	ErrEncryptedKey = errors.New("the private key is encrypted; httpd needs an unencrypted key")
)

// Files are httpd's PEM files: server.key, server.crt and server.chain.
type Files struct {
	Key   []byte
	Cert  []byte
	Chain []byte // optional when validating
}

// Generate creates an RSA-2048 key and a self-signed TLS server certificate
// for hostname, valid for Validity from now. The certificate names
// localhost, 127.0.0.1 and hostname, its CN is hostname when that fits in
// a CN, and the chain is the certificate itself. rand is the randomness
// source: crypto/rand.Reader in production.
// A fixed rand doesn't make the key deterministic.
func Generate(rand io.Reader, hostname string, now time.Time) (Files, error) {
	dnsNames, ips, err := subjectAltNames(hostname)
	if err != nil {
		return Files{}, err
	}
	serial, err := serialNumber(rand)
	if err != nil {
		return Files{}, err
	}
	key, err := rsa.GenerateKey(rand, 2048)
	if err != nil {
		return Files{}, fmt.Errorf("generate the TLS key: %w", err)
	}

	subject := pkix.Name{Organization: []string{"PIC-SURE"}}
	if cn := strings.ToLower(hostname); len(cn) <= maxCommonName {
		subject.CommonName = cn
	}
	notBefore := now.UTC().Truncate(time.Second)
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               subject,
		NotBefore:             notBefore,
		NotAfter:              notBefore.Add(Validity),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		DNSNames:              dnsNames,
		IPAddresses:           ips,
	}
	der, err := x509.CreateCertificate(rand, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return Files{}, fmt.Errorf("create the TLS certificate: %w", err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return Files{}, fmt.Errorf("encode the TLS key: %w", err)
	}

	cert := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	return Files{
		Key:   pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}),
		Cert:  cert,
		Chain: bytes.Clone(cert),
	}, nil
}

func subjectAltNames(hostname string) ([]string, []net.IP, error) {
	dnsNames := []string{"localhost"}
	loopback := net.IPv4(127, 0, 0, 1)
	ips := []net.IP{loopback}
	if ip := net.ParseIP(hostname); ip != nil {
		if !ip.Equal(loopback) {
			ips = append(ips, ip)
		}
		return dnsNames, ips, nil
	}
	name := strings.ToLower(hostname)
	if !validDNSName(name) {
		return nil, nil, fmt.Errorf("can't make a TLS certificate for hostname %q: want a DNS name or an IP address", hostname)
	}
	if name != "localhost" {
		dnsNames = append(dnsNames, name)
	}
	return dnsNames, ips, nil
}

// validDNSName reports whether name is a lowercase host name: dot-separated
// labels of letters, digits, hyphens and underscores, no label starting or
// ending with a hyphen, and a last label that isn't all digits, because
// browsers parse such a name (10.1.2.300, say) as an IPv4 address.
func validDNSName(name string) bool {
	if name == "" || len(name) > 253 {
		return false
	}
	labels := strings.Split(name, ".")
	if strings.Trim(labels[len(labels)-1], "0123456789") == "" {
		return false
	}
	for _, label := range labels {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, c := range label {
			if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' && c != '_' {
				return false
			}
		}
	}
	return true
}

func serialNumber(rand io.Reader) (*big.Int, error) {
	b := make([]byte, 16)
	if _, err := io.ReadFull(rand, b); err != nil {
		return nil, fmt.Errorf("generate the TLS certificate serial: %w", err)
	}
	n := new(big.Int).SetBytes(b)
	if n.Sign() == 0 {
		n.SetInt64(1) // RFC 5280 requires a positive serial
	}
	return n, nil
}

// Report describes certificate files that passed Validate.
type Report struct {
	// NotAfter is when the certificate expires.
	NotAfter time.Time
	// Warnings are problems httpd can still serve with, such as a
	// certificate that doesn't name the configured hostname.
	Warnings []string
}

// Validate checks operator-provided files for tls.mode provided: the
// private key must be the certificate's, and the certificate must be valid
// at now. In f.Cert the first certificate is the server's; any after it are
// intermediates. A non-empty f.Chain must hold at least one certificate,
// and a PEM block that doesn't decode is an error in any of the files.
// A certificate that doesn't name hostname is a warning, not an error,
// because a client may reach the stack by another name. Problems found
// after parsing are joined, so one error can wrap ErrKeyMismatch and
// ErrExpired.
func Validate(f Files, hostname string, now time.Time) (Report, error) {
	certs, err := parseCertificates(f.Cert)
	if err != nil {
		return Report{}, fmt.Errorf("certificate file: %w", err)
	}
	leaf := certs[0]
	key, err := parsePrivateKey(f.Key)
	if err != nil {
		return Report{}, fmt.Errorf("key file: %w", err)
	}
	if len(bytes.TrimSpace(f.Chain)) > 0 {
		if _, err := parseCertificates(f.Chain); err != nil {
			return Report{}, fmt.Errorf("chain file: %w", err)
		}
	}

	var errs []error
	if pub, ok := key.Public().(interface{ Equal(crypto.PublicKey) bool }); !ok || !pub.Equal(leaf.PublicKey) {
		errs = append(errs, ErrKeyMismatch)
	}
	switch {
	case now.After(leaf.NotAfter):
		errs = append(errs, fmt.Errorf("%w (not after %s)", ErrExpired, leaf.NotAfter.UTC().Format(time.RFC3339)))
	case now.Before(leaf.NotBefore):
		errs = append(errs, fmt.Errorf("%w (not before %s)", ErrNotYetValid, leaf.NotBefore.UTC().Format(time.RFC3339)))
	}
	if err := errors.Join(errs...); err != nil {
		return Report{}, err
	}

	r := Report{NotAfter: leaf.NotAfter}
	if err := leaf.VerifyHostname(hostname); err != nil {
		r.Warnings = append(r.Warnings, fmt.Sprintf(
			"the certificate doesn't name %q (it names %s); browsers will reject it for that name",
			hostname, describeNames(leaf)))
	}
	return r, nil
}

// pemBlocks decodes every PEM block in data. pem.Decode silently skips a
// block it can't decode, but httpd refuses the file, so a BEGIN line that
// yields no block is an error.
func pemBlocks(data []byte) ([]*pem.Block, error) {
	var blocks []*pem.Block
	for rest := data; ; {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			break
		}
		blocks = append(blocks, block)
	}
	begins := bytes.Count(data, []byte("\n-----BEGIN "))
	if bytes.HasPrefix(data, []byte("-----BEGIN ")) {
		begins++
	}
	if len(blocks) != begins {
		return nil, errors.New("malformed PEM block")
	}
	return blocks, nil
}

// parseCertificates parses every CERTIFICATE block in data, skipping other
// blocks, and fails if there are none.
func parseCertificates(data []byte) ([]*x509.Certificate, error) {
	blocks, err := pemBlocks(data)
	if err != nil {
		return nil, err
	}
	var certs []*x509.Certificate
	for _, block := range blocks {
		if block.Type != "CERTIFICATE" {
			continue
		}
		c, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("parse certificate %d: %w", len(certs)+1, err)
		}
		certs = append(certs, c)
	}
	if len(certs) == 0 {
		return nil, errors.New("no PEM CERTIFICATE block found")
	}
	return certs, nil
}

// parsePrivateKey returns the first private key in data, in PKCS #8,
// PKCS #1 or SEC 1 form.
func parsePrivateKey(data []byte) (crypto.Signer, error) {
	blocks, err := pemBlocks(data)
	if err != nil {
		return nil, err
	}
	for _, block := range blocks {
		if block.Type == "ENCRYPTED PRIVATE KEY" ||
			(strings.HasSuffix(block.Type, "PRIVATE KEY") && strings.Contains(block.Headers["Proc-Type"], "ENCRYPTED")) {
			return nil, ErrEncryptedKey
		}
		var key any
		switch block.Type {
		case "PRIVATE KEY":
			key, err = x509.ParsePKCS8PrivateKey(block.Bytes)
		case "RSA PRIVATE KEY":
			key, err = x509.ParsePKCS1PrivateKey(block.Bytes)
		case "EC PRIVATE KEY":
			key, err = x509.ParseECPrivateKey(block.Bytes)
		default:
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("parse %s: %w", block.Type, err)
		}
		signer, ok := key.(crypto.Signer)
		if !ok {
			return nil, fmt.Errorf("unsupported private key type %T", key)
		}
		return signer, nil
	}
	return nil, errors.New("no PEM private key block found")
}

func describeNames(c *x509.Certificate) string {
	names := append([]string(nil), c.DNSNames...)
	for _, ip := range c.IPAddresses {
		names = append(names, ip.String())
	}
	if len(names) == 0 {
		return "no subject alternative names"
	}
	return strings.Join(names, ", ")
}
