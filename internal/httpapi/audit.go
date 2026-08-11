// SPDX-License-Identifier: AGPL-3.0-only

package httpapi

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/scnplt/devmon-agent/internal/policy"
	"github.com/scnplt/devmon-agent/internal/state"
)

// auditCtxKey is the context key under which withAudit stores the in-flight
// entry. An empty struct type, not a string, for the same reason
// deviceCtxKey is (middleware.go:20).
type auditCtxKey struct{}

// auditEntry is the mutable record for one in-flight mutating request. Inner
// layers refine Outcome and Detail; withAudit writes exactly one row on the
// way out, whatever happened (D14).
type auditEntry struct {
	operation policy.Operation
	target    string
	outcome   string
	detail    string
}

// withAudit records one audit row per mutating request.
//
// Placement is load-bearing (D15). It sits INSIDE requireDevice, so every row
// carries a real device — an unauthenticated caller is unattributable, and
// letting one write rows would hand a scanner a way to flood the operator's
// security record. It sits OUTSIDE requireOp, so refusals by policy are
// recorded, which the PRD requires explicitly.
func (s *Server) withAudit(op policy.Operation, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		entry := &auditEntry{
			operation: op,
			target:    r.PathValue("id"),
			outcome:   state.OutcomeSuccess,
		}

		ctx := context.WithValue(r.Context(), auditCtxKey{}, entry)
		next.ServeHTTP(w, r.WithContext(ctx))

		// The row is written here, after next returns, not in a defer: a defer
		// would also fire on a panic path where withRecovery has already written
		// a 500, and a missing row there is better than one claiming an outcome
		// that never happened.
		device, _ := DeviceFrom(ctx)
		row := state.AuditEntry{
			DeviceID:  device.ID,
			Operation: string(entry.operation),
			Target:    entry.target,
			Outcome:   entry.outcome,
			Detail:    entry.detail,
		}
		// An AppendAudit failure is logged and never changes the response (D16):
		// the act already happened, so a 500 here would tell the caller it did
		// not, when it did.
		if err := s.st.AppendAudit(context.Background(), row); err != nil {
			s.log.Error("append audit entry",
				slog.String("operation", string(entry.operation)),
				slog.String("device_id", device.ID),
				slog.Any("err", err),
			)
		}
	})
}

// setAuditOutcome refines the in-flight entry. A no-op when the request is not
// under withAudit, so a handler shared with a non-mutating route cannot panic.
func setAuditOutcome(ctx context.Context, outcome, detail string) {
	entry, ok := ctx.Value(auditCtxKey{}).(*auditEntry)
	if !ok {
		return
	}
	entry.outcome = outcome
	entry.detail = detail
}
