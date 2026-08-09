package httpapi

import (
	"context"
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/scnplt/devmon-agent/internal/policy"
	"github.com/scnplt/devmon-agent/internal/state"
)

// auditChain wires the same three guards Task 9 registers on the real
// mutating routes — s.requireDevice(s.withAudit(op, s.requireOp(op, next))) —
// without touching server.go, whose lifecycle routes do not exist yet.
func auditChain(s *Server, op policy.Operation, next http.Handler) http.Handler {
	return s.requireDevice(s.withAudit(op, s.requireOp(op, next)))
}

// newAuditRequest builds a request carrying serial as its TLS peer
// certificate and "target-id" as the {id} path value, mirroring how
// ServeMux would populate it for a pattern like "POST /v1/containers/{id}/stop".
func newAuditRequest(serial *tls.ConnectionState) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/v1/containers/target-id/stop", nil)
	req.TLS = serial
	req.SetPathValue("id", "target-id")
	return req
}

func TestAuditSuccessWritesOneRow(t *testing.T) {
	t.Parallel()

	// Arrange
	s, st := testServerWithStore(t)
	serial := pairDeviceForRead(t, st)
	var ran bool
	handler := auditChain(s, policy.OpRestart, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ran = true
		w.WriteHeader(http.StatusNoContent)
	}))

	// Act
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, newAuditRequest(peerCertWithSerial(serial)))

	// Assert
	if !ran {
		t.Fatal("handler did not run")
	}
	if rec.Code != http.StatusNoContent {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusNoContent)
	}
	entries, err := st.ListAudit(context.Background(), 10)
	if err != nil {
		t.Fatalf("ListAudit: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("len(entries) = %d, want 1", len(entries))
	}
	if entries[0].Outcome != state.OutcomeSuccess {
		t.Errorf("outcome = %q, want %q", entries[0].Outcome, state.OutcomeSuccess)
	}
	if entries[0].Operation != string(policy.OpRestart) {
		t.Errorf("operation = %q, want %q", entries[0].Operation, policy.OpRestart)
	}
	if entries[0].Target != "target-id" {
		t.Errorf("target = %q, want %q", entries[0].Target, "target-id")
	}
	if entries[0].DeviceID == "" {
		t.Error("device_id is empty, want the authenticated device's ID")
	}
}

func TestAuditPolicyRefusalWritesDeniedRow(t *testing.T) {
	t.Parallel()

	// Arrange — policy.OpKill needs ModeFull, but testServerWithStore builds
	// ModeDefault, so requireOp refuses this operation.
	s, st := testServerWithStore(t)
	serial := pairDeviceForRead(t, st)
	var ran bool
	handler := auditChain(s, policy.OpKill, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ran = true
	}))

	// Act
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, newAuditRequest(peerCertWithSerial(serial)))

	// Assert
	if ran {
		t.Error("handler ran, want the policy gate to refuse before it")
	}
	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusForbidden)
	}
	entries, err := st.ListAudit(context.Background(), 10)
	if err != nil {
		t.Fatalf("ListAudit: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("len(entries) = %d, want 1", len(entries))
	}
	if entries[0].Outcome != state.OutcomeDeniedPolicy {
		t.Errorf("outcome = %q, want %q", entries[0].Outcome, state.OutcomeDeniedPolicy)
	}
}

func TestAuditUnauthenticatedWritesZeroRows(t *testing.T) {
	t.Parallel()

	// Arrange
	s, st := testServerWithStore(t)
	var ran bool
	handler := auditChain(s, policy.OpRestart, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ran = true
	}))

	// Act — no client certificate at all.
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, newAuditRequest(nil))

	// Assert
	if ran {
		t.Error("handler ran, want requireDevice to reject first")
	}
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}
	entries, err := st.ListAudit(context.Background(), 10)
	if err != nil {
		t.Fatalf("ListAudit: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("len(entries) = %d, want 0", len(entries))
	}
}

func TestAuditStoreFailureLogsButKeepsResponse(t *testing.T) {
	t.Parallel()

	// Arrange — the handler closes the store's database handle itself, after
	// requireDevice and requireOp have already used it successfully but
	// before withAudit's post-call AppendAudit runs. That isolates the
	// failure to the audit write alone: DB.Close is idempotent, so the
	// t.Cleanup registered by testServerWithStore closing it again is safe.
	s, st := testServerWithStore(t)
	serial := pairDeviceForRead(t, st)

	handler := auditChain(s, policy.OpRestart, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = st.Close()
		w.WriteHeader(http.StatusNoContent)
	}))

	log, buf := newCapturingLogger()
	s.log = log

	// Act
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, newAuditRequest(peerCertWithSerial(serial)))

	// Assert — the handler's own response is untouched by the audit failure.
	if rec.Code != http.StatusNoContent {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusNoContent)
	}
	if !strings.Contains(buf.String(), "append audit entry") {
		t.Errorf("log output = %q, want it to contain the audit append failure", buf.String())
	}
}
