// Package tlstest issues throwaway certificates for tests: an in-memory CA
// and ECDSA P-256 leaf certificates signed by it, as PEM or as files. It
// needs no openssl. Nothing here is fit for anything but tests.
package tlstest

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// CA is a certificate authority held in memory.
type CA struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
	// PEM is the CA certificate, PEM-encoded.
	PEM []byte
}

// NewCA creates a CA valid from an hour ago for a day.
func NewCA(t testing.TB) *CA {
	t.Helper()
	key := newKey(t)
	tmpl := &x509.Certificate{
		SerialNumber:          serial(t),
		Subject:               pkix.Name{CommonName: "tlstest CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		MaxPathLenZero:        true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return &CA{cert: cert, key: key, PEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})}
}

// Pool returns a certificate pool holding only the CA.
func (ca *CA) Pool() *x509.CertPool {
	p := x509.NewCertPool()
	p.AddCert(ca.cert)
	return p
}

// LeafOptions adjusts an issued certificate. The zero value gives one valid
// now for a day.
type LeafOptions struct {
	NotBefore, NotAfter time.Time
}

// Issue returns a PEM certificate and PEM key for a server certificate
// whose DNS SANs are hosts (wildcards such as "*.test" allowed).
func (ca *CA) Issue(t testing.TB, opts LeafOptions, hosts ...string) (certPEM, keyPEM []byte) {
	t.Helper()
	if opts.NotBefore.IsZero() {
		opts.NotBefore = time.Now().Add(-time.Hour)
	}
	if opts.NotAfter.IsZero() {
		opts.NotAfter = time.Now().Add(24 * time.Hour)
	}
	key := newKey(t)
	cn := ""
	if len(hosts) > 0 {
		cn = hosts[0]
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial(t),
		Subject:      pkix.Name{CommonName: cn},
		DNSNames:     hosts,
		NotBefore:    opts.NotBefore,
		NotAfter:     opts.NotAfter,
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		t.Fatal(err)
	}
	kder, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: kder})
}

// Pair returns a server certificate for hosts as a tls.Certificate.
func (ca *CA) Pair(t testing.TB, hosts ...string) tls.Certificate {
	t.Helper()
	c, k := ca.Issue(t, LeafOptions{}, hosts...)
	pair, err := tls.X509KeyPair(c, k)
	if err != nil {
		t.Fatal(err)
	}
	return pair
}

// WriteFiles issues a certificate for hosts and writes it to dir as
// name.crt and name.key (mode 0600), returning both paths.
func (ca *CA) WriteFiles(t testing.TB, dir, name string, opts LeafOptions, hosts ...string) (certFile, keyFile string) {
	t.Helper()
	c, k := ca.Issue(t, opts, hosts...)
	certFile, keyFile = filepath.Join(dir, name+".crt"), filepath.Join(dir, name+".key")
	if err := os.WriteFile(certFile, c, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyFile, k, 0o600); err != nil {
		t.Fatal(err)
	}
	return certFile, keyFile
}

// WriteCA writes the CA certificate to dir/ca.crt and returns the path.
func (ca *CA) WriteCA(t testing.TB, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "ca.crt")
	if err := os.WriteFile(path, ca.PEM, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// ClientConfig returns a client config that trusts only the CA and sends
// serverName as SNI (none if empty).
func (ca *CA) ClientConfig(serverName string) *tls.Config {
	return &tls.Config{RootCAs: ca.Pool(), ServerName: serverName}
}

func newKey(t testing.TB) *ecdsa.PrivateKey {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

func serial(t testing.TB) *big.Int {
	t.Helper()
	n, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	if err != nil {
		t.Fatal(err)
	}
	return n
}
