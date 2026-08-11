// SPDX-License-Identifier: AGPL-3.0-only

package dockerx

// This file exercises the five lifecycle methods and their resolveTarget
// chokepoint against the fake Engine harness in engine_test.go. The
// self-exclusion cases are the load-bearing ones: every device-supplied ref
// form (name, short ID, full ID) must resolve through the Engine inspect
// before it is compared against the agent's own ID.

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/moby/moby/api/types/container"
)

// selfFullID is a 64-hex container ID used as the agent's own resolved
// identity across the self-exclusion tests.
const selfFullID = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

// selfShortID is the 12-hex prefix of selfFullID, one of the three forms a
// device can use to name the agent's own container.
const selfShortID = "aaaaaaaaaaaa"

// lifecycleOp names one of the five lifecycle methods so the self-exclusion
// and shared-behavior tests can iterate all five without duplicating call
// wiring per method.
type lifecycleOp struct {
	name   string
	method string
	path   string // the Engine path segment appended after "/containers/<id>"
	call   func(c *Client, ctx context.Context, ref string) error
}

func lifecycleOps() []lifecycleOp {
	return []lifecycleOp{
		{name: "Start", method: http.MethodPost, path: "/start", call: (*Client).StartContainer},
		{name: "Restart", method: http.MethodPost, path: "/restart", call: (*Client).RestartContainer},
		{name: "Stop", method: http.MethodPost, path: "/stop", call: (*Client).StopContainer},
		{name: "Kill", method: http.MethodPost, path: "/kill", call: (*Client).KillContainer},
		{name: "Remove", method: http.MethodDelete, path: "", call: (*Client).RemoveContainer},
	}
}

// actionRoute returns the method-prefixed ServeMux pattern this op's mutating
// call issues against the resolved container ID.
func (op lifecycleOp) actionRoute(id string) string {
	return op.method + " /containers/" + id + op.path
}

// withSelf returns a copy of c whose self identity is set, for the
// self-exclusion tests that need a Client already believing it knows its own
// container.
func withSelf(c *Client, containerized bool, id string) *Client {
	c.self = selfInfo{containerized: containerized, id: id}
	return c
}

func inspectRoute(ref string) string {
	return "GET /containers/" + ref + "/json"
}

// mutatingCallSpy registers a handler on a mutating Engine route and reports
// whether it was ever invoked, so a self-exclusion test can prove the
// mutating endpoint was never reached without depending on the exact number
// of ancillary requests (e.g. the SDK's own lazy version-negotiation ping)
// the transport happens to make.
type mutatingCallSpy struct{ hit bool }

func (s *mutatingCallSpy) handler(w http.ResponseWriter, r *http.Request) {
	s.hit = true
	w.WriteHeader(http.StatusOK)
}

// TestLifecycleRejectsSelfByName proves the self check runs on the resolved
// ID, not the raw ref: a device naming the agent by container name still
// gets ErrSelfProtected because the inspect response normalizes it.
func TestLifecycleRejectsSelfByName(t *testing.T) {
	t.Parallel()

	// Arrange
	const ref = "devmon-agent"
	spy := &mutatingCallSpy{}
	c, _ := newFakeEngine(t, map[string]http.HandlerFunc{
		inspectRoute(ref): jsonHandler(http.StatusOK, container.InspectResponse{ID: selfFullID}),
		"POST /containers/" + selfFullID + "/start": spy.handler,
	})
	c = withSelf(c, true, selfFullID)

	// Act
	err := c.StartContainer(context.Background(), ref)

	// Assert
	if !errors.Is(err, ErrSelfProtected) {
		t.Fatalf("err = %v, want errors.Is(err, ErrSelfProtected)", err)
	}
	if spy.hit {
		t.Error("mutating Engine endpoint was reached, want it never contacted")
	}
}

// TestLifecycleRejectsSelfByShortID proves a device naming the agent by its
// 12-hex short ID is also rejected, via the same normalized-ID comparison.
func TestLifecycleRejectsSelfByShortID(t *testing.T) {
	t.Parallel()

	// Arrange
	spy := &mutatingCallSpy{}
	c, _ := newFakeEngine(t, map[string]http.HandlerFunc{
		inspectRoute(selfShortID):                  jsonHandler(http.StatusOK, container.InspectResponse{ID: selfFullID}),
		"POST /containers/" + selfFullID + "/stop": spy.handler,
	})
	c = withSelf(c, true, selfFullID)

	// Act
	err := c.StopContainer(context.Background(), selfShortID)

	// Assert
	if !errors.Is(err, ErrSelfProtected) {
		t.Fatalf("err = %v, want errors.Is(err, ErrSelfProtected)", err)
	}
	if spy.hit {
		t.Error("mutating Engine endpoint was reached, want it never contacted")
	}
}

// TestLifecycleRejectsSelfAllFive proves every one of the five lifecycle
// methods goes through the same resolveTarget chokepoint: none of them can
// reach the Engine's mutating endpoint when the target is the agent itself.
func TestLifecycleRejectsSelfAllFive(t *testing.T) {
	t.Parallel()

	for _, op := range lifecycleOps() {
		t.Run(op.name, func(t *testing.T) {
			t.Parallel()

			// Arrange
			spy := &mutatingCallSpy{}
			c, _ := newFakeEngine(t, map[string]http.HandlerFunc{
				inspectRoute(selfFullID):   jsonHandler(http.StatusOK, container.InspectResponse{ID: selfFullID}),
				op.actionRoute(selfFullID): spy.handler,
			})
			c = withSelf(c, true, selfFullID)

			// Act
			err := op.call(c, context.Background(), selfFullID)

			// Assert
			if !errors.Is(err, ErrSelfProtected) {
				t.Fatalf("err = %v, want errors.Is(err, ErrSelfProtected)", err)
			}
			if spy.hit {
				t.Error("mutating Engine endpoint was reached, want it never contacted")
			}
		})
	}
}

// TestLifecycleSelfUnknown proves that when the agent is containerized but
// never resolved its own ID, every lifecycle call fails closed with
// ErrSelfUnknown before it ever contacts the Engine.
func TestLifecycleSelfUnknown(t *testing.T) {
	t.Parallel()

	for _, op := range lifecycleOps() {
		t.Run(op.name, func(t *testing.T) {
			t.Parallel()

			// Arrange
			c, rec := newFakeEngine(t, map[string]http.HandlerFunc{})
			c = withSelf(c, true, "")

			// Act
			err := op.call(c, context.Background(), "some-container")

			// Assert
			if !errors.Is(err, ErrSelfUnknown) {
				t.Fatalf("err = %v, want errors.Is(err, ErrSelfUnknown)", err)
			}
			if got := rec.Count(); got != 0 {
				t.Errorf("engine request count = %d, want 0", got)
			}
		})
	}
}

// TestLifecycleNotContainerized proves that when the agent is not running in
// a container at all, self-exclusion is inapplicable and the operation
// proceeds to the Engine normally.
func TestLifecycleNotContainerized(t *testing.T) {
	t.Parallel()

	// Arrange
	const ref = "other-container"
	const resolvedID = "other0000000000000000000000000000000000000000000000000000000"
	c, _ := newFakeEngine(t, map[string]http.HandlerFunc{
		inspectRoute(ref): jsonHandler(http.StatusOK, container.InspectResponse{ID: resolvedID}),
		"POST /containers/" + resolvedID + "/start": jsonHandler(http.StatusOK, nil),
	})
	c = withSelf(c, false, "")

	// Act
	err := c.StartContainer(context.Background(), ref)

	// Assert
	if err != nil {
		t.Fatalf("StartContainer() error = %v, want nil", err)
	}
}

// TestLifecycleInvalidRef proves ValidateRef runs before the self check and
// before any Engine call: a traversal ref is rejected with zero requests.
func TestLifecycleInvalidRef(t *testing.T) {
	t.Parallel()

	const badRef = "../../info"

	for _, op := range lifecycleOps() {
		t.Run(op.name, func(t *testing.T) {
			t.Parallel()

			// Arrange
			c, rec := newFakeEngine(t, map[string]http.HandlerFunc{})

			// Act
			err := op.call(c, context.Background(), badRef)

			// Assert
			if !errors.Is(err, ErrInvalidRef) {
				t.Fatalf("err = %v, want errors.Is(err, ErrInvalidRef)", err)
			}
			if got := rec.Count(); got != 0 {
				t.Errorf("engine request count = %d, want 0", got)
			}
		})
	}
}

// TestRemoveRunningConflict proves the Engine's conflict response (removing
// a running container) surfaces as ErrConflict (D10).
func TestRemoveRunningConflict(t *testing.T) {
	t.Parallel()

	// Arrange
	const ref = "running-container"
	const resolvedID = "running0000000000000000000000000000000000000000000000000000"
	c, _ := newFakeEngine(t, map[string]http.HandlerFunc{
		inspectRoute(ref):                  jsonHandler(http.StatusOK, container.InspectResponse{ID: resolvedID}),
		"DELETE /containers/" + resolvedID: errorHandler(http.StatusConflict),
	})
	c = withSelf(c, false, "")

	// Act
	err := c.RemoveContainer(context.Background(), ref)

	// Assert
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("err = %v, want errors.Is(err, ErrConflict)", err)
	}
}

// TestStartAlreadyRunning proves the Engine's not-modified response (start on
// a running container, HTTP 304) is treated as success (D9), never as an
// error the caller has to special-case.
//
// The moby v0.5.1 client's checkResponseErr classifies every 2xx/3xx
// response as success before the body is even inspected (request.go:225),
// so ContainerStart returns a nil error for 304 exactly as it does for 204.
// classify's ErrNotModified branch (errors.go) exists for callers that
// receive an actual non-2xx error carrying that condition elsewhere in the
// package; it is not reachable from this call path, and D9's "success"
// requirement is already met without it.
func TestStartAlreadyRunning(t *testing.T) {
	t.Parallel()

	// Arrange
	const ref = "running-container"
	const resolvedID = "running0000000000000000000000000000000000000000000000000000"
	c, _ := newFakeEngine(t, map[string]http.HandlerFunc{
		inspectRoute(ref): jsonHandler(http.StatusOK, container.InspectResponse{ID: resolvedID}),
		"POST /containers/" + resolvedID + "/start": jsonHandler(http.StatusNotModified, nil),
	})
	c = withSelf(c, false, "")

	// Act
	err := c.StartContainer(context.Background(), ref)

	// Assert
	if err != nil {
		t.Fatalf("StartContainer() error = %v, want nil (already-running is success)", err)
	}
}

// TestStopTimeoutIsExplicit proves StopContainer sends an explicit 10s
// Timeout, never nil, so an operator's Engine-side default cannot hang the
// request (D12).
func TestStopTimeoutIsExplicit(t *testing.T) {
	t.Parallel()

	// Arrange
	const ref = "some-container"
	const resolvedID = "some00000000000000000000000000000000000000000000000000000000"
	var gotTimeout *int
	var gotTimeoutSet bool
	c, _ := newFakeEngine(t, map[string]http.HandlerFunc{
		inspectRoute(ref): jsonHandler(http.StatusOK, container.InspectResponse{ID: resolvedID}),
		"POST /containers/" + resolvedID + "/stop": func(w http.ResponseWriter, r *http.Request) {
			q := r.URL.Query().Get("t")
			gotTimeoutSet = q != ""
			if gotTimeoutSet {
				n := 0
				for _, ch := range q {
					n = n*10 + int(ch-'0')
				}
				gotTimeout = &n
			}
			w.WriteHeader(http.StatusOK)
		},
	})
	c = withSelf(c, false, "")

	// Act
	err := c.StopContainer(context.Background(), ref)

	// Assert
	if err != nil {
		t.Fatalf("StopContainer() error = %v, want nil", err)
	}
	if !gotTimeoutSet {
		t.Fatal("stop request carried no explicit timeout, want t=10")
	}
	if gotTimeout == nil || *gotTimeout != stopGraceSeconds {
		t.Errorf("stop timeout = %v, want %d", gotTimeout, stopGraceSeconds)
	}
}

// TestLifecycleUnknownRef proves an unresolvable ref surfaces as ErrNotFound,
// via the pre-flight inspect resolveTarget always performs.
func TestLifecycleUnknownRef(t *testing.T) {
	t.Parallel()

	// Arrange
	const ref = "missing-container"
	c, _ := newFakeEngine(t, map[string]http.HandlerFunc{
		inspectRoute(ref): errorHandler(http.StatusNotFound),
	})
	c = withSelf(c, false, "")

	// Act
	err := c.KillContainer(context.Background(), ref)

	// Assert
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want errors.Is(err, ErrNotFound)", err)
	}
}
