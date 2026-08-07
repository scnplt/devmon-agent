package tlsconf

import (
	"crypto/tls"
	"crypto/x509"
	"testing"
	"time"

	"github.com/scnplt/devmon-agent/internal/certs"
)

func testCert(t *testing.T) tls.Certificate {
	t.Helper()
	certPEM, keyPEM, err := certs.GenerateServerCert([]string{"vps.example.com"}, time.Now())
	if err != nil {
		t.Fatalf("generate test cert: %v", err)
	}
	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatalf("build keypair: %v", err)
	}
	return pair
}

func TestBuild(t *testing.T) {
	t.Parallel()

	cert := testCert(t)

	tests := []struct {
		name           string
		clientCAs      *x509.CertPool
		wantClientAuth tls.ClientAuthType
		wantPoolSet    bool
	}{
		{
			// Phase 1: no CA exists, so no client certificate can be verified.
			name:           "no CA leaves client auth off",
			clientCAs:      nil,
			wantClientAuth: tls.NoClientCert,
		},
		{
			// Phase 2 onward. RequireAndVerifyClientCert here would make
			// /v1/status unreachable at the handshake — see the Build doc.
			name:           "with a CA, a presented cert is verified but not required",
			clientCAs:      x509.NewCertPool(),
			wantClientAuth: tls.VerifyClientCertIfGiven,
			wantPoolSet:    true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			// Act
			cfg := Build(cert, tt.clientCAs)

			// Assert
			if cfg.ClientAuth != tt.wantClientAuth {
				t.Errorf("ClientAuth = %v, want %v", cfg.ClientAuth, tt.wantClientAuth)
			}
			if (cfg.ClientCAs != nil) != tt.wantPoolSet {
				t.Errorf("ClientCAs set = %v, want %v", cfg.ClientCAs != nil, tt.wantPoolSet)
			}
			if cfg.MinVersion != tls.VersionTLS13 {
				t.Errorf("MinVersion = %#x, want TLS 1.3 (%#x)", cfg.MinVersion, tls.VersionTLS13)
			}
			if len(cfg.Certificates) != 1 {
				t.Errorf("len(Certificates) = %d, want 1", len(cfg.Certificates))
			}
			if len(cfg.CipherSuites) != 0 {
				t.Error("CipherSuites is set; it is ignored for TLS 1.3 and must stay empty")
			}
			if cfg.InsecureSkipVerify {
				t.Error("InsecureSkipVerify = true, want false")
			}
		})
	}
}

func TestBuildAdvertisesHTTP2(t *testing.T) {
	t.Parallel()

	// Arrange / Act
	cfg := Build(testCert(t), nil)

	// Assert
	want := []string{"h2", "http/1.1"}
	if len(cfg.NextProtos) != len(want) {
		t.Fatalf("NextProtos = %v, want %v", cfg.NextProtos, want)
	}
	for i, p := range want {
		if cfg.NextProtos[i] != p {
			t.Errorf("NextProtos[%d] = %q, want %q", i, cfg.NextProtos[i], p)
		}
	}
}
