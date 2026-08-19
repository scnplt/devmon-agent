// SPDX-License-Identifier: AGPL-3.0-only

package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/scnplt/devmon-agent/internal/state"
)

// Limits and messages for the two routes an already-paired device uses on
// itself: renewing its own certificate and giving up its own access. Both
// sit behind requireDevice, so the caller is always an authenticated,
// active device acting on its own identity — never another device's.
const (
	// maxRenewBodyBytes bounds the renewal request body. The route sits
	// behind requireDevice, so it is not the open-internet vector
	// maxPairBodyBytes guards against, but a JSON body is still bounded on
	// every route as a matter of course.
	maxRenewBodyBytes = 8 << 10

	// msgRenewFailed is the terse rejection served for every reason a
	// renewal fails: a malformed CSR or an issuance error. The client must
	// not be able to tell which.
	msgRenewFailed = "renewal failed"

	// msgDeviceInternalError covers an unexpected failure recording or
	// superseding a certificate, or revoking the caller's own device.
	msgDeviceInternalError = "internal error"
)

type renewRequest struct {
	CSRPEM string `json:"csr_pem"`
}

type renewResponse struct {
	CertificatePEM string `json:"certificate_pem"`
	NotAfter       string `json:"not_after"` // RFC3339
}

// handleRenew issues a fresh certificate for the calling device's own
// identity. The device ID comes only from DeviceFrom — never from the
// request body or a path parameter — so a device can renew only itself.
func (s *Server) handleRenew(w http.ResponseWriter, r *http.Request) {
	device, ok := DeviceFrom(r.Context())
	if !ok {
		// requireDevice always injects a device before this handler runs;
		// this branch exists only to fail closed if that ever changes.
		s.writeError(w, http.StatusInternalServerError, msgDeviceInternalError)
		return
	}

	req, ok := s.decodeRenewRequest(w, r)
	if !ok {
		setAuditOutcome(r.Context(), state.OutcomeInvalid, "malformed request body")
		return
	}

	csrDER, ok := decodeCSRPEM(req.CSRPEM)
	if !ok {
		setAuditOutcome(r.Context(), state.OutcomeInvalid, "malformed csr")
		s.writeError(w, http.StatusUnauthorized, msgRenewFailed)
		return
	}

	resp, err := s.renewDevice(r.Context(), device.ID, csrDER)
	if err != nil {
		s.log.Error("renew device certificate",
			slog.String("device_id", device.ID),
			slog.Any("err", err),
		)
		setAuditOutcome(r.Context(), state.OutcomeInternalError, "")
		s.writeError(w, http.StatusInternalServerError, msgDeviceInternalError)
		return
	}

	s.writeJSON(w, http.StatusOK, resp)
}

// decodeRenewRequest reads and bounds the request body, reporting 413 for an
// oversized body and 400 for anything else that fails to decode.
func (s *Server) decodeRenewRequest(w http.ResponseWriter, r *http.Request) (renewRequest, bool) {
	r.Body = http.MaxBytesReader(w, r.Body, maxRenewBodyBytes)

	var req renewRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			s.writeError(w, http.StatusRequestEntityTooLarge, "request body too large")
			return renewRequest{}, false
		}
		s.writeError(w, http.StatusBadRequest, "malformed request body")
		return renewRequest{}, false
	}
	return req, true
}

// renewDevice issues a new certificate for deviceID, records it, and marks
// every certificate the device held before as superseded (D6). The prior
// certificate is never deleted and its validity is never shortened: it
// keeps working until its own not_after, so a lost renewal response cannot
// strand the device.
func (s *Server) renewDevice(ctx context.Context, deviceID string, csrDER []byte) (renewResponse, error) {
	now := time.Now()
	certPEM, serial, notAfter, err := s.ca.IssueDeviceCert(csrDER, deviceID, now)
	if err != nil {
		return renewResponse{}, fmt.Errorf("issue renewed certificate for device %s: %w", deviceID, err)
	}

	if err := s.st.RecordDeviceCert(ctx, deviceID, serial, now, notAfter); err != nil {
		return renewResponse{}, fmt.Errorf("record renewed certificate for device %s: %w", deviceID, err)
	}

	if err := s.st.SupersedePriorCerts(ctx, deviceID, serial); err != nil {
		return renewResponse{}, fmt.Errorf("supersede prior certificates for device %s: %w", deviceID, err)
	}

	return renewResponse{
		CertificatePEM: string(certPEM),
		NotAfter:       notAfter.UTC().Format(time.RFC3339),
	}, nil
}

// handleUnpairSelf revokes the calling device's own access and returns 204
// with no body. Self-unpair is permitted under every policy mode: giving up
// your own access is not a privileged act, so this is never gated by
// policy.Mode.Allows.
func (s *Server) handleUnpairSelf(w http.ResponseWriter, r *http.Request) {
	device, ok := DeviceFrom(r.Context())
	if !ok {
		// requireDevice always injects a device before this handler runs;
		// this branch exists only to fail closed if that ever changes.
		s.writeError(w, http.StatusInternalServerError, msgDeviceInternalError)
		return
	}

	if err := s.st.RevokeDevice(r.Context(), device.ID); err != nil {
		s.log.Error("revoke self",
			slog.String("device_id", device.ID),
			slog.Any("err", err),
		)
		setAuditOutcome(r.Context(), state.OutcomeInternalError, "")
		s.writeError(w, http.StatusInternalServerError, msgDeviceInternalError)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}
