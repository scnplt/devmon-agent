// SPDX-License-Identifier: AGPL-3.0-only

package state

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// AuditEntry is one mutating operation, attributed to the device that requested it.
type AuditEntry struct {
	ID         int64
	OccurredAt time.Time
	DeviceID   string // always set: withAudit runs inside requireDevice (D15)
	Operation  string // policy.Operation value: start, restart, stop, kill, delete
	Target     string // the reference as the device supplied it (D21)
	Outcome    string // one of the Outcome* constants below
	Detail     string // resolved container ID, or a short reason; never an Engine message
}

const (
	OutcomeSuccess      = "success"
	OutcomeDeniedPolicy = "denied_policy"
	OutcomeDeniedSelf   = "denied_self"
	OutcomeNotFound     = "not_found"
	OutcomeInvalid      = "invalid"
	OutcomeConflict     = "conflict"
	OutcomeUnavailable  = "unavailable" // self ID unknown (D3)
	OutcomeEngineError  = "engine_error"
)

// AppendAudit records one mutating operation. It is called once per mutating
// request, on every path including refusals (D14), so it must stay a single
// INSERT against the pool's one connection.
func (s *Store) AppendAudit(ctx context.Context, e AuditEntry) error {
	occurredAt := e.OccurredAt
	if occurredAt.IsZero() {
		occurredAt = time.Now()
	}

	_, err := s.db.ExecContext(ctx,
		`INSERT INTO audit (occurred_at, device_id, operation, target, outcome, detail)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		occurredAt.Unix(), e.DeviceID, e.Operation, e.Target, e.Outcome, e.Detail,
	)
	if err != nil {
		return fmt.Errorf("append audit entry for operation %s: %w", e.Operation, err)
	}
	return nil
}

// ListAudit returns the most recent entries first, capped at limit. It exists
// for the host-side CLI (D19); nothing on the HTTPS API calls it (D20).
func (s *Store) ListAudit(ctx context.Context, limit int) ([]AuditEntry, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, occurred_at, device_id, operation, target, outcome, detail
		 FROM audit ORDER BY id DESC LIMIT ?`,
		limit,
	)
	if err != nil {
		return nil, fmt.Errorf("list audit entries: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var entries []AuditEntry
	for rows.Next() {
		entry, err := scanAuditEntry(rows)
		if err != nil {
			return nil, fmt.Errorf("scan audit row: %w", err)
		}
		entries = append(entries, entry)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list audit entries: %w", err)
	}
	return entries, nil
}

// scanAuditEntry reads one audit row, converting the nullable device_id,
// target, and detail columns into empty strings when absent.
func scanAuditEntry(row rowScanner) (AuditEntry, error) {
	var (
		id                       int64
		occurredAt               int64
		operation, outcome       string
		deviceID, target, detail sql.NullString
	)
	if err := row.Scan(&id, &occurredAt, &deviceID, &operation, &target, &outcome, &detail); err != nil {
		return AuditEntry{}, err
	}

	entry := AuditEntry{
		ID:         id,
		OccurredAt: time.Unix(occurredAt, 0).UTC(),
		Operation:  operation,
		Outcome:    outcome,
	}
	if deviceID.Valid {
		entry.DeviceID = deviceID.String
	}
	if target.Valid {
		entry.Target = target.String
	}
	if detail.Valid {
		entry.Detail = detail.String
	}
	return entry, nil
}
