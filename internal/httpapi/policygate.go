package httpapi

import (
	"log/slog"
	"net/http"

	"github.com/scnplt/devmon-agent/internal/policy"
	"github.com/scnplt/devmon-agent/internal/state"
)

// msgPolicyForbidden is served when the host's startup policy does not permit
// the operation. Unlike the terse authentication rejections, this one is
// deliberately specific: the caller is an authenticated device, and the whole
// point of advertising the policy mode is that a client can tell "the host
// forbids this" apart from "the agent is broken".
const msgPolicyForbidden = "operation not permitted by host policy"

// requireOp rejects a request when the host's policy mode does not permit op,
// and otherwise runs next unchanged.
//
// Composition order matters: routes wrap this as requireDevice(requireOp(op,
// handler)), so the policy check runs after authentication. Inverting that
// order would let an unauthenticated scanner probe the host's policy tier by
// observing 403 vs 401 on routes it has no certificate for.
func (s *Server) requireOp(op policy.Operation, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.cfg.PolicyMode.Allows(op) {
			attrs := []any{slog.String("operation", string(op))}
			if device, ok := DeviceFrom(r.Context()); ok {
				attrs = append(attrs, slog.String("device_id", device.ID))
			}
			s.log.Warn("rejected request forbidden by host policy", attrs...)
			setAuditOutcome(r.Context(), state.OutcomeDeniedPolicy, "")
			s.writeError(w, http.StatusForbidden, msgPolicyForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}
