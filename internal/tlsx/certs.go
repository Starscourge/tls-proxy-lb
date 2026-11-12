package tlsx

import (
	"crypto/tls"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

// CertSet holds the certificates of one config snapshot: one per route host
// and an optional default. It never changes after NewCertSet returns, so
// handshakes on any number of goroutines may read it without locks.
type CertSet struct {
	byHost map[string]*tls.Certificate
	def    *tls.Certificate
}

// NewCertSet returns a set serving byHost, keyed by normalised host name
// (lower case, no trailing dot), and def for every other name and for
// clients that send no SNI. def may be nil, in which case those handshakes
// fail. The map is not copied and must not be modified afterwards.
func NewCertSet(byHost map[string]*tls.Certificate, def *tls.Certificate) *CertSet {
	if byHost == nil {
		byHost = map[string]*tls.Certificate{}
	}
	return &CertSet{byHost: byHost, def: def}
}

// Len returns the number of host certificates, not counting the default.
func (s *CertSet) Len() int { return len(s.byHost) }

// HasDefault reports whether the set has a default certificate.
func (s *CertSet) HasDefault() bool { return s.def != nil }

// Has reports whether host has a certificate of its own.
func (s *CertSet) Has(host string) bool {
	_, ok := s.byHost[NormaliseName(host)]
	return ok
}

// Lookup returns the certificate for a ClientHello's server name: the
// host's own, else the default, else nil.
func (s *CertSet) Lookup(serverName string) *tls.Certificate {
	if serverName != "" {
		if c, ok := s.byHost[NormaliseName(serverName)]; ok {
			return c
		}
	}
	return s.def
}

// NormaliseName lower-cases ASCII letters and drops a trailing dot. It
// allocates only when the name has upper-case letters. crypto/tls already
// refuses a ClientHello whose server name ends in a dot but keeps its case.
func NormaliseName(name string) string {
	name = strings.TrimSuffix(name, ".")
	for i := 0; i < len(name); i++ {
		if c := name[i]; c >= 'A' && c <= 'Z' {
			return strings.ToLower(name)
		}
	}
	return name
}

// LoadPair loads a certificate chain and its private key from PEM files.
// If host is not empty, the leaf must be valid for it.
func LoadPair(certFile, keyFile, host string) (*tls.Certificate, error) {
	pair, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, err
	}
	if pair.Leaf == nil {
		return nil, errors.New("certificate has no parsed leaf")
	}
	if host != "" {
		if err := pair.Leaf.VerifyHostname(host); err != nil {
			return nil, fmt.Errorf("certificate is not valid for %q (names %v)", host, leafNames(&pair))
		}
	}
	return &pair, nil
}

func leafNames(c *tls.Certificate) []string {
	names := slices.Clone(c.Leaf.DNSNames)
	for _, ip := range c.Leaf.IPAddresses {
		names = append(names, ip.String())
	}
	if len(names) == 0 && c.Leaf.Subject.CommonName != "" {
		names = append(names, "CN="+c.Leaf.Subject.CommonName)
	}
	return names
}

// expiryWarning is how long before NotAfter a certificate is reported.
const expiryWarning = 30 * 24 * time.Hour

// Warnings describes certificates that are expired, not yet valid or due to
// expire within 30 days at now, sorted. They are reported rather than
// rejected: an expired certificate is still better served with a warning
// than refused at startup.
func (s *CertSet) Warnings(now time.Time) []string {
	var out []string
	check := func(name string, c *tls.Certificate) {
		leaf := c.Leaf
		switch {
		case leaf == nil:
		case now.After(leaf.NotAfter):
			out = append(out, fmt.Sprintf("%s: certificate expired at %s", name, leaf.NotAfter.UTC().Format(time.RFC3339)))
		case now.Before(leaf.NotBefore):
			out = append(out, fmt.Sprintf("%s: certificate not valid until %s", name, leaf.NotBefore.UTC().Format(time.RFC3339)))
		case leaf.NotAfter.Sub(now) < expiryWarning:
			out = append(out, fmt.Sprintf("%s: certificate expires at %s", name, leaf.NotAfter.UTC().Format(time.RFC3339)))
		}
	}
	for host, c := range s.byHost {
		check(host, c)
	}
	if s.def != nil {
		check("default", s.def)
	}
	slices.Sort(out)
	return out
}
