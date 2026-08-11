// SPDX-License-Identifier: AGPL-3.0-only

package httpapi

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// plainResponseWriter implements only the http.ResponseWriter interface —
// no Flush, no SetWriteDeadline, no Unwrap. http.ResponseController has
// nothing to reach through, so both calls return http.ErrNotSupported.
type plainResponseWriter struct {
	header http.Header
	body   strings.Builder
	status int
}

func newPlainResponseWriter() *plainResponseWriter {
	return &plainResponseWriter{header: http.Header{}}
}

func (p *plainResponseWriter) Header() http.Header         { return p.header }
func (p *plainResponseWriter) Write(b []byte) (int, error) { return p.body.Write(b) }
func (p *plainResponseWriter) WriteHeader(code int)        { p.status = code }

// TestSSEFrameBytes asserts the exact bytes of one emitted event: the id:,
// event:, and data: lines in order, each newline-terminated, followed by the
// blank line that terminates the frame. A frame missing that blank line is
// buffered by the client's SSE parser until the next frame arrives, which is
// nearly impossible to diagnose from the app side — so this test pins the
// byte layout rather than trusting a looser "contains" check.
func TestSSEFrameBytes(t *testing.T) {
	t.Parallel()

	// Arrange
	rec := &deadlineAwareRecorder{ResponseRecorder: httptest.NewRecorder()}
	sw := newSSEWriter(rec)

	// Act
	err := sw.event("2026-08-08T10:02:11.441Z", sseEventLog, map[string]string{
		"ts": "2026-08-08T10:02:11.441Z", "stream": "stdout", "line": "listening on :8080",
	})

	// Assert
	if err != nil {
		t.Fatalf("event() error = %v, want nil", err)
	}

	want := "id: 2026-08-08T10:02:11.441Z\n" +
		"event: log\n" +
		`data: {"line":"listening on :8080","stream":"stdout","ts":"2026-08-08T10:02:11.441Z"}` + "\n" +
		"\n"
	got := rec.Body.String()
	if got != want {
		t.Errorf("event() body = %q, want %q", got, want)
	}
}

// TestSSEKeepaliveBytes pins the exact bytes of a keepalive comment frame:
// no id, no event, no data — just the bare comment and the terminating
// blank line.
func TestSSEKeepaliveBytes(t *testing.T) {
	t.Parallel()

	// Arrange
	rec := &deadlineAwareRecorder{ResponseRecorder: httptest.NewRecorder()}
	sw := newSSEWriter(rec)

	// Act
	err := sw.keepalive()

	// Assert
	if err != nil {
		t.Fatalf("keepalive() error = %v, want nil", err)
	}
	want := ": keepalive\n\n"
	if got := rec.Body.String(); got != want {
		t.Errorf("keepalive() body = %q, want %q", got, want)
	}
}

// TestSSELazyStart is D12's contract: nothing is written to the underlying
// ResponseWriter, and Started() stays false, until the first event is
// emitted. Committing headers eagerly would make a pre-stream failure (a
// container that turns out not to exist) look like a successful empty
// stream, because the status code can never be corrected afterward.
func TestSSELazyStart(t *testing.T) {
	t.Parallel()

	// Arrange
	rec := &deadlineAwareRecorder{ResponseRecorder: httptest.NewRecorder()}
	sw := newSSEWriter(rec)

	// Act / Assert — before any event
	if sw.Started() {
		t.Fatal("Started() = true before any event, want false")
	}
	if rec.Header().Get("Content-Type") != "" {
		t.Fatalf("Content-Type set = %q before any event, want empty", rec.Header().Get("Content-Type"))
	}

	// Act — the first event
	if err := sw.event("1", sseEventLog, map[string]string{"line": "hello"}); err != nil {
		t.Fatalf("event() error = %v, want nil", err)
	}

	// Assert — after the first event
	if !sw.Started() {
		t.Fatal("Started() = false after the first event, want true")
	}
	if got := rec.Header().Get("Content-Type"); got != sseContentType {
		t.Errorf("Content-Type = %q, want %q", got, sseContentType)
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want %q", got, "no-store")
	}
	if got := rec.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("X-Content-Type-Options = %q, want %q", got, "nosniff")
	}
	if got := rec.Header().Get("Connection"); got != "keep-alive" {
		t.Errorf("Connection = %q, want %q", got, "keep-alive")
	}
	if got := rec.Header().Get("X-Accel-Buffering"); got != "no" {
		t.Errorf("X-Accel-Buffering = %q, want %q", got, "no")
	}
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusOK)
	}
}

// TestSSEDataHasNoRawNewline asserts a line containing a raw newline is
// JSON-escaped into a single data: line rather than splitting the SSE frame
// across multiple data: lines or corrupting the wire format.
func TestSSEDataHasNoRawNewline(t *testing.T) {
	t.Parallel()

	// Arrange
	rec := &deadlineAwareRecorder{ResponseRecorder: httptest.NewRecorder()}
	sw := newSSEWriter(rec)

	// Act
	err := sw.event("1", sseEventLog, map[string]string{"line": "first\nsecond"})

	// Assert
	if err != nil {
		t.Fatalf("event() error = %v, want nil", err)
	}

	body := rec.Body.String()
	lines := strings.Split(strings.TrimRight(body, "\n"), "\n")

	dataLines := 0
	for _, line := range lines {
		if strings.HasPrefix(line, "data:") {
			dataLines++
			if strings.Contains(line, "\n") {
				t.Errorf("data: line contains a raw newline: %q", line)
			}
			if !strings.Contains(line, `\n`) {
				t.Errorf("data: line = %q, want the embedded newline JSON-escaped as \\n", line)
			}
		}
	}
	if dataLines != 1 {
		t.Errorf("frame has %d data: lines, want exactly 1", dataLines)
	}
}

// TestSSEKeepaliveStartsResponse is H1's regression: a keepalive with zero
// prior events must commit the response exactly as event() does — SSE
// headers set, write deadline cleared, and Started() true — rather than
// falling through to a plain 200 that dies at the server's WriteTimeout.
func TestSSEKeepaliveStartsResponse(t *testing.T) {
	t.Parallel()

	// Arrange
	rec := &deadlineAwareRecorder{ResponseRecorder: httptest.NewRecorder()}
	sw := newSSEWriter(rec)

	// Act
	err := sw.keepalive()

	// Assert
	if err != nil {
		t.Fatalf("keepalive() error = %v, want nil", err)
	}
	if !sw.Started() {
		t.Error("Started() = false after a zero-event keepalive, want true")
	}
	if got := rec.Header().Get("Content-Type"); got != sseContentType {
		t.Errorf("Content-Type = %q, want %q", got, sseContentType)
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want %q", got, "no-store")
	}
	if got := rec.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("X-Content-Type-Options = %q, want %q", got, "nosniff")
	}
	if got := rec.Header().Get("Connection"); got != "keep-alive" {
		t.Errorf("Connection = %q, want %q", got, "keep-alive")
	}
	if got := rec.Header().Get("X-Accel-Buffering"); got != "no" {
		t.Errorf("X-Accel-Buffering = %q, want %q", got, "no")
	}
	if !rec.deadlineCleared {
		t.Error("write deadline not cleared after a zero-event keepalive")
	}
}

// TestSSEUnflushableWriter is the negative half of D7/D8: a ResponseWriter
// with no Unwrap and no SetWriteDeadline of its own leaves
// http.ResponseController unable to reach anything, so start() must surface
// that failure rather than silently continuing into a stream that would die
// at the server's WriteTimeout with nothing in the log to explain why.
func TestSSEUnflushableWriter(t *testing.T) {
	t.Parallel()

	// Arrange
	w := newPlainResponseWriter()
	sw := newSSEWriter(w)

	// Act
	err := sw.start()

	// Assert
	if err == nil {
		t.Fatal("start() error = nil, want a write-deadline failure")
	}
	if sw.Started() {
		t.Error("Started() = true after a failed start(), want false")
	}
}
