// SPDX-License-Identifier: AGPL-3.0-only

package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/scnplt/devmon-agent/internal/dockerx"
	"github.com/scnplt/devmon-agent/internal/policy"
)

// flushFailingRecorder wraps httptest.ResponseRecorder to make one specific
// Flush call fail, so a test can force sse.event's "flush sse frame: %w" or
// "flush sse headers: %w" path without a real disconnected socket.
// FlushError, not Flush, is the method http.ResponseController looks for
// first (see net/http's ResponseController.Flush), so implementing that one
// method is enough to make the whole flush chain return failErr on the
// failOn'th call and nil on every other call.
type flushFailingRecorder struct {
	*httptest.ResponseRecorder
	failOn     int
	failErr    error
	flushCount int
}

func (f *flushFailingRecorder) SetWriteDeadline(time.Time) error { return nil }

func (f *flushFailingRecorder) FlushError() error {
	f.flushCount++
	if f.flushCount == f.failOn {
		return f.failErr
	}
	return nil
}

// logRoutePaths lists the two log routes, for the tests that assert identical
// behaviour across both (401, 405, nil-reader, leak checks).
var logRoutePaths = []string{"/v1/containers/c1/logs", "/v1/containers/c1/logs/stream"}

// TestHistoricalLogsOK asserts the historical route answers 200 with the
// fake's lines, marshalled through the same ListResult envelope every other
// list route uses.
func TestHistoricalLogsOK(t *testing.T) {
	t.Parallel()

	// Arrange
	fd := &fakeDocker{
		containerLogsFn: func(context.Context, string, dockerx.LogOptions) (dockerx.ListResult[dockerx.LogLine], error) {
			return dockerx.ListResult[dockerx.LogLine]{
				Items: []dockerx.LogLine{
					{Timestamp: "2026-08-08T10:02:11.441Z", Stream: "stdout", Line: "listening on :8080"},
					{Timestamp: "2026-08-08T10:02:14.882Z", Stream: "stderr", Line: "panic: nil map write"},
				},
			}, nil
		},
	}
	s, st := testServerWithDocker(t, policy.ModeDefault, fd)
	serial := pairDeviceForRead(t, st)
	req := requestWithPeerSerial(http.MethodGet, "/v1/containers/c1/logs", nil, serial)
	rec := httptest.NewRecorder()

	// Act
	s.routes().ServeHTTP(rec, req)

	// Assert
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	var got dockerx.ListResult[dockerx.LogLine]
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Items) != 2 || got.Truncated {
		t.Errorf("items = %+v, truncated = %v; want 2 items, truncated=false", got.Items, got.Truncated)
	}
	if got.Items[0].Line != "listening on :8080" || got.Items[1].Stream != "stderr" {
		t.Errorf("items = %+v, unexpected content", got.Items)
	}
}

// TestHistoricalLogsFailureModes asserts the historical route maps each
// dockerx failure onto the same status/body pair as every other read route
// (writeDockerError is reused verbatim).
func TestHistoricalLogsFailureModes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		err        error
		wantStatus int
		wantMsg    string
	}{
		{name: "not found", err: dockerx.ErrNotFound, wantStatus: http.StatusNotFound, wantMsg: msgNotFound},
		{name: "invalid ref", err: dockerx.ErrInvalidRef, wantStatus: http.StatusBadRequest, wantMsg: msgInvalidRef},
		{name: "generic engine error", err: errors.New("engine exploded"), wantStatus: http.StatusBadGateway, wantMsg: msgEngineUnavailable},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			// Arrange
			fd := &fakeDocker{
				containerLogsFn: func(context.Context, string, dockerx.LogOptions) (dockerx.ListResult[dockerx.LogLine], error) {
					return dockerx.ListResult[dockerx.LogLine]{}, tt.err
				},
			}
			s, st := testServerWithDocker(t, policy.ModeDefault, fd)
			serial := pairDeviceForRead(t, st)
			req := requestWithPeerSerial(http.MethodGet, "/v1/containers/c1/logs", nil, serial)
			rec := httptest.NewRecorder()

			// Act
			s.routes().ServeHTTP(rec, req)

			// Assert
			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d; body: %s", rec.Code, tt.wantStatus, rec.Body.String())
			}
			var body errorBody
			if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
				t.Fatalf("decode body: %v", err)
			}
			if body.Error != tt.wantMsg {
				t.Errorf("error = %q, want %q", body.Error, tt.wantMsg)
			}
		})
	}
}

// TestLogsInvalidSince asserts an unparsable ?since= is a 400 with
// msgInvalidSince, on the historical route.
func TestLogsInvalidSince(t *testing.T) {
	t.Parallel()

	// Arrange
	s, st := testServerWithDocker(t, policy.ModeDefault, &fakeDocker{})
	serial := pairDeviceForRead(t, st)
	req := requestWithPeerSerial(http.MethodGet, "/v1/containers/c1/logs?since=yesterday", nil, serial)
	rec := httptest.NewRecorder()

	// Act
	s.routes().ServeHTTP(rec, req)

	// Assert
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body: %s", rec.Code, rec.Body.String())
	}
	var body errorBody
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body.Error != msgInvalidSince {
		t.Errorf("error = %q, want %q", body.Error, msgInvalidSince)
	}
}

// TestHistoricalTailBounds drives ?tail= through absent, unparsable, and
// both boundary directions, and asserts what the fake actually observes.
//
// NOTE ON THE PLAN: Task 9's prose says the fake should observe
// "200, 2000, 200, 200" for tail=0, tail=9999, tail=abc, and absent — i.e.
// that an over-the-maximum value should be *clamped* to maxTail (2000).
// That contradicts both tailParam's own doc comment ("falling back to def
// when the value is ... outside [minTail, maxTail]") and the API Contract
// table ("out of range or unparsable -> the default"), and the shipped
// tailParam implementation in logs.go faithfully follows the latter two: an
// out-of-range value falls back to def, it is not clamped. This test asserts
// the implemented (and separately documented) behaviour; see the final
// report for this discrepancy rather than a silent production edit.
func TestHistoricalTailBounds(t *testing.T) {
	t.Parallel()

	// Arrange
	var gotTail []int
	fd := &fakeDocker{
		containerLogsFn: func(_ context.Context, _ string, opts dockerx.LogOptions) (dockerx.ListResult[dockerx.LogLine], error) {
			gotTail = append(gotTail, opts.Tail)
			return dockerx.ListResult[dockerx.LogLine]{}, nil
		},
	}
	s, st := testServerWithDocker(t, policy.ModeDefault, fd)
	serial := pairDeviceForRead(t, st)

	queries := []string{"?tail=0", "?tail=9999", "?tail=abc", ""}

	// Act
	for _, q := range queries {
		req := requestWithPeerSerial(http.MethodGet, "/v1/containers/c1/logs"+q, nil, serial)
		rec := httptest.NewRecorder()
		s.routes().ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("query %q: status = %d, want 200; body: %s", q, rec.Code, rec.Body.String())
		}
	}

	// Assert
	want := []int{defaultHistoricalTail, defaultHistoricalTail, defaultHistoricalTail, defaultHistoricalTail}
	if len(gotTail) != len(want) {
		t.Fatalf("fake observed %d calls (%v), want %d", len(gotTail), gotTail, len(want))
	}
	for i := range want {
		if gotTail[i] != want[i] {
			t.Errorf("call %d (%q): tail = %d, want %d", i, queries[i], gotTail[i], want[i])
		}
	}
}

// TestTailParamAcceptsInRangeValue is tailParam's missing positive case: a
// value inside [minTail, maxTail] must pass through unchanged rather than
// falling back to the route's default, which every other tailParam test
// exercises.
func TestTailParamAcceptsInRangeValue(t *testing.T) {
	t.Parallel()

	// Arrange
	var gotTail int
	fd := &fakeDocker{
		containerLogsFn: func(_ context.Context, _ string, opts dockerx.LogOptions) (dockerx.ListResult[dockerx.LogLine], error) {
			gotTail = opts.Tail
			return dockerx.ListResult[dockerx.LogLine]{}, nil
		},
	}
	s, st := testServerWithDocker(t, policy.ModeDefault, fd)
	serial := pairDeviceForRead(t, st)
	req := requestWithPeerSerial(http.MethodGet, "/v1/containers/c1/logs?tail=50", nil, serial)
	rec := httptest.NewRecorder()

	// Act
	s.routes().ServeHTTP(rec, req)

	// Assert
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	if gotTail != 50 {
		t.Errorf("tail = %d, want 50 (an in-range value passed through unchanged)", gotTail)
	}
}

// TestSinceParamAcceptsValidTimestamp is sinceParam's missing positive case:
// a well-formed RFC3339Nano value must reach the Engine's request options
// unchanged, rather than falling into the error branch every other
// sinceParam test exercises.
func TestSinceParamAcceptsValidTimestamp(t *testing.T) {
	t.Parallel()

	// Arrange
	const since = "2026-08-08T10:02:11.441Z"
	var gotSince string
	fd := &fakeDocker{
		containerLogsFn: func(_ context.Context, _ string, opts dockerx.LogOptions) (dockerx.ListResult[dockerx.LogLine], error) {
			gotSince = opts.Since
			return dockerx.ListResult[dockerx.LogLine]{}, nil
		},
	}
	s, st := testServerWithDocker(t, policy.ModeDefault, fd)
	serial := pairDeviceForRead(t, st)
	req := requestWithPeerSerial(http.MethodGet, "/v1/containers/c1/logs?since="+since, nil, serial)
	rec := httptest.NewRecorder()

	// Act
	s.routes().ServeHTTP(rec, req)

	// Assert
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	if gotSince != since {
		t.Errorf("since = %q, want %q", gotSince, since)
	}
}

// TestStreamHappyPathExactBytes pins the exact SSE wire bytes for a stream
// that emits three lines and ends cleanly: the id:/event:/data: lines, the
// blank-line terminator on every frame, and the response's Content-Type.
func TestStreamHappyPathExactBytes(t *testing.T) {
	t.Parallel()

	// Arrange
	lines := []dockerx.LogLine{
		{Timestamp: "2026-08-08T10:02:11.441Z", Stream: "stdout", Line: "line one"},
		{Timestamp: "2026-08-08T10:02:12.001Z", Stream: "stderr", Line: "line two"},
		{Timestamp: "2026-08-08T10:02:13.500Z", Stream: "stdout", Line: "line three"},
	}
	fd := &fakeDocker{
		streamContainerLogsFn: func(_ context.Context, _ string, _ dockerx.LogOptions, emit func(dockerx.LogLine) error) error {
			for _, l := range lines {
				if err := emit(l); err != nil {
					return err
				}
			}
			return nil
		},
	}
	s, st := testServerWithDocker(t, policy.ModeDefault, fd)
	serial := pairDeviceForRead(t, st)
	req := requestWithPeerSerial(http.MethodGet, "/v1/containers/c1/logs/stream", nil, serial)
	rec := &deadlineAwareRecorder{ResponseRecorder: httptest.NewRecorder()}

	// Act
	s.routes().ServeHTTP(rec, req)

	// Assert
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != sseContentType {
		t.Errorf("Content-Type = %q, want %q", ct, sseContentType)
	}

	var want strings.Builder
	for _, l := range lines {
		payload, err := json.Marshal(l)
		if err != nil {
			t.Fatalf("marshal expected line: %v", err)
		}
		fmt.Fprintf(&want, "id: %s\nevent: log\ndata: %s\n\n", l.Timestamp, payload)
	}
	if got := rec.Body.String(); got != want.String() {
		t.Errorf("stream body = %q, want %q", got, want.String())
	}
}

// TestStreamErrorBeforeFirstLine is D12's guarantee: a failure before any
// line is emitted must be a JSON 404, never a 200 stream that turns out to
// carry nothing.
func TestStreamErrorBeforeFirstLine(t *testing.T) {
	t.Parallel()

	// Arrange
	fd := &fakeDocker{
		streamContainerLogsFn: func(context.Context, string, dockerx.LogOptions, func(dockerx.LogLine) error) error {
			return dockerx.ErrNotFound
		},
	}
	s, st := testServerWithDocker(t, policy.ModeDefault, fd)
	serial := pairDeviceForRead(t, st)
	req := requestWithPeerSerial(http.MethodGet, "/v1/containers/c1/logs/stream", nil, serial)
	rec := &deadlineAwareRecorder{ResponseRecorder: httptest.NewRecorder()}

	// Act
	s.routes().ServeHTTP(rec, req)

	// Assert
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body: %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	var body errorBody
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body.Error != msgNotFound {
		t.Errorf("error = %q, want %q", body.Error, msgNotFound)
	}
}

// TestStreamErrorAfterFirstLine asserts that once a line has already been
// emitted, a subsequent Engine failure becomes a terminal SSE event: error
// frame on an already-200 response rather than a status change.
func TestStreamErrorAfterFirstLine(t *testing.T) {
	t.Parallel()

	// Arrange
	fd := &fakeDocker{
		streamContainerLogsFn: func(_ context.Context, _ string, _ dockerx.LogOptions, emit func(dockerx.LogLine) error) error {
			if err := emit(dockerx.LogLine{Timestamp: "t1", Stream: "stdout", Line: "one line before the fault"}); err != nil {
				return err
			}
			return errors.New("engine connection dropped")
		},
	}
	s, st := testServerWithDocker(t, policy.ModeDefault, fd)
	serial := pairDeviceForRead(t, st)
	req := requestWithPeerSerial(http.MethodGet, "/v1/containers/c1/logs/stream", nil, serial)
	rec := &deadlineAwareRecorder{ResponseRecorder: httptest.NewRecorder()}

	// Act
	s.routes().ServeHTTP(rec, req)

	// Assert
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if got := strings.Count(body, "event: log\n"); got != 1 {
		t.Errorf("body has %d event: log frames, want 1; body: %q", got, body)
	}
	if got := strings.Count(body, "event: error\n"); got != 1 {
		t.Errorf("body has %d event: error frames, want 1; body: %q", got, body)
	}
	wantSuffix := fmt.Sprintf("event: error\ndata: {\"error\":%q}\n\n", msgEngineUnavailable)
	if !strings.HasSuffix(body, wantSuffix) {
		t.Errorf("body = %q, want it to end with %q", body, wantSuffix)
	}
}

// TestStreamClientDisconnectLogsNothingAtError is issue #9's regression: a
// client that disconnects mid-stream must not produce an ERROR line. The
// agent failing to tell a departed client that it is gone is not an agent
// fault, so no terminal frame should even be attempted. This asserts on the
// captured log content, not merely "no panic" — the old behaviour never
// panicked, it just logged ERROR on every ordinary disconnect.
func TestStreamClientDisconnectLogsNothingAtError(t *testing.T) {
	t.Parallel()

	// Arrange — the fake cancels the request's own context before returning
	// its error, exactly as net/http cancels it when the underlying
	// connection closes (see isClientGone's doc comment), and the recorder's
	// first Flush call fails with the disconnect text actually observed on
	// this route, so the error path is exercised the way it happens for
	// real rather than via a synthetic error value.
	rec := &flushFailingRecorder{
		ResponseRecorder: httptest.NewRecorder(),
		failOn:           1,
		failErr:          errors.New("client disconnected"),
	}
	var cancelReq context.CancelFunc
	fd := &fakeDocker{
		streamContainerLogsFn: func(_ context.Context, _ string, _ dockerx.LogOptions, emit func(dockerx.LogLine) error) error {
			cancelReq()
			return emit(dockerx.LogLine{Timestamp: "t1", Stream: "stdout", Line: "line before disconnect"})
		},
	}
	log, buf := newCapturingLoggerAtLevel(slog.LevelDebug)
	s, st := testServerWithDockerAndLogger(t, policy.ModeDefault, fd, log)
	serial := pairDeviceForRead(t, st)
	req := requestWithPeerSerial(http.MethodGet, "/v1/containers/c1/logs/stream", nil, serial)
	cancelableCtx, cancel := context.WithCancel(req.Context())
	cancelReq = cancel
	req = req.WithContext(cancelableCtx)

	// Act
	s.routes().ServeHTTP(rec, req)

	// Assert
	if strings.Contains(buf.String(), "level=ERROR") {
		t.Errorf("agent log contains an ERROR line for an ordinary client disconnect: %s", buf.String())
	}
	if got := strings.Count(rec.Body.String(), "event: error"); got != 0 {
		t.Errorf("body has %d terminal error frames, want 0: a disconnected client cannot receive one", got)
	}
}

// TestStreamGenuineFailureStillSendsTerminalFrameWhenConnected pins the
// unchanged half of issue #9's fix: a real Engine fault, with the client
// still connected, must still produce the terminal event: error frame and
// must not be swallowed as a false-positive "client gone".
func TestStreamGenuineFailureStillSendsTerminalFrameWhenConnected(t *testing.T) {
	t.Parallel()

	// Arrange
	fd := &fakeDocker{
		streamContainerLogsFn: func(_ context.Context, _ string, _ dockerx.LogOptions, emit func(dockerx.LogLine) error) error {
			if err := emit(dockerx.LogLine{Timestamp: "t1", Stream: "stdout", Line: "one line before the fault"}); err != nil {
				return err
			}
			return errors.New("engine connection dropped")
		},
	}
	log, buf := newCapturingLoggerAtLevel(slog.LevelDebug)
	s, st := testServerWithDockerAndLogger(t, policy.ModeDefault, fd, log)
	serial := pairDeviceForRead(t, st)
	req := requestWithPeerSerial(http.MethodGet, "/v1/containers/c1/logs/stream", nil, serial)
	rec := &deadlineAwareRecorder{ResponseRecorder: httptest.NewRecorder()}

	// Act
	s.routes().ServeHTTP(rec, req)

	// Assert
	body := rec.Body.String()
	if got := strings.Count(body, "event: error\n"); got != 1 {
		t.Errorf("body has %d event: error frames, want 1 for a genuine Engine fault; body: %q", got, body)
	}
	// The frame write itself succeeded (deadlineAwareRecorder never fails a
	// flush), so nothing about the terminal frame should have been logged
	// at any level.
	if strings.Contains(buf.String(), "write terminal error frame") {
		t.Errorf("agent log unexpectedly mentions the terminal frame for a successful send: %s", buf.String())
	}
}

// TestStreamTerminalFrameClientGoneFailureLogsDebug is issue #9's second
// arm: when the terminal frame IS attempted, because streamErr was a genuine
// Engine fault, but the frame write itself then fails for a client-gone
// reason, that failure logs at DEBUG rather than ERROR — the client vanished
// one step later than in the first-arm case, but the argument is identical.
func TestStreamTerminalFrameClientGoneFailureLogsDebug(t *testing.T) {
	t.Parallel()

	// Arrange — flush #1 (SSE headers) and #2 (the "log" frame) succeed;
	// flush #3, the terminal "error" frame, fails with the other disconnect
	// string actually observed on this route. The request context is never
	// canceled here, so this exercises isClientGone's string-match fallback
	// arm specifically, independent of the context-cancellation arm covered
	// by TestStreamClientDisconnectLogsNothingAtError.
	rec := &flushFailingRecorder{
		ResponseRecorder: httptest.NewRecorder(),
		failOn:           3,
		failErr:          errors.New("http2: stream closed"),
	}
	fd := &fakeDocker{
		streamContainerLogsFn: func(_ context.Context, _ string, _ dockerx.LogOptions, emit func(dockerx.LogLine) error) error {
			if err := emit(dockerx.LogLine{Timestamp: "t1", Stream: "stdout", Line: "one line before the fault"}); err != nil {
				return err
			}
			return errors.New("engine connection dropped")
		},
	}
	log, buf := newCapturingLoggerAtLevel(slog.LevelDebug)
	s, st := testServerWithDockerAndLogger(t, policy.ModeDefault, fd, log)
	serial := pairDeviceForRead(t, st)
	req := requestWithPeerSerial(http.MethodGet, "/v1/containers/c1/logs/stream", nil, serial)

	// Act
	s.routes().ServeHTTP(rec, req)

	// Assert
	logged := buf.String()
	if !strings.Contains(logged, "level=DEBUG") || !strings.Contains(logged, "write terminal error frame") {
		t.Errorf("log = %q, want a DEBUG line mentioning the terminal frame write", logged)
	}
	if strings.Contains(logged, "level=ERROR") {
		t.Errorf("log = %q, want no ERROR line: the terminal frame failed because the client was gone", logged)
	}
}

// TestStreamSlotExhaustion holds maxConcurrentStreams streams open
// concurrently, spread across enough distinct devices to reach the global
// ceiling without any one of them hitting maxStreamsPerDevice, asserts the
// next request (from yet another device) is 503 with msgTooManyStreams, then
// releases one and asserts the following request succeeds — proving the slot
// is returned rather than leaked. One stream per device keeps this a
// host-wide-exhaustion test distinct from
// TestStreamPerDeviceCapDoesNotLockOutOtherDevices below.
func TestStreamSlotExhaustion(t *testing.T) {
	t.Parallel()

	// Arrange
	started := make(chan struct{}, maxConcurrentStreams)
	release := make(chan struct{})
	fd := &fakeDocker{
		streamContainerLogsFn: func(ctx context.Context, _ string, _ dockerx.LogOptions, _ func(dockerx.LogLine) error) error {
			started <- struct{}{}
			select {
			case <-release:
			case <-ctx.Done():
			}
			return nil
		},
	}
	s, st := testServerWithDocker(t, policy.ModeDefault, fd)
	serials := make([]*big.Int, maxConcurrentStreams)
	for i := range serials {
		serials[i] = pairDeviceForRead(t, st)
	}

	done := make(chan struct{}, maxConcurrentStreams)
	for _, serial := range serials {
		serial := serial
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			req := requestWithPeerSerial(http.MethodGet, "/v1/containers/c1/logs/stream", nil, serial).WithContext(ctx)
			rec := &deadlineAwareRecorder{ResponseRecorder: httptest.NewRecorder()}
			s.routes().ServeHTTP(rec, req)
			done <- struct{}{}
		}()
	}

	for i := 0; i < maxConcurrentStreams; i++ {
		select {
		case <-started:
		case <-time.After(5 * time.Second):
			t.Fatalf("only %d of %d streams reported started", i, maxConcurrentStreams)
		}
	}

	// Act — every slot is held across maxConcurrentStreams distinct devices,
	// so the next request, from yet another device, must be rejected with
	// the host-wide message rather than the per-device one.
	probeSerial := pairDeviceForRead(t, st)
	probe := requestWithPeerSerial(http.MethodGet, "/v1/containers/c1/logs/stream", nil, probeSerial)
	probeRec := &deadlineAwareRecorder{ResponseRecorder: httptest.NewRecorder()}
	s.routes().ServeHTTP(probeRec, probe)

	// Assert
	if probeRec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503; body: %s", probeRec.Code, probeRec.Body.String())
	}
	var body errorBody
	if err := json.NewDecoder(probeRec.Body).Decode(&body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body.Error != msgTooManyStreams {
		t.Errorf("error = %q, want %q", body.Error, msgTooManyStreams)
	}

	// Act — release exactly one held stream and prove the slot comes back.
	release <- struct{}{}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("released stream did not finish")
	}

	fd.streamContainerLogsFn = func(context.Context, string, dockerx.LogOptions, func(dockerx.LogLine) error) error {
		return nil
	}
	nextReq := requestWithPeerSerial(http.MethodGet, "/v1/containers/c1/logs/stream", nil, probeSerial)
	nextRec := &deadlineAwareRecorder{ResponseRecorder: httptest.NewRecorder()}
	s.routes().ServeHTTP(nextRec, nextReq)

	// Assert
	if nextRec.Code != http.StatusOK {
		t.Errorf("status after releasing a slot = %d, want 200; body: %s", nextRec.Code, nextRec.Body.String())
	}

	// Cleanup: release the remaining held streams so their goroutines exit
	// before the test returns.
	for i := 0; i < maxConcurrentStreams-1; i++ {
		release <- struct{}{}
	}
	for i := 0; i < maxConcurrentStreams-1; i++ {
		<-done
	}
}

// TestStreamPerDeviceCapDoesNotLockOutOtherDevices is the issue's stated
// verification: one device holding maxStreamsPerDevice streams is refused
// its next one with msgTooManyDeviceStreams, while a second device — with
// global capacity still free — still gets a stream. Before this change both
// requests were served (or refused) from a single shared channel with no
// notion of caller identity, so device A alone could have exhausted the
// whole host budget.
func TestStreamPerDeviceCapDoesNotLockOutOtherDevices(t *testing.T) {
	t.Parallel()

	// Arrange
	started := make(chan struct{}, maxStreamsPerDevice)
	release := make(chan struct{})
	fd := &fakeDocker{
		streamContainerLogsFn: func(ctx context.Context, _ string, _ dockerx.LogOptions, _ func(dockerx.LogLine) error) error {
			started <- struct{}{}
			select {
			case <-release:
			case <-ctx.Done():
			}
			return nil
		},
	}
	s, st := testServerWithDocker(t, policy.ModeDefault, fd)
	deviceA := pairDeviceForRead(t, st)
	deviceB := pairDeviceForRead(t, st)

	done := make(chan struct{}, maxStreamsPerDevice)
	for i := 0; i < maxStreamsPerDevice; i++ {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			req := requestWithPeerSerial(http.MethodGet, "/v1/containers/c1/logs/stream", nil, deviceA).WithContext(ctx)
			rec := &deadlineAwareRecorder{ResponseRecorder: httptest.NewRecorder()}
			s.routes().ServeHTTP(rec, req)
			done <- struct{}{}
		}()
	}

	for i := 0; i < maxStreamsPerDevice; i++ {
		select {
		case <-started:
		case <-time.After(5 * time.Second):
			t.Fatalf("only %d of %d device A streams reported started", i, maxStreamsPerDevice)
		}
	}

	// Act — device A is at its own cap; its next request must be refused
	// with the per-device message, not the host-wide one.
	probeA := requestWithPeerSerial(http.MethodGet, "/v1/containers/c1/logs/stream", nil, deviceA)
	probeARec := &deadlineAwareRecorder{ResponseRecorder: httptest.NewRecorder()}
	s.routes().ServeHTTP(probeARec, probeA)

	// Assert
	if probeARec.Code != http.StatusServiceUnavailable {
		t.Fatalf("device A status = %d, want 503; body: %s", probeARec.Code, probeARec.Body.String())
	}
	var bodyA errorBody
	if err := json.NewDecoder(probeARec.Body).Decode(&bodyA); err != nil {
		t.Fatalf("decode device A body: %v", err)
	}
	if bodyA.Error != msgTooManyDeviceStreams {
		t.Errorf("device A error = %q, want %q", bodyA.Error, msgTooManyDeviceStreams)
	}

	// Act — device B, at zero streams, must still be served: this is the
	// regression the issue is about.
	fd.streamContainerLogsFn = func(context.Context, string, dockerx.LogOptions, func(dockerx.LogLine) error) error {
		return nil
	}
	probeB := requestWithPeerSerial(http.MethodGet, "/v1/containers/c1/logs/stream", nil, deviceB)
	probeBRec := &deadlineAwareRecorder{ResponseRecorder: httptest.NewRecorder()}
	s.routes().ServeHTTP(probeBRec, probeB)

	// Assert
	if probeBRec.Code != http.StatusOK {
		t.Errorf("device B status = %d, want 200; body: %s", probeBRec.Code, probeBRec.Body.String())
	}

	// Cleanup: release device A's held streams so their goroutines exit
	// before the test returns.
	for i := 0; i < maxStreamsPerDevice; i++ {
		release <- struct{}{}
	}
	for i := 0; i < maxStreamsPerDevice; i++ {
		<-done
	}
}

// TestStreamContainerLogsRejectsWithoutDeviceInContext mirrors
// TestWithDeviceLimitRejectsWithoutDeviceInContext (ratelimit_test.go):
// handleStreamContainerLogs only ever runs behind requireDevice. Called
// directly — bypassing routes() and therefore requireDevice — with a
// request that carries no resolved device, it must answer 500 rather than
// silently falling back to an unkeyed slot, which would quietly restore the
// bug this phase fixes.
func TestStreamContainerLogsRejectsWithoutDeviceInContext(t *testing.T) {
	t.Parallel()

	// Arrange
	fd := &fakeDocker{
		streamContainerLogsFn: func(context.Context, string, dockerx.LogOptions, func(dockerx.LogLine) error) error {
			return nil
		},
	}
	s, _ := testServerWithDocker(t, policy.ModeDefault, fd)
	req := httptest.NewRequest(http.MethodGet, "/v1/containers/c1/logs/stream", nil)
	rec := httptest.NewRecorder()

	// Act — call the handler directly, skipping requireDevice.
	s.handleStreamContainerLogs(rec, req)

	// Assert
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500; body: %s", rec.Code, rec.Body.String())
	}
}

// TestStreamSlotReleasedOnError issues maxConcurrentStreams+1 requests that
// all fail with 404, then asserts a valid stream still succeeds — catching
// the leaked-slot bug directly, without needing concurrency: the slot is
// released with defer on the error path just as much as on success.
func TestStreamSlotReleasedOnError(t *testing.T) {
	t.Parallel()

	// Arrange
	fd := &fakeDocker{
		streamContainerLogsFn: func(context.Context, string, dockerx.LogOptions, func(dockerx.LogLine) error) error {
			return dockerx.ErrNotFound
		},
	}
	s, st := testServerWithDocker(t, policy.ModeDefault, fd)
	serial := pairDeviceForRead(t, st)

	// Act
	for i := 0; i < maxConcurrentStreams+1; i++ {
		req := requestWithPeerSerial(http.MethodGet, "/v1/containers/c1/logs/stream", nil, serial)
		rec := &deadlineAwareRecorder{ResponseRecorder: httptest.NewRecorder()}
		s.routes().ServeHTTP(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("request %d: status = %d, want 404; body: %s", i, rec.Code, rec.Body.String())
		}
	}

	fd.streamContainerLogsFn = func(context.Context, string, dockerx.LogOptions, func(dockerx.LogLine) error) error {
		return nil
	}
	req := requestWithPeerSerial(http.MethodGet, "/v1/containers/c1/logs/stream", nil, serial)
	rec := &deadlineAwareRecorder{ResponseRecorder: httptest.NewRecorder()}
	s.routes().ServeHTTP(rec, req)

	// Assert
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200; a leaked slot would answer 503 forever after %d failures", rec.Code, maxConcurrentStreams+1)
	}
}

// TestStreamKeepaliveIsRaceFree runs a fake that emits lines while the
// keepalive ticker fires concurrently, under -race. keepaliveInterval is a
// package variable exactly so this test can shorten it instead of waiting 20
// real seconds; it is restored via t.Cleanup, and this test does not run in
// parallel because it mutates package state.
func TestStreamKeepaliveIsRaceFree(t *testing.T) {
	// Not t.Parallel(): mutates the package-level keepaliveInterval.

	// Arrange
	original := keepaliveInterval
	keepaliveInterval = 2 * time.Millisecond
	t.Cleanup(func() { keepaliveInterval = original })

	fd := &fakeDocker{
		streamContainerLogsFn: func(_ context.Context, _ string, _ dockerx.LogOptions, emit func(dockerx.LogLine) error) error {
			for i := 0; i < 20; i++ {
				if err := emit(dockerx.LogLine{Timestamp: "t", Stream: "stdout", Line: "line"}); err != nil {
					return err
				}
				// Bounded, short sleep — not a wall-clock wait on the phase's
				// real timings — giving the 2ms keepalive ticker room to race
				// with this write on the shared underlying ResponseWriter.
				// This is not a synchronization wait (issue #54): there is no
				// observable signal to poll for here, since the goroutine
				// under test writes into the same handler call this loop is
				// blocking, and the assertion is race-freedom under `-race`,
				// not any state this sleep could instead wait on.
				time.Sleep(time.Millisecond)
			}
			return nil
		},
	}
	s, st := testServerWithDocker(t, policy.ModeDefault, fd)
	serial := pairDeviceForRead(t, st)
	req := requestWithPeerSerial(http.MethodGet, "/v1/containers/c1/logs/stream", nil, serial)
	rec := &deadlineAwareRecorder{ResponseRecorder: httptest.NewRecorder()}

	// Act — the real assertion here is `go test -race`: a data race between
	// this emit loop and the keepalive goroutine on the shared
	// ResponseWriter fails the run regardless of the status code below.
	s.routes().ServeHTTP(rec, req)

	// Assert
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
}

// TestStreamKeepaliveGoroutineJoinedBeforeReturn is H2's regression: the
// keepalive goroutine must have stopped writing to the ResponseWriter by the
// time ServeHTTP returns, not merely "eventually" as
// TestStreamGoroutineDoesNotLeak already checks. keepaliveInterval is
// shortened so ticks are frequent and the fake returns while ticks are still
// firing, opening the window between the emit loop finishing and the
// deferred cancel(). If the goroutine is only signalled and not joined, a
// tick can land after ServeHTTP returns and grow the recorded body.
func TestStreamKeepaliveGoroutineJoinedBeforeReturn(t *testing.T) {
	// Not t.Parallel(): mutates the package-level keepaliveInterval.

	// Arrange
	original := keepaliveInterval
	keepaliveInterval = time.Millisecond
	t.Cleanup(func() { keepaliveInterval = original })

	fd := &fakeDocker{
		streamContainerLogsFn: func(context.Context, string, dockerx.LogOptions, func(dockerx.LogLine) error) error {
			// Return promptly while the 1ms keepalive ticker is still firing,
			// so cancel() races the next tick. Also not a synchronization
			// wait (issue #54): the fake has no handle on the ticker or the
			// ResponseWriter to poll, and the point of this sleep is to
			// consume real wall-clock time so the ticker fires while the
			// fake is still running.
			time.Sleep(5 * time.Millisecond)
			return nil
		},
	}
	s, st := testServerWithDocker(t, policy.ModeDefault, fd)
	serial := pairDeviceForRead(t, st)
	req := requestWithPeerSerial(http.MethodGet, "/v1/containers/c1/logs/stream", nil, serial)
	rec := &deadlineAwareRecorder{ResponseRecorder: httptest.NewRecorder()}

	// Act
	s.routes().ServeHTTP(rec, req)
	lenAtReturn := rec.Body.Len()

	// Assert — watch for several tick intervals rather than sleeping once
	// and checking after the fact: this fails on the very first observed
	// growth instead of only after the whole window has elapsed.
	watchWindow := 20 * keepaliveInterval
	giveUpAt := time.Now().Add(watchWindow)
	for {
		if got := rec.Body.Len(); got != lenAtReturn {
			t.Fatalf("body length grew from %d to %d after ServeHTTP returned; the keepalive goroutine was not joined", lenAtReturn, got)
		}
		if time.Now().After(giveUpAt) {
			return
		}
		time.Sleep(keepaliveInterval)
	}
}

// TestStreamGoroutineDoesNotLeak opens and closes 20 streams and asserts the
// goroutine count returns to its baseline, proving the keepalive goroutine
// and its ticker exit when the handler returns rather than leaking one per
// stream.
func TestStreamGoroutineDoesNotLeak(t *testing.T) {
	t.Parallel()

	// Arrange
	fd := &fakeDocker{
		streamContainerLogsFn: func(context.Context, string, dockerx.LogOptions, func(dockerx.LogLine) error) error {
			return nil
		},
	}
	s, st := testServerWithDocker(t, policy.ModeDefault, fd)
	serial := pairDeviceForRead(t, st)
	baseline := runtime.NumGoroutine()

	// Act
	for i := 0; i < 20; i++ {
		req := requestWithPeerSerial(http.MethodGet, "/v1/containers/c1/logs/stream", nil, serial)
		rec := &deadlineAwareRecorder{ResponseRecorder: httptest.NewRecorder()}
		s.routes().ServeHTTP(rec, req)
	}

	// Assert — a short settle loop rather than a bare sleep: the keepalive
	// goroutine exits on ctx.Done(), which the scheduler may not have
	// delivered the instant ServeHTTP returns.
	waitForCondition(t, 2*time.Second, 5*time.Millisecond,
		func() bool { return runtime.NumGoroutine() <= baseline },
		func() string {
			return fmt.Sprintf("goroutine count = %d, want <= baseline %d", runtime.NumGoroutine(), baseline)
		})
}

// TestLogRoutesRequireDevice asserts both log routes answer 401 with the
// standard terse body when no client certificate is presented.
func TestLogRoutesRequireDevice(t *testing.T) {
	t.Parallel()

	for _, path := range logRoutePaths {
		t.Run(path, func(t *testing.T) {
			t.Parallel()

			// Arrange
			s, _ := testServerWithDocker(t, policy.ModeDefault, &fakeDocker{})
			req := httptest.NewRequest(http.MethodGet, path, nil)
			rec := httptest.NewRecorder()

			// Act
			s.routes().ServeHTTP(rec, req)

			// Assert
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401; body: %s", rec.Code, rec.Body.String())
			}
			var body errorBody
			if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
				t.Fatalf("decode body: %v", err)
			}
			if body.Error != msgClientCertRequired {
				t.Errorf("error = %q, want %q", body.Error, msgClientCertRequired)
			}
		})
	}
}

// TestLogRoutesRejectOtherMethods asserts POST on both log routes answers
// 405.
func TestLogRoutesRejectOtherMethods(t *testing.T) {
	t.Parallel()

	for _, path := range logRoutePaths {
		t.Run(path, func(t *testing.T) {
			t.Parallel()

			// Arrange
			s, _ := testServerWithDocker(t, policy.ModeDefault, &fakeDocker{})
			req := httptest.NewRequest(http.MethodPost, path, nil)
			rec := httptest.NewRecorder()

			// Act
			s.routes().ServeHTTP(rec, req)

			// Assert
			if rec.Code != http.StatusMethodNotAllowed {
				t.Errorf("POST %s = %d, want 405", path, rec.Code)
			}
		})
	}
}

// TestLogLineFieldCount is the FIELD_ALLOWLIST_GUARD for LogLine, exercised
// through an actual handler response: an ordinary line marshals to exactly
// three keys, a truncated one to exactly four, because Truncated is the only
// omitempty field.
func TestLogLineFieldCount(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		line     dockerx.LogLine
		wantKeys int
	}{
		{name: "ordinary line", line: dockerx.LogLine{Timestamp: "t1", Stream: "stdout", Line: "hello"}, wantKeys: 3},
		{name: "truncated line", line: dockerx.LogLine{Timestamp: "t1", Stream: "stdout", Line: "hello", Truncated: true}, wantKeys: 4},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			// Arrange
			fd := &fakeDocker{
				containerLogsFn: func(context.Context, string, dockerx.LogOptions) (dockerx.ListResult[dockerx.LogLine], error) {
					return dockerx.ListResult[dockerx.LogLine]{Items: []dockerx.LogLine{tt.line}}, nil
				},
			}
			s, st := testServerWithDocker(t, policy.ModeDefault, fd)
			serial := pairDeviceForRead(t, st)
			req := requestWithPeerSerial(http.MethodGet, "/v1/containers/c1/logs", nil, serial)
			rec := httptest.NewRecorder()

			// Act
			s.routes().ServeHTTP(rec, req)

			// Assert
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
			}
			var got struct {
				Items []map[string]any `json:"items"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if len(got.Items) != 1 {
				t.Fatalf("items = %+v, want exactly one", got.Items)
			}
			if len(got.Items[0]) != tt.wantKeys {
				t.Errorf("line has %d keys (%v), want exactly %d", len(got.Items[0]), got.Items[0], tt.wantKeys)
			}
		})
	}
}

// TestAgentLogNeverCarriesLineContent is D16's enforcement: a container's own
// output — and any error text derived from it — must never reach the
// agent's own log, at any level.
func TestAgentLogNeverCarriesLineContent(t *testing.T) {
	t.Parallel()

	// Arrange
	const secret = "hunter2"
	fd := &fakeDocker{
		streamContainerLogsFn: func(_ context.Context, _ string, _ dockerx.LogOptions, emit func(dockerx.LogLine) error) error {
			if err := emit(dockerx.LogLine{Timestamp: "t1", Stream: "stdout", Line: "password: " + secret}); err != nil {
				return err
			}
			return errors.New("engine dropped connection: " + secret)
		},
	}
	log, buf := newCapturingLogger()
	s, st := testServerWithDockerAndLogger(t, policy.ModeDefault, fd, log)
	serial := pairDeviceForRead(t, st)
	req := requestWithPeerSerial(http.MethodGet, "/v1/containers/c1/logs/stream", nil, serial)
	rec := &deadlineAwareRecorder{ResponseRecorder: httptest.NewRecorder()}

	// Act
	s.routes().ServeHTTP(rec, req)

	// Assert
	if bodyContains(buf.String(), secret) {
		t.Errorf("agent log contains the container's own output: %s", buf.String())
	}
}

// TestNilDockerReaderOnLogRoutes asserts both log routes answer 502 without
// panicking when the server has no Docker reader configured.
func TestNilDockerReaderOnLogRoutes(t *testing.T) {
	t.Parallel()

	for _, path := range logRoutePaths {
		t.Run(path, func(t *testing.T) {
			t.Parallel()

			// Arrange
			s, st := testServerWithDocker(t, policy.ModeDefault, nil)
			serial := pairDeviceForRead(t, st)
			req := requestWithPeerSerial(http.MethodGet, path, nil, serial)
			rec := &deadlineAwareRecorder{ResponseRecorder: httptest.NewRecorder()}

			// Act
			s.routes().ServeHTTP(rec, req)

			// Assert
			if rec.Code != http.StatusBadGateway {
				t.Fatalf("status = %d, want 502; body: %s", rec.Code, rec.Body.String())
			}
			var body errorBody
			if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
				t.Fatalf("decode body: %v", err)
			}
			if body.Error != msgEngineUnavailable {
				t.Errorf("error = %q, want %q", body.Error, msgEngineUnavailable)
			}
		})
	}
}

// TestLogErrorBodiesLeakNothing asserts that across every failure path on
// both log routes, the response body contains none of the host's state
// directory path, the Docker socket, or the state database filename.
func TestLogErrorBodiesLeakNothing(t *testing.T) {
	t.Parallel()

	// Arrange
	fd := &fakeDocker{}
	s, st := testServerWithDocker(t, policy.ModeDefault, fd)
	serial := pairDeviceForRead(t, st)

	var bodies []string

	setBothLogFns := func(err error) {
		fd.containerLogsFn = func(context.Context, string, dockerx.LogOptions) (dockerx.ListResult[dockerx.LogLine], error) {
			return dockerx.ListResult[dockerx.LogLine]{}, err
		}
		fd.streamContainerLogsFn = func(context.Context, string, dockerx.LogOptions, func(dockerx.LogLine) error) error {
			return err
		}
	}

	// Act — collect every failure body: 401 (no cert), 405 (wrong method),
	// 400 (invalid since), and the mapped 502/404/400 dockerx failures.
	for _, path := range logRoutePaths {
		unauth := httptest.NewRequest(http.MethodGet, path, nil)
		unauthRec := &deadlineAwareRecorder{ResponseRecorder: httptest.NewRecorder()}
		s.routes().ServeHTTP(unauthRec, unauth)
		bodies = append(bodies, unauthRec.Body.String())

		wrongMethod := httptest.NewRequest(http.MethodPost, path, nil)
		wrongMethodRec := &deadlineAwareRecorder{ResponseRecorder: httptest.NewRecorder()}
		s.routes().ServeHTTP(wrongMethodRec, wrongMethod)
		bodies = append(bodies, wrongMethodRec.Body.String())

		invalidSince := requestWithPeerSerial(http.MethodGet, path+"?since=yesterday", nil, serial)
		invalidSinceRec := &deadlineAwareRecorder{ResponseRecorder: httptest.NewRecorder()}
		s.routes().ServeHTTP(invalidSinceRec, invalidSince)
		bodies = append(bodies, invalidSinceRec.Body.String())

		setBothLogFns(errors.New("dial unix " + `/var/run/docker.sock` + ": connect: no such file or directory"))
		engineFail := requestWithPeerSerial(http.MethodGet, path, nil, serial)
		engineFailRec := &deadlineAwareRecorder{ResponseRecorder: httptest.NewRecorder()}
		s.routes().ServeHTTP(engineFailRec, engineFail)
		bodies = append(bodies, engineFailRec.Body.String())

		setBothLogFns(dockerx.ErrNotFound)
		notFound := requestWithPeerSerial(http.MethodGet, path, nil, serial)
		notFoundRec := &deadlineAwareRecorder{ResponseRecorder: httptest.NewRecorder()}
		s.routes().ServeHTTP(notFoundRec, notFound)
		bodies = append(bodies, notFoundRec.Body.String())

		setBothLogFns(dockerx.ErrInvalidRef)
		invalidRef := requestWithPeerSerial(http.MethodGet, path, nil, serial)
		invalidRefRec := &deadlineAwareRecorder{ResponseRecorder: httptest.NewRecorder()}
		s.routes().ServeHTTP(invalidRefRec, invalidRef)
		bodies = append(bodies, invalidRefRec.Body.String())
	}

	// Assert
	for _, leak := range []string{s.cfg.StateDir, "docker.sock", "devmon.db"} {
		for _, body := range bodies {
			if leak != "" && bodyContains(body, leak) {
				t.Errorf("a failure body leaks %q: %s", leak, body)
			}
		}
	}
}
