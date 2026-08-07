// Package tlsconf builds the TLS configuration for the agent's single
// listening port.
package tlsconf

import (
	"crypto/tls"
	"crypto/x509"
)

// Build returns the TLS config for the agent's single listening port.
//
// clientCAs is the agent's own certificate authority pool (internal/certs),
// used to verify a device's client certificate at the handshake. It is nil
// only in tests that do not exercise mTLS.
//
// When clientCAs is non-nil the mode is VerifyClientCertIfGiven, deliberately
// NOT RequireAndVerifyClientCert. ClientAuth is a property of the LISTENER,
// settled during the TLS handshake, before any HTTP request line is parsed —
// Go's crypto/tls offers no per-route control. The agent must serve
// mTLS-protected routes and GET /v1/status, which carries no credential, on one
// port; RequireAndVerifyClientCert would terminate the status request at the
// handshake, making the endpoint unreachable in exactly the situation it exists
// to diagnose.
//
// Authentication strength is unchanged by this. A presented certificate is still
// fully verified against clientCAs and a bad one is still rejected at the
// handshake. Only the enforcement point moves, from the handshake to the
// requireDevice middleware, and only for one allowlisted path.
func Build(cert tls.Certificate, clientCAs *x509.CertPool) *tls.Config {
	cfg := &tls.Config{
		Certificates: []tls.Certificate{cert},

		// Both peers are ours, so there is nothing to negotiate down to. TLS 1.3
		// removes the entire downgrade and cipher-negotiation surface, and
		// Android has supported it since API 29.
		//
		// CipherSuites is deliberately unset: it is ignored for TLS 1.3.
		MinVersion: tls.VersionTLS13,

		NextProtos: []string{"h2", "http/1.1"},
	}

	if clientCAs != nil {
		cfg.ClientCAs = clientCAs
		cfg.ClientAuth = tls.VerifyClientCertIfGiven
	}
	return cfg
}
