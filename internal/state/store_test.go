package state

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"sync"
	"testing"
	"time"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func tempDBPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "devmon.db")
}

func openStore(t *testing.T, path string) *Store {
	t.Helper()
	s, err := Open(context.Background(), path, testLogger())
	if err != nil {
		t.Fatalf("Open(%s) unexpected error: %v", path, err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestOpenFirstRunCreatesSchema(t *testing.T) {
	t.Parallel()

	// Arrange
	path := tempDBPath(t)

	// Act
	s := openStore(t, path)
	version, err := s.SchemaVersion(context.Background())

	// Assert
	if err != nil {
		t.Fatalf("SchemaVersion() unexpected error: %v", err)
	}
	if !s.FirstRun {
		t.Error("FirstRun = false on an empty directory, want true")
	}
	if version != currentSchemaVersion {
		t.Errorf("SchemaVersion() = %d, want %d", version, currentSchemaVersion)
	}
	for _, table := range []string{"schema_meta", "devices", "device_certs", "pairing_codes", "audit"} {
		var name string
		err := s.db.QueryRow(
			`SELECT name FROM sqlite_master WHERE type='table' AND name=?`, table,
		).Scan(&name)
		if err != nil {
			t.Errorf("table %q missing: %v", table, err)
		}
	}
}

func TestOpenReopenIsNotFirstRun(t *testing.T) {
	t.Parallel()

	// Arrange
	path := tempDBPath(t)
	first, err := Open(context.Background(), path, testLogger())
	if err != nil {
		t.Fatalf("first Open() unexpected error: %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("Close() unexpected error: %v", err)
	}

	// Act
	second := openStore(t, path)
	version, err := second.SchemaVersion(context.Background())

	// Assert
	if err != nil {
		t.Fatalf("SchemaVersion() unexpected error: %v", err)
	}
	if second.FirstRun {
		t.Error("FirstRun = true on reopen, want false; identity would look lost every restart")
	}
	if version != currentSchemaVersion {
		t.Errorf("SchemaVersion() = %d, want %d", version, currentSchemaVersion)
	}
}

func TestOpenCorruptFile(t *testing.T) {
	t.Parallel()

	// Arrange — the shape of a botched restore: a file that exists and is not a
	// database. Ping alone accepts this; integrity_check is what catches it.
	path := tempDBPath(t)
	if err := os.WriteFile(path, []byte("not a database, just 32 bytes.xx"), 0o600); err != nil {
		t.Fatalf("write garbage: %v", err)
	}

	// Act
	_, err := Open(context.Background(), path, testLogger())

	// Assert
	if !errors.Is(err, ErrStateCorrupt) {
		t.Fatalf("Open() error = %v, want ErrStateCorrupt", err)
	}
}

func TestOpenSchemaTooNew(t *testing.T) {
	t.Parallel()

	// Arrange — a store stamped by a hypothetical future agent.
	path := tempDBPath(t)
	seed, err := Open(context.Background(), path, testLogger())
	if err != nil {
		t.Fatalf("seed Open() unexpected error: %v", err)
	}
	_, err = seed.db.Exec(`UPDATE schema_meta SET value = '99' WHERE key = ?`, schemaVersionKey)
	if err != nil {
		t.Fatalf("bump version: %v", err)
	}
	if err := seed.Close(); err != nil {
		t.Fatalf("Close() unexpected error: %v", err)
	}

	// Act
	_, err = Open(context.Background(), path, testLogger())

	// Assert
	if !errors.Is(err, ErrSchemaTooNew) {
		t.Fatalf("Open() error = %v, want ErrSchemaTooNew", err)
	}
}

func TestOpenMigratesV1StoreToV2(t *testing.T) {
	t.Parallel()

	// Arrange — build a v1 store by hand, exec'ing schemaV1 directly and
	// stamping version 1, bypassing Open's own migration ladder entirely.
	path := tempDBPath(t)
	raw, err := sql.Open("sqlite", dsn(path))
	if err != nil {
		t.Fatalf("sql.Open(%s) unexpected error: %v", path, err)
	}
	if _, err := raw.Exec(schemaMetaTableSQL); err != nil {
		t.Fatalf("create schema_meta: %v", err)
	}
	if _, err := raw.Exec(schemaV1); err != nil {
		t.Fatalf("apply schemaV1: %v", err)
	}
	if _, err := raw.Exec(
		`INSERT INTO schema_meta (key, value) VALUES (?, ?)`, schemaVersionKey, "1",
	); err != nil {
		t.Fatalf("stamp version 1: %v", err)
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("close seed connection: %v", err)
	}

	// Act
	s := openStore(t, path)
	version, err := s.SchemaVersion(context.Background())

	// Assert
	if err != nil {
		t.Fatalf("SchemaVersion() unexpected error: %v", err)
	}
	if version != currentSchemaVersion {
		t.Errorf("SchemaVersion() = %d, want %d after migrating a v1 store", version, currentSchemaVersion)
	}
	for _, table := range []string{"schema_meta", "devices", "device_certs", "pairing_codes", "audit"} {
		var name string
		err := s.db.QueryRow(
			`SELECT name FROM sqlite_master WHERE type='table' AND name=?`, table,
		).Scan(&name)
		if err != nil {
			t.Errorf("table %q missing after migration: %v", table, err)
		}
	}

	// The schema_meta row itself must survive the migration, not just be
	// re-created — a fresh INSERT with an unchanged value would hide a
	// migration that failed to actually stamp anything.
	var stamped string
	if err := s.db.QueryRow(
		`SELECT value FROM schema_meta WHERE key = ?`, schemaVersionKey,
	).Scan(&stamped); err != nil {
		t.Fatalf("read stamped version: %v", err)
	}
	if stamped != strconv.Itoa(currentSchemaVersion) {
		t.Errorf("stamped schema_meta value = %q, want %q", stamped, strconv.Itoa(currentSchemaVersion))
	}
}

func TestOpenSchemaVersionThreeIsTooNew(t *testing.T) {
	t.Parallel()

	// Arrange — a store one version ahead of what this build understands,
	// distinct from the far-future TestOpenSchemaTooNew case above.
	path := tempDBPath(t)
	seed, err := Open(context.Background(), path, testLogger())
	if err != nil {
		t.Fatalf("seed Open() unexpected error: %v", err)
	}
	_, err = seed.db.Exec(`UPDATE schema_meta SET value = '3' WHERE key = ?`, schemaVersionKey)
	if err != nil {
		t.Fatalf("bump version: %v", err)
	}
	if err := seed.Close(); err != nil {
		t.Fatalf("Close() unexpected error: %v", err)
	}

	// Act
	_, err = Open(context.Background(), path, testLogger())

	// Assert
	if !errors.Is(err, ErrSchemaTooNew) {
		t.Fatalf("Open() error = %v, want ErrSchemaTooNew", err)
	}
}

func TestSchemaVersionNonNumericIsCorrupt(t *testing.T) {
	t.Parallel()

	// Arrange
	path := tempDBPath(t)
	s := openStore(t, path)
	if _, err := s.db.Exec(`UPDATE schema_meta SET value = 'one' WHERE key = ?`, schemaVersionKey); err != nil {
		t.Fatalf("corrupt version: %v", err)
	}

	// Act
	_, err := s.SchemaVersion(context.Background())

	// Assert
	if !errors.Is(err, ErrStateCorrupt) {
		t.Fatalf("SchemaVersion() error = %v, want ErrStateCorrupt", err)
	}
}

func TestOpenDBFileIsOwnerOnly(t *testing.T) {
	t.Parallel()

	// Windows has no POSIX permission bits — os.Chmod there only toggles the
	// read-only flag, so the mode always reads back 0666. The agent ships as a
	// Linux container; this guarantee is only meaningful, and only checkable,
	// on the platform it actually runs on.
	if runtime.GOOS == "windows" {
		t.Skip("POSIX file modes are not modelled on Windows")
	}

	// Arrange / Act
	path := tempDBPath(t)
	openStore(t, path)

	// Assert — the file holds device records; SQLite would otherwise leave 0644.
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		t.Errorf("devmon.db mode = %#o, want no group or world bits", perm)
	}
}

func TestPruneAudit(t *testing.T) {
	t.Parallel()

	// Arrange — 100 rows spread across 400 days, oldest first.
	path := tempDBPath(t)
	s := openStore(t, path)
	ctx := context.Background()
	now := time.Now()

	for i := range 100 {
		age := time.Duration(400-i*4) * 24 * time.Hour
		_, err := s.db.ExecContext(ctx,
			`INSERT INTO audit (occurred_at, operation, outcome) VALUES (?, 'read', 'allowed')`,
			now.Add(-age).Unix(),
		)
		if err != nil {
			t.Fatalf("insert row %d: %v", i, err)
		}
	}

	// Act
	removed, err := s.PruneAudit(ctx, 365*24*time.Hour, 50)

	// Assert
	if err != nil {
		t.Fatalf("PruneAudit() unexpected error: %v", err)
	}
	if removed != 50 {
		t.Errorf("PruneAudit() removed = %d, want 50", removed)
	}

	var remaining int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM audit`).Scan(&remaining); err != nil {
		t.Fatalf("count: %v", err)
	}
	if remaining != 50 {
		t.Errorf("remaining rows = %d, want 50", remaining)
	}

	cutoff := now.Add(-365 * 24 * time.Hour).Unix()
	var tooOld int
	err = s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM audit WHERE occurred_at < ?`, cutoff).Scan(&tooOld)
	if err != nil {
		t.Fatalf("count stale: %v", err)
	}
	if tooOld != 0 {
		t.Errorf("%d rows survived past the age cutoff", tooOld)
	}
}

func TestPruneAuditOnEmptyTable(t *testing.T) {
	t.Parallel()

	// Arrange — Phase 1's real situation: nothing writes audit rows yet.
	s := openStore(t, tempDBPath(t))

	// Act
	removed, err := s.PruneAudit(context.Background(), 24*time.Hour, 10)

	// Assert
	if err != nil {
		t.Fatalf("PruneAudit() unexpected error: %v", err)
	}
	if removed != 0 {
		t.Errorf("PruneAudit() removed = %d on an empty table, want 0", removed)
	}
}

// TestConcurrentStores is the cross-process contract: the Phase 2 host-side CLI
// opens the same file while the agent is running, and neither may see
// SQLITE_BUSY.
func TestConcurrentStores(t *testing.T) {
	t.Parallel()

	// Arrange
	path := tempDBPath(t)
	agent := openStore(t, path)
	cli := openStore(t, path)

	const writesPerStore = 40
	var wg sync.WaitGroup
	errs := make(chan error, 2*writesPerStore)

	write := func(s *Store, device string) {
		defer wg.Done()
		for i := range writesPerStore {
			_, err := s.db.ExecContext(context.Background(),
				`INSERT INTO audit (occurred_at, device_id, operation, outcome) VALUES (?, ?, 'read', 'allowed')`,
				time.Now().Unix(), device,
			)
			if err != nil {
				errs <- err
				return
			}
			_ = i
		}
	}

	// Act
	wg.Add(2)
	go write(agent, "agent")
	go write(cli, "cli")
	wg.Wait()
	close(errs)

	// Assert
	for err := range errs {
		t.Fatalf("concurrent write failed: %v", err)
	}
	var total int
	if err := agent.db.QueryRow(`SELECT COUNT(*) FROM audit`).Scan(&total); err != nil {
		t.Fatalf("count: %v", err)
	}
	if total != 2*writesPerStore {
		t.Errorf("audit rows = %d, want %d", total, 2*writesPerStore)
	}
}

func TestPrunerRunStopsOnContextCancel(t *testing.T) {
	t.Parallel()

	// Arrange
	s := openStore(t, tempDBPath(t))
	p := NewPruner(s, 24*time.Hour, 10, testLogger())
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)

	// Act
	go func() { done <- p.Run(ctx) }()
	cancel()

	// Assert
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("Run() = %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run() did not return after the context was cancelled")
	}
}

func TestPrunerPruneOnce(t *testing.T) {
	t.Parallel()

	// Arrange
	s := openStore(t, tempDBPath(t))
	ctx := context.Background()
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO audit (occurred_at, operation, outcome) VALUES (?, 'read', 'allowed')`,
		time.Now().Add(-48*time.Hour).Unix(),
	)
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	p := NewPruner(s, 24*time.Hour, 100, testLogger())

	// Act
	p.pruneOnce(ctx)

	// Assert
	var remaining int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM audit`).Scan(&remaining); err != nil {
		t.Fatalf("count: %v", err)
	}
	if remaining != 0 {
		t.Errorf("remaining rows = %d, want 0", remaining)
	}
}

func TestSchemaVersionMissingRowReportsNoRows(t *testing.T) {
	t.Parallel()

	// Arrange — the state reconcileVersion sees on a genuinely fresh store.
	s := openStore(t, tempDBPath(t))
	if _, err := s.db.Exec(`DELETE FROM schema_meta`); err != nil {
		t.Fatalf("clear schema_meta: %v", err)
	}

	// Act
	_, err := s.SchemaVersion(context.Background())

	// Assert
	if !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("SchemaVersion() error = %v, want sql.ErrNoRows", err)
	}
}
