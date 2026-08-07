package httpapi

import (
	"encoding/json"
	"log/slog"
	"net/http"
)

// writeJSON is the single exit point for every response body, so the envelope
// cannot drift between handlers as later phases add routes.
//
// It is a method on *Server rather than a free function so the encode-failure
// path below reaches the injected logger. Using slog.Default() here would send
// that one line to the process-default handler instead of the file-backed sink,
// so it would never reach agent.log — losing exactly the diagnostic an operator
// needs after the fact.
func (s *Server) writeJSON(w http.ResponseWriter, code int, body any) {
	w.Header().Set("Content-Type", "application/json")
	// This port is internet-facing; nothing it serves should be cached by an
	// intermediary, least of all a status payload used for liveness checks.
	w.Header().Set("Cache-Control", "no-store")
	// The one endpoint every scanner can reach unauthenticated serves JSON only;
	// there is no reason to let a client content-sniff it into something else.
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(code)

	if err := json.NewEncoder(w).Encode(body); err != nil {
		// The status line and headers are already committed, so there is no way
		// to turn this into an error response. Recording it is all that is left.
		s.log.Error("write response", slog.Any("err", err))
	}
}

type errorBody struct {
	Error string `json:"error"`
}

// writeError returns a deliberately terse message.
//
// The caller may be unauthenticated and on the open internet, so error text must
// never carry the state path, the Docker host, a certificate subject, or the
// reason a credential was rejected. "Which of these two things did I get wrong"
// is exactly the signal an attacker wants and a legitimate operator can get from
// the agent's own logs instead.
func (s *Server) writeError(w http.ResponseWriter, code int, msg string) {
	s.writeJSON(w, code, errorBody{Error: msg})
}
