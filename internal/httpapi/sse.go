// SPDX-License-Identifier: AGPL-3.0-only

package httpapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"
)

// SSE framing and liveness constants.
const (
	sseContentType   = "text/event-stream"
	sseEventLog      = "log"
	sseEventError    = "error"
	sseEventSnapshot = "snapshot"
	sseEventHealth   = "health"
)

// keepaliveInterval bounds how long the stream may be silent. A container
// that logs nothing for minutes is ordinary; a TCP connection that sends
// nothing for minutes is dropped by mobile-carrier NAT and by any proxy in
// between. The keepalive write is also the only way this agent learns the
// client is gone — the write fails, and the stream unwinds. Without it an
// abandoned stream leaks a goroutine and an Engine connection.
//
// A package variable rather than a const so Task 9's race test can shorten it
// and observe a keepalive without waiting 20 real seconds.
var keepaliveInterval = 20 * time.Second

// eventHeartbeatInterval bounds how long the container event stream may be
// silent. It is 25s rather than keepaliveInterval's 20s because the two routes
// have different silence profiles and the event stream's interval is fixed by
// its own contract. A separate package variable rather than a shared one: both
// exist as variables so tests can shorten them, and a shared variable would
// mean one route's test silently retiming the other route's stream.
var eventHeartbeatInterval = 25 * time.Second

// sseWriter frames one SSE response. Headers are written lazily, on the first
// event: once 200 and the headers are committed the status can never be
// corrected, so deferring the commit keeps the 400/404/502 mapping available
// for every failure that happens before there is anything to send.
//
// mu makes "one frame is atomic" an invariant of the type rather than a
// caller obligation: the keepalive goroutine and the line-emitting goroutine
// both write to the same underlying http.ResponseWriter, which is not safe
// for concurrent use, and it is held across each whole frame in start, event,
// and keepalive.
type sseWriter struct {
	mu      sync.Mutex
	w       http.ResponseWriter
	rc      *http.ResponseController
	started bool
}

// newSSEWriter wraps w for SSE framing. It does not touch w until the first
// call to start or event.
func newSSEWriter(w http.ResponseWriter) *sseWriter {
	return &sseWriter{w: w, rc: http.NewResponseController(w)}
}

// start commits the response: sets the SSE headers, clears the server's write
// deadline so the stream can outlive the global WriteTimeout, writes 200, and
// flushes so the client sees headers immediately rather than after the first
// event is buffered.
//
// A SetWriteDeadline failure is fatal to the stream and is returned rather
// than ignored — silently continuing produces a stream that dies at the
// server's WriteTimeout with nothing in the log to explain why.
func (s *sseWriter) start() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.startLocked()
}

// startLocked is start's body, factored out so event can call it while
// already holding mu — sync.Mutex is not reentrant, so start itself cannot
// be called from inside a locked section.
func (s *sseWriter) startLocked() error {
	s.w.Header().Set("Content-Type", sseContentType)
	s.w.Header().Set("Cache-Control", "no-store")
	s.w.Header().Set("X-Content-Type-Options", "nosniff")
	s.w.Header().Set("Connection", "keep-alive")
	// X-Accel-Buffering disables response buffering on nginx-style reverse
	// proxies, which would otherwise hold the stream until it closes.
	s.w.Header().Set("X-Accel-Buffering", "no")

	// The zero time.Time CLEARS the deadline; time.Now() would expire it
	// immediately. This is the one call that escapes the server's 30s
	// WriteTimeout for the lifetime of the connection.
	if err := s.rc.SetWriteDeadline(time.Time{}); err != nil {
		return fmt.Errorf("clear write deadline: %w", err)
	}

	s.w.WriteHeader(http.StatusOK)
	s.started = true

	if err := s.rc.Flush(); err != nil {
		return fmt.Errorf("flush sse headers: %w", err)
	}
	return nil
}

// event writes one SSE frame: id:, event:, and data: lines followed by the
// terminating blank line, then flushes. It calls start first if the response
// has not been committed yet.
//
// data is JSON-encoded rather than written as raw text for two reasons: a
// data: line must never contain a raw newline, and JSON-encoding escapes any
// that the payload carries.
func (s *sseWriter) event(id, name string, data any) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if !s.started {
		if err := s.startLocked(); err != nil {
			return err
		}
	}

	// json.Marshal, not json.NewEncoder(...).Encode: Encode appends its own
	// trailing '\n', which would land in the middle of the frame rather than
	// only at its end.
	payload, err := json.Marshal(data)
	if err != nil {
		return fmt.Errorf("marshal sse payload: %w", err)
	}

	if _, err := fmt.Fprintf(s.w, "id: %s\nevent: %s\ndata: %s\n\n", id, name, payload); err != nil {
		return fmt.Errorf("write sse frame: %w", err)
	}

	if err := s.rc.Flush(); err != nil {
		return fmt.Errorf("flush sse frame: %w", err)
	}
	return nil
}

// comment writes a bare SSE comment frame. It does not count as an event and
// carries no id or data; its only purpose is to keep the connection warm
// through NAT and proxy idle timeouts, and to discover a client that vanished
// without closing — the write fails, and the stream unwinds.
//
// It calls startLocked first if the response has not been committed yet, for
// the reason keepalive's doc comment gives — a silent container's first
// activity may well be a comment tick rather than a log line, and without
// this the response would commit as a plain 200 with none of the SSE
// headers and, critically, the server's write deadline never cleared, so the
// stream would still die at the server's WriteTimeout. Committing here
// cannot steal D12's 404 window: the pre-stream inspect is bounded by
// callTimeout and always returns well before the first tick.
//
// text must be a compile-time literal with no embedded newline: the only two
// call sites pass "keepalive" and "heartbeat", so there is no runtime check
// for a case that cannot occur.
func (s *sseWriter) comment(text string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if !s.started {
		if err := s.startLocked(); err != nil {
			return err
		}
	}

	if _, err := fmt.Fprintf(s.w, ": %s\n\n", text); err != nil {
		return fmt.Errorf("write sse comment: %w", err)
	}
	if err := s.rc.Flush(); err != nil {
		return fmt.Errorf("flush sse comment: %w", err)
	}
	return nil
}

// keepalive writes the log stream's comment frame. Kept as a named method so
// that route's exact wire bytes cannot drift.
func (s *sseWriter) keepalive() error { return s.comment("keepalive") }

// Started reports whether the response has been committed. Callers use it
// after a stream ends to decide between writeDockerError (nothing sent yet)
// and a terminal event: error frame (headers and at least one byte already
// on the wire).
func (s *sseWriter) Started() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.started
}
