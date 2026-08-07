package certs

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"time"
)

// File names and modes within $DEVMON_STATE_DIR/certs.
const (
	serverCertFile = "server.crt"
	serverKeyFile  = "server.key"

	certsDirMode      = 0o700
	serverKeyFileMode = 0o600
	serverCrtFileMode = 0o644
)

// LoadOrCreateServerCert returns the persisted server keypair, generating and
// writing one if it is absent.
//
// sanDrift reports that the stored certificate does not cover every address in
// sans — typically because the VPS changed address. Phase 1 only reports it;
// re-issuance needs the CA that arrives in Phase 2. Treating drift as fatal here
// would make an address change unrecoverable without deleting the state
// directory, which is the exact outcome the durability guarantee exists to
// prevent.
func LoadOrCreateServerCert(dir string, sans []string, log *slog.Logger) (tls.Certificate, bool, error) {
	if err := os.MkdirAll(dir, certsDirMode); err != nil {
		return tls.Certificate{}, false, fmt.Errorf("create certs dir %s: %w", dir, err)
	}

	certPath := filepath.Join(dir, serverCertFile)
	keyPath := filepath.Join(dir, serverKeyFile)

	if err := ensureKeypair(certPath, keyPath, sans, log); err != nil {
		return tls.Certificate{}, false, err
	}

	pair, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		return tls.Certificate{}, false, fmt.Errorf("load server keypair from %s: %w", dir, err)
	}

	leaf, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		return tls.Certificate{}, false, fmt.Errorf("parse server certificate in %s: %w", dir, err)
	}
	pair.Leaf = leaf

	drift := sanDrift(leaf, sans)
	if drift {
		log.Warn("server certificate does not cover every configured address",
			slog.Any("configured", sans),
			slog.Any("covered_dns", leaf.DNSNames),
			slog.Any("covered_ip", ipStrings(leaf.IPAddresses)),
		)
	}
	return pair, drift, nil
}

// ensureKeypair generates and writes a keypair when one is not already present.
// An existing pair is left untouched: silently minting a new identity on every
// start would unpair every device the moment Phase 2 lands.
func ensureKeypair(certPath, keyPath string, sans []string, log *slog.Logger) error {
	switch exists, err := keypairExists(certPath, keyPath); {
	case err != nil:
		return err
	case exists:
		return nil
	}

	log.Warn("no server certificate found, generating self-signed",
		slog.Any("sans", sans),
	)

	certPEM, keyPEM, err := GenerateServerCert(sans, time.Now())
	if err != nil {
		return err
	}

	// Writes go through an os.Root scoped to the certs directory, so a path can
	// never escape it — including via a symlink planted in the bind mount, which
	// is reachable by anyone with host access to the operator's state directory.
	dir := filepath.Dir(keyPath)
	root, err := os.OpenRoot(dir)
	if err != nil {
		return fmt.Errorf("open certs dir %s: %w", dir, err)
	}
	defer func() { _ = root.Close() }()

	// The key is written first and exclusively. O_EXCL with mode 0600 means the
	// file is never briefly world-readable — unlike WriteFile-then-Chmod, which
	// leaves exactly that window and is flagged by gosec G306 — and two racing
	// starts cannot interleave their writes.
	if err := writeExclusive(root, serverKeyFile, keyPEM, serverKeyFileMode); err != nil {
		return err
	}
	if err := writeExclusive(root, serverCertFile, certPEM, serverCrtFileMode); err != nil {
		// Leaving a key without its certificate would trip the "not both
		// present" check on the next start, which is a safe but confusing place
		// to land. If the cleanup itself fails, say so — that message is the
		// only thing connecting the orphaned key to what actually happened.
		if rmErr := root.Remove(serverKeyFile); rmErr != nil {
			log.Warn("could not remove the orphaned server key after a failed certificate write",
				slog.Any("err", rmErr))
		}
		return err
	}
	return nil
}

func keypairExists(certPath, keyPath string) (bool, error) {
	certErr := statExists(certPath)
	keyErr := statExists(keyPath)

	switch {
	case certErr == nil && keyErr == nil:
		return true, nil
	case errors.Is(certErr, fs.ErrNotExist) && errors.Is(keyErr, fs.ErrNotExist):
		return false, nil
	case certErr != nil && !errors.Is(certErr, fs.ErrNotExist):
		return false, fmt.Errorf("stat %s: %w", certPath, certErr)
	case keyErr != nil && !errors.Is(keyErr, fs.ErrNotExist):
		return false, fmt.Errorf("stat %s: %w", keyPath, keyErr)
	default:
		// Exactly one half is present. Regenerating would produce a mismatched
		// pair, so this is a loud failure for the operator to resolve.
		return false, fmt.Errorf(
			"server certificate and key are not both present in %s; remove the remaining file to regenerate",
			filepath.Dir(certPath))
	}
}

func statExists(path string) error {
	_, err := os.Stat(path)
	return err
}

// writeExclusive creates name under root and writes data, failing if the file
// already exists.
func writeExclusive(root *os.Root, name string, data []byte, mode os.FileMode) error {
	f, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return fmt.Errorf("create %s: %w", name, err)
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return fmt.Errorf("write %s: %w", name, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close %s: %w", name, err)
	}
	return nil
}

// sanDrift reports whether any configured address is not covered by the leaf.
func sanDrift(leaf *x509.Certificate, sans []string) bool {
	for _, s := range sans {
		if leaf.VerifyHostname(s) != nil {
			return true
		}
	}
	return false
}

func ipStrings(ips []net.IP) []string {
	out := make([]string, 0, len(ips))
	for _, ip := range ips {
		out = append(out, ip.String())
	}
	return out
}
