package state

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"time"
)

// pairingCodeTTL bounds how long a minted pairing code stays redeemable.
// Ten minutes is long enough for an operator to read the code aloud or type
// it on a phone, short enough that a leaked, unused code stops mattering
// quickly — rate limiting is Phase 6, so the code's own lifetime is part of
// what keeps it safe until then (D3).
const pairingCodeTTL = 10 * time.Minute

// MintPairingCode generates a single-use pairing code for deviceName and
// persists only its hex SHA-256 (D4) — the plaintext is never written to
// disk and is unrecoverable once this call returns. The caller must show it
// to the operator once and never log it.
func (s *Store) MintPairingCode(ctx context.Context, deviceName string) (string, time.Time, error) {
	code := rand.Text()
	hash := sha256.Sum256([]byte(code))
	codeHash := hex.EncodeToString(hash[:])

	now := time.Now()
	expiresAt := now.Add(pairingCodeTTL)

	_, err := s.db.ExecContext(ctx,
		`INSERT INTO pairing_codes (code_hash, device_name, created_at, expires_at)
		 VALUES (?, ?, ?, ?)`,
		codeHash, deviceName, now.Unix(), expiresAt.Unix(),
	)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("mint pairing code: %w", err)
	}

	return code, expiresAt.UTC(), nil
}

// RedeemPairingCode atomically marks code as used by deviceID and returns
// the device name it was minted for.
//
// The single-use guarantee lives entirely in this one conditional UPDATE,
// not in a read-then-write pair: a SELECT followed by an UPDATE is a TOCTOU
// race that two simultaneous redemptions of a leaked code would win. The
// DSN's _txlock=immediate (store.go) plus the used_at IS NULL predicate is
// what makes this atomic. RETURNING folds the "read the name" and "mark it
// used" steps into a single statement, so there is no window between them
// for a second caller to observe the code as still unused.
func (s *Store) RedeemPairingCode(ctx context.Context, code, deviceID string) (string, error) {
	hash := sha256.Sum256([]byte(code))
	codeHash := hex.EncodeToString(hash[:])
	now := time.Now().Unix()

	var deviceName string
	err := s.db.QueryRowContext(ctx,
		`UPDATE pairing_codes SET used_at = ?, used_by = ?
		 WHERE code_hash = ? AND used_at IS NULL AND expires_at > ?
		 RETURNING device_name`,
		now, deviceID, codeHash, now,
	).Scan(&deviceName)
	if errors.Is(err, sql.ErrNoRows) {
		// Unknown, expired, and already-used codes are indistinguishable here
		// on purpose: separate errors would let a caller enumerate code state.
		return "", ErrPairingCodeInvalid
	}
	if err != nil {
		return "", fmt.Errorf("redeem pairing code: %w", err)
	}

	return deviceName, nil
}

// PrunePairingCodes removes expired pairing codes and returns the number of
// rows deleted. It runs independently of whether the code was ever
// redeemed — an expired, unused code is just as unwanted as an expired,
// used one.
func (s *Store) PrunePairingCodes(ctx context.Context) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM pairing_codes WHERE expires_at < ?`, time.Now().Unix(),
	)
	if err != nil {
		return 0, fmt.Errorf("prune pairing codes: %w", err)
	}

	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("prune pairing codes: rows affected: %w", err)
	}
	return n, nil
}
