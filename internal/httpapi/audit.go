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

// Identity operation names for the audit `operation` column. These are not
// policy.Operation values: pairing, renewal, and self-revocation are never
// gated by policy.Mode (handleUnpairSelf's own doc comment explains why for
// self-revoke; pairing and renewal are identity bootstrapping, not container
// control), so they never pass through requireOp and have no place in
// policy's minMode map. They exist here, as plain strings, purely to name the
// row.
const (
	opPair       = "pair"
	opRenew      = "renew"
	opUnpairSelf = "unpair_self"
)

// auditEntry is the mutable record for one in-flight mutating request. Inner
// layers refine Outcome, Detail, and — for the one pre-authentication route,
// pair — DeviceID; withAudit and its identity-route siblings write exactly
// one row on the way out, whatever happened (D14).
type auditEntry struct {
	operation string
	target    string
	outcome   string
	detail    string
	// deviceID carries the device ID for routes that run before a device is
	// resolved in the request context. handlePair sets it via
	// setAuditDeviceID once it knows the newly created device's ID; every
	// other audited route leaves it empty and recordAudit reads DeviceFrom
	// instead.
	deviceID string
}

// recordAudit writes entry as one row, attributing it to the request's
// resolved device if one is present in ctx and to entry.deviceID otherwise —
// the latter is how the pre-authentication pair route (which has no device
// in context) still attributes a successful pairing to the device it just
// created.
//
// It is called after next returns, never in a defer: a defer would also fire
// on a panic path where withRecovery has already written a 500, and a
// missing row there is better than one claiming an outcome that never
// happened.
func (s *Server) recordAudit(ctx context.Context, entry *auditEntry) {
	deviceID := entry.deviceID
	if device, ok := DeviceFrom(ctx); ok {
		deviceID = device.ID
	}

	row := state.AuditEntry{
		DeviceID:  deviceID,
		Operation: entry.operation,
		Target:    entry.target,
		Outcome:   entry.outcome,
		Detail:    entry.detail,
	}
	// An AppendAudit failure is logged and never changes the response (D16):
	// the act already happened, so a 500 here would tell the caller it did
	// not, when it did.
	if err := s.st.AppendAudit(context.Background(), row); err != nil {
		s.log.Error("append audit entry",
			slog.String("operation", entry.operation),
			slog.String("device_id", deviceID),
			slog.Any("err", err),
		)
	}
}

// withAudit records one audit row per mutating container-lifecycle request.
//
// Placement is load-bearing (D15). It sits INSIDE requireDevice, so every row
// carries a real device — an unauthenticated caller is unattributable, and
// letting one write rows would hand a scanner a way to flood the operator's
// security record. It sits OUTSIDE requireOp, so refusals by policy are
// recorded, which is an explicit requirement.
func (s *Server) withAudit(op policy.Operation, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		entry := &auditEntry{
			operation: string(op),
			target:    r.PathValue("id"),
			outcome:   state.OutcomeSuccess,
		}

		ctx := context.WithValue(r.Context(), auditCtxKey{}, entry)
		next.ServeHTTP(w, r.WithContext(ctx))
		s.recordAudit(ctx, entry)
	})
}

// withIdentityAudit records one audit row per request for the two guarded
// identity routes, renew and self-revoke. It mirrors withAudit exactly
// except for two things neither route has: a policy.Operation (neither is
// gated by policy.Mode, see the opRenew/opUnpairSelf doc comment) and a
// path-value target (both act on the caller's own identity, never on a path
// parameter). It still sits inside requireDevice and withDeviceLimit, so
// D7 and D15 hold the same way they do for withAudit.
func (s *Server) withIdentityAudit(operation string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		entry := &auditEntry{operation: operation, outcome: state.OutcomeSuccess}

		ctx := context.WithValue(r.Context(), auditCtxKey{}, entry)
		next.ServeHTTP(w, r.WithContext(ctx))
		s.recordAudit(ctx, entry)
	})
}

// withPairAudit records one audit row per call to the pre-authentication
// pair route. It must sit INSIDE both the global unauthenticated limiter and
// the per-IP pair limiter (D7): a request either of those rejects must never
// reach here, or a throttled scanner could still fill the audit table.
//
// Unlike withAudit and withIdentityAudit, there is no device in the request
// context at any point — the caller has no certificate yet. handlePair
// itself calls setAuditDeviceID once pairing succeeds, so the row it
// produces carries the newly created device's ID on success and an empty
// device ID on every failure path.
func (s *Server) withPairAudit(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		entry := &auditEntry{operation: opPair, outcome: state.OutcomeSuccess}

		ctx := context.WithValue(r.Context(), auditCtxKey{}, entry)
		next.ServeHTTP(w, r.WithContext(ctx))
		s.recordAudit(ctx, entry)
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

// setAuditDeviceID records the ID of a device created during this request,
// for the one route that runs before authentication (pair) and therefore has
// no device in context for recordAudit to read via DeviceFrom. A no-op
// outside an audited request, for the same reason setAuditOutcome is.
func setAuditDeviceID(ctx context.Context, deviceID string) {
	entry, ok := ctx.Value(auditCtxKey{}).(*auditEntry)
	if !ok {
		return
	}
	entry.deviceID = deviceID
}
