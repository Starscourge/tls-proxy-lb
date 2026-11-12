// Package tlsx implements certificate loading, SNI certificate selection and
// the proxy's TLS settings: TLS 1.2 minimum, TLS 1.2 suites pinned to ECDHE
// with AEAD ciphers, ALPN http/1.1 and session tickets.
package tlsx
