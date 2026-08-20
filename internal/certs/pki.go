// SPDX-License-Identifier: AGPL-3.0-only

// Package certs owns the agent's TLS material on disk: creating or loading a
// local CA on first start, issuing the server certificate and per-device
// client certificates from it, and reporting when the configured addresses
// have drifted away from what the server certificate covers.
//
// Nothing in this package ever logs key material, PEM bytes, or a file's
// contents — only SANs, serials, and expiry.
package certs

import (
	"net"
	"time"
)

const (
	// serverCertValidity is the CA/Browser Forum maximum for a leaf certificate.
	// It is not a round number by accident: modern mobile TLS stacks reject
	// longer-lived leaves outright, so a "convenient" 10-year self-signed cert
	// would fail on the client with an opaque error.
	serverCertValidity = 398 * 24 * time.Hour

	// certCommonName is cosmetic. Verification uses SANs; CN has been ignored by
	// every relevant TLS stack for years.
	certCommonName = "devmon-agent"

	certOrganization = "DevMon"

	// serialBits is the entropy in the certificate serial number.
	serialBits = 128
)

const (
	pemTypeCertificate = "CERTIFICATE"
	// PKCS#8 rather than SEC1 ("EC PRIVATE KEY"): both work with Go, but PKCS#8
	// is the portable form across client platforms.
	pemTypePrivateKey = "PRIVATE KEY"
)

// splitSANs sorts entries into IP addresses and DNS names. Config has already
// rejected anything that is neither.
func splitSANs(sans []string) (dnsNames []string, ips []net.IP) {
	for _, s := range sans {
		if ip := net.ParseIP(s); ip != nil {
			ips = append(ips, ip)
			continue
		}
		dnsNames = append(dnsNames, s)
	}
	return dnsNames, ips
}
