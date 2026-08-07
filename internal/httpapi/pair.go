package httpapi

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/scnplt/devmon-agent/internal/state"
)

// Limits and messages for the one route reachable without a client
// certificate. Every value here is a security decision, not a tunable.
const (
	// maxPairBodyBytes bounds the pairing request body. This route has no
	// client certificate to gate it and there is no rate limiting until
	// Phase 6, so an unbounded JSON body is a trivial memory-exhaustion
	// vector.
	maxPairBodyBytes = 8 << 10

	// msgPairFailed is the terse rejection served for every reason a pairing
	// attempt fails: an unknown, expired, or already-used code, or a
	// malformed CSR. The client must never be able to tell which.
	msgPairFailed = "pairing failed"

	// msgPairInternalError covers the case where the operator's code was
	// already spent but the agent could not finish minting a certificate.
	msgPairInternalError = "internal error"

	// pendingDeviceName is the placeholder recorded when the device row is
	// created, before its pairing code is redeemed. RedeemPairingCode
	// returns the operator-chosen name, which replaces this placeholder: the
	// device row must exist before redemption (RedeemPairingCode needs its
	// ID), but the request body must never be the source of the name.
	pendingDeviceName = "pending"

	// pemTypeCertificateRequest is the PEM block type of a PKCS#10 CSR.
	pemTypeCertificateRequest = "CERTIFICATE REQUEST"
)

type pairRequest struct {
	PairingCode string `json:"pairing_code"`
	CSRPEM      string `json:"csr_pem"`
}

type pairResponse struct {
	DeviceID       string `json:"device_id"`
	CertificatePEM string `json:"certificate_pem"`
	CACertificate  string `json:"ca_certificate_pem"`
	NotAfter       string `json:"not_after"` // RFC3339
}

// handlePair issues a device certificate against a one-time, operator-minted
// pairing code. It is the only route reachable without a client certificate
// (D2 in the Phase 2 plan): the device has none yet, so the code itself is
// what authenticates the call.
func (s *Server) handlePair(w http.ResponseWriter, r *http.Request) {
	req, ok := s.decodePairRequest(w, r)
	if !ok {
		return
	}

	csrDER, ok := decodeCSRPEM(req.CSRPEM)
	if !ok {
		s.writeError(w, http.StatusUnauthorized, msgPairFailed)
		return
	}

	resp, err := s.pairDevice(r.Context(), req.PairingCode, csrDER)
	if err != nil {
		if errors.Is(err, state.ErrPairingCodeInvalid) {
			s.writeError(w, http.StatusUnauthorized, msgPairFailed)
			return
		}
		s.log.Error("pair device", slog.Any("err", err))
		s.writeError(w, http.StatusInternalServerError, msgPairInternalError)
		return
	}

	s.writeJSON(w, http.StatusCreated, resp)
}

// decodePairRequest reads and bounds the request body, reporting 413 for an
// oversized body and 400 for anything else that fails to decode.
func (s *Server) decodePairRequest(w http.ResponseWriter, r *http.Request) (pairRequest, bool) {
	r.Body = http.MaxBytesReader(w, r.Body, maxPairBodyBytes)

	var req pairRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			s.writeError(w, http.StatusRequestEntityTooLarge, "request body too large")
			return pairRequest{}, false
		}
		s.writeError(w, http.StatusBadRequest, "malformed request body")
		return pairRequest{}, false
	}
	return req, true
}

// decodeCSRPEM extracts the raw DER bytes of a PEM-encoded PKCS#10 CSR,
// rejecting anything that is not a well-formed "CERTIFICATE REQUEST" block
// before the operator's pairing code is ever touched.
func decodeCSRPEM(csrPEM string) ([]byte, bool) {
	block, _ := pem.Decode([]byte(csrPEM))
	if block == nil || block.Type != pemTypeCertificateRequest {
		return nil, false
	}
	return block.Bytes, true
}

// pairDevice runs the pairing sequence in the order that keeps a failure
// from wasting the operator's code for nothing: create the device row first,
// redeem the code second, issue the certificate third. Any failure after the
// row was created deletes it — RedeemPairingCode failing means the code was
// never actually spent, so the deletion is bookkeeping rather than a
// rollback of a granted credential, but it is identical either way from the
// caller's point of view: no orphan device row survives.
func (s *Server) pairDevice(ctx context.Context, code string, csrDER []byte) (pairResponse, error) {
	device, err := s.st.CreateDevice(ctx, pendingDeviceName)
	if err != nil {
		return pairResponse{}, fmt.Errorf("create device for pairing: %w", err)
	}

	deviceName, err := s.st.RedeemPairingCode(ctx, code, device.ID)
	if err != nil {
		s.deleteOrphanedDevice(ctx, device.ID)
		return pairResponse{}, fmt.Errorf("redeem pairing code: %w", err)
	}

	if err := s.st.RenameDevice(ctx, device.ID, deviceName); err != nil {
		s.deleteOrphanedDevice(ctx, device.ID)
		return pairResponse{}, fmt.Errorf("rename paired device: %w", err)
	}

	now := time.Now()
	certPEM, serial, notAfter, err := s.ca.IssueDeviceCert(csrDER, device.ID, now)
	if err != nil {
		s.deleteOrphanedDevice(ctx, device.ID)
		return pairResponse{}, fmt.Errorf("issue device certificate: %w", err)
	}

	if err := s.st.RecordDeviceCert(ctx, device.ID, serial, now, notAfter); err != nil {
		s.deleteOrphanedDevice(ctx, device.ID)
		return pairResponse{}, fmt.Errorf("record device certificate: %w", err)
	}

	return pairResponse{
		DeviceID:       device.ID,
		CertificatePEM: string(certPEM),
		CACertificate:  string(s.ca.CertPEM()),
		NotAfter:       notAfter.UTC().Format(time.RFC3339),
	}, nil
}

// deleteOrphanedDevice removes a device row left behind by a failed pairing
// attempt. The original failure is logged by the caller; this logs only a
// second failure, if the cleanup itself does not succeed.
func (s *Server) deleteOrphanedDevice(ctx context.Context, deviceID string) {
	if err := s.st.DeleteDevice(ctx, deviceID); err != nil {
		s.log.Error("delete orphaned device after failed pairing",
			slog.String("device_id", deviceID),
			slog.Any("err", err),
		)
	}
}
