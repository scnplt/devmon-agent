// SPDX-License-Identifier: AGPL-3.0-only

package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/scnplt/devmon-agent/internal/policy"
)

// TestRequireOp exercises every policy mode against both an operation every
// tier permits (OpRead) and one only the destructive tier permits (OpDelete),
// asserting the exact status and, on rejection, the exact msgPolicyForbidden
// body.
func TestRequireOp(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		mode       policy.Mode
		op         policy.Operation
		wantStatus int
	}{
		{name: "read-only allows read", mode: policy.ModeReadOnly, op: policy.OpRead, wantStatus: http.StatusOK},
		{name: "read-only denies delete", mode: policy.ModeReadOnly, op: policy.OpDelete, wantStatus: http.StatusForbidden},
		{name: "default allows read", mode: policy.ModeDefault, op: policy.OpRead, wantStatus: http.StatusOK},
		{name: "default denies delete", mode: policy.ModeDefault, op: policy.OpDelete, wantStatus: http.StatusForbidden},
		{name: "full allows read", mode: policy.ModeFull, op: policy.OpRead, wantStatus: http.StatusOK},
		{name: "full allows delete", mode: policy.ModeFull, op: policy.OpDelete, wantStatus: http.StatusOK},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			// Arrange
			s := testServer(t, tt.mode)
			var ran bool
			guarded := s.requireOp(tt.op, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				ran = true
				w.WriteHeader(http.StatusOK)
			}))
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, "/v1/protected", nil)

			// Act
			guarded.ServeHTTP(rec, req)

			// Assert
			if rec.Code != tt.wantStatus {
				t.Errorf("status = %d, want %d", rec.Code, tt.wantStatus)
			}
			wantRan := tt.wantStatus == http.StatusOK
			if ran != wantRan {
				t.Errorf("handler ran = %v, want %v", ran, wantRan)
			}
			if tt.wantStatus == http.StatusForbidden {
				var body errorBody
				if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
					t.Fatalf("decode body: %v", err)
				}
				if body.Error != msgPolicyForbidden {
					t.Errorf("error = %q, want the exact %q", body.Error, msgPolicyForbidden)
				}
			}
		})
	}
}
