//go:build e2e

package harness

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// SSEFrame is one parsed Server-Sent Event frame — the id:, event:, and
// data: lines up to (and not including) the terminating blank line
// (internal/httpapi/sse.go's event method). A bare comment line
// (": keepalive") carries no id or event and is never delivered as a frame;
// it is tallied by Stream.Keepalives instead, because it is not an event by
// the SSE spec and no client-observable behaviour in this suite treats it as
// one.
type SSEFrame struct {
	ID    string
	Event string
	Data  json.RawMessage
}

// isKeepaliveOnly reports whether the block that produced f carried neither
// an id nor an event line — the shape sse.go's keepalive() method writes,
// and the only comment-only block this agent ever sends.
func (f SSEFrame) isKeepaliveOnly() bool {
	return f.ID == "" && f.Event == ""
}

// parseSSEBlock reads one SSE block from r, up to and including its
// terminating blank line, and reports whether it was a keepalive comment or
// a real id/event/data frame. A block left incomplete by the connection
// ending mid-frame — exactly what AbruptClose (D15) produces on the other
// end — is reported as an error, never as a partial frame: a caller must be
// able to tell "the stream ended cleanly with nothing more to read" from
// "the stream ended while a frame was still arriving".
func parseSSEBlock(r *bufio.Reader) (frame SSEFrame, isKeepalive bool, err error) {
	var sawContent bool
	for {
		line, readErr := r.ReadString('\n')
		hasNewline := len(line) > 0 && line[len(line)-1] == '\n'
		trimmed := strings.TrimRight(line, "\r\n")

		if hasNewline && trimmed == "" {
			if !sawContent {
				// A stray blank line with nothing before it. SSE producers in
				// this codebase never emit one; skip rather than surface a
				// phantom empty frame.
				if readErr != nil {
					return SSEFrame{}, false, readErr
				}
				continue
			}
			return frame, frame.isKeepaliveOnly(), nil
		}

		switch {
		case strings.HasPrefix(trimmed, "id: "):
			frame.ID = strings.TrimPrefix(trimmed, "id: ")
			sawContent = true
		case strings.HasPrefix(trimmed, "event: "):
			frame.Event = strings.TrimPrefix(trimmed, "event: ")
			sawContent = true
		case strings.HasPrefix(trimmed, "data: "):
			frame.Data = []byte(strings.TrimPrefix(trimmed, "data: "))
			sawContent = true
		case strings.HasPrefix(trimmed, ":"):
			// A bare comment line — sse.go's keepalive() writes ": keepalive".
			// Any comment counts: this parser does not care about its text,
			// only that the block carried no id or event.
			sawContent = true
		}

		if readErr != nil {
			return SSEFrame{}, false, readErr
		}
	}
}

// connCapture holds the raw *net.TCPConn a Stream's DialContext hook
// observed, so AbruptClose (D15) can reach beneath the TLS layer to send an
// RST instead of the orderly FIN a plain Close would produce.
type connCapture struct {
	mu   sync.Mutex
	conn *net.TCPConn
}

func (c *connCapture) set(conn net.Conn) {
	tc, ok := conn.(*net.TCPConn)
	if !ok {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.conn = tc
}

func (c *connCapture) get() *net.TCPConn {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.conn
}

// Stream is one live SSE connection opened against a log-stream route, or
// the terminal response of a request that never became one (a 404 before
// any SSE header was committed, a 503 for slot exhaustion).
//
// For a 200 response, Frames delivers every real event frame the server
// sends and closes when the connection ends, by any means — server EOF, an
// orderly Close, or an AbruptClose. Body is empty in that case; draining it
// would race the streaming reader that already owns resp.Body. For any other
// status, Frames is already closed (empty) and Body holds the whole,
// already-read response.
type Stream struct {
	Status int
	Header http.Header
	Body   []byte
	Frames <-chan SSEFrame

	resp    *http.Response
	capture *connCapture
	stopped chan struct{}

	mu         sync.Mutex
	keepalives int
	readErr    error
}

// streamDialTimeout bounds only the TCP+TLS handshake that opens a stream.
// It is not the request's overall timeout — OpenStream's client has none,
// deliberately, so a 30-minute endurance run and a 20s keepalive wait are
// never killed by a client-side deadline.
const streamDialTimeout = 15 * time.Second

// OpenStream issues GET path with d's pinned client certificate, on a
// dedicated http.Transport whose DialContext captures the raw *net.TCPConn
// before the TLS handshake — verified in this session: with TLSClientConfig
// set, DialContext yields the raw TCP connection, not a *tls.Conn, which is
// exactly the handle Stream.AbruptClose needs (D15's gotcha; DialTLSContext
// would hand back a *tls.Conn with no SetLinger). The resulting http.Client
// carries no Timeout at all: it must outlive both a 20s keepalive wait and a
// 30-minute endurance run, either of which a Device.Do-style client would
// kill (Task 6's gotcha).
//
// It never fails the test on a non-200 status: a caller probing a 404 (an
// unknown container) or a 503 (slot exhaustion) needs the raw status and
// body to assert against, not a fatal failure at the point of connecting.
func OpenStream(t *testing.T, d *Device, path string) *Stream {
	t.Helper()

	capture := &connCapture{}
	dialer := &net.Dialer{Timeout: streamDialTimeout}
	transport := &http.Transport{
		TLSClientConfig: d.TLSConfig(),
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			conn, err := dialer.DialContext(ctx, network, addr)
			if err != nil {
				return nil, err
			}
			capture.set(conn)
			return conn, nil
		},
	}
	client := &http.Client{Transport: transport}

	req, err := http.NewRequest(http.MethodGet, d.baseURL+path, nil)
	if err != nil {
		t.Fatalf("build stream request %s: %v", path, err)
	}

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("open stream %s: %v", path, err)
	}

	s := &Stream{
		Status:  resp.StatusCode,
		Header:  resp.Header,
		resp:    resp,
		capture: capture,
		stopped: make(chan struct{}),
	}

	if resp.StatusCode != http.StatusOK {
		raw, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if err != nil {
			t.Fatalf("read non-200 stream response body for %s: %v", path, err)
		}
		s.Body = raw
		close(s.stopped)
		frames := make(chan SSEFrame)
		close(frames)
		s.Frames = frames
		return s
	}

	frames := make(chan SSEFrame, 64)
	s.Frames = frames
	go s.readLoop(frames)
	return s
}

// readLoop parses SSE blocks until the connection ends, delivering every
// real frame to out and tallying every keepalive. It always closes out and
// stopped before returning, and always closes resp.Body — the one path that
// does not is AbruptClose, which closes the underlying TCP connection
// directly and lets this loop discover that as a read error.
func (s *Stream) readLoop(out chan<- SSEFrame) {
	defer close(out)
	defer close(s.stopped)
	defer func() { _ = s.resp.Body.Close() }()

	r := bufio.NewReader(s.resp.Body)
	for {
		frame, keepalive, err := parseSSEBlock(r)
		if err == nil {
			if keepalive {
				s.mu.Lock()
				s.keepalives++
				s.mu.Unlock()
			} else {
				out <- frame
			}
			continue
		}
		s.mu.Lock()
		s.readErr = err
		s.mu.Unlock()
		return
	}
}

// Keepalives returns the number of bare comment frames observed so far.
func (s *Stream) Keepalives() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.keepalives
}

// Err returns the error that ended the read loop — io.EOF for an orderly
// server-side close, or a connection-reset-shaped error after AbruptClose.
// It returns nil while the stream is still open.
func (s *Stream) Err() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.readErr
}

// WaitClosed blocks until the read loop has returned — the client-side
// signal that the connection is fully torn down — or fails the test after
// timeout.
func (s *Stream) WaitClosed(t *testing.T, timeout time.Duration) {
	t.Helper()
	select {
	case <-s.stopped:
	case <-time.After(timeout):
		t.Fatalf("stream: read loop did not stop within %s", timeout)
	}
}

// Close ends the stream the ordinary way: closing resp.Body sends a TLS
// close_notify followed by a FIN, the graceful path Phase 4 already
// verified against a real host. It is safe to call on a Stream whose
// Status was never 200 (the response body is already closed) and is
// idempotent.
func (s *Stream) Close(t *testing.T) {
	t.Helper()
	_ = s.resp.Body.Close()
}

// AbruptClose is D15's primitive: it reaches through the captured raw
// *net.TCPConn, calls SetLinger(0), then Close, which discards unsent data
// and sends an RST rather than the FIN a plain Close produces — the shape of
// a phone entering a tunnel, not the orderly disconnect Phase 4 already
// verified. It fails the test if no connection was ever captured, which
// would mean OpenStream never actually dialed (a harness bug, not a stream
// state to tolerate silently).
func (s *Stream) AbruptClose(t *testing.T) {
	t.Helper()

	conn := s.capture.get()
	if conn == nil {
		t.Fatalf("stream: AbruptClose called with no captured TCP connection")
	}
	if err := conn.SetLinger(0); err != nil {
		t.Fatalf("stream: SetLinger(0): %v", err)
	}
	if err := conn.Close(); err != nil {
		// The OS may have already torn the socket down as a side effect of
		// SetLinger(0) on some platforms; a second error here is not this
		// test's concern.
		t.Logf("stream: close after SetLinger(0): %v", err)
	}
}

// String renders a Stream safely for a failure message: status and frame
// count only, never Body — a 4xx/5xx body from this agent never carries a
// secret, but a future handler could add one, and this type has no way to
// know the difference (mirrors Device.String's caution).
func (s *Stream) String() string {
	return fmt.Sprintf("Stream{Status: %d, Keepalives: %d}", s.Status, s.Keepalives())
}
