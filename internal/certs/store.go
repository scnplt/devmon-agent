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

// ErrIdentityIncomplete reports that the state directory holds some of the
// agent's identity but not all of it — the signature of a partial restore or
// a destroyed bind mount, not of a first install. See CheckIdentityConsistency.
var ErrIdentityIncomplete = errors.New("agent identity is incomplete")

// LoadOrCreateServerCert returns the persisted server keypair, issuing one
// from ca if none is present.
//
// When the stored certificate no longer covers every address in sans —
// typically because the VPS's address changed — it is re-issued from ca
// automatically (D10). Clients pin the CA, not this leaf, so re-issuance is
// invisible to them: no device has to re-pair.
func LoadOrCreateServerCert(dir string, sans []string, ca *CA, log *slog.Logger) (tls.Certificate, error) {
	if err := os.MkdirAll(dir, certsDirMode); err != nil {
		return tls.Certificate{}, fmt.Errorf("create certs dir %s: %w", dir, err)
	}

	certPath := filepath.Join(dir, serverCertFile)
	keyPath := filepath.Join(dir, serverKeyFile)

	if err := ensureKeypair(dir, certPath, keyPath, sans, ca, log); err != nil {
		return tls.Certificate{}, err
	}

	pair, leaf, err := loadServerKeypair(dir, certPath, keyPath)
	if err != nil {
		return tls.Certificate{}, err
	}

	if !sanDrift(leaf, sans) {
		return pair, nil
	}

	log.Warn("server certificate does not cover every configured address",
		slog.Any("configured", sans),
		slog.Any("covered_dns", leaf.DNSNames),
		slog.Any("covered_ip", ipStrings(leaf.IPAddresses)),
	)

	if err := reissueServerCert(dir, certPath, keyPath, sans, ca, log); err != nil {
		return tls.Certificate{}, err
	}

	pair, _, err = loadServerKeypair(dir, certPath, keyPath)
	if err != nil {
		return tls.Certificate{}, err
	}
	return pair, nil
}

// loadServerKeypair reads the persisted server keypair from disk and parses
// its leaf certificate.
func loadServerKeypair(dir, certPath, keyPath string) (tls.Certificate, *x509.Certificate, error) {
	pair, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		return tls.Certificate{}, nil, fmt.Errorf("load server keypair from %s: %w", dir, err)
	}

	leaf, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		return tls.Certificate{}, nil, fmt.Errorf("parse server certificate in %s: %w", dir, err)
	}
	pair.Leaf = leaf

	return pair, leaf, nil
}

// ensureKeypair issues and writes a keypair when one is not already present.
// An existing pair is left untouched: silently minting a new identity on
// every start would unpair every device.
func ensureKeypair(dir, certPath, keyPath string, sans []string, ca *CA, log *slog.Logger) error {
	switch exists, err := keypairExists(certPath, keyPath); {
	case err != nil:
		return err
	case exists:
		return nil
	}

	log.Warn("no server certificate found, issuing one from the certificate authority",
		slog.Any("sans", sans),
	)

	return writeServerCert(dir, sans, ca, log)
}

// reissueServerCert replaces a server certificate that no longer covers
// every configured address. Both files are removed before writing the new
// pair: writeExclusive's O_EXCL fails with "file exists" against a stale
// file, which would otherwise make an address change unrecoverable without
// deleting the whole state directory — the exact outcome D10 exists to
// prevent.
func reissueServerCert(dir, certPath, keyPath string, sans []string, ca *CA, log *slog.Logger) error {
	if err := os.Remove(certPath); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("remove stale server certificate %s: %w", certPath, err)
	}
	if err := os.Remove(keyPath); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("remove stale server key %s: %w", keyPath, err)
	}
	return writeServerCert(dir, sans, ca, log)
}

// writeServerCert issues a fresh server keypair from ca and writes it to
// dir, replacing whatever ensureKeypair or reissueServerCert already
// removed or found absent.
func writeServerCert(dir string, sans []string, ca *CA, log *slog.Logger) error {
	certPEM, keyPEM, err := ca.IssueServerCert(sans, time.Now())
	if err != nil {
		return err
	}

	// Writes go through an os.Root scoped to the certs directory, so a path
	// can never escape it — including via a symlink planted in the bind
	// mount, which is reachable by anyone with host access to the
	// operator's state directory.
	root, err := os.OpenRoot(dir)
	if err != nil {
		return fmt.Errorf("open certs dir %s: %w", dir, err)
	}
	defer func() { _ = root.Close() }()

	// The key is written first and exclusively. O_EXCL with mode 0600 means
	// the file is never briefly world-readable — unlike WriteFile-then-Chmod,
	// which leaves exactly that window and is flagged by gosec G306 — and
	// two racing starts cannot interleave their writes.
	if err := writeExclusive(root, serverKeyFile, keyPEM, serverKeyFileMode); err != nil {
		return err
	}
	if err := writeExclusive(root, serverCertFile, certPEM, serverCrtFileMode); err != nil {
		// Leaving a key without its certificate would trip the "not both
		// present" check on the next start, which is a safe but confusing
		// place to land. If the cleanup itself fails, say so — that message
		// is the only thing connecting the orphaned key to what actually
		// happened.
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
		// pair, so this is a loud failure for the operator to resolve. This is
		// a torn-write failure, distinct from ErrIdentityIncomplete below,
		// which reports a lost directory rather than an interrupted write.
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

// CheckIdentityConsistency implements D9's matrix over (state database
// present, certificate authority present):
//
//   - neither present: nil — a genuine first run.
//   - both present: nil — the normal running state.
//   - exactly one present: ErrIdentityIncomplete, naming which half is
//     missing. This is the signature of a partial restore or a destroyed
//     bind mount, not of a first install, and must never be treated as one:
//     silently minting a fresh identity here would be indistinguishable from
//     an attacker replacing the agent's identity outright.
//
// certsDir is the directory holding ca.crt/ca.key (CheckIdentityConsistency
// does not touch server.crt/server.key, which are re-derived from the CA and
// carry no independent identity). dbExisted reports whether the state
// database file was already on disk before this start (state.Store's
// FirstRun, negated).
func CheckIdentityConsistency(certsDir string, dbExisted bool) error {
	certPath := filepath.Join(certsDir, caCertFile)
	keyPath := filepath.Join(certsDir, caKeyFile)

	caExists, err := keypairExists(certPath, keyPath)
	if err != nil {
		return err
	}

	const reissueGuidance = "if you proceed by clearing the state directory, every paired device must re-pair"

	switch {
	case dbExisted == caExists:
		return nil
	case dbExisted && !caExists:
		return fmt.Errorf("%w: the state database exists but the certificate authority in %s does not; %s",
			ErrIdentityIncomplete, certsDir, reissueGuidance)
	default: // !dbExisted && caExists
		return fmt.Errorf("%w: the certificate authority in %s exists but the state database does not; %s",
			ErrIdentityIncomplete, certsDir, reissueGuidance)
	}
}
