// SPDX-License-Identifier: AGPL-3.0-only

package certs

import (
	"crypto/x509"
	"encoding/pem"
	"io"
	"log/slog"
	"testing"
	"time"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestGenerateServerCert(t *testing.T) {
	t.Parallel()

	// Arrange
	sans := []string{"vps.example.com", "203.0.113.7"}
	notBefore := time.Now()

	// Act
	certPEM, keyPEM, err := GenerateServerCert(sans, notBefore)

	// Assert
	if err != nil {
		t.Fatalf("GenerateServerCert() unexpected error: %v", err)
	}

	block, _ := pem.Decode(certPEM)
	if block == nil || block.Type != pemTypeCertificate {
		t.Fatalf("certificate PEM block = %v, want %s", block, pemTypeCertificate)
	}
	leaf, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("parse certificate: %v", err)
	}

	if len(leaf.DNSNames) != 1 || leaf.DNSNames[0] != "vps.example.com" {
		t.Errorf("DNSNames = %v, want [vps.example.com]", leaf.DNSNames)
	}
	if len(leaf.IPAddresses) != 1 || leaf.IPAddresses[0].String() != "203.0.113.7" {
		t.Errorf("IPAddresses = %v, want [203.0.113.7]", leaf.IPAddresses)
	}

	wantNotAfter := notBefore.Add(serverCertValidity)
	if diff := leaf.NotAfter.Sub(wantNotAfter); diff > time.Minute || diff < -time.Minute {
		t.Errorf("NotAfter = %v, want ~%v (398 days is the mobile TLS leaf maximum)",
			leaf.NotAfter, wantNotAfter)
	}
	if leaf.IsCA {
		t.Error("IsCA = true, want false for a server leaf")
	}
	if leaf.KeyUsage&x509.KeyUsageDigitalSignature == 0 {
		t.Error("KeyUsage lacks DigitalSignature")
	}
	if len(leaf.ExtKeyUsage) != 1 || leaf.ExtKeyUsage[0] != x509.ExtKeyUsageServerAuth {
		t.Errorf("ExtKeyUsage = %v, want [ServerAuth]", leaf.ExtKeyUsage)
	}
	if leaf.SerialNumber.BitLen() == 0 {
		t.Error("SerialNumber is zero, want random entropy")
	}

	// The key must be PKCS#8 — the portable form across client platforms.
	keyBlock, _ := pem.Decode(keyPEM)
	if keyBlock == nil || keyBlock.Type != pemTypePrivateKey {
		t.Fatalf("key PEM block = %v, want %s", keyBlock, pemTypePrivateKey)
	}
	if _, err := x509.ParsePKCS8PrivateKey(keyBlock.Bytes); err != nil {
		t.Errorf("key is not valid PKCS#8: %v", err)
	}
}

func TestGenerateServerCertRejectsEmptySANs(t *testing.T) {
	t.Parallel()

	// Arrange / Act
	_, _, err := GenerateServerCert(nil, time.Now())

	// Assert — a certificate with no SAN matches nothing and would fail on the
	// phone rather than at startup.
	if err == nil {
		t.Fatal("GenerateServerCert(nil) error = nil, want a failure")
	}
}

func TestSplitSANs(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		in       []string
		wantDNS  int
		wantAddr int
	}{
		{name: "dns only", in: []string{"a.example.com", "b.example.com"}, wantDNS: 2},
		{name: "ip only", in: []string{"203.0.113.7", "198.51.100.4"}, wantAddr: 2},
		{name: "mixed", in: []string{"a.example.com", "203.0.113.7"}, wantDNS: 1, wantAddr: 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			// Act
			dns, ips := splitSANs(tt.in)

			// Assert
			if len(dns) != tt.wantDNS {
				t.Errorf("len(dnsNames) = %d, want %d", len(dns), tt.wantDNS)
			}
			if len(ips) != tt.wantAddr {
				t.Errorf("len(ips) = %d, want %d", len(ips), tt.wantAddr)
			}
		})
	}
}
