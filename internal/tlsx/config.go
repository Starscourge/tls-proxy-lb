package tlsx

import "crypto/tls"

// TLS12CipherSuites are the only suites offered for TLS 1.2: ECDHE key
// exchange, for forward secrecy, with an AEAD cipher. TLS 1.3 suites are not
// configurable in crypto/tls; all three of them are AEAD with (EC)DHE.
var TLS12CipherSuites = []uint16{
	tls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256,
	tls.TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384,
	tls.TLS_ECDHE_ECDSA_WITH_CHACHA20_POLY1305_SHA256,
	tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256,
	tls.TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384,
	tls.TLS_ECDHE_RSA_WITH_CHACHA20_POLY1305_SHA256,
}

// ALPN is the only application protocol offered. A client that offers ALPN
// without http/1.1 (only h2, say) fails the handshake with
// no_application_protocol.
const ALPN = "http/1.1"

// ServerConfig returns the server-side TLS settings. certs is called on
// every handshake and returns the current snapshot's certificates, so a
// config reload changes the certificates presented without a new
// tls.Config.
//
// One tls.Config should serve every TLS listener for the life of the
// process: its session ticket keys are generated on first use and rotated
// by crypto/tls, and a new tls.Config would invalidate every ticket issued
// so far.
//
// For a server name with no certificate and no default, GetCertificate
// returns nil with no error, which crypto/tls answers with an
// unrecognized_name alert. Certificates stays empty for that to hold.
func ServerConfig(certs func() *CertSet) *tls.Config {
	return &tls.Config{
		MinVersion:   tls.VersionTLS12,
		CipherSuites: TLS12CipherSuites,
		NextProtos:   []string{ALPN},
		GetCertificate: func(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
			s := certs()
			if s == nil {
				return nil, nil
			}
			return s.Lookup(hello.ServerName), nil
		},
	}
}
