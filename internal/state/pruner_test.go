// SPDX-License-Identifier: AGPL-3.0-only

package state

import (
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"
)

// newCapturingLoggerForTest mirrors the pattern used across this repo's other
// packages: a text-handler logger writing into a buffer a test can inspect.
func newCapturingLoggerForTest() (*slog.Logger, *strings.Builder) {
	var buf strings.Builder
	return slog.New(slog.NewTextHandler(&buf, nil)), &buf
}

// newCapturingPruner wires a Pruner to a fresh store and a logger a test can
// inspect.
func newCapturingPruner(t *testing.T, maxAge time.Duration, maxRows int) (*Pruner, *Store, *strings.Builder) {
	t.Helper()
	s := openStore(t, tempDBPath(t))
	log, buf := newCapturingLoggerForTest()
	return NewPruner(s, maxAge, maxRows, log), s, buf
}

// seedExpiredPairingCode mints a pairing code for deviceName and immediately
// expires it, since there is no way to observe expiry through the public API
// alone within a fast test.
func seedExpiredPairingCode(t *testing.T, ctx context.Context, s *Store, deviceName string) {
	t.Helper()
	if _, _, err := s.MintPairingCode(ctx, deviceName); err != nil {
		t.Fatalf("MintPairingCode() unexpected error: %v", err)
	}
	if _, err := s.db.ExecContext(ctx,
		`UPDATE pairing_codes SET expires_at = ? WHERE device_name = ?`,
		time.Now().Add(-time.Minute).Unix(), deviceName,
	); err != nil {
		t.Fatalf("seed expired pairing code: %v", err)
	}
}

func countPairingCodes(t *testing.T, ctx context.Context, s *Store) int {
	t.Helper()
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM pairing_codes`).Scan(&n); err != nil {
		t.Fatalf("count pairing_codes: %v", err)
	}
	return n
}

// TestPruneOnceRemovesExpiredPairingCodes proves PrunePairingCodes is wired
// into pruneOnce: before this fix, an expired pairing code survived forever
// because nothing in production code ever called it.
func TestPruneOnceRemovesExpiredPairingCodes(t *testing.T) {
	t.Parallel()

	// Arrange
	p, s, buf := newCapturingPruner(t, 365*24*time.Hour, 1000)
	ctx := context.Background()
	seedExpiredPairingCode(t, ctx, s, "Expired Phone")

	// Act
	p.pruneOnce(ctx)

	// Assert
	if got := countPairingCodes(t, ctx, s); got != 0 {
		t.Errorf("pairing_codes rows = %d after pruneOnce(), want 0", got)
	}
	if !strings.Contains(buf.String(), "pairing codes pruned") {
		t.Errorf("log = %q, want it to mention pairing codes pruned", buf.String())
	}
	if strings.Contains(buf.String(), "Expired Phone") {
		t.Error("log contains the device name from a pruned pairing code; pairing codes must never be logged")
	}
}

// TestPruneOnceContinuesPairingPruneWhenAuditPruneFails proves a failing
// PruneAudit does not skip PrunePairingCodes: dropping the audit table makes
// PruneAudit fail deterministically while leaving pairing_codes untouched.
func TestPruneOnceContinuesPairingPruneWhenAuditPruneFails(t *testing.T) {
	t.Parallel()

	// Arrange
	p, s, buf := newCapturingPruner(t, 365*24*time.Hour, 1000)
	ctx := context.Background()
	seedExpiredPairingCode(t, ctx, s, "Expired Phone")
	if _, err := s.db.ExecContext(ctx, `DROP TABLE audit`); err != nil {
		t.Fatalf("drop audit table: %v", err)
	}

	// Act
	p.pruneOnce(ctx)

	// Assert
	if got := countPairingCodes(t, ctx, s); got != 0 {
		t.Errorf("pairing_codes rows = %d after pruneOnce(), want 0 despite the audit prune failing", got)
	}
	if !strings.Contains(buf.String(), "audit prune failed") {
		t.Errorf("log = %q, want it to mention the audit prune failure", buf.String())
	}
}

// TestPruneOnceContinuesAuditPruneWhenPairingPruneFails proves the reverse:
// a failing PrunePairingCodes does not skip PruneAudit.
func TestPruneOnceContinuesAuditPruneWhenPairingPruneFails(t *testing.T) {
	t.Parallel()

	// Arrange
	p, s, buf := newCapturingPruner(t, time.Hour, 0)
	ctx := context.Background()
	if err := s.AppendAudit(ctx, AuditEntry{Operation: "start", Outcome: OutcomeSuccess}); err != nil {
		t.Fatalf("AppendAudit() unexpected error: %v", err)
	}
	if _, err := s.db.ExecContext(ctx, `DROP TABLE pairing_codes`); err != nil {
		t.Fatalf("drop pairing_codes table: %v", err)
	}

	// Act
	p.pruneOnce(ctx)

	// Assert
	var auditRows int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM audit`).Scan(&auditRows); err != nil {
		t.Fatalf("count audit: %v", err)
	}
	if auditRows != 0 {
		t.Errorf("audit rows = %d after pruneOnce(), want 0 despite the pairing prune failing", auditRows)
	}
	if !strings.Contains(buf.String(), "pairing code prune failed") {
		t.Errorf("log = %q, want it to mention the pairing code prune failure", buf.String())
	}
}

// TestPrunerRunPrunesOnStartup is the regression test for issue #42: a Pruner
// that has just started must not wait a full interval before its first pass.
func TestPrunerRunPrunesOnStartup(t *testing.T) {
	t.Parallel()

	// Arrange — an interval far longer than the test timeout proves the
	// removal below cannot be explained by a tick firing.
	p, s, _ := newCapturingPruner(t, 365*24*time.Hour, 1000)
	p.interval = time.Hour
	ctx := context.Background()
	seedExpiredPairingCode(t, ctx, s, "Expired Phone")

	runCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	done := make(chan error, 1)

	// Act
	go func() { done <- p.Run(runCtx) }()

	// Assert
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if countPairingCodes(t, ctx, s) == 0 {
			cancel()
			<-done
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-done
	t.Error("Run() never pruned the expired pairing code within the test window; the startup pass regressed")
}

// TestPrunerRunReturnsOnContextCancel proves Run still reports cancellation
// once its startup pass is added.
func TestPrunerRunReturnsOnContextCancel(t *testing.T) {
	t.Parallel()

	// Arrange
	p, _, _ := newCapturingPruner(t, time.Hour, 100)
	p.interval = time.Hour
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)

	// Act
	go func() { done <- p.Run(ctx) }()
	cancel()

	// Assert
	select {
	case err := <-done:
		if err != context.Canceled {
			t.Errorf("Run() = %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run() did not return after the context was cancelled")
	}
}

// TestPrunerRunDoesNothingWhenContextAlreadyCancelled proves the startup pass
// is skipped entirely when ctx is already dead on entry — a cancelled Run
// must not touch the store at all.
func TestPrunerRunDoesNothingWhenContextAlreadyCancelled(t *testing.T) {
	t.Parallel()

	// Arrange
	p, s, _ := newCapturingPruner(t, 365*24*time.Hour, 1000)
	p.interval = time.Hour
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	seedExpiredPairingCode(t, context.Background(), s, "Expired Phone")

	// Act
	err := p.Run(ctx)

	// Assert
	if err != context.Canceled {
		t.Errorf("Run() = %v, want context.Canceled", err)
	}
	if got := countPairingCodes(t, context.Background(), s); got != 1 {
		t.Errorf("pairing_codes rows = %d, want 1: Run() must not prune when ctx is already cancelled", got)
	}
}
