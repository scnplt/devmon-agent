package httpapi

import (
	"net/http"
	"time"

	"github.com/scnplt/devmon-agent/internal/version"
)

// APIVersion is the contract version the Android app negotiates against. The app
// ships independently of the agent, so it must be able to detect an incompatible
// agent before the user hits an error rather than after.
const APIVersion = "v1"

// statusResponse is the ONLY payload served without a client certificate.
//
// Its fields are a strict allowlist: version, policy, server time — and, from
// Phase 2, the CA fingerprint. This endpoint may inform, never issue. It must
// never carry host, container, credential, or configuration detail, because
// anything here is readable by every scanner that finds the port.
type statusResponse struct {
	APIVersion   string `json:"api_version"`
	AgentVersion string `json:"agent_version"`
	PolicyMode   string `json:"policy_mode"`
	ServerTime   string `json:"server_time"`
}

// handleStatus serves the unauthenticated status payload.
//
// Advertising the policy mode is deliberate: it lets a client tell "the agent
// refuses to restart containers" apart from "the agent is broken" without
// needing a credential.
func (s *Server) handleStatus(w http.ResponseWriter, _ *http.Request) {
	s.writeJSON(w, http.StatusOK, statusResponse{
		APIVersion:   APIVersion,
		AgentVersion: version.Version,
		PolicyMode:   s.cfg.PolicyMode.String(),
		ServerTime:   time.Now().UTC().Format(time.RFC3339),
	})
}
