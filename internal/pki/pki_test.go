package pki_test

import (
	"bytes"
	"crypto"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"net"
	"slices"
	"strings"
	"testing"
	"testing/iotest"
	"time"

	"github.com/JamesPeck/pic-sure-cli/internal/pki"
)

var t0 = time.Date(2026, 10, 6, 12, 30, 15, 0, time.UTC)

func generate(t *testing.T, hostname string) pki.Files {
	t.Helper()
	f, err := pki.Generate(rand.Reader, hostname, t0)
	if err != nil {
		t.Fatalf("Generate(%q): %v", hostname, err)
	}
	return f
}

func decodeOne(t *testing.T, data []byte, wantType string) []byte {
	t.Helper()
	block, rest := pem.Decode(data)
	if block == nil {
		t.Fatalf("no PEM block in %q", data)
	}
	if block.Type != wantType {
		t.Fatalf("PEM type = %q, want %q", block.Type, wantType)
	}
	if len(bytes.TrimSpace(rest)) != 0 {
		t.Fatalf("trailing data after the %s block: %q", wantType, rest)
	}
	return block.Bytes
}

func parseCert(t *testing.T, data []byte) *x509.Certificate {
	t.Helper()
	c, err := x509.ParseCertificate(decodeOne(t, data, "CERTIFICATE"))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestGenerateMakesASelfSignedServerCertificate(t *testing.T) {
	t.Parallel()
	f := generate(t, "picsure.example.org")

	k, err := x509.ParsePKCS8PrivateKey(decodeOne(t, f.Key, "PRIVATE KEY"))
	if err != nil {
		t.Fatal(err)
	}
	key, ok := k.(*rsa.PrivateKey)
	if !ok {
		t.Fatalf("key is %T, want *rsa.PrivateKey", k)
	}
	if bits := key.N.BitLen(); bits != 2048 {
		t.Errorf("key is %d bits, want 2048", bits)
	}

	cert := parseCert(t, f.Cert)
	if !key.PublicKey.Equal(cert.PublicKey) {
		t.Error("the certificate's public key isn't the generated key's")
	}
	if !bytes.Equal(f.Chain, f.Cert) {
		t.Error("the chain isn't the certificate")
	}

	if cert.Subject.CommonName != "picsure.example.org" {
		t.Errorf("CN = %q, want the hostname", cert.Subject.CommonName)
	}
	if !bytes.Equal(cert.RawIssuer, cert.RawSubject) {
		t.Errorf("issuer %q != subject %q", cert.Issuer, cert.Subject)
	}
	// CheckSignatureFrom would refuse a non-CA parent, so check the
	// signature against the certificate's own key directly.
	if err := cert.CheckSignature(cert.SignatureAlgorithm, cert.RawTBSCertificate, cert.Signature); err != nil {
		t.Errorf("not self-signed: %v", err)
	}
	if !cert.NotBefore.Equal(t0) || !cert.NotAfter.Equal(t0.Add(365*24*time.Hour)) {
		t.Errorf("validity = %s to %s, want %s plus 365 days", cert.NotBefore, cert.NotAfter, t0)
	}

	if !cert.BasicConstraintsValid || cert.IsCA {
		t.Errorf("basic constraints valid=%v CA=%v, want a valid non-CA constraint", cert.BasicConstraintsValid, cert.IsCA)
	}
	if want := x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment; cert.KeyUsage != want {
		t.Errorf("key usage = %b, want %b", cert.KeyUsage, want)
	}
	if !slices.Equal(cert.ExtKeyUsage, []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}) {
		t.Errorf("extended key usage = %v, want server auth only", cert.ExtKeyUsage)
	}
	if n := cert.SerialNumber; n.Sign() <= 0 || n.BitLen() > 128 {
		t.Errorf("serial %s isn't a positive 128-bit number", n)
	}

	if _, err := tls.X509KeyPair(f.Cert, f.Key); err != nil {
		t.Errorf("crypto/tls can't load the pair: %v", err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(cert)
	for _, name := range []string{"picsure.example.org", "localhost", "127.0.0.1"} {
		_, err := cert.Verify(x509.VerifyOptions{
			DNSName:     name,
			Roots:       roots,
			CurrentTime: t0.Add(time.Hour),
			KeyUsages:   []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		})
		if err != nil {
			t.Errorf("a client trusting the certificate rejects it for %s: %v", name, err)
		}
	}
}

func TestGenerateSerialsDiffer(t *testing.T) {
	t.Parallel()
	a := parseCert(t, generate(t, "localhost").Cert)
	b := parseCert(t, generate(t, "localhost").Cert)
	if a.SerialNumber.Cmp(b.SerialNumber) == 0 {
		t.Errorf("two certificates share serial %s", a.SerialNumber)
	}
}

func TestGenerateSubjectAltNames(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("a", 60) + ".example.org"
	tests := []struct {
		hostname string
		wantCN   string
		wantDNS  []string
		wantIPs  []string
	}{
		{"localhost", "localhost", []string{"localhost"}, []string{"127.0.0.1"}},
		{"picsure.example.org", "picsure.example.org", []string{"localhost", "picsure.example.org"}, []string{"127.0.0.1"}},
		{"PicSure.Example.ORG", "picsure.example.org", []string{"localhost", "picsure.example.org"}, []string{"127.0.0.1"}},
		{"picsure", "picsure", []string{"localhost", "picsure"}, []string{"127.0.0.1"}},
		{"127.0.0.1", "127.0.0.1", []string{"localhost"}, []string{"127.0.0.1"}},
		{"10.1.2.3", "10.1.2.3", []string{"localhost"}, []string{"127.0.0.1", "10.1.2.3"}},
		{"fd00::1", "fd00::1", []string{"localhost"}, []string{"127.0.0.1", "fd00::1"}},
		// Longer than a common name may be, so the SAN alone names it.
		{long, "", []string{"localhost", long}, []string{"127.0.0.1"}},
	}
	for _, tt := range tests {
		t.Run(tt.hostname, func(t *testing.T) {
			t.Parallel()
			cert := parseCert(t, generate(t, tt.hostname).Cert)
			if cert.Subject.CommonName != tt.wantCN {
				t.Errorf("CN = %q, want %q", cert.Subject.CommonName, tt.wantCN)
			}
			if !slices.Equal(cert.DNSNames, tt.wantDNS) {
				t.Errorf("DNS SANs = %q, want %q", cert.DNSNames, tt.wantDNS)
			}
			var ips []string
			for _, ip := range cert.IPAddresses {
				ips = append(ips, ip.String())
			}
			if !slices.Equal(ips, tt.wantIPs) {
				t.Errorf("IP SANs = %q, want %q", ips, tt.wantIPs)
			}
		})
	}
}

func TestGenerateRejectsHostnamesACertificateCannotName(t *testing.T) {
	t.Parallel()
	for _, hostname := range []string{
		"",
		"https://picsure.example.org",
		"picsure.example.org:8443",
		"pic sure",
		"*.example.org",
		"-picsure.example.org",
		"picsure-.example.org",
		"picsure..example.org",
		"picsure.example.org.",
		"[::1]",
		"fe80::1%en0",
		"bücher.example",
		"10.1.2.300",
		"10.0.0",
		"1234",
		strings.Repeat("a", 64) + ".example.org",
	} {
		if _, err := pki.Generate(rand.Reader, hostname, t0); err == nil || !strings.Contains(err.Error(), "hostname") {
			t.Errorf("Generate(%q) error = %v, want a hostname error", hostname, err)
		}
	}
}

func TestGenerateReportsRandomnessFailure(t *testing.T) {
	t.Parallel()
	boom := errors.New("entropy exhausted")
	if _, err := pki.Generate(iotest.ErrReader(boom), "localhost", t0); !errors.Is(err, boom) {
		t.Errorf("error = %v, want it to wrap %v", err, boom)
	}
}

func TestValidateAcceptsAGeneratedPair(t *testing.T) {
	t.Parallel()
	f := generate(t, "picsure.example.org")
	for _, hostname := range []string{"picsure.example.org", "localhost", "127.0.0.1"} {
		for _, now := range []time.Time{t0, t0.Add(pki.Validity)} {
			r, err := pki.Validate(f, hostname, now)
			if err != nil {
				t.Fatalf("Validate(%s, %s): %v", hostname, now, err)
			}
			if len(r.Warnings) != 0 {
				t.Errorf("Validate(%s, %s) warnings: %q", hostname, now, r.Warnings)
			}
			if !r.NotAfter.Equal(t0.Add(pki.Validity)) {
				t.Errorf("NotAfter = %s, want %s", r.NotAfter, t0.Add(pki.Validity))
			}
		}
	}
}

func TestValidateWarnsWhenTheCertificateDoesNotNameTheHostname(t *testing.T) {
	t.Parallel()
	f := generate(t, "picsure.example.org")
	r, err := pki.Validate(f, "other.example.org", t0)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Warnings) != 1 {
		t.Fatalf("warnings = %q, want one", r.Warnings)
	}
	for _, want := range []string{`"other.example.org"`, "localhost, picsure.example.org, 127.0.0.1"} {
		if !strings.Contains(r.Warnings[0], want) {
			t.Errorf("warning %q doesn't mention %q", r.Warnings[0], want)
		}
	}
}

func TestValidateRejectsAnExpiredOrNotYetValidCertificate(t *testing.T) {
	t.Parallel()
	f := generate(t, "localhost")
	_, err := pki.Validate(f, "localhost", t0.Add(pki.Validity+time.Second))
	if !errors.Is(err, pki.ErrExpired) {
		t.Errorf("after NotAfter: error = %v, want ErrExpired", err)
	} else if !strings.Contains(err.Error(), t0.Add(pki.Validity).Format(time.RFC3339)) {
		t.Errorf("error %q doesn't give the expiry date", err)
	}
	if _, err := pki.Validate(f, "localhost", t0.Add(-time.Second)); !errors.Is(err, pki.ErrNotYetValid) {
		t.Errorf("before NotBefore: error = %v, want ErrNotYetValid", err)
	}
}

func TestValidateRejectsAKeyThatIsNotTheCertificates(t *testing.T) {
	t.Parallel()
	generated := generate(t, "localhost")
	ecKey := newECKey(t)
	other := selfSigned(t, newECKey(t), t0, t0.Add(time.Hour))

	for name, f := range map[string]pki.Files{
		"same key type":  {Cert: other, Key: pkcs8PEM(t, ecKey)},
		"other key type": {Cert: generated.Cert, Key: pkcs8PEM(t, ecKey)},
	} {
		if _, err := pki.Validate(f, "localhost", t0); !errors.Is(err, pki.ErrKeyMismatch) {
			t.Errorf("%s: error = %v, want ErrKeyMismatch", name, err)
		}
	}

	_, err := pki.Validate(pki.Files{Cert: other, Key: pkcs8PEM(t, ecKey)}, "localhost", t0.Add(2*time.Hour))
	if !errors.Is(err, pki.ErrKeyMismatch) || !errors.Is(err, pki.ErrExpired) {
		t.Errorf("error = %v, want both ErrKeyMismatch and ErrExpired", err)
	}
}

func TestValidateAcceptsCommonKeyAndCertificateLayouts(t *testing.T) {
	t.Parallel()
	generated := generate(t, "localhost")
	rsaKey, err := x509.ParsePKCS8PrivateKey(decodeOne(t, generated.Key, "PRIVATE KEY"))
	if err != nil {
		t.Fatal(err)
	}
	ecKey := newECKey(t)
	ecCert := selfSigned(t, ecKey, t0, t0.Add(time.Hour))
	ecDER, err := x509.MarshalECPrivateKey(ecKey)
	if err != nil {
		t.Fatal(err)
	}
	ecParams := pem.EncodeToMemory(&pem.Block{Type: "EC PARAMETERS", Bytes: []byte{0x06, 0x08, 0x2a, 0x86, 0x48, 0xce, 0x3d, 0x03, 0x01, 0x07}})
	sec1 := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: ecDER})
	_, edKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}

	for name, f := range map[string]pki.Files{
		"PKCS #1 RSA key": {
			Cert: generated.Cert,
			Key:  pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(rsaKey.(*rsa.PrivateKey))}),
		},
		"SEC 1 EC key after its parameters": {Cert: ecCert, Key: append(ecParams, sec1...)},
		"PKCS #8 EC key":                    {Cert: ecCert, Key: pkcs8PEM(t, ecKey)},
		"PKCS #8 Ed25519 key":               {Cert: selfSigned(t, edKey, t0, t0.Add(time.Hour)), Key: pkcs8PEM(t, edKey)},
		"key and certificate in one file":   {Cert: append(slices.Clone(sec1), ecCert...), Key: append(slices.Clone(ecCert), sec1...)},
		"certificate then an intermediate":  {Cert: append(slices.Clone(ecCert), generated.Cert...), Key: sec1},
		"separate chain file":               {Cert: ecCert, Key: sec1, Chain: generated.Cert},
		"blank chain file":                  {Cert: ecCert, Key: sec1, Chain: []byte("\n")},
		"CRLF line endings":                 {Cert: bytes.ReplaceAll(ecCert, []byte("\n"), []byte("\r\n")), Key: bytes.ReplaceAll(sec1, []byte("\n"), []byte("\r\n"))},
		"text before the certificate":       {Cert: append([]byte("subject=CN=localhost\nissuer=CN=localhost\n"), ecCert...), Key: sec1},
	} {
		if _, err := pki.Validate(f, "localhost", t0); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestValidateRejectsUnusableFiles(t *testing.T) {
	t.Parallel()
	ecKey := newECKey(t)
	cert := selfSigned(t, ecKey, t0, t0.Add(time.Hour))
	key := pkcs8PEM(t, ecKey)
	junkCert := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: []byte("junk")})
	badBase64 := []byte("-----BEGIN CERTIFICATE-----\n!!!!\n-----END CERTIFICATE-----\n")
	then := func(parts ...[]byte) []byte { return bytes.Join(parts, nil) }
	x25519, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name    string
		f       pki.Files
		want    string
		wantErr error
	}{
		{"empty certificate", pki.Files{Key: key}, "certificate file: no PEM CERTIFICATE block", nil},
		{"certificate isn't PEM", pki.Files{Cert: []byte("not a certificate"), Key: key}, "certificate file: no PEM CERTIFICATE block", nil},
		{"certificate file holds only a key", pki.Files{Cert: key, Key: key}, "certificate file: no PEM CERTIFICATE block", nil},
		{"certificate is junk", pki.Files{Cert: junkCert, Key: key}, "certificate file: parse certificate 1", nil},
		{"certificate then a block that doesn't decode", pki.Files{Cert: then(cert, badBase64), Key: key}, "certificate file: malformed PEM block", nil},
		{"empty key", pki.Files{Cert: cert}, "key file: no PEM private key block", nil},
		{"key file holds only a certificate", pki.Files{Cert: cert, Key: cert}, "key file: no PEM private key block", nil},
		{"key is junk", pki.Files{Cert: cert, Key: pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: []byte("junk")})}, "key file: parse PRIVATE KEY", nil},
		{"key then a block that doesn't decode", pki.Files{Cert: cert, Key: then(key, badBase64)}, "key file: malformed PEM block", nil},
		{"key can't sign", pki.Files{Cert: cert, Key: pkcs8PEM(t, x25519)}, "key file: unsupported private key type", nil},
		{"PKCS #8 encrypted key", pki.Files{Cert: cert, Key: pem.EncodeToMemory(&pem.Block{Type: "ENCRYPTED PRIVATE KEY", Bytes: []byte("x")})}, "key file:", pki.ErrEncryptedKey},
		{"legacy encrypted key", pki.Files{Cert: cert, Key: pem.EncodeToMemory(&pem.Block{
			Type:    "RSA PRIVATE KEY",
			Headers: map[string]string{"Proc-Type": "4,ENCRYPTED", "DEK-Info": "AES-128-CBC,00000000000000000000000000000000"},
			Bytes:   []byte("x"),
		})}, "key file:", pki.ErrEncryptedKey},
		{"chain isn't PEM", pki.Files{Cert: cert, Key: key, Chain: []byte("not a chain")}, "chain file: no PEM CERTIFICATE block", nil},
		{"chain is junk", pki.Files{Cert: cert, Key: key, Chain: junkCert}, "chain file: parse certificate 1", nil},
		{"chain with a block that doesn't decode", pki.Files{Cert: cert, Key: key, Chain: then(cert, badBase64, cert)}, "chain file: malformed PEM block", nil},
		{"chain with a mismatched END line", pki.Files{Cert: cert, Key: key, Chain: then(cert, bytes.Replace(cert, []byte("END CERTIFICATE"), []byte("END X509 CRL"), 1))}, "chain file: malformed PEM block", nil},
	}
	for _, tt := range tests {
		_, err := pki.Validate(tt.f, "localhost", t0)
		if err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("%s: error = %v, want it to contain %q", tt.name, err, tt.want)
		}
		if tt.wantErr != nil && !errors.Is(err, tt.wantErr) {
			t.Errorf("%s: error = %v, want it to wrap %v", tt.name, err, tt.wantErr)
		}
	}
}

func newECKey(t *testing.T) *ecdsa.PrivateKey {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func pkcs8PEM(t *testing.T, key any) []byte {
	t.Helper()
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
}

// selfSigned makes a PEM certificate for localhost, standing in for one an
// operator provides.
func selfSigned(t *testing.T, key crypto.Signer, notBefore, notAfter time.Time) []byte {
	t.Helper()
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "localhost"},
		NotBefore:    notBefore,
		NotAfter:     notAfter,
		DNSNames:     []string{"localhost"},
		IPAddresses:  []net.IP{net.IPv4(127, 0, 0, 1)},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, key.Public(), key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}
