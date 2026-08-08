package httpapi

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/scnplt/devmon-agent/internal/dockerx"
	"github.com/scnplt/devmon-agent/internal/state"
)

// Four small interfaces, declared here rather than in dockerx, because the
// consumer owns the contract (repo rule: accept interfaces, return structs).
// Splitting by object type keeps each within the 1-3 method guideline and
// lets Phase 4 and Phase 5 add their own without widening this one.

// ContainerReader lists and inspects containers.
type ContainerReader interface {
	ListContainers(ctx context.Context, all bool) (dockerx.ListResult[dockerx.ContainerSummary], error)
	InspectContainer(ctx context.Context, ref string) (dockerx.ContainerDetail, error)
}

// ImageReader lists and inspects images.
type ImageReader interface {
	ListImages(ctx context.Context) (dockerx.ListResult[dockerx.ImageSummary], error)
	InspectImage(ctx context.Context, ref string) (dockerx.ImageDetail, error)
}

// NetworkReader lists and inspects networks.
type NetworkReader interface {
	ListNetworks(ctx context.Context) (dockerx.ListResult[dockerx.NetworkSummary], error)
	InspectNetwork(ctx context.Context, ref string) (dockerx.NetworkDetail, error)
}

// VolumeReader lists and inspects volumes.
type VolumeReader interface {
	ListVolumes(ctx context.Context) (dockerx.ListResult[dockerx.VolumeSummary], error)
	InspectVolume(ctx context.Context, ref string) (dockerx.VolumeSummary, error)
}

// DockerReader is the full Docker surface the routes in this file, in
// logs.go, and in lifecycle.go depend on. *dockerx.Client satisfies it; a
// test fake can satisfy it without a live Engine. LogReader and
// ContainerController are embedded rather than added as extra constructor
// parameters (D14, D7): NewServer's signature stays untouched, and each
// remains independently referenceable.
type DockerReader interface {
	ContainerReader
	ImageReader
	NetworkReader
	VolumeReader
	LogReader
	ContainerController
}

// Compile-time proof the concrete client still satisfies the contract. A
// signature mismatch fails here, in the package that owns the contract,
// rather than as a confusing error at the cmd/devmon-agent call site.
var _ DockerReader = (*dockerx.Client)(nil)

// Messages for the three failure bodies a dockerx read can produce. Every
// value here is a security decision: none of them names the underlying
// Engine error, which may carry a socket path or other host detail.
const (
	// msgInvalidRef is served when a path-supplied object reference fails
	// dockerx.ValidateRef. The request never reached the Engine.
	msgInvalidRef = "invalid object reference"

	// msgNotFound is served when the Engine confirms the object does not
	// exist. Safe to state plainly here: the caller is already an
	// authenticated device (requireDevice ran first), so the anti-enumeration
	// reasoning behind /v1/pair's uniform rejection does not apply.
	msgNotFound = "not found"

	// msgEngineUnavailable is served for every Engine failure that is not a
	// validation error or a confirmed absence: unreachable socket, timeout,
	// or any other upstream fault. The Engine is a dependency of this agent,
	// so its failures are gateway failures (502), not agent bugs (500) —
	// 500 must keep meaning "the agent itself broke".
	msgEngineUnavailable = "docker engine unavailable"

	// msgSelfProtected is served when a lifecycle operation targets the
	// agent's own container — a fixed rule, not a policy tier (D1).
	msgSelfProtected = "the agent cannot act on itself"

	// msgSelfUnknown is served when the agent is containerised but could not
	// determine which container it is, so the self-exclusion rule cannot be
	// enforced and lifecycle operations fail closed (D3).
	msgSelfUnknown = "agent cannot identify its own container"

	// msgContainerConflict is served when the Engine refuses an operation
	// because of the object's current state — in practice, deleting a
	// running container (D10).
	msgContainerConflict = "container is running"
)

// writeDockerError maps a dockerx failure onto a status code. ErrInvalidRef
// and ErrConflict are client mistakes (400, 409), ErrNotFound is an answer
// (404), ErrSelfProtected and ErrSelfUnknown are the self-exclusion rule
// refusing an operation (403, 503), and everything else is the Engine
// failing upstream of us (502) — never 500, which must keep meaning "the
// agent itself broke". r carries the in-flight audit entry (a no-op outside
// a mutating route, via setAuditOutcome), so every failure path — including
// the pre-existing not-found and invalid-ref branches — is recorded.
func (s *Server) writeDockerError(w http.ResponseWriter, r *http.Request, op string, err error) {
	switch {
	case errors.Is(err, dockerx.ErrInvalidRef):
		setAuditOutcome(r.Context(), state.OutcomeInvalid, "")
		s.writeError(w, http.StatusBadRequest, msgInvalidRef)
	case errors.Is(err, dockerx.ErrNotFound):
		setAuditOutcome(r.Context(), state.OutcomeNotFound, "")
		s.writeError(w, http.StatusNotFound, msgNotFound)
	case errors.Is(err, dockerx.ErrSelfProtected):
		setAuditOutcome(r.Context(), state.OutcomeDeniedSelf, "")
		s.writeError(w, http.StatusForbidden, msgSelfProtected)
	case errors.Is(err, dockerx.ErrSelfUnknown):
		setAuditOutcome(r.Context(), state.OutcomeUnavailable, "")
		s.writeError(w, http.StatusServiceUnavailable, msgSelfUnknown)
	case errors.Is(err, dockerx.ErrConflict):
		setAuditOutcome(r.Context(), state.OutcomeConflict, "")
		s.writeError(w, http.StatusConflict, msgContainerConflict)
	default:
		setAuditOutcome(r.Context(), state.OutcomeEngineError, "")
		s.log.Error(op, slog.Any("err", err))
		s.writeError(w, http.StatusBadGateway, msgEngineUnavailable)
	}
}

// requireDocker reports whether the server has a Docker reader configured,
// answering 502 without panicking otherwise. The existing test helpers
// construct servers with nil dependencies deliberately, and the eight routes
// below are registered unconditionally, so every handler must tolerate a nil
// s.dc rather than assume routes() only wires them when dc is present.
func (s *Server) requireDocker(w http.ResponseWriter) bool {
	if s.dc == nil {
		s.log.Error("docker reader not configured")
		s.writeError(w, http.StatusBadGateway, msgEngineUnavailable)
		return false
	}
	return true
}

// listAllParam parses the ?all= query parameter. A parse error is a client
// typo, not an attack: it defaults to false rather than failing the request.
func listAllParam(r *http.Request) bool {
	all, err := strconv.ParseBool(r.URL.Query().Get("all"))
	if err != nil {
		return false
	}
	return all
}

// handleListContainers lists containers, optionally including stopped ones.
func (s *Server) handleListContainers(w http.ResponseWriter, r *http.Request) {
	if !s.requireDocker(w) {
		return
	}

	resp, err := s.dc.ListContainers(r.Context(), listAllParam(r))
	if err != nil {
		s.writeDockerError(w, r, "list containers", err)
		return
	}

	s.writeJSON(w, http.StatusOK, resp)
}

// handleInspectContainer returns the full projection of a single container.
func (s *Server) handleInspectContainer(w http.ResponseWriter, r *http.Request) {
	if !s.requireDocker(w) {
		return
	}

	resp, err := s.dc.InspectContainer(r.Context(), r.PathValue("id"))
	if err != nil {
		s.writeDockerError(w, r, "inspect container", err)
		return
	}

	s.writeJSON(w, http.StatusOK, resp)
}

// handleListImages lists images on the host.
func (s *Server) handleListImages(w http.ResponseWriter, r *http.Request) {
	if !s.requireDocker(w) {
		return
	}

	resp, err := s.dc.ListImages(r.Context())
	if err != nil {
		s.writeDockerError(w, r, "list images", err)
		return
	}

	s.writeJSON(w, http.StatusOK, resp)
}

// handleInspectImage returns the full projection of a single image.
func (s *Server) handleInspectImage(w http.ResponseWriter, r *http.Request) {
	if !s.requireDocker(w) {
		return
	}

	resp, err := s.dc.InspectImage(r.Context(), r.PathValue("id"))
	if err != nil {
		s.writeDockerError(w, r, "inspect image", err)
		return
	}

	s.writeJSON(w, http.StatusOK, resp)
}

// handleListNetworks lists networks on the host.
func (s *Server) handleListNetworks(w http.ResponseWriter, r *http.Request) {
	if !s.requireDocker(w) {
		return
	}

	resp, err := s.dc.ListNetworks(r.Context())
	if err != nil {
		s.writeDockerError(w, r, "list networks", err)
		return
	}

	s.writeJSON(w, http.StatusOK, resp)
}

// handleInspectNetwork returns the full projection of a single network,
// including its attached containers.
func (s *Server) handleInspectNetwork(w http.ResponseWriter, r *http.Request) {
	if !s.requireDocker(w) {
		return
	}

	resp, err := s.dc.InspectNetwork(r.Context(), r.PathValue("id"))
	if err != nil {
		s.writeDockerError(w, r, "inspect network", err)
		return
	}

	s.writeJSON(w, http.StatusOK, resp)
}

// handleListVolumes lists volumes on the host.
func (s *Server) handleListVolumes(w http.ResponseWriter, r *http.Request) {
	if !s.requireDocker(w) {
		return
	}

	resp, err := s.dc.ListVolumes(r.Context())
	if err != nil {
		s.writeDockerError(w, r, "list volumes", err)
		return
	}

	s.writeJSON(w, http.StatusOK, resp)
}

// handleInspectVolume returns the full projection of a single volume.
// Volumes are named, not IDed, so the reference comes from the "name" path
// value rather than "id".
func (s *Server) handleInspectVolume(w http.ResponseWriter, r *http.Request) {
	if !s.requireDocker(w) {
		return
	}

	resp, err := s.dc.InspectVolume(r.Context(), r.PathValue("name"))
	if err != nil {
		s.writeDockerError(w, r, "inspect volume", err)
		return
	}

	s.writeJSON(w, http.StatusOK, resp)
}
