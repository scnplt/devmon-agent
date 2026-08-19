// SPDX-License-Identifier: AGPL-3.0-only

// This file drives the error branches of the state package that the happy-path
// tests in the other *_test.go files never reach: closed-connection failures,
// row-scan failures on corrupted data, and constraint violations. Every trick
// here is portable (no reliance on POSIX file modes) since it must also pass
// on the Windows dev machine this package is built on.
package state

import (
	"context"
	"errors"
	"testing"
	"time"
)

// errScanner is a rowScanner whose Scan always fails, letting scanDevice and
// scanAuditEntry be exercised directly without needing a real corrupted row.
type errScanner struct{ err error }

func (e errScanner) Scan(dest ...any) error { return e.err }

func TestScanDevicePropagatesScanError(t *testing.T) {
	t.Parallel()

	// Arrange
	want := errors.New("boom")

	// Act
	_, err := scanDevice(errScanner{err: want})

	// Assert
	if !errors.Is(err, want) {
		t.Fatalf("scanDevice() error = %v, want %v", err, want)
	}
}

func TestScanAuditEntryPropagatesScanError(t *testing.T) {
	t.Parallel()

	// Arrange
	want := errors.New("boom")

	// Act
	_, err := scanAuditEntry(errScanner{err: want})

	// Assert
	if !errors.Is(err, want) {
		t.Fatalf("scanAuditEntry() error = %v, want %v", err, want)
	}
}

// closedStore returns a Store whose underlying *sql.DB is already closed, so
// every query and exec against it fails with "sql: database is closed" —
// the portable, driver-independent way to exercise a method's error-wrapping
// branch without depending on filesystem permissions.
func closedStore(t *testing.T) *Store {
	t.Helper()
	s := openStore(t, tempDBPath(t))
	if err := s.db.Close(); err != nil {
		t.Fatalf("close underlying db: %v", err)
	}
	return s
}

func TestMethodsFailWhenStoreIsClosed(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		fn   func(ctx context.Context, s *Store) error
	}{
		{"AppendAudit", func(ctx context.Context, s *Store) error {
			return s.AppendAudit(ctx, AuditEntry{Operation: "start", Outcome: OutcomeSuccess})
		}},
		{"ListAudit", func(ctx context.Context, s *Store) error {
			_, err := s.ListAudit(ctx, 10)
			return err
		}},
		{"CreateDevice", func(ctx context.Context, s *Store) error {
			_, err := s.CreateDevice(ctx, "device")
			return err
		}},
		{"RenameDevice", func(ctx context.Context, s *Store) error {
			return s.RenameDevice(ctx, "id", "name")
		}},
		{"DeleteDevice", func(ctx context.Context, s *Store) error {
			return s.DeleteDevice(ctx, "id")
		}},
		{"SupersedePriorCerts", func(ctx context.Context, s *Store) error {
			return s.SupersedePriorCerts(ctx, "id", "serial")
		}},
		{"DeviceByCertSerial", func(ctx context.Context, s *Store) error {
			_, err := s.DeviceByCertSerial(ctx, "serial")
			return err
		}},
		{"ListDevices", func(ctx context.Context, s *Store) error {
			_, err := s.ListDevices(ctx)
			return err
		}},
		{"RevokeDevice", func(ctx context.Context, s *Store) error {
			return s.RevokeDevice(ctx, "id")
		}},
		{"TouchDevice", func(ctx context.Context, s *Store) error {
			return s.TouchDevice(ctx, "id")
		}},
		{"MintPairingCode", func(ctx context.Context, s *Store) error {
			_, _, err := s.MintPairingCode(ctx, "device")
			return err
		}},
		{"RedeemPairingCode", func(ctx context.Context, s *Store) error {
			_, err := s.RedeemPairingCode(ctx, "code", "device")
			return err
		}},
		{"PrunePairingCodes", func(ctx context.Context, s *Store) error {
			_, err := s.PrunePairingCodes(ctx)
			return err
		}},
		{"SchemaVersion", func(ctx context.Context, s *Store) error {
			_, err := s.SchemaVersion(ctx)
			return err
		}},
		{"PruneAudit", func(ctx context.Context, s *Store) error {
			_, err := s.PruneAudit(ctx, time.Hour, 10)
			return err
		}},
		{"migrate", func(ctx context.Context, s *Store) error {
			return s.migrate(ctx, "closed-store")
		}},
		{"applyMigration", func(ctx context.Context, s *Store) error {
			return s.applyMigration(ctx, "closed-store", migrationStep{version: 99, sql: "SELECT 1"})
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			// Arrange
			s := closedStore(t)

			// Act
			err := tc.fn(context.Background(), s)

			// Assert
			if err == nil {
				t.Fatalf("%s() error = nil on a closed store, want an error", tc.name)
			}
		})
	}
}

func TestRecordDeviceCertForUnknownDeviceFailsForeignKey(t *testing.T) {
	t.Parallel()

	// Arrange — no device row was ever created for this id, and the DSN sets
	// _foreign_keys=1, so the insert must be rejected rather than silently
	// creating an orphaned certificate.
	s := openStore(t, tempDBPath(t))
	ctx := context.Background()
	now := time.Now()

	// Act
	err := s.RecordDeviceCert(ctx, "unknown-device-id", "serial-x", now, now.Add(time.Hour))

	// Assert
	if err == nil {
		t.Fatal("RecordDeviceCert() error = nil for an unknown device id, want a foreign key violation")
	}
}

func TestListAuditScanErrorOnCorruptRow(t *testing.T) {
	t.Parallel()

	// Arrange — bypass AppendAudit to plant a row whose occurred_at cannot
	// convert to int64, the shape a hand-edited or corrupted database takes.
	s := openStore(t, tempDBPath(t))
	ctx := context.Background()
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO audit (occurred_at, operation, outcome) VALUES ('not-a-number', 'start', 'success')`,
	)
	if err != nil {
		t.Fatalf("insert corrupt row: %v", err)
	}

	// Act
	_, err = s.ListAudit(ctx, 10)

	// Assert
	if err == nil {
		t.Fatal("ListAudit() error = nil for a row with a non-numeric occurred_at, want a scan error")
	}
}

func TestListDevicesScanErrorOnCorruptRow(t *testing.T) {
	t.Parallel()

	// Arrange — same corruption shape as above, on the devices table.
	s := openStore(t, tempDBPath(t))
	ctx := context.Background()
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO devices (id, name, paired_at) VALUES ('deadbeefdeadbeef', 'Corrupt', 'not-a-number')`,
	)
	if err != nil {
		t.Fatalf("insert corrupt row: %v", err)
	}

	// Act
	_, err = s.ListDevices(ctx)

	// Assert
	if err == nil {
		t.Fatal("ListDevices() error = nil for a row with a non-numeric paired_at, want a scan error")
	}
}

func TestRevokeDeviceUpdateFailsWhenColumnRenamed(t *testing.T) {
	t.Parallel()

	// Arrange — the EXISTS check RevokeDevice runs first never references
	// revoked_at, so renaming that column leaves the check passing and
	// isolates the failure to the UPDATE statement that follows it.
	s := openStore(t, tempDBPath(t))
	ctx := context.Background()
	device, err := s.CreateDevice(ctx, "Pixel 9")
	if err != nil {
		t.Fatalf("CreateDevice() unexpected error: %v", err)
	}
	if _, err := s.db.ExecContext(ctx,
		`ALTER TABLE devices RENAME COLUMN revoked_at TO revoked_at_renamed`,
	); err != nil {
		t.Fatalf("rename column: %v", err)
	}

	// Act
	err = s.RevokeDevice(ctx, device.ID)

	// Assert
	if err == nil {
		t.Fatal("RevokeDevice() error = nil after the revoked_at column was renamed, want an error")
	}
}

func TestPruneAuditFailsWhenAuditTableMissing(t *testing.T) {
	t.Parallel()

	// Arrange
	s := openStore(t, tempDBPath(t))
	ctx := context.Background()
	if _, err := s.db.ExecContext(ctx, `DROP TABLE audit`); err != nil {
		t.Fatalf("drop audit table: %v", err)
	}

	// Act
	_, err := s.PruneAudit(ctx, time.Hour, 10)

	// Assert
	if err == nil {
		t.Fatal("PruneAudit() error = nil after the audit table was dropped, want an error")
	}
}

func TestPruneAuditByCountFailsWhenIDColumnRenamed(t *testing.T) {
	t.Parallel()

	// Arrange — the age-based DELETE never references id, so it still
	// succeeds; only the count-based DELETE's subquery breaks.
	s := openStore(t, tempDBPath(t))
	ctx := context.Background()
	if _, err := s.db.ExecContext(ctx, `ALTER TABLE audit RENAME COLUMN id TO id_renamed`); err != nil {
		t.Fatalf("rename column: %v", err)
	}

	// Act
	_, err := s.PruneAudit(ctx, time.Hour, 10)

	// Assert
	if err == nil {
		t.Fatal("PruneAudit() error = nil after the audit id column was renamed, want an error")
	}
}

func TestApplyMigrationFailsOnInvalidSQL(t *testing.T) {
	t.Parallel()

	// Arrange
	s := openStore(t, tempDBPath(t))

	// Act
	err := s.applyMigration(context.Background(), tempDBPath(t),
		migrationStep{version: 99, sql: "THIS IS NOT VALID SQL"})

	// Assert
	if err == nil {
		t.Fatal("applyMigration() error = nil for invalid step SQL, want an error")
	}
}

func TestApplyMigrationFailsWhenSchemaMetaDropped(t *testing.T) {
	t.Parallel()

	// Arrange — the step's own SQL removes the table applyMigration then
	// tries to stamp, isolating the failure to the stamping INSERT.
	s := openStore(t, tempDBPath(t))

	// Act
	err := s.applyMigration(context.Background(), tempDBPath(t),
		migrationStep{version: 99, sql: "DROP TABLE schema_meta;"})

	// Assert
	if err == nil {
		t.Fatal("applyMigration() error = nil when the migration drops schema_meta before stamping, want an error")
	}
}

func TestApplyMigrationFailsOnDeferredForeignKeyViolationAtCommit(t *testing.T) {
	t.Parallel()

	// Arrange — defer_foreign_keys postpones the check to COMMIT, so the
	// step's own INSERT succeeds and only the transaction commit fails.
	s := openStore(t, tempDBPath(t))
	step := migrationStep{
		version: 99,
		sql: `PRAGMA defer_foreign_keys = ON;
			INSERT INTO device_certs (serial, device_id, not_before, not_after, issued_at)
			VALUES ('bogus-serial', 'missing-device', 0, 0, 0);`,
	}

	// Act
	err := s.applyMigration(context.Background(), tempDBPath(t), step)

	// Assert
	if err == nil {
		t.Fatal("applyMigration() error = nil for a deferred foreign key violation, want a commit error")
	}
}

func TestCurrentVersionOrZeroPropagatesNonNoRowsError(t *testing.T) {
	t.Parallel()

	// Arrange
	s := openStore(t, tempDBPath(t))
	ctx := context.Background()
	if _, err := s.db.ExecContext(ctx,
		`UPDATE schema_meta SET value = 'one' WHERE key = ?`, schemaVersionKey,
	); err != nil {
		t.Fatalf("corrupt version: %v", err)
	}

	// Act
	_, err := s.currentVersionOrZero(ctx)

	// Assert
	if !errors.Is(err, ErrStateCorrupt) {
		t.Fatalf("currentVersionOrZero() error = %v, want ErrStateCorrupt", err)
	}
}

func TestMigratePropagatesCurrentVersionOrZeroError(t *testing.T) {
	t.Parallel()

	// Arrange
	s := openStore(t, tempDBPath(t))
	ctx := context.Background()
	if _, err := s.db.ExecContext(ctx,
		`UPDATE schema_meta SET value = 'one' WHERE key = ?`, schemaVersionKey,
	); err != nil {
		t.Fatalf("corrupt version: %v", err)
	}

	// Act
	err := s.migrate(ctx, "corrupt-store")

	// Assert
	if !errors.Is(err, ErrStateCorrupt) {
		t.Fatalf("migrate() error = %v, want ErrStateCorrupt", err)
	}
}

func TestPrunerRunTicksAndPrunes(t *testing.T) {
	t.Parallel()

	// Arrange — a short interval, constructed directly (whitebox, in-package)
	// rather than through NewPruner, since the production interval is a fixed
	// six hours and must not become a test-only knob.
	s := openStore(t, tempDBPath(t))
	ctx := context.Background()
	old := time.Now().Add(-48 * time.Hour)
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO audit (occurred_at, operation, outcome) VALUES (?, 'read', 'allowed')`, old.Unix(),
	); err != nil {
		t.Fatalf("insert stale row: %v", err)
	}
	p := &Pruner{store: s, maxAge: 24 * time.Hour, maxRows: 100, interval: 10 * time.Millisecond, log: testLogger()}
	runCtx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)

	// Act
	go func() { done <- p.Run(runCtx) }()
	waitForRowCount(t, s, 0, 2*time.Second) // wait for the first tick to prune the stale row
	cancel()

	// Assert
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Run() = %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run() did not return after the context was cancelled")
	}
	var remaining int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM audit`).Scan(&remaining); err != nil {
		t.Fatalf("count: %v", err)
	}
	if remaining != 0 {
		t.Errorf("remaining rows = %d after Run() ticked, want 0", remaining)
	}
}

func TestPrunerPruneOnceDoesNotPanicWhenStoreClosed(t *testing.T) {
	t.Parallel()

	// Arrange — pruneOnce only logs a failed prune; it must not propagate
	// or panic, since the pruner keeps serving on the next tick regardless.
	s := closedStore(t)
	p := NewPruner(s, 24*time.Hour, 10, testLogger())

	// Act / Assert
	p.pruneOnce(context.Background())
}
