// SPDX-License-Identifier: AGPL-3.0-only

package httpapi

import (
	"context"
	"net/http"

	"github.com/scnplt/devmon-agent/internal/state"
)

// ContainerController is the mutating container surface. Declared here rather
// than in dockerx because the consumer owns the contract, and embedded into
// DockerReader rather than added as a NewServer parameter — exactly what
// Phase 4 did with LogReader, so five existing test helpers stay untouched (D7).
type ContainerController interface {
	StartContainer(ctx context.Context, ref string) error
	RestartContainer(ctx context.Context, ref string) error
	StopContainer(ctx context.Context, ref string) error
	KillContainer(ctx context.Context, ref string) error
	RemoveContainer(ctx context.Context, ref string) error
}

// handleLifecycle runs one lifecycle action and answers 204. Every action
// shares this body: five near-identical handlers is the repetition the DRY
// rule exists to prevent, and a shared body means the audit refinement and
// the error mapping cannot differ between operations.
//
// act is called only after requireDocker confirms s.dc is set, so a nil
// s.dc never reaches the interface dispatch that would otherwise panic.
//
// Success carries no body (D8): every Engine lifecycle call is asynchronous
// at the edges, so any state returned here would already be stale. That
// includes the Engine's "already in the requested state" answer (D9): this
// SDK version treats it as a nil error at the client layer, so it reaches
// the success path below, and writeDockerError's dockerx.ErrNotModified
// branch answers the same 204 if a future SDK version ever surfaces it as an
// error instead.
func (s *Server) handleLifecycle(op string, act func(context.Context, string) error) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.requireDocker(w) {
			return
		}

		if err := act(r.Context(), r.PathValue("id")); err != nil {
			s.writeDockerError(w, r, op, err)
			return
		}

		setAuditOutcome(r.Context(), state.OutcomeSuccess, "")
		w.WriteHeader(http.StatusNoContent)
	}
}

// handleStartContainer starts a stopped container.
func (s *Server) handleStartContainer(w http.ResponseWriter, r *http.Request) {
	s.handleLifecycle("start container", func(ctx context.Context, ref string) error {
		return s.dc.StartContainer(ctx, ref)
	})(w, r)
}

// handleRestartContainer stops and starts a container.
func (s *Server) handleRestartContainer(w http.ResponseWriter, r *http.Request) {
	s.handleLifecycle("restart container", func(ctx context.Context, ref string) error {
		return s.dc.RestartContainer(ctx, ref)
	})(w, r)
}

// handleStopContainer stops a running container.
func (s *Server) handleStopContainer(w http.ResponseWriter, r *http.Request) {
	s.handleLifecycle("stop container", func(ctx context.Context, ref string) error {
		return s.dc.StopContainer(ctx, ref)
	})(w, r)
}

// handleKillContainer sends SIGKILL to a running container.
func (s *Server) handleKillContainer(w http.ResponseWriter, r *http.Request) {
	s.handleLifecycle("kill container", func(ctx context.Context, ref string) error {
		return s.dc.KillContainer(ctx, ref)
	})(w, r)
}

// handleRemoveContainer deletes a stopped container.
func (s *Server) handleRemoveContainer(w http.ResponseWriter, r *http.Request) {
	s.handleLifecycle("remove container", func(ctx context.Context, ref string) error {
		return s.dc.RemoveContainer(ctx, ref)
	})(w, r)
}
