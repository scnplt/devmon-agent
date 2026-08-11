// SPDX-License-Identifier: AGPL-3.0-only

package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/scnplt/devmon-agent/internal/dockerx"
	"github.com/scnplt/devmon-agent/internal/policy"
	"github.com/scnplt/devmon-agent/internal/state"
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

// apiLifecycleRoute describes one of the five mutating routes exactly as
// server.go's routes() registers it (Task 9): the real method and path
// pattern, the policy operation that gates it, the lowest mode that permits
// it, and a way to observe whether the underlying fakeDocker method ran.
type apiLifecycleRoute struct {
	name       string
	method     string
	pathf      string // fmt-style, one %s for the container id
	op         policy.Operation
	minMode    policy.Mode
	markCalled func(fd *fakeDocker, called *bool)
}

func apiLifecycleRoutes() []apiLifecycleRoute {
	return []apiLifecycleRoute{
		{
			name: "start", method: http.MethodPost, pathf: "/v1/containers/%s/start",
			op: policy.OpStart, minMode: policy.ModeDefault,
			markCalled: func(fd *fakeDocker, called *bool) {
				fd.startContainerFn = func(context.Context, string) error { *called = true; return nil }
			},
		},
		{
			name: "restart", method: http.MethodPost, pathf: "/v1/containers/%s/restart",
			op: policy.OpRestart, minMode: policy.ModeDefault,
			markCalled: func(fd *fakeDocker, called *bool) {
				fd.restartContainerFn = func(context.Context, string) error { *called = true; return nil }
			},
		},
		{
			name: "stop", method: http.MethodPost, pathf: "/v1/containers/%s/stop",
			op: policy.OpStop, minMode: policy.ModeDefault,
			markCalled: func(fd *fakeDocker, called *bool) {
				fd.stopContainerFn = func(context.Context, string) error { *called = true; return nil }
			},
		},
		{
			name: "kill", method: http.MethodPost, pathf: "/v1/containers/%s/kill",
			op: policy.OpKill, minMode: policy.ModeFull,
			markCalled: func(fd *fakeDocker, called *bool) {
				fd.killContainerFn = func(context.Context, string) error { *called = true; return nil }
			},
		},
		{
			name: "delete", method: http.MethodDelete, pathf: "/v1/containers/%s",
			op: policy.OpDelete, minMode: policy.ModeFull,
			markCalled: func(fd *fakeDocker, called *bool) {
				fd.removeContainerFn = func(context.Context, string) error { *called = true; return nil }
			},
		},
	}
}

// apiLifecycleRequest builds a request for one real registered route, with
// serial as the authenticating peer certificate (nil for unauthenticated
// requests).
func apiLifecycleRequest(route apiLifecycleRoute, serial *big.Int) *http.Request {
	path := fmt.Sprintf(route.pathf, "target-id")
	req := httptest.NewRequest(route.method, path, nil)
	if serial != nil {
		req.TLS = peerCertWithSerial(serial)
	}
	return req
}

// TestLifecyclePolicyMatrix drives all five mutating routes, registered
// exactly as server.go's routes() registers them, across every policy mode.
// Each combination must answer 204 when the mode permits the operation and
// 403 with msgPolicyForbidden otherwise, matching minMode in
// internal/policy/mode.go.
func TestLifecyclePolicyMatrix(t *testing.T) {
	t.Parallel()

	modes := []policy.Mode{policy.ModeReadOnly, policy.ModeDefault, policy.ModeFull}

	for _, mode := range modes {
		for _, route := range apiLifecycleRoutes() {
			t.Run(mode.String()+"/"+route.name, func(t *testing.T) {
				t.Parallel()

				// Arrange
				fd := &fakeDocker{}
				var called bool
				route.markCalled(fd, &called)
				s, st := testServerWithDocker(t, mode, fd)
				serial := pairDeviceForRead(t, st)
				req := apiLifecycleRequest(route, serial)
				rec := httptest.NewRecorder()

				wantAllowed := mode.Allows(route.op)
				wantStatus := http.StatusForbidden
				if wantAllowed {
					wantStatus = http.StatusNoContent
				}

				// Act
				s.routes().ServeHTTP(rec, req)

				// Assert
				if rec.Code != wantStatus {
					t.Fatalf("status = %d, want %d; body: %s", rec.Code, wantStatus, rec.Body.String())
				}
				if called != wantAllowed {
					t.Errorf("docker method called = %v, want %v", called, wantAllowed)
				}

				entries, err := st.ListAudit(context.Background(), 10)
				if err != nil {
					t.Fatalf("ListAudit: %v", err)
				}
				if len(entries) != 1 {
					t.Fatalf("len(entries) = %d, want 1", len(entries))
				}
				wantOutcome := state.OutcomeDeniedPolicy
				if wantAllowed {
					wantOutcome = state.OutcomeSuccess
				}
				if entries[0].Outcome != wantOutcome {
					t.Errorf("outcome = %q, want %q", entries[0].Outcome, wantOutcome)
				}
			})
		}
	}
}

// TestLifecycleRequiresDevice asserts every mutating route answers 401 and
// writes zero audit rows when the request carries no client certificate —
// requireDevice must run before withAudit ever seeds a row (D15).
func TestLifecycleRequiresDevice(t *testing.T) {
	t.Parallel()

	for _, route := range apiLifecycleRoutes() {
		t.Run(route.name, func(t *testing.T) {
			t.Parallel()

			// Arrange
			fd := &fakeDocker{}
			var called bool
			route.markCalled(fd, &called)
			s, st := testServerWithDocker(t, policy.ModeFull, fd)
			req := apiLifecycleRequest(route, nil)
			rec := httptest.NewRecorder()

			// Act
			s.routes().ServeHTTP(rec, req)

			// Assert
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want %d; body: %s", rec.Code, http.StatusUnauthorized, rec.Body.String())
			}
			if called {
				t.Error("docker method called, want requireDevice to reject before it")
			}
			entries, err := st.ListAudit(context.Background(), 10)
			if err != nil {
				t.Fatalf("ListAudit: %v", err)
			}
			if len(entries) != 0 {
				t.Fatalf("len(entries) = %d, want 0", len(entries))
			}
		})
	}
}

// TestLifecycleRejectsOtherMethods asserts a method the pattern does not
// register falls through to ServeMux's default 405, exactly as it does for
// the read and log routes.
func TestLifecycleRejectsOtherMethods(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		method string
		path   string
	}{
		{name: "GET on start", method: http.MethodGet, path: "/v1/containers/target-id/start"},
		{name: "GET on restart", method: http.MethodGet, path: "/v1/containers/target-id/restart"},
		{name: "GET on stop", method: http.MethodGet, path: "/v1/containers/target-id/stop"},
		{name: "GET on kill", method: http.MethodGet, path: "/v1/containers/target-id/kill"},
		{name: "POST on delete path", method: http.MethodPost, path: "/v1/containers/target-id"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			// Arrange
			s, st := testServerWithDocker(t, policy.ModeFull, &fakeDocker{})
			serial := pairDeviceForRead(t, st)
			req := requestWithPeerSerial(tt.method, tt.path, nil, serial)
			rec := httptest.NewRecorder()

			// Act
			s.routes().ServeHTTP(rec, req)

			// Assert
			if rec.Code != http.StatusMethodNotAllowed {
				t.Errorf("status = %d, want %d", rec.Code, http.StatusMethodNotAllowed)
			}
		})
	}
}
