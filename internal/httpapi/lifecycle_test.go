package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"math/big"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/scnplt/devmon-agent/internal/dockerx"
	"github.com/scnplt/devmon-agent/internal/policy"
)

// lifecycleRouteCase describes one of the five mutating container routes:
// which fakeDocker field backs it, which handler serves it, and which policy
// operation gates it. server.go does not register these routes until Task 9,
// so tests wire the same guard chain audit_test.go's auditChain builds,
// driving the handler directly.
type lifecycleRouteCase struct {
	name    string
	op      policy.Operation
	handler func(s *Server) http.HandlerFunc
	setErr  func(fd *fakeDocker, err error)
}

func lifecycleRouteCases() []lifecycleRouteCase {
	return []lifecycleRouteCase{
		{
			name:    "start",
			op:      policy.OpStart,
			handler: func(s *Server) http.HandlerFunc { return s.handleStartContainer },
			setErr: func(fd *fakeDocker, err error) {
				fd.startContainerFn = func(context.Context, string) error { return err }
			},
		},
		{
			name:    "restart",
			op:      policy.OpRestart,
			handler: func(s *Server) http.HandlerFunc { return s.handleRestartContainer },
			setErr: func(fd *fakeDocker, err error) {
				fd.restartContainerFn = func(context.Context, string) error { return err }
			},
		},
		{
			name:    "stop",
			op:      policy.OpStop,
			handler: func(s *Server) http.HandlerFunc { return s.handleStopContainer },
			setErr: func(fd *fakeDocker, err error) {
				fd.stopContainerFn = func(context.Context, string) error { return err }
			},
		},
		{
			name:    "kill",
			op:      policy.OpKill,
			handler: func(s *Server) http.HandlerFunc { return s.handleKillContainer },
			setErr: func(fd *fakeDocker, err error) {
				fd.killContainerFn = func(context.Context, string) error { return err }
			},
		},
		{
			name:    "remove",
			op:      policy.OpDelete,
			handler: func(s *Server) http.HandlerFunc { return s.handleRemoveContainer },
			setErr: func(fd *fakeDocker, err error) {
				fd.removeContainerFn = func(context.Context, string) error { return err }
			},
		},
	}
}

// lifecycleRequest builds a guarded request for one lifecycle route: serial
// authenticates the caller as a client certificate, "target-id" is the {id}
// path value, mirroring how ServeMux would populate it for a pattern like
// "POST /v1/containers/{id}/stop".
func lifecycleRequest(serial *big.Int) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/v1/containers/target-id/lifecycle", nil)
	req.TLS = peerCertWithSerial(serial)
	req.SetPathValue("id", "target-id")
	return req
}

// TestLifecycleRoutesStatusMatrix asserts each of the five mutating routes
// maps every dockerx failure mode onto the status the API contract promises,
// and answers 204 on success. It drives requireDevice(withAudit(requireOp(
// handler))) directly, the same chain Task 9 registers in server.go.
func TestLifecycleRoutesStatusMatrix(t *testing.T) {
	t.Parallel()

	engineErr := errors.New("engine handle closed unexpectedly on host-internal-path")

	cases := []struct {
		name       string
		err        error
		wantStatus int
		wantBody   string // empty means no error body is expected (success)
	}{
		{name: "success", err: nil, wantStatus: http.StatusNoContent},
		{name: "self protected", err: dockerx.ErrSelfProtected, wantStatus: http.StatusForbidden, wantBody: msgSelfProtected},
		{name: "self unknown", err: dockerx.ErrSelfUnknown, wantStatus: http.StatusServiceUnavailable, wantBody: msgSelfUnknown},
		{name: "conflict", err: dockerx.ErrConflict, wantStatus: http.StatusConflict, wantBody: msgContainerConflict},
		{name: "not found", err: dockerx.ErrNotFound, wantStatus: http.StatusNotFound, wantBody: msgNotFound},
		{name: "invalid ref", err: dockerx.ErrInvalidRef, wantStatus: http.StatusBadRequest, wantBody: msgInvalidRef},
		{name: "engine error", err: engineErr, wantStatus: http.StatusBadGateway, wantBody: msgEngineUnavailable},
	}

	for _, route := range lifecycleRouteCases() {
		for _, tc := range cases {
			t.Run(route.name+"/"+tc.name, func(t *testing.T) {
				t.Parallel()

				// Arrange
				fd := &fakeDocker{}
				route.setErr(fd, tc.err)
				s, st := testServerWithDocker(t, policy.ModeFull, fd)
				serial := pairDeviceForRead(t, st)
				handler := auditChain(s, route.op, route.handler(s))
				rec := httptest.NewRecorder()

				// Act
				handler.ServeHTTP(rec, lifecycleRequest(serial))

				// Assert
				if rec.Code != tc.wantStatus {
					t.Fatalf("status = %d, want %d; body: %s", rec.Code, tc.wantStatus, rec.Body.String())
				}
				if tc.wantBody == "" {
					if rec.Body.Len() != 0 {
						t.Errorf("body = %q, want empty (204 carries no body)", rec.Body.String())
					}
					return
				}
				var body errorBody
				if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
					t.Fatalf("decode body: %v", err)
				}
				if body.Error != tc.wantBody {
					t.Errorf("error = %q, want %q", body.Error, tc.wantBody)
				}
			})
		}
	}
}

// TestLifecycleNilDockerReader asserts every lifecycle route answers 502
// without panicking when the server has no Docker controller configured —
// the same nil-tolerance the eight read routes already guarantee.
func TestLifecycleNilDockerReader(t *testing.T) {
	t.Parallel()

	for _, route := range lifecycleRouteCases() {
		t.Run(route.name, func(t *testing.T) {
			t.Parallel()

			// Arrange
			s, st := testServerWithDocker(t, policy.ModeFull, nil)
			serial := pairDeviceForRead(t, st)
			handler := auditChain(s, route.op, route.handler(s))
			rec := httptest.NewRecorder()

			// Act
			handler.ServeHTTP(rec, lifecycleRequest(serial))

			// Assert
			if rec.Code != http.StatusBadGateway {
				t.Fatalf("status = %d, want %d; body: %s", rec.Code, http.StatusBadGateway, rec.Body.String())
			}
			var body errorBody
			if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
				t.Fatalf("decode body: %v", err)
			}
			if body.Error != msgEngineUnavailable {
				t.Errorf("error = %q, want %q", body.Error, msgEngineUnavailable)
			}
		})
	}
}
