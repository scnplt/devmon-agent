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
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/scnplt/devmon-agent/internal/config"
	"github.com/scnplt/devmon-agent/internal/dockerx"
	"github.com/scnplt/devmon-agent/internal/policy"
	"github.com/scnplt/devmon-agent/internal/state"
)

// writeCountingRecorder wraps deadlineAwareRecorder (server_test.go) and
// atomically counts calls to Write, so a test running the event-stream
// handler in its own goroutine can safely learn "N frames have reached the
// wire" — via waitForCondition polling the counter — without ever reading
// rec.Body from a second goroutine while the handler is still writing to it.
// That is the mandatory GOTCHA for this task: never read an
// httptest.ResponseRecorder while its handler goroutine is still running.
type writeCountingRecorder struct {
	*deadlineAwareRecorder
	writes atomic.Int64
}

func newWriteCountingRecorder() *writeCountingRecorder {
	return &writeCountingRecorder{deadlineAwareRecorder: &deadlineAwareRecorder{ResponseRecorder: httptest.NewRecorder()}}
}

func (w *writeCountingRecorder) Write(p []byte) (int, error) {
	n, err := w.deadlineAwareRecorder.Write(p)
	w.writes.Add(1)
	return n, err
}

// startEventStream runs the event route for req, whose context it makes
// cancellable, in its own goroutine, and blocks only until minWrites frames
// have reached the wire — observed through rec's atomic counter, never by
// reading rec.Body, which would race the handler goroutine still running.
// The caller cancels the returned context (via the returned cancel) and
// drains done before reading rec.
func startEventStream(t *testing.T, s *Server, req *http.Request, rec *writeCountingRecorder, minWrites int64) (cancel context.CancelFunc, done <-chan struct{}) {
	t.Helper()

	ctx, cancelFn := context.WithCancel(req.Context())
	req = req.WithContext(ctx)
	doneCh := make(chan struct{})
	go func() {
		defer close(doneCh)
		s.routes().ServeHTTP(rec, req)
	}()
	waitForCondition(t, 2*time.Second, 2*time.Millisecond,
		func() bool { return rec.writes.Load() >= minWrites },
		func() string { return fmt.Sprintf("writes = %d, want >= %d", rec.writes.Load(), minWrites) })
	return cancelFn, doneCh
}

// sseFrame is one parsed id:/event:/data: frame.
type sseFrame struct {
	id    string
	event string
	data  string
}

// parseSSEFrames splits body on the blank-line frame terminator sseWriter's
// event and comment methods both use, and parses each id:/event:/data:
// frame, skipping bare comment lines (heartbeats), which carry no event: or
// data: line at all.
func parseSSEFrames(body string) []sseFrame {
	var frames []sseFrame
	for _, chunk := range strings.Split(strings.TrimRight(body, "\n"), "\n\n") {
		if chunk == "" || strings.HasPrefix(chunk, ": ") {
			continue
		}
		var f sseFrame
		for _, line := range strings.Split(chunk, "\n") {
			switch {
			case strings.HasPrefix(line, "id: "):
				f.id = strings.TrimPrefix(line, "id: ")
			case strings.HasPrefix(line, "event: "):
				f.event = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				f.data = strings.TrimPrefix(line, "data: ")
			}
		}
		frames = append(frames, f)
	}
	return frames
}

// TestEventStreamSnapshotIsFirstFrame is the feature spec's core ordering
// requirement: the body's first frame is event: snapshot, its data is a JSON
// array, and it appears before any event: health frame.
func TestEventStreamSnapshotIsFirstFrame(t *testing.T) {
	t.Parallel()

	// Arrange
	fd := &fakeDocker{
		containerStatesFn: func(context.Context) ([]dockerx.ContainerStateSummary, error) {
			return []dockerx.ContainerStateSummary{{ID: "c1", Name: "api", State: "running", Health: "healthy"}}, nil
		},
		streamContainerEventsFn: func(ctx context.Context, onReady func(), emit func(dockerx.ContainerEvent) error) error {
			onReady()
			if err := emit(dockerx.ContainerEvent{ID: "c1", Name: "api", Event: "die", Time: "2026-01-01T00:00:00Z"}); err != nil {
				return err
			}
			<-ctx.Done()
			return nil
		},
	}
	s, st := testServerWithDocker(t, policy.ModeDefault, fd)
	serial := pairDeviceForRead(t, st)
	req := requestWithPeerSerial(http.MethodGet, "/v1/events/stream", nil, serial)
	rec := newWriteCountingRecorder()

	// Act
	cancel, done := startEventStream(t, s, req, rec, 2)
	cancel()
	<-done

	// Assert
	frames := parseSSEFrames(rec.Body.String())
	if len(frames) < 2 {
		t.Fatalf("got %d frames, want at least 2 (snapshot, then health); body: %q", len(frames), rec.Body.String())
	}
	if frames[0].event != sseEventSnapshot {
		t.Fatalf("first frame event = %q, want %q", frames[0].event, sseEventSnapshot)
	}
	if !strings.HasPrefix(frames[0].data, "[") {
		t.Errorf("snapshot data = %q, want a JSON array", frames[0].data)
	}
	var sawHealth bool
	for _, f := range frames[1:] {
		if f.event == sseEventHealth {
			sawHealth = true
		}
	}
	if !sawHealth {
		t.Errorf("no event: health frame followed the snapshot; frames: %+v", frames)
	}
}

// TestEventStreamSnapshotShape asserts each snapshot element has exactly
// id, name, state, health, and that a "none" health value round-trips.
func TestEventStreamSnapshotShape(t *testing.T) {
	t.Parallel()

	// Arrange
	fd := &fakeDocker{
		containerStatesFn: func(context.Context) ([]dockerx.ContainerStateSummary, error) {
			return []dockerx.ContainerStateSummary{
				{ID: "c1", Name: "api", State: "running", Health: "healthy"},
				{ID: "c2", Name: "cron", State: "exited", Health: "none"},
			}, nil
		},
	}
	s, st := testServerWithDocker(t, policy.ModeDefault, fd)
	serial := pairDeviceForRead(t, st)
	req := requestWithPeerSerial(http.MethodGet, "/v1/events/stream", nil, serial)
	rec := newWriteCountingRecorder()

	// Act
	cancel, done := startEventStream(t, s, req, rec, 1)
	cancel()
	<-done

	// Assert
	frames := parseSSEFrames(rec.Body.String())
	if len(frames) == 0 || frames[0].event != sseEventSnapshot {
		t.Fatalf("frames = %+v, want the first to be event: snapshot", frames)
	}
	var items []map[string]any
	if err := json.Unmarshal([]byte(frames[0].data), &items); err != nil {
		t.Fatalf("decode snapshot data: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("snapshot has %d items, want 2", len(items))
	}
	for _, item := range items {
		if len(item) != 4 {
			t.Errorf("item %+v has %d keys, want exactly 4 (id, name, state, health)", item, len(item))
		}
		for _, key := range []string{"id", "name", "state", "health"} {
			if _, ok := item[key]; !ok {
				t.Errorf("item %+v is missing key %q", item, key)
			}
		}
	}
	if items[1]["health"] != "none" {
		t.Errorf("second item health = %v, want %q", items[1]["health"], "none")
	}
}

// TestEventStreamForwardsHealthStatus is the feature spec's requirement: a
// fake emitting healthy then unhealthy produces two event: health frames
// carrying the right health value and the "health_status" event literal.
func TestEventStreamForwardsHealthStatus(t *testing.T) {
	t.Parallel()

	// Arrange
	fd := &fakeDocker{
		streamContainerEventsFn: func(ctx context.Context, onReady func(), emit func(dockerx.ContainerEvent) error) error {
			onReady()
			if err := emit(dockerx.ContainerEvent{ID: "c1", Name: "db", Event: "health_status", Health: "healthy", Time: "2026-01-01T00:00:00Z"}); err != nil {
				return err
			}
			if err := emit(dockerx.ContainerEvent{ID: "c1", Name: "db", Event: "health_status", Health: "unhealthy", Time: "2026-01-01T00:00:01Z"}); err != nil {
				return err
			}
			<-ctx.Done()
			return nil
		},
	}
	s, st := testServerWithDocker(t, policy.ModeDefault, fd)
	serial := pairDeviceForRead(t, st)
	req := requestWithPeerSerial(http.MethodGet, "/v1/events/stream", nil, serial)
	rec := newWriteCountingRecorder()

	// Act
	cancel, done := startEventStream(t, s, req, rec, 3)
	cancel()
	<-done

	// Assert
	var healthValues []string
	for _, f := range parseSSEFrames(rec.Body.String()) {
		if f.event != sseEventHealth {
			continue
		}
		var ev dockerx.ContainerEvent
		if err := json.Unmarshal([]byte(f.data), &ev); err != nil {
			t.Fatalf("decode health frame: %v", err)
		}
		if ev.Event != "health_status" {
			t.Errorf("event field = %q, want health_status", ev.Event)
		}
		healthValues = append(healthValues, ev.Health)
	}
	want := []string{"healthy", "unhealthy"}
	if len(healthValues) != len(want) {
		t.Fatalf("got %d health frames (%v), want %d", len(healthValues), healthValues, len(want))
	}
	for i := range want {
		if healthValues[i] != want[i] {
			t.Errorf("frame %d health = %q, want %q", i, healthValues[i], want[i])
		}
	}
}

// TestEventStreamForwardsLifecycleEvents asserts die/start/stop/oom each
// arrive as event: health frames whose data carries no "health" key.
func TestEventStreamForwardsLifecycleEvents(t *testing.T) {
	t.Parallel()

	// Arrange
	lifecycle := []string{"die", "start", "stop", "oom"}
	fd := &fakeDocker{
		streamContainerEventsFn: func(ctx context.Context, onReady func(), emit func(dockerx.ContainerEvent) error) error {
			onReady()
			for i, action := range lifecycle {
				ev := dockerx.ContainerEvent{ID: "c1", Name: "api", Event: action, Time: fmt.Sprintf("2026-01-01T00:00:0%dZ", i)}
				if err := emit(ev); err != nil {
					return err
				}
			}
			<-ctx.Done()
			return nil
		},
	}
	s, st := testServerWithDocker(t, policy.ModeDefault, fd)
	serial := pairDeviceForRead(t, st)
	req := requestWithPeerSerial(http.MethodGet, "/v1/events/stream", nil, serial)
	rec := newWriteCountingRecorder()

	// Act
	cancel, done := startEventStream(t, s, req, rec, int64(1+len(lifecycle)))
	cancel()
	<-done

	// Assert
	var got []string
	for _, f := range parseSSEFrames(rec.Body.String()) {
		if f.event != sseEventHealth {
			continue
		}
		if strings.Contains(f.data, `"health"`) {
			t.Errorf("lifecycle frame carries a health key: %s", f.data)
		}
		var ev dockerx.ContainerEvent
		if err := json.Unmarshal([]byte(f.data), &ev); err != nil {
			t.Fatalf("decode frame: %v", err)
		}
		got = append(got, ev.Event)
	}
	if len(got) != len(lifecycle) {
		t.Fatalf("got %d lifecycle frames (%v), want %d", len(got), got, len(lifecycle))
	}
	for i := range lifecycle {
		if got[i] != lifecycle[i] {
			t.Errorf("frame %d event = %q, want %q", i, got[i], lifecycle[i])
		}
	}
}

// TestEventStreamEventsAfterSnapshotAreNotLost is D7's proof: the fake blocks
// ContainerStates until an event has already been emitted (queued into
// sub.events), so the event fired strictly between the subscription going
// live and the snapshot call returning. It must still reach the client.
func TestEventStreamEventsAfterSnapshotAreNotLost(t *testing.T) {
	t.Parallel()

	// Arrange
	emitted := make(chan struct{})
	fd := &fakeDocker{
		streamContainerEventsFn: func(ctx context.Context, onReady func(), emit func(dockerx.ContainerEvent) error) error {
			onReady()
			if err := emit(dockerx.ContainerEvent{ID: "c1", Name: "api", Event: "die", Time: "2026-01-01T00:00:00Z"}); err != nil {
				return err
			}
			close(emitted)
			<-ctx.Done()
			return nil
		},
		containerStatesFn: func(ctx context.Context) ([]dockerx.ContainerStateSummary, error) {
			<-emitted // the event is already queued by the time this returns
			return []dockerx.ContainerStateSummary{}, nil
		},
	}
	s, st := testServerWithDocker(t, policy.ModeDefault, fd)
	serial := pairDeviceForRead(t, st)
	req := requestWithPeerSerial(http.MethodGet, "/v1/events/stream", nil, serial)
	rec := newWriteCountingRecorder()

	// Act
	cancel, done := startEventStream(t, s, req, rec, 2)
	cancel()
	<-done

	// Assert
	frames := parseSSEFrames(rec.Body.String())
	if len(frames) < 2 || frames[0].event != sseEventSnapshot {
		t.Fatalf("frames = %+v, want a snapshot followed by the health frame that fired before it", frames)
	}
	if frames[1].event != sseEventHealth {
		t.Fatalf("second frame event = %q, want %q", frames[1].event, sseEventHealth)
	}
}

// TestEventStreamPerDeviceSingleStream is D11's spec requirement: a second
// connection from the same device evicts the first, which ends with a
// terminal event: error / "event stream superseded" frame, while the second
// stays live. Per the mandatory GOTCHA, the first recorder is never read
// while its handler goroutine is still running: the second request is only
// fired once the first's snapshot write is observed via its write counter,
// and the first recorder is only read after its own goroutine has returned.
func TestEventStreamPerDeviceSingleStream(t *testing.T) {
	t.Parallel()

	// Arrange
	fd := &fakeDocker{}
	s, st := testServerWithDocker(t, policy.ModeDefault, fd)
	serial := pairDeviceForRead(t, st)

	req1 := requestWithPeerSerial(http.MethodGet, "/v1/events/stream", nil, serial)
	rec1 := newWriteCountingRecorder()
	done1 := make(chan struct{})
	go func() {
		defer close(done1)
		s.routes().ServeHTTP(rec1, req1)
	}()
	waitForCondition(t, 2*time.Second, 2*time.Millisecond,
		func() bool { return rec1.writes.Load() >= 1 },
		func() string { return "first stream never sent its snapshot" })

	// Act — a second connection from the same device.
	req2 := requestWithPeerSerial(http.MethodGet, "/v1/events/stream", nil, serial)
	rec2 := newWriteCountingRecorder()
	ctx2, cancel2 := context.WithCancel(req2.Context())
	req2 = req2.WithContext(ctx2)
	done2 := make(chan struct{})
	go func() {
		defer close(done2)
		s.routes().ServeHTTP(rec2, req2)
	}()
	waitForCondition(t, 2*time.Second, 2*time.Millisecond,
		func() bool { return rec2.writes.Load() >= 1 },
		func() string { return "second stream never sent its snapshot" })

	// Assert — the first stream ends on its own, evicted by the second.
	select {
	case <-done1:
	case <-time.After(2 * time.Second):
		t.Fatal("first stream did not end after being superseded")
	}
	if rec1.Code != http.StatusOK {
		t.Errorf("first stream status = %d, want 200 (it committed before being superseded)", rec1.Code)
	}
	body1 := rec1.Body.String()
	if !strings.Contains(body1, "event: error") || !strings.Contains(body1, msgEventStreamSuperseded) {
		t.Errorf("first stream body = %q, want a terminal event: error frame carrying %q", body1, msgEventStreamSuperseded)
	}

	// Cleanup — the second stream stays live until cancelled here.
	cancel2()
	<-done2
}

// TestEventStreamDevicesAreIndependent proves a second connection from a
// DIFFERENT device does not touch the first device's stream: device A ends
// on a plain disconnect, carrying no terminal error frame.
func TestEventStreamDevicesAreIndependent(t *testing.T) {
	t.Parallel()

	// Arrange
	fd := &fakeDocker{}
	s, st := testServerWithDocker(t, policy.ModeDefault, fd)
	serialA := pairDeviceForRead(t, st)
	serialB := pairDeviceForRead(t, st)

	reqA := requestWithPeerSerial(http.MethodGet, "/v1/events/stream", nil, serialA)
	recA := newWriteCountingRecorder()
	cancelA, doneA := startEventStream(t, s, reqA, recA, 1)

	// Act
	reqB := requestWithPeerSerial(http.MethodGet, "/v1/events/stream", nil, serialB)
	recB := newWriteCountingRecorder()
	cancelB, doneB := startEventStream(t, s, reqB, recB, 1)

	cancelA()
	<-doneA
	cancelB()
	<-doneB

	// Assert
	if strings.Contains(recA.Body.String(), "event: error") {
		t.Errorf("device A body = %q, want no terminal error frame: device B must never touch A's stream", recA.Body.String())
	}
}

// TestEventStreamDoesNotConsumeLogStreamBudget is D11's spec requirement:
// with s.streams driven to the host ceiling by log streams, the event route
// still answers 200 — it never touches streamBudget.
func TestEventStreamDoesNotConsumeLogStreamBudget(t *testing.T) {
	t.Parallel()

	// Arrange — fill every log-stream slot across maxConcurrentStreams
	// distinct devices, mirroring TestStreamSlotExhaustion.
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
			t.Fatalf("only %d of %d log streams reported started", i, maxConcurrentStreams)
		}
	}

	// Act — the event route, from a device already holding a log stream,
	// must still succeed.
	eventReq := requestWithPeerSerial(http.MethodGet, "/v1/events/stream", nil, serials[0])
	rec := newWriteCountingRecorder()
	cancel, streamDone := startEventStream(t, s, eventReq, rec, 1)
	cancel()
	<-streamDone

	// Assert
	if rec.Code != http.StatusOK {
		t.Fatalf("event stream status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}

	// Cleanup — release the held log streams.
	close(release)
	for i := 0; i < maxConcurrentStreams; i++ {
		<-done
	}
}

// TestEventStreamRequiresDevice asserts no req.TLS answers 401 with the
// standard terse body.
func TestEventStreamRequiresDevice(t *testing.T) {
	t.Parallel()

	// Arrange
	s, _ := testServerWithDocker(t, policy.ModeDefault, &fakeDocker{})
	req := httptest.NewRequest(http.MethodGet, "/v1/events/stream", nil)
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
}

// TestEventStreamRevokedDeviceIs401 mirrors middleware_test.go's revoked
// device case on the event route.
func TestEventStreamRevokedDeviceIs401(t *testing.T) {
	t.Parallel()

	// Arrange
	s, st := testServerWithDocker(t, policy.ModeDefault, &fakeDocker{})
	serial := pairDeviceForRead(t, st)
	ctx := context.Background()
	device, err := st.DeviceByCertSerial(ctx, serial.Text(16))
	if err != nil {
		t.Fatalf("DeviceByCertSerial: %v", err)
	}
	if err := st.RevokeDevice(ctx, device.ID); err != nil {
		t.Fatalf("RevokeDevice: %v", err)
	}
	req := requestWithPeerSerial(http.MethodGet, "/v1/events/stream", nil, serial)
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
}

// TestEventStreamPolicyForbidden asserts requireOp's fail-closed branch.
// policy.Mode(-1) is SYNTHETIC: no shipped mode denies OpRead (every mode
// permits it, D3), so this out-of-range value is the only way to exercise
// this branch on this route — Allows returns false for any Mode below
// minMode[OpRead] (ModeReadOnly), which -1 satisfies by construction.
func TestEventStreamPolicyForbidden(t *testing.T) {
	t.Parallel()

	// Arrange
	fd := &fakeDocker{}
	s, st := testServerWithDocker(t, policy.Mode(-1), fd)
	serial := pairDeviceForRead(t, st)
	req := requestWithPeerSerial(http.MethodGet, "/v1/events/stream", nil, serial)
	rec := httptest.NewRecorder()

	// Act
	s.routes().ServeHTTP(rec, req)

	// Assert
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body: %s", rec.Code, rec.Body.String())
	}
	var body errorBody
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body.Error != msgPolicyForbidden {
		t.Errorf("error = %q, want %q", body.Error, msgPolicyForbidden)
	}
}

// TestEventStreamReadOnlyModeIs200 is D3's real-world counterpart: the
// stream is served under the most restrictive shipped policy mode.
func TestEventStreamReadOnlyModeIs200(t *testing.T) {
	t.Parallel()

	// Arrange
	fd := &fakeDocker{}
	s, st := testServerWithDocker(t, policy.ModeReadOnly, fd)
	serial := pairDeviceForRead(t, st)
	req := requestWithPeerSerial(http.MethodGet, "/v1/events/stream", nil, serial)
	rec := newWriteCountingRecorder()

	// Act
	cancel, done := startEventStream(t, s, req, rec, 1)
	cancel()
	<-done

	// Assert
	if rec.Code != http.StatusOK {
		t.Errorf("status under read-only policy = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
}

// TestEventStreamRateLimited mirrors ratelimit_test.go's guarded-tier
// pattern: a burst of 2 (guardedPerSec 1 x guardedBurstMultiplier), each
// request resolving without ever entering the streaming loop by having
// ContainerStates fail fast, so the third request's 429 is unambiguously the
// limiter's doing.
func TestEventStreamRateLimited(t *testing.T) {
	t.Parallel()

	// Arrange
	dir := t.TempDir()
	cfg := config.Config{StateDir: dir, ListenAddr: ":8443", PolicyMode: policy.ModeDefault, RateGuardedPerSec: 1}
	st, err := state.Open(context.Background(), filepath.Join(dir, "devmon.db"), testLogger())
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	fd := &fakeDocker{
		containerStatesFn: func(context.Context) ([]dockerx.ContainerStateSummary, error) {
			return nil, errors.New("engine unreachable")
		},
	}
	s := NewServer(cfg, st, nil, fd, nil, testLogger())
	serial := pairDeviceForRead(t, st)

	// Act — drain the burst of 2.
	for i := 0; i < 2; i++ {
		req := requestWithPeerSerial(http.MethodGet, "/v1/events/stream", nil, serial)
		rec := httptest.NewRecorder()
		s.routes().ServeHTTP(rec, req)
		if rec.Code != http.StatusBadGateway {
			t.Fatalf("request %d status = %d, want 502 (past the limiter, failing at the Engine call); body: %s", i, rec.Code, rec.Body.String())
		}
	}

	// Act — a third request, past the burst.
	req := requestWithPeerSerial(http.MethodGet, "/v1/events/stream", nil, serial)
	rec := httptest.NewRecorder()
	s.routes().ServeHTTP(rec, req)

	// Assert
	assertRateLimitedResponse(t, rec)
}

// TestEventStreamEngineUnavailable asserts attach failing (before onReady)
// answers a real 502, not a stream that turns out to carry nothing (D4).
func TestEventStreamEngineUnavailable(t *testing.T) {
	t.Parallel()

	// Arrange
	fd := &fakeDocker{
		streamContainerEventsFn: func(context.Context, func(), func(dockerx.ContainerEvent) error) error {
			return errors.New("engine unreachable")
		},
	}
	s, st := testServerWithDocker(t, policy.ModeDefault, fd)
	serial := pairDeviceForRead(t, st)
	req := requestWithPeerSerial(http.MethodGet, "/v1/events/stream", nil, serial)
	rec := httptest.NewRecorder()

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
}

// TestEventStreamSnapshotFailureIsJSON asserts a ContainerStates failure —
// after a successful attach — is still a 502 JSON body, headers never
// committed to SSE.
func TestEventStreamSnapshotFailureIsJSON(t *testing.T) {
	t.Parallel()

	// Arrange
	fd := &fakeDocker{
		containerStatesFn: func(context.Context) ([]dockerx.ContainerStateSummary, error) {
			return nil, errors.New("engine unreachable")
		},
	}
	s, st := testServerWithDocker(t, policy.ModeDefault, fd)
	serial := pairDeviceForRead(t, st)
	req := requestWithPeerSerial(http.MethodGet, "/v1/events/stream", nil, serial)
	rec := httptest.NewRecorder()

	// Act
	s.routes().ServeHTTP(rec, req)

	// Assert
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502; body: %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json (headers never committed to SSE)", ct)
	}
	var body errorBody
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body.Error != msgEngineUnavailable {
		t.Errorf("error = %q, want %q", body.Error, msgEngineUnavailable)
	}
}

// TestEventStreamNilDockerReader asserts a nil s.dc answers 502 without
// panicking.
func TestEventStreamNilDockerReader(t *testing.T) {
	t.Parallel()

	// Arrange
	s, st := testServerWithDocker(t, policy.ModeDefault, nil)
	serial := pairDeviceForRead(t, st)
	req := requestWithPeerSerial(http.MethodGet, "/v1/events/stream", nil, serial)
	rec := httptest.NewRecorder()

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
}

// TestEventStreamFeedDeathSendsTerminalError asserts a hub feed ending
// after the snapshot has committed produces the "docker engine unavailable"
// terminal frame, and the handler then returns on its own.
func TestEventStreamFeedDeathSendsTerminalError(t *testing.T) {
	t.Parallel()

	// Arrange
	fd := &fakeDocker{
		streamContainerEventsFn: func(_ context.Context, onReady func(), _ func(dockerx.ContainerEvent) error) error {
			onReady()
			return dockerx.ErrEventFeedClosed
		},
	}
	s, st := testServerWithDocker(t, policy.ModeDefault, fd)
	serial := pairDeviceForRead(t, st)
	req := requestWithPeerSerial(http.MethodGet, "/v1/events/stream", nil, serial)
	rec := &deadlineAwareRecorder{ResponseRecorder: httptest.NewRecorder()}

	// Act
	s.routes().ServeHTTP(rec, req)

	// Assert
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (the snapshot already committed); body: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if got := strings.Count(body, "event: error\n"); got != 1 {
		t.Errorf("body has %d event: error frames, want 1; body: %q", got, body)
	}
	wantSuffix := fmt.Sprintf("event: error\ndata: {\"error\":%q}\n\n", msgEngineUnavailable)
	if !strings.HasSuffix(body, wantSuffix) {
		t.Errorf("body = %q, want it to end with %q", body, wantSuffix)
	}
}

// TestEventStreamLaggedClientTerminalError shrinks eventClientBuffer to 1
// and drives two emits before ContainerStates is allowed to return, so the
// overflow (and the resulting drop) is guaranteed to happen before the
// handler even starts its select loop — the same synchronization technique
// TestEventStreamEventsAfterSnapshotAreNotLost uses, rather than a timing
// sleep.
func TestEventStreamLaggedClientTerminalError(t *testing.T) {
	// Not t.Parallel(): mutates the package-level eventClientBuffer.
	original := eventClientBuffer
	eventClientBuffer = 1
	t.Cleanup(func() { eventClientBuffer = original })

	// Arrange
	pushed := make(chan struct{})
	fd := &fakeDocker{
		streamContainerEventsFn: func(ctx context.Context, onReady func(), emit func(dockerx.ContainerEvent) error) error {
			onReady()
			_ = emit(dockerx.ContainerEvent{ID: "c1", Event: "die", Time: "2026-01-01T00:00:00Z"})
			_ = emit(dockerx.ContainerEvent{ID: "c2", Event: "start", Time: "2026-01-01T00:00:01Z"})
			close(pushed)
			<-ctx.Done()
			return nil
		},
		containerStatesFn: func(ctx context.Context) ([]dockerx.ContainerStateSummary, error) {
			<-pushed
			return []dockerx.ContainerStateSummary{}, nil
		},
	}
	s, st := testServerWithDocker(t, policy.ModeDefault, fd)
	serial := pairDeviceForRead(t, st)
	req := requestWithPeerSerial(http.MethodGet, "/v1/events/stream", nil, serial)
	rec := &deadlineAwareRecorder{ResponseRecorder: httptest.NewRecorder()}

	// Act
	s.routes().ServeHTTP(rec, req)

	// Assert
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if got := strings.Count(body, "event: error\n"); got != 1 {
		t.Errorf("body has %d event: error frames, want 1; body: %q", got, body)
	}
	wantSuffix := fmt.Sprintf("event: error\ndata: {\"error\":%q}\n\n", msgEventStreamLagged)
	if !strings.HasSuffix(body, wantSuffix) {
		t.Errorf("body = %q, want it to end with %q", body, wantSuffix)
	}
}

// TestEventStreamHeartbeat shortens eventHeartbeatInterval so a heartbeat
// comment appears well within the test's deadline.
func TestEventStreamHeartbeat(t *testing.T) {
	// Not t.Parallel(): mutates the package-level eventHeartbeatInterval.
	original := eventHeartbeatInterval
	eventHeartbeatInterval = 2 * time.Millisecond
	t.Cleanup(func() { eventHeartbeatInterval = original })

	// Arrange
	fd := &fakeDocker{}
	s, st := testServerWithDocker(t, policy.ModeDefault, fd)
	serial := pairDeviceForRead(t, st)
	req := requestWithPeerSerial(http.MethodGet, "/v1/events/stream", nil, serial)
	rec := newWriteCountingRecorder()

	// Act — snapshot, then at least one heartbeat.
	cancel, done := startEventStream(t, s, req, rec, 2)
	cancel()
	<-done

	// Assert
	if !strings.Contains(rec.Body.String(), ": heartbeat\n\n") {
		t.Errorf("body = %q, want at least one heartbeat comment", rec.Body.String())
	}
}

// TestEventStreamHeartbeatIsRaceFree runs a fake that emits events while the
// shortened heartbeat ticker fires concurrently, under -race.
func TestEventStreamHeartbeatIsRaceFree(t *testing.T) {
	// Not t.Parallel(): mutates the package-level eventHeartbeatInterval.
	original := eventHeartbeatInterval
	eventHeartbeatInterval = 2 * time.Millisecond
	t.Cleanup(func() { eventHeartbeatInterval = original })

	// Arrange
	fd := &fakeDocker{
		streamContainerEventsFn: func(ctx context.Context, onReady func(), emit func(dockerx.ContainerEvent) error) error {
			onReady()
			for i := 0; i < 10; i++ {
				if err := emit(dockerx.ContainerEvent{ID: "c1", Event: "die", Time: "2026-01-01T00:00:00Z"}); err != nil {
					return err
				}
				// Bounded, short sleep to give the 2ms heartbeat ticker room
				// to race with this write on the shared sseWriter, mirroring
				// TestStreamKeepaliveIsRaceFree (logs_test.go). The real
				// assertion is `-race`, not any state this sleep could
				// instead wait on.
				time.Sleep(time.Millisecond)
			}
			<-ctx.Done()
			return nil
		},
	}
	s, st := testServerWithDocker(t, policy.ModeDefault, fd)
	serial := pairDeviceForRead(t, st)
	req := requestWithPeerSerial(http.MethodGet, "/v1/events/stream", nil, serial)
	rec := newWriteCountingRecorder()

	// Act — the real assertion here is `go test -race`.
	cancel, done := startEventStream(t, s, req, rec, 5)
	cancel()
	<-done

	// Assert
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
}

// eventStreamHeartbeatGoroutineFrame is the stack-trace frame of the
// anonymous heartbeat goroutine handleEventStream starts. A renamed or moved
// goroutine must fail TestEventStreamGoroutineDoesNotLeak loudly instead of
// passing vacuously — see countGoroutinesMatching (logs_test.go).
const eventStreamHeartbeatGoroutineFrame = "internal/httpapi.(*Server).handleEventStream.func1"

// TestEventStreamGoroutineDoesNotLeak opens and closes 20 streams and
// asserts the heartbeat goroutine count returns to zero.
func TestEventStreamGoroutineDoesNotLeak(t *testing.T) {
	t.Parallel()

	// Arrange
	inFlightCount := 0
	fd := &fakeDocker{}
	s, st := testServerWithDocker(t, policy.ModeDefault, fd)
	serial := pairDeviceForRead(t, st)

	// Act
	for i := 0; i < 20; i++ {
		req := requestWithPeerSerial(http.MethodGet, "/v1/events/stream", nil, serial)
		rec := newWriteCountingRecorder()
		cancel, done := startEventStream(t, s, req, rec, 1)
		if got := countGoroutinesMatching(eventStreamHeartbeatGoroutineFrame); got > inFlightCount {
			inFlightCount = got
		}
		cancel()
		<-done
	}
	if inFlightCount == 0 {
		t.Fatalf("eventStreamHeartbeatGoroutineFrame %q never matched a running goroutine; "+
			"the frame name likely drifted from handleEventStream.func1", eventStreamHeartbeatGoroutineFrame)
	}

	// Assert
	waitForCondition(t, 2*time.Second, 5*time.Millisecond,
		func() bool { return countGoroutinesMatching(eventStreamHeartbeatGoroutineFrame) == 0 },
		func() string {
			return fmt.Sprintf("heartbeat goroutine count = %d, want 0", countGoroutinesMatching(eventStreamHeartbeatGoroutineFrame))
		})
}

// TestEventStreamAgentLogNeverCarriesAttributes mirrors
// TestAgentLogNeverCarriesLineContent: an event carrying a value that would
// have come from the container's own labels (D14) must never reach the
// agent's own log.
func TestEventStreamAgentLogNeverCarriesAttributes(t *testing.T) {
	t.Parallel()

	// Arrange
	const secretName = "hunter2"
	fd := &fakeDocker{
		streamContainerEventsFn: func(ctx context.Context, onReady func(), emit func(dockerx.ContainerEvent) error) error {
			onReady()
			ev := dockerx.ContainerEvent{ID: "c1", Name: secretName, Event: "die", Time: "2026-01-01T00:00:00Z"}
			if err := emit(ev); err != nil {
				return err
			}
			<-ctx.Done()
			return nil
		},
	}
	log, buf := newCapturingLogger()
	s, st := testServerWithDockerAndLogger(t, policy.ModeDefault, fd, log)
	serial := pairDeviceForRead(t, st)
	req := requestWithPeerSerial(http.MethodGet, "/v1/events/stream", nil, serial)
	rec := newWriteCountingRecorder()

	// Act
	cancel, done := startEventStream(t, s, req, rec, 2)
	cancel()
	<-done

	// Assert
	if bodyContains(buf.String(), secretName) {
		t.Errorf("agent log contains a value derived from the event's attributes: %s", buf.String())
	}
}

// TestEventStreamErrorBodiesLeakNothing asserts every failure path on this
// route keeps the host's state directory, the Docker socket, and the state
// database filename out of the response body.
func TestEventStreamErrorBodiesLeakNothing(t *testing.T) {
	t.Parallel()

	// Arrange
	fd := &fakeDocker{}
	s, st := testServerWithDocker(t, policy.ModeDefault, fd)
	serial := pairDeviceForRead(t, st)
	var bodies []string

	// Act — collect every failure body: 401 (no cert), 405 (wrong method),
	// and the mapped 502 failures for attach and the snapshot call.
	unauth := httptest.NewRequest(http.MethodGet, "/v1/events/stream", nil)
	unauthRec := httptest.NewRecorder()
	s.routes().ServeHTTP(unauthRec, unauth)
	bodies = append(bodies, unauthRec.Body.String())

	wrongMethod := httptest.NewRequest(http.MethodPost, "/v1/events/stream", nil)
	wrongMethodRec := httptest.NewRecorder()
	s.routes().ServeHTTP(wrongMethodRec, wrongMethod)
	bodies = append(bodies, wrongMethodRec.Body.String())

	fd.streamContainerEventsFn = func(context.Context, func(), func(dockerx.ContainerEvent) error) error {
		return errors.New("dial unix " + `/var/run/docker.sock` + ": connect: no such file or directory")
	}
	attachFail := requestWithPeerSerial(http.MethodGet, "/v1/events/stream", nil, serial)
	attachFailRec := httptest.NewRecorder()
	s.routes().ServeHTTP(attachFailRec, attachFail)
	bodies = append(bodies, attachFailRec.Body.String())

	fd.streamContainerEventsFn = nil
	fd.containerStatesFn = func(context.Context) ([]dockerx.ContainerStateSummary, error) {
		return nil, errors.New("dial unix " + `/var/run/docker.sock` + ": connect: no such file or directory")
	}
	snapshotFail := requestWithPeerSerial(http.MethodGet, "/v1/events/stream", nil, serial)
	snapshotFailRec := httptest.NewRecorder()
	s.routes().ServeHTTP(snapshotFailRec, snapshotFail)
	bodies = append(bodies, snapshotFailRec.Body.String())

	// Assert
	for _, leak := range []string{s.cfg.StateDir, "docker.sock", "devmon.db"} {
		for _, body := range bodies {
			if leak != "" && bodyContains(body, leak) {
				t.Errorf("a failure body leaks %q: %s", leak, body)
			}
		}
	}
}

// TestEventRouteRejectsOtherMethods asserts POST on the event route answers
// 405.
func TestEventRouteRejectsOtherMethods(t *testing.T) {
	t.Parallel()

	// Arrange
	s, _ := testServerWithDocker(t, policy.ModeDefault, &fakeDocker{})
	req := httptest.NewRequest(http.MethodPost, "/v1/events/stream", nil)
	rec := httptest.NewRecorder()

	// Act
	s.routes().ServeHTTP(rec, req)

	// Assert
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST /v1/events/stream = %d, want 405", rec.Code)
	}
}

// TestEventStreamTerminatesWhenDeviceRevokedMidStream is
// GHSA-qrxm-qm54-xc44's regression on the event route: a device revoked
// after its stream is already open must still have the stream terminated,
// via the heartbeat goroutine's per-tick re-check, rather than keep
// receiving health frames forever. Mirrors
// TestStreamTerminatesWhenDeviceRevokedMidStream (logs_test.go).
func TestEventStreamTerminatesWhenDeviceRevokedMidStream(t *testing.T) {
	// Not t.Parallel(): mutates the package-level eventHeartbeatInterval.

	// Arrange
	original := eventHeartbeatInterval
	eventHeartbeatInterval = 5 * time.Millisecond
	t.Cleanup(func() { eventHeartbeatInterval = original })

	fd := &fakeDocker{
		streamContainerEventsFn: func(ctx context.Context, onReady func(), _ func(dockerx.ContainerEvent) error) error {
			onReady()
			<-ctx.Done()
			return nil
		},
	}
	s, st := testServerWithDocker(t, policy.ModeDefault, fd)
	serial := pairDeviceForRead(t, st)
	device, err := st.DeviceByCertSerial(context.Background(), serial.Text(16))
	if err != nil {
		t.Fatalf("DeviceByCertSerial: %v", err)
	}
	req := requestWithPeerSerial(http.MethodGet, "/v1/events/stream", nil, serial)
	rec := &syncBodyRecorder{deadlineAwareRecorder: &deadlineAwareRecorder{ResponseRecorder: httptest.NewRecorder()}}

	done := make(chan struct{})
	go func() {
		s.routes().ServeHTTP(rec, req)
		close(done)
	}()

	// Act — revoke the device once the stream is confirmed open (snapshot
	// received), then wait for the handler to notice and unwind.
	waitForCondition(t, 2*time.Second, 2*time.Millisecond,
		func() bool { return strings.Contains(rec.bodySnapshot(), sseEventSnapshot) },
		func() string { return fmt.Sprintf("body = %q, want the snapshot frame first", rec.bodySnapshot()) })
	if err := st.RevokeDevice(context.Background(), device.ID); err != nil {
		t.Fatalf("RevokeDevice: %v", err)
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("stream did not terminate within the heartbeat window after revocation")
	}

	// Assert
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.bodySnapshot())
	}
	wantSuffix := fmt.Sprintf("event: error\ndata: {\"error\":%q}\n\n", msgStreamRevoked)
	if !strings.HasSuffix(rec.bodySnapshot(), wantSuffix) {
		t.Errorf("body = %q, want it to end with %q", rec.bodySnapshot(), wantSuffix)
	}
}

// TestEventStreamTerminatesWhenDeviceDeletedMidStream mirrors the revocation
// test above for the other terminal condition streamRevoked checks: the
// device row disappearing entirely (e.g. an operator's DeleteDevice call)
// ends access exactly like an explicit revocation.
func TestEventStreamTerminatesWhenDeviceDeletedMidStream(t *testing.T) {
	// Not t.Parallel(): mutates the package-level eventHeartbeatInterval.

	// Arrange
	original := eventHeartbeatInterval
	eventHeartbeatInterval = 5 * time.Millisecond
	t.Cleanup(func() { eventHeartbeatInterval = original })

	fd := &fakeDocker{
		streamContainerEventsFn: func(ctx context.Context, onReady func(), _ func(dockerx.ContainerEvent) error) error {
			onReady()
			<-ctx.Done()
			return nil
		},
	}
	s, st := testServerWithDocker(t, policy.ModeDefault, fd)
	serial := pairDeviceForRead(t, st)
	device, err := st.DeviceByCertSerial(context.Background(), serial.Text(16))
	if err != nil {
		t.Fatalf("DeviceByCertSerial: %v", err)
	}
	req := requestWithPeerSerial(http.MethodGet, "/v1/events/stream", nil, serial)
	rec := &syncBodyRecorder{deadlineAwareRecorder: &deadlineAwareRecorder{ResponseRecorder: httptest.NewRecorder()}}

	done := make(chan struct{})
	go func() {
		s.routes().ServeHTTP(rec, req)
		close(done)
	}()

	// Act
	waitForCondition(t, 2*time.Second, 2*time.Millisecond,
		func() bool { return strings.Contains(rec.bodySnapshot(), sseEventSnapshot) },
		func() string { return fmt.Sprintf("body = %q, want the snapshot frame first", rec.bodySnapshot()) })
	if err := st.DeleteDevice(context.Background(), device.ID); err != nil {
		t.Fatalf("DeleteDevice: %v", err)
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("stream did not terminate within the heartbeat window after deletion")
	}

	// Assert
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.bodySnapshot())
	}
	wantSuffix := fmt.Sprintf("event: error\ndata: {\"error\":%q}\n\n", msgStreamRevoked)
	if !strings.HasSuffix(rec.bodySnapshot(), wantSuffix) {
		t.Errorf("body = %q, want it to end with %q", rec.bodySnapshot(), wantSuffix)
	}
}

// TestEventStreamHealthySurvivesRevocationChecks asserts that a device which
// is never revoked keeps its stream alive across several heartbeat ticks,
// with no terminal error frame ever written — the per-tick re-check must not
// disturb an otherwise healthy stream. Mirrors
// TestStreamHealthySurvivesRevocationChecks (logs_test.go).
func TestEventStreamHealthySurvivesRevocationChecks(t *testing.T) {
	// Not t.Parallel(): mutates the package-level eventHeartbeatInterval.

	// Arrange
	original := eventHeartbeatInterval
	eventHeartbeatInterval = 2 * time.Millisecond
	t.Cleanup(func() { eventHeartbeatInterval = original })

	fd := &fakeDocker{
		streamContainerEventsFn: func(ctx context.Context, onReady func(), _ func(dockerx.ContainerEvent) error) error {
			onReady()
			<-ctx.Done()
			return nil
		},
	}
	s, st := testServerWithDocker(t, policy.ModeDefault, fd)
	serial := pairDeviceForRead(t, st)
	req := requestWithPeerSerial(http.MethodGet, "/v1/events/stream", nil, serial)
	ctx, cancel := context.WithCancel(req.Context())
	req = req.WithContext(ctx)
	rec := &syncBodyRecorder{deadlineAwareRecorder: &deadlineAwareRecorder{ResponseRecorder: httptest.NewRecorder()}}

	done := make(chan struct{})
	go func() {
		s.routes().ServeHTTP(rec, req)
		close(done)
	}()

	// Act — let several heartbeat ticks land while the device stays active,
	// then end the stream via an ordinary client disconnect (cancelling the
	// request context), not via the fake returning nil: the latter would
	// route through sub.closed and produce its own terminal frame, which is
	// not what this test is asserting the absence of.
	waitForCondition(t, 2*time.Second, 2*time.Millisecond,
		func() bool { return strings.Count(rec.bodySnapshot(), ": heartbeat") >= 3 },
		func() string { return fmt.Sprintf("body = %q, want at least 3 heartbeat comments", rec.bodySnapshot()) })
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("stream did not finish after cancellation")
	}

	// Assert
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.bodySnapshot())
	}
	if got := strings.Count(rec.bodySnapshot(), "event: error"); got != 0 {
		t.Errorf("body has %d terminal error frames, want 0 for a never-revoked device: %q", got, rec.bodySnapshot())
	}
}

// TestEventStreamRevocationRaceProducesSingleTerminalFrame is the regression
// test for the review defect fixed alongside GHSA-qrxm-qm54-xc44: the
// heartbeat goroutine's revocation frame-then-cancel used to rely on the
// main select's <-ctx.Done() case winning next, but select picks
// pseudo-randomly among simultaneously-ready channels, so <-sub.closed
// racing the same cancel() could append a SECOND terminal frame. This test
// drives both terminal sources — revocation and a lagged-client drop — as
// close together as the test can arrange without a sleep-based race:
// eventClientBuffer is shrunk to 1 so two queued emits deterministically
// overflow this subscriber's buffer once released, which the hub answers
// with its own eventClosedLagged close independent of ctx cancellation. The
// assertion holds regardless of which source's frame the scheduler happens
// to deliver first: the shared sync.Once in terminalEventFrameSender makes
// "at most one" a structural guarantee, not a timing outcome.
func TestEventStreamRevocationRaceProducesSingleTerminalFrame(t *testing.T) {
	// Not t.Parallel(): mutates the package-level eventHeartbeatInterval and
	// eventClientBuffer.

	originalInterval := eventHeartbeatInterval
	eventHeartbeatInterval = 2 * time.Millisecond
	t.Cleanup(func() { eventHeartbeatInterval = originalInterval })

	originalBuffer := eventClientBuffer
	eventClientBuffer = 1
	t.Cleanup(func() { eventClientBuffer = originalBuffer })

	// Arrange
	proceed := make(chan struct{})
	fd := &fakeDocker{
		streamContainerEventsFn: func(ctx context.Context, onReady func(), emit func(dockerx.ContainerEvent) error) error {
			onReady()
			<-proceed
			// Two emits against a buffer of 1 overflow this subscriber,
			// which the hub answers by dropping it with eventClosedLagged —
			// see eventHub.fanout's default branch.
			_ = emit(dockerx.ContainerEvent{ID: "c1", Event: "die", Time: "2026-01-01T00:00:00Z"})
			_ = emit(dockerx.ContainerEvent{ID: "c2", Event: "start", Time: "2026-01-01T00:00:01Z"})
			<-ctx.Done()
			return nil
		},
	}
	s, st := testServerWithDocker(t, policy.ModeDefault, fd)
	serial := pairDeviceForRead(t, st)
	device, err := st.DeviceByCertSerial(context.Background(), serial.Text(16))
	if err != nil {
		t.Fatalf("DeviceByCertSerial: %v", err)
	}
	req := requestWithPeerSerial(http.MethodGet, "/v1/events/stream", nil, serial)
	rec := &syncBodyRecorder{deadlineAwareRecorder: &deadlineAwareRecorder{ResponseRecorder: httptest.NewRecorder()}}

	done := make(chan struct{})
	go func() {
		s.routes().ServeHTTP(rec, req)
		close(done)
	}()

	// Act — once the stream is confirmed open, fire both competing terminal
	// sources back to back: revoke the device (racing the heartbeat's next
	// tick) and release the blocked emits (racing the hub's lagged drop),
	// with no ordering guaranteed between the two.
	waitForCondition(t, 2*time.Second, 2*time.Millisecond,
		func() bool { return strings.Contains(rec.bodySnapshot(), sseEventSnapshot) },
		func() string { return fmt.Sprintf("body = %q, want the snapshot frame first", rec.bodySnapshot()) })
	if err := st.RevokeDevice(context.Background(), device.ID); err != nil {
		t.Fatalf("RevokeDevice: %v", err)
	}
	close(proceed)

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("stream did not terminate after the competing terminal sources fired")
	}

	// Assert — exactly one terminal frame reached the wire, whichever of the
	// two competing sources actually won the race.
	body := rec.bodySnapshot()
	if got := strings.Count(body, "event: error\n"); got != 1 {
		t.Fatalf("body has %d terminal event: error frames, want exactly 1 (revocation and the lagged-drop path raced): %q", got, body)
	}
}

// TestEventStreamRevocationCheckFailsOpen is the fail-open half of
// GHSA-qrxm-qm54-xc44's fix on the event route: a transient failure of the
// revocation lookup itself — simulated here by closing the store out from
// under an open stream — must not tear the stream down. It survives at
// least one more heartbeat tick after the store is closed. Mirrors
// TestStreamRevocationCheckFailsOpen (logs_test.go).
func TestEventStreamRevocationCheckFailsOpen(t *testing.T) {
	// Not t.Parallel(): mutates the package-level eventHeartbeatInterval.

	// Arrange
	original := eventHeartbeatInterval
	eventHeartbeatInterval = 2 * time.Millisecond
	t.Cleanup(func() { eventHeartbeatInterval = original })

	fd := &fakeDocker{
		streamContainerEventsFn: func(ctx context.Context, onReady func(), _ func(dockerx.ContainerEvent) error) error {
			onReady()
			<-ctx.Done()
			return nil
		},
	}
	log, buf := newCapturingLoggerAtLevel(slog.LevelWarn)
	s, st := testServerWithDockerAndLogger(t, policy.ModeDefault, fd, log)
	serial := pairDeviceForRead(t, st)
	req := requestWithPeerSerial(http.MethodGet, "/v1/events/stream", nil, serial)
	ctx, cancel := context.WithCancel(req.Context())
	req = req.WithContext(ctx)
	rec := &syncBodyRecorder{deadlineAwareRecorder: &deadlineAwareRecorder{ResponseRecorder: httptest.NewRecorder()}}

	done := make(chan struct{})
	go func() {
		s.routes().ServeHTTP(rec, req)
		close(done)
	}()

	// Act — wait for the stream to actually open, close the store so every
	// subsequent revocation lookup fails, then wait for at least one more
	// heartbeat tick to land while the store stays closed. The stream is
	// then ended via an ordinary client disconnect (cancelling the request
	// context), not via the fake returning nil: the latter would route
	// through sub.closed and produce its own terminal frame, which is not
	// what this test is asserting the absence of.
	waitForCondition(t, 2*time.Second, 2*time.Millisecond,
		func() bool { return strings.Count(rec.bodySnapshot(), ": heartbeat") >= 1 },
		func() string { return fmt.Sprintf("body = %q, want at least 1 heartbeat comment", rec.bodySnapshot()) })
	countAtClose := strings.Count(rec.bodySnapshot(), ": heartbeat")
	if err := st.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	waitForCondition(t, 2*time.Second, 2*time.Millisecond,
		func() bool { return strings.Count(rec.bodySnapshot(), ": heartbeat") > countAtClose },
		func() string {
			return fmt.Sprintf("body = %q, want more heartbeat comments after the store closed (fail-open)", rec.bodySnapshot())
		})
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("stream did not finish after cancellation")
	}

	// Assert — the stream survived, produced no terminal frame despite the
	// closed store, and the transient lookup failure was logged at Warn
	// rather than silently swallowed.
	if got := strings.Count(rec.bodySnapshot(), "event: error"); got != 0 {
		t.Errorf("body has %d terminal error frames, want 0 (fail-open): %q", got, rec.bodySnapshot())
	}
	if !strings.Contains(buf.String(), "level=WARN") {
		t.Errorf("log = %q, want a WARN line for the failed revocation lookup", buf.String())
	}
}
