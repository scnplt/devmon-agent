// SPDX-License-Identifier: AGPL-3.0-only

package state

import (
	"context"
	"testing"
	"time"
)

func TestAppendAuditThenListAuditRoundTrip(t *testing.T) {
	t.Parallel()

	// Arrange
	s := openStore(t, tempDBPath(t))
	ctx := context.Background()
	entry := AuditEntry{
		DeviceID:  "deviceabc123",
		Operation: "restart",
		Target:    "web",
		Outcome:   OutcomeSuccess,
		Detail:    "abc123def456",
	}

	// Act
	if err := s.AppendAudit(ctx, entry); err != nil {
		t.Fatalf("AppendAudit() unexpected error: %v", err)
	}
	entries, err := s.ListAudit(ctx, 10)

	// Assert
	if err != nil {
		t.Fatalf("ListAudit() unexpected error: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("ListAudit() len = %d, want 1", len(entries))
	}
	got := entries[0]
	if got.ID == 0 {
		t.Error("ListAudit() ID = 0, want a non-zero autoincrement value")
	}
	if got.DeviceID != entry.DeviceID {
		t.Errorf("ListAudit() DeviceID = %q, want %q", got.DeviceID, entry.DeviceID)
	}
	if got.Operation != entry.Operation {
		t.Errorf("ListAudit() Operation = %q, want %q", got.Operation, entry.Operation)
	}
	if got.Target != entry.Target {
		t.Errorf("ListAudit() Target = %q, want %q", got.Target, entry.Target)
	}
	if got.Outcome != entry.Outcome {
		t.Errorf("ListAudit() Outcome = %q, want %q", got.Outcome, entry.Outcome)
	}
	if got.Detail != entry.Detail {
		t.Errorf("ListAudit() Detail = %q, want %q", got.Detail, entry.Detail)
	}
	if got.OccurredAt.IsZero() {
		t.Error("ListAudit() OccurredAt = zero, want stamped from time.Now()")
	}
}

func TestAppendAuditStampsOccurredAtWhenZero(t *testing.T) {
	t.Parallel()

	// Arrange
	s := openStore(t, tempDBPath(t))
	ctx := context.Background()
	before := time.Now().Add(-time.Second)

	// Act
	if err := s.AppendAudit(ctx, AuditEntry{Operation: "start", Outcome: OutcomeSuccess}); err != nil {
		t.Fatalf("AppendAudit() unexpected error: %v", err)
	}
	entries, err := s.ListAudit(ctx, 1)

	// Assert
	if err != nil {
		t.Fatalf("ListAudit() unexpected error: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("ListAudit() len = %d, want 1", len(entries))
	}
	if entries[0].OccurredAt.Before(before) {
		t.Errorf("ListAudit() OccurredAt = %v, want at or after %v", entries[0].OccurredAt, before)
	}
}

func TestAppendAuditPreservesExplicitOccurredAt(t *testing.T) {
	t.Parallel()

	// Arrange
	s := openStore(t, tempDBPath(t))
	ctx := context.Background()
	occurredAt := time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC)

	// Act
	err := s.AppendAudit(ctx, AuditEntry{
		OccurredAt: occurredAt,
		Operation:  "kill",
		Outcome:    OutcomeSuccess,
	})

	// Assert
	if err != nil {
		t.Fatalf("AppendAudit() unexpected error: %v", err)
	}
	entries, err := s.ListAudit(ctx, 1)
	if err != nil {
		t.Fatalf("ListAudit() unexpected error: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("ListAudit() len = %d, want 1", len(entries))
	}
	if !entries[0].OccurredAt.Equal(occurredAt) {
		t.Errorf("ListAudit() OccurredAt = %v, want %v", entries[0].OccurredAt, occurredAt)
	}
}

func TestAppendAuditWithEmptyNullableFieldsScansAsEmptyString(t *testing.T) {
	t.Parallel()

	// Arrange — device_id, target, and detail are all nullable in the schema;
	// a refusal on the way in from requireDevice, for instance, has no target.
	s := openStore(t, tempDBPath(t))
	ctx := context.Background()

	// Act
	err := s.AppendAudit(ctx, AuditEntry{
		Operation: "stop",
		Outcome:   OutcomeDeniedPolicy,
	})

	// Assert
	if err != nil {
		t.Fatalf("AppendAudit() unexpected error: %v", err)
	}
	entries, err := s.ListAudit(ctx, 1)
	if err != nil {
		t.Fatalf("ListAudit() unexpected error: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("ListAudit() len = %d, want 1", len(entries))
	}
	got := entries[0]
	if got.DeviceID != "" {
		t.Errorf("ListAudit() DeviceID = %q, want empty string", got.DeviceID)
	}
	if got.Target != "" {
		t.Errorf("ListAudit() Target = %q, want empty string", got.Target)
	}
	if got.Detail != "" {
		t.Errorf("ListAudit() Detail = %q, want empty string", got.Detail)
	}
}

func TestListAuditOrdersMostRecentFirstByID(t *testing.T) {
	t.Parallel()

	// Arrange — insert three entries whose id order and occurred_at order
	// agree, then verify ordering is by id, not occurred_at (store.go:272-276
	// treats id as the ordering that matters for the same reason).
	s := openStore(t, tempDBPath(t))
	ctx := context.Background()
	for i, target := range []string{"first", "second", "third"} {
		err := s.AppendAudit(ctx, AuditEntry{
			OccurredAt: time.Now().Add(time.Duration(i) * time.Second),
			Operation:  "start",
			Target:     target,
			Outcome:    OutcomeSuccess,
		})
		if err != nil {
			t.Fatalf("AppendAudit() unexpected error: %v", err)
		}
	}

	// Act
	entries, err := s.ListAudit(ctx, 10)

	// Assert
	if err != nil {
		t.Fatalf("ListAudit() unexpected error: %v", err)
	}
	if len(entries) != 3 {
		t.Fatalf("ListAudit() len = %d, want 3", len(entries))
	}
	want := []string{"third", "second", "first"}
	for i, entry := range entries {
		if entry.Target != want[i] {
			t.Errorf("ListAudit() entries[%d].Target = %q, want %q", i, entry.Target, want[i])
		}
	}
	if entries[0].ID <= entries[1].ID || entries[1].ID <= entries[2].ID {
		t.Errorf("ListAudit() ids not descending: %d, %d, %d", entries[0].ID, entries[1].ID, entries[2].ID)
	}
}

func TestListAuditRespectsLimit(t *testing.T) {
	t.Parallel()

	// Arrange
	s := openStore(t, tempDBPath(t))
	ctx := context.Background()
	for range 5 {
		err := s.AppendAudit(ctx, AuditEntry{Operation: "start", Outcome: OutcomeSuccess})
		if err != nil {
			t.Fatalf("AppendAudit() unexpected error: %v", err)
		}
	}

	// Act
	entries, err := s.ListAudit(ctx, 2)

	// Assert
	if err != nil {
		t.Fatalf("ListAudit() unexpected error: %v", err)
	}
	if len(entries) != 2 {
		t.Errorf("ListAudit() len = %d, want 2", len(entries))
	}
}

func TestListAuditOnEmptyTableReturnsNoEntries(t *testing.T) {
	t.Parallel()

	// Arrange
	s := openStore(t, tempDBPath(t))
	ctx := context.Background()

	// Act
	entries, err := s.ListAudit(ctx, 10)

	// Assert
	if err != nil {
		t.Fatalf("ListAudit() unexpected error: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("ListAudit() len = %d on an empty table, want 0", len(entries))
	}
}

func TestPruneAuditRemovesRowsWrittenByAppendAudit(t *testing.T) {
	t.Parallel()

	// Arrange — an old row via AppendAudit's own path, so this exercises the
	// interaction between the writer this task adds and the pruner Phase 1
	// already shipped.
	s := openStore(t, tempDBPath(t))
	ctx := context.Background()
	old := time.Now().Add(-400 * 24 * time.Hour)
	err := s.AppendAudit(ctx, AuditEntry{
		OccurredAt: old,
		Operation:  "delete",
		Outcome:    OutcomeSuccess,
	})
	if err != nil {
		t.Fatalf("AppendAudit() unexpected error: %v", err)
	}

	// Act
	removed, err := s.PruneAudit(ctx, 365*24*time.Hour, 50)

	// Assert
	if err != nil {
		t.Fatalf("PruneAudit() unexpected error: %v", err)
	}
	if removed != 1 {
		t.Errorf("PruneAudit() removed = %d, want 1", removed)
	}
	entries, err := s.ListAudit(ctx, 10)
	if err != nil {
		t.Fatalf("ListAudit() unexpected error: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("ListAudit() len = %d after prune, want 0", len(entries))
	}
}
