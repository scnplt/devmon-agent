// SPDX-License-Identifier: AGPL-3.0-only

// Package state owns the agent's durable store: the device registry, the audit
// trail, and the schema version that gates upgrades.
//
// The store is a SQLite database in WAL mode on the operator's bind mount.
// Callers never see *sql.DB or a SQL string — everything goes through methods on
// Store, each taking a context.Context first.
package state

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"strconv"
	"time"

	// Registers the pure-Go driver under the name "sqlite" (not "sqlite3").
	_ "modernc.org/sqlite"
)

// Sentinel errors for the two conditions a caller must branch on. Both are fatal
// at startup, because a loud, early, specific failure beats an obscure one at
// first query.
var (
	// ErrStateCorrupt means the file exists but is not a usable database — the
	// realistic outcome of a botched restore or a truncated copy.
	ErrStateCorrupt = errors.New("state store is unreadable or corrupt")
	// ErrSchemaTooNew means the store was written by a newer agent version.
	ErrSchemaTooNew = errors.New("state store was written by a newer agent version")
	// ErrDeviceNotFound means no device matches the given id or certificate
	// serial. Callers branch on this to distinguish "unknown" from other
	// failures; it never reveals whether the caller has ever seen that id.
	ErrDeviceNotFound = errors.New("device not found")
	// ErrPairingCodeInvalid covers every reason a pairing code cannot be
	// redeemed — unknown, expired, or already used. It is deliberately one
	// error: distinguishing those cases to a caller would let it enumerate
	// code state.
	ErrPairingCodeInvalid = errors.New("pairing code is invalid or expired")
)

// dbFileMode keeps the database owner-only. SQLite creates it 0644 by default,
// and it holds device records and certificate serials.
const dbFileMode = 0o600

// openTimeout bounds the whole open-and-migrate sequence so a wedged filesystem
// on the bind mount surfaces as a startup error instead of hanging forever.
const openTimeout = 30 * time.Second

// Store is the durable state repository.
type Store struct {
	db  *sql.DB
	log *slog.Logger

	// FirstRun is true when the database file did not exist before Open.
	//
	// Phase 2 hangs the "loud on missing identity" check off this: once a
	// CA exists, a first run on a mount that should already hold one means the
	// operator lost their state directory, and the agent must say so rather than
	// silently minting a new identity that unpairs every device.
	FirstRun bool
}

// Open opens or creates the state store at path, verifies it is readable, and
// brings it to the current schema version.
func Open(ctx context.Context, path string, log *slog.Logger) (*Store, error) {
	ctx, cancel := context.WithTimeout(ctx, openTimeout)
	defer cancel()

	// Detected BEFORE sql.Open: the driver creates the file lazily on first use,
	// so a check afterwards would always report "already existed".
	_, statErr := os.Stat(path)
	firstRun := errors.Is(statErr, fs.ErrNotExist)

	db, err := sql.Open("sqlite", dsn(path))
	if err != nil {
		return nil, fmt.Errorf("open state store at %s: %w", path, err)
	}

	// WAL allows one writer and many readers, but database/sql hands out pooled
	// connections opaquely, which produces intermittent SQLITE_BUSY under
	// concurrent writes. At a few writes per minute a single connection costs
	// nothing and removes the whole class of flake. Contention with the OTHER
	// process — the Phase 2 host-side CLI — is covered by _busy_timeout.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(0)

	s := &Store{db: db, log: log, FirstRun: firstRun}
	if err := s.verifyAndMigrate(ctx, path); err != nil {
		_ = db.Close()
		return nil, err
	}

	if err := tightenPermissions(path); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

// tightenPermissions restricts the database and its WAL sidecars to the owner.
//
// The sidecars matter as much as the main file: -wal holds committed pages that
// have not been checkpointed yet, so from Phase 2 onward it can contain device
// records that are absent from devmon.db itself. SQLite creates all three 0644.
// The 0700 state directory already covers this, but a directory mode is the
// operator's to get wrong — on a restored backup or a re-mounted volume — and
// defence in depth here costs two syscalls.
func tightenPermissions(dbPath string) error {
	for _, path := range []string{dbPath, dbPath + "-wal", dbPath + "-shm"} {
		// The sidecars exist only while a WAL connection is open, so a missing
		// one is normal rather than a fault.
		if err := os.Chmod(path, dbFileMode); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("tighten permissions on %s: %w", path, err)
		}
	}
	return nil
}

// dsn builds the connection string. Each pragma is load-bearing:
//
//   - WAL:               readers do not block the writer, which is what lets the
//     Phase 2 host-side CLI read while the agent writes.
//   - busy_timeout=5000: waits out contention with that other process rather
//     than failing the request immediately.
//   - txlock=immediate:  takes the write lock at BEGIN instead of at first
//     write, avoiding the deadlock where two processes open deferred
//     transactions and then both try to upgrade.
func dsn(path string) string {
	return "file:" + path +
		"?_journal_mode=WAL" +
		"&_busy_timeout=5000" +
		"&_foreign_keys=1" +
		"&_synchronous=NORMAL" +
		"&_txlock=immediate"
}

func (s *Store) verifyAndMigrate(ctx context.Context, path string) error {
	if err := s.db.PingContext(ctx); err != nil {
		return fmt.Errorf("%w: %s: %v", ErrStateCorrupt, path, err)
	}

	// Required in addition to Ping. A zero-length or truncated devmon.db is
	// accepted by Ping and then fails obscurely at the first real query.
	var result string
	if err := s.db.QueryRowContext(ctx, `PRAGMA integrity_check`).Scan(&result); err != nil {
		return fmt.Errorf("%w: %s: %v", ErrStateCorrupt, path, err)
	}
	if result != "ok" {
		return fmt.Errorf("%w: %s: integrity_check=%q", ErrStateCorrupt, path, result)
	}

	return s.migrate(ctx, path)
}

// migrate walks the migration ladder, applying every step whose version
// exceeds the store's current one, in order.
//
// schema_meta is created unconditionally first so its version can be read
// even on a brand-new file where no migration has run yet; every table each
// migration owns is created inside that migration's own step.
func (s *Store) migrate(ctx context.Context, path string) error {
	if _, err := s.db.ExecContext(ctx, schemaMetaTableSQL); err != nil {
		return fmt.Errorf("create schema_meta table in %s: %w", path, err)
	}

	version, err := s.currentVersionOrZero(ctx)
	if err != nil {
		return err
	}
	if version > currentSchemaVersion {
		return fmt.Errorf("%w: %s: found v%d, this build understands v%d",
			ErrSchemaTooNew, path, version, currentSchemaVersion)
	}

	for _, step := range migrations {
		if step.version <= version {
			continue
		}
		if err := s.applyMigration(ctx, path, step); err != nil {
			return err
		}
	}
	return nil
}

// currentVersionOrZero reads the stamped schema version, treating an absent
// schema_meta row (a genuinely fresh store) as version 0 rather than an error.
func (s *Store) currentVersionOrZero(ctx context.Context) (int, error) {
	version, err := s.SchemaVersion(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	return version, nil
}

// applyMigration runs one migration step's schema statements and stamps its
// version in the same transaction, mirroring the PruneAudit pattern: a
// deferred rollback before commit, so a failed statement or a failed stamp
// leaves the store at its prior, consistent version.
func (s *Store) applyMigration(ctx context.Context, path string, step migrationStep) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin migration to v%d on %s: %w", step.version, path, err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, step.sql); err != nil {
		return fmt.Errorf("apply schema v%d to %s: %w", step.version, path, err)
	}

	_, err = tx.ExecContext(ctx,
		`INSERT INTO schema_meta (key, value) VALUES (?, ?)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
		schemaVersionKey, strconv.Itoa(step.version),
	)
	if err != nil {
		return fmt.Errorf("stamp schema version v%d on %s: %w", step.version, path, err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit migration to v%d on %s: %w", step.version, path, err)
	}
	return nil
}

// SchemaVersion returns the schema version recorded in the store.
func (s *Store) SchemaVersion(ctx context.Context) (int, error) {
	var raw string
	err := s.db.QueryRowContext(ctx,
		`SELECT value FROM schema_meta WHERE key = ?`, schemaVersionKey,
	).Scan(&raw)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, err
		}
		return 0, fmt.Errorf("read schema version: %w", err)
	}

	version, convErr := strconv.Atoi(raw)
	if convErr != nil {
		return 0, fmt.Errorf("%w: schema version %q is not a number", ErrStateCorrupt, raw)
	}
	return version, nil
}

// PruneAudit enforces both retention bounds in one transaction and returns the
// number of rows removed.
//
// Age and row count are applied together, not as alternatives: whichever bites
// first is the one that limits growth, which is what "bounded by both age and
// size" means here.
func (s *Store) PruneAudit(ctx context.Context, maxAge time.Duration, maxRows int) (int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin audit prune: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	cutoff := time.Now().Add(-maxAge).Unix()
	byAge, err := tx.ExecContext(ctx, `DELETE FROM audit WHERE occurred_at < ?`, cutoff)
	if err != nil {
		return 0, fmt.Errorf("prune audit by age: %w", err)
	}

	byCount, err := tx.ExecContext(ctx,
		`DELETE FROM audit WHERE id NOT IN (SELECT id FROM audit ORDER BY id DESC LIMIT ?)`,
		maxRows,
	)
	if err != nil {
		return 0, fmt.Errorf("prune audit by row count: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit audit prune: %w", err)
	}

	ageRows, _ := byAge.RowsAffected()
	countRows, _ := byCount.RowsAffected()
	return ageRows + countRows, nil
}

// Close releases the database handle.
func (s *Store) Close() error {
	if err := s.db.Close(); err != nil {
		return fmt.Errorf("close state store: %w", err)
	}
	return nil
}
