// SPDX-License-Identifier: AGPL-3.0-only

//go:build e2e

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"

	"github.com/scnplt/devmon-agent/internal/e2e/harness"
)

// This file replays the container health and lifecycle event stream
// (GET /v1/events/stream) against a real Engine: the snapshot-first
// guarantee, live health and lifecycle forwarding, the one-stream-per-device
// eviction rule (D11), and that this route never touches the log-stream
// budget (D11's own reasoning, restated in server.go).
//
// What this file deliberately does NOT cover: "the agent's own container
// appears in the snapshot" is the self-exclusion story, and like
// contract_lifecycle_test.go's own file comment already records for the
// mutate routes, that needs a containerised agent and lives in
// internal/e2e/incontainer. The host-binary group StartAgent starts here
// runs as a bare process, not a Docker container, so there is no "own
// container" for it to appear as. TestEventStreamSnapshotIncludesKnownContainer
// below proves the substance the plan's snapshot-first bullet is actually
// after — that the very first frame reflects a container that already
// existed before the stream opened, not an empty scaffold — using an
// ordinary fixture container instead.
//
// No test in this file runs t.Parallel(): every one of them opens at least
// one SSE stream, and the assertions below depend on ordering (which frame
// arrives first, which stream gets superseded) that a sibling test's own
// stream traffic on the same agent could perturb — the identical reasoning
// contract_logs_test.go's own file comment gives for the same choice.

// wireContainerState mirrors one entry of the event stream's opening
// snapshot (internal/dockerx/types.go's ContainerStateSummary), declared
// here rather than imported (D4): the suite must notice a renamed field,
// which it cannot do sharing a struct with the code that produces it.
type wireContainerState struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	State  string `json:"state"`
	Health string `json:"health"`
}

// wireContainerEvent mirrors one live container event
// (internal/dockerx/types.go's ContainerEvent), for the same D4 reason
// wireContainerState exists.
type wireContainerEvent struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Event  string `json:"event"`
	Health string `json:"health"`
	Time   string `json:"time"`
}

// wireEventStreamError mirrors the terminal event: error frame's payload
// (internal/httpapi/respond.go's errorBody), declared separately for the
// same D4 reason.
type wireEventStreamError struct {
	Error string `json:"error"`
}

// decodeContainerStates unmarshals one snapshot frame's data into the slice
// of states it carries, failing the test on a shape it cannot parse.
func decodeContainerStates(t *testing.T, data []byte) []wireContainerState {
	t.Helper()
	var states []wireContainerState
	if err := json.Unmarshal(data, &states); err != nil {
		t.Fatalf("decode snapshot frame data: %v; data = %s", err, data)
	}
	return states
}

// decodeContainerEvent unmarshals one live event frame's data, failing the
// test on a shape it cannot parse.
func decodeContainerEvent(t *testing.T, data []byte) wireContainerEvent {
	t.Helper()
	var ev wireContainerEvent
	if err := json.Unmarshal(data, &ev); err != nil {
		t.Fatalf("decode container event frame data: %v; data = %s", err, data)
	}
	return ev
}

// findContainerState returns the entry for id out of states, so a caller can
// assert on it rather than only on presence.
func findContainerState(states []wireContainerState, id string) (wireContainerState, bool) {
	for _, s := range states {
		if s.ID == id {
			return s, true
		}
	}
	return wireContainerState{}, false
}

// healthTransitionHealthcheck is the fixture Cmd every healthcheck-driven
// test in this file uses: a check that always fails, fast, so the Engine
// reports "unhealthy" well within a test's deadline. Interval, Timeout and
// Retries mirror TestContainerListReportsHealth's own fixture
// (contract_reads_test.go) so the two suites' health-flip timing cannot
// drift apart.
var healthTransitionHealthcheck = &container.HealthConfig{
	Test:        []string{"CMD-SHELL", "exit 1"},
	Interval:    500 * time.Millisecond,
	Timeout:     500 * time.Millisecond,
	Retries:     1,
	StartPeriod: 0,
}

// waitForContainerEvent reads frames off s until one carries a container
// event for id whose normalised action equals wantEvent, or fails the test
// after deadline. Every live container event this route serves is framed
// under the single SSE event name "health" (events.go's handleEventStream:
// sse.event("", sseEventHealth, ev) for every allowlisted action, not only
// health_status) — this helper asserts that framing rather than assuming it,
// so a change to it fails loudly here instead of silently making every
// assertion below vacuous.
func waitForContainerEvent(t *testing.T, s *harness.Stream, id, wantEvent string, deadline time.Duration) wireContainerEvent {
	t.Helper()

	timeout := time.After(deadline)
	for {
		select {
		case f, ok := <-s.Frames:
			if !ok {
				t.Fatalf("stream closed while waiting for %q event on container %s (Err = %v)", wantEvent, id, s.Err())
			}
			if f.Event != "health" {
				t.Fatalf("frame event = %q, want %q (every container event frame this route sends uses that one SSE event name)", f.Event, "health")
			}
			ev := decodeContainerEvent(t, f.Data)
			if ev.ID == id && ev.Event == wantEvent {
				return ev
			}
		case <-timeout:
			t.Fatalf("timed out waiting for a %q event on container %s within %s", wantEvent, id, deadline)
		}
	}
	// unreachable: the loop above only returns or fails the test.
}

// TestEventStreamSnapshotIncludesKnownContainer asserts the very first frame
// off a freshly opened event stream is event: snapshot, and that it already
// carries a container started before the stream opened — the reconciliation
// guarantee the snapshot exists for (D8): a client must never see a gap
// between "I connected" and "I know what already exists".
func TestEventStreamSnapshotIncludesKnownContainer(t *testing.T) {
	engine := harness.RequireEngine(t)
	_, d := readReadyAgent(t, "events-snapshot")

	id := harness.StartFixture(t, engine, harness.FixtureOptions{NameSuffix: "snapshot"})

	s := harness.OpenStream(t, d, "/v1/events/stream")
	defer s.Close(t)
	if s.Status != http.StatusOK {
		t.Fatalf("open event stream: status = %d, want %d; body = %s", s.Status, http.StatusOK, s.Body)
	}

	frames := collectFrames(t, s, 1, 10*time.Second)
	if frames[0].Event != "snapshot" {
		t.Fatalf("first frame event = %q, want %q", frames[0].Event, "snapshot")
	}

	states := decodeContainerStates(t, frames[0].Data)
	if _, ok := findContainerState(states, id); !ok {
		t.Errorf("snapshot does not contain container %s, which was already running when the stream opened", id)
	}
}

// TestEventStreamSnapshotReflectsUnhealthy asserts a container the Engine
// already reports unhealthy shows health: "unhealthy" in the very next
// snapshot.
//
// Requires Engine 29+ / API v1.52, mirroring the e2e-health skip the
// Makefile documents for TestContainerListReportsHealth (contract_reads_test.go):
// ContainerStateSummary.Health is built from the same ContainerList response
// as ContainerSummary.Health (internal/dockerx/events.go's
// toContainerStateSummary), and Health was only added to that response in API
// v1.52. Below that version the field is uniformly "none" no matter what the
// fixture container actually does, so the assertion cannot hold and this
// test skips rather than failing on an Engine that never claimed the
// capability.
func TestEventStreamSnapshotReflectsUnhealthy(t *testing.T) {
	engine := harness.RequireEngine(t)
	harness.RequireEngineAPIAtLeast(t, engine, "1.52")
	_, d := readReadyAgent(t, "events-snapshot-health")

	id := harness.StartFixture(t, engine, harness.FixtureOptions{
		NameSuffix:  "snapshot-health",
		Healthcheck: healthTransitionHealthcheck,
	})

	waitContainerUnhealthy(t, engine, id, 20*time.Second)

	s := harness.OpenStream(t, d, "/v1/events/stream")
	defer s.Close(t)
	if s.Status != http.StatusOK {
		t.Fatalf("open event stream: status = %d, want %d; body = %s", s.Status, http.StatusOK, s.Body)
	}

	frames := collectFrames(t, s, 1, 10*time.Second)
	if frames[0].Event != "snapshot" {
		t.Fatalf("first frame event = %q, want %q", frames[0].Event, "snapshot")
	}

	states := decodeContainerStates(t, frames[0].Data)
	state, ok := findContainerState(states, id)
	if !ok {
		t.Fatalf("snapshot does not contain container %s", id)
	}
	if state.Health != "unhealthy" {
		t.Errorf("snapshot health for container %s = %q, want %q", id, state.Health, "unhealthy")
	}
}

// TestEventStreamHealthEventArrives asserts a container whose healthcheck
// flips to unhealthy after the stream opens produces a live event carrying
// "health":"unhealthy" — the health_status forwarding
// internal/dockerx/events.go's toContainerEvent exists for. Unlike
// TestEventStreamSnapshotReflectsUnhealthy above, this needs no Engine
// version guard: health_status events come off the raw Engine event feed,
// not the v1.52-gated list response.
func TestEventStreamHealthEventArrives(t *testing.T) {
	engine := harness.RequireEngine(t)
	_, d := readReadyAgent(t, "events-health-live")

	id := harness.StartFixture(t, engine, harness.FixtureOptions{
		NameSuffix:  "health-live",
		Healthcheck: healthTransitionHealthcheck,
	})

	s := harness.OpenStream(t, d, "/v1/events/stream")
	defer s.Close(t)
	if s.Status != http.StatusOK {
		t.Fatalf("open event stream: status = %d, want %d; body = %s", s.Status, http.StatusOK, s.Body)
	}
	collectFrames(t, s, 1, 10*time.Second) // drain the opening snapshot

	ev := waitForContainerEvent(t, s, id, "health_status", 20*time.Second)
	if ev.Health != "unhealthy" {
		t.Errorf("health event for container %s: health = %q, want %q", id, ev.Health, "unhealthy")
	}
}

// TestEventStreamLifecycleEvents asserts stopping a running container
// produces both a "die" and a "stop" frame, in that order — the two
// lifecycle actions eventActions allowlists besides health_status
// (internal/dockerx/events.go).
func TestEventStreamLifecycleEvents(t *testing.T) {
	engine := harness.RequireEngine(t)
	_, d := readReadyAgent(t, "events-lifecycle")

	id := harness.StartFixture(t, engine, harness.FixtureOptions{NameSuffix: "lifecycle"})
	waitContainerRunning(t, engine, id)

	s := harness.OpenStream(t, d, "/v1/events/stream")
	defer s.Close(t)
	if s.Status != http.StatusOK {
		t.Fatalf("open event stream: status = %d, want %d; body = %s", s.Status, http.StatusOK, s.Body)
	}
	collectFrames(t, s, 1, 10*time.Second) // drain the opening snapshot

	status, _, raw := d.Do(t, http.MethodPost, "/v1/containers/"+id+"/stop", nil)
	if status != http.StatusNoContent {
		t.Fatalf("POST .../stop: status = %d, want %d; body = %s", status, http.StatusNoContent, raw)
	}

	die := waitForContainerEvent(t, s, id, "die", 20*time.Second)
	if die.ID != id {
		t.Errorf("die event id = %q, want %q", die.ID, id)
	}
	stop := waitForContainerEvent(t, s, id, "stop", 20*time.Second)
	if stop.ID != id {
		t.Errorf("stop event id = %q, want %q", stop.ID, id)
	}
}

// TestEventStreamSupersedesOnlySameDevice asserts D11's whole eviction
// contract in one test: a second stream from the same device evicts the
// first (which receives event: error carrying msgEventStreamSuperseded and
// then closes) and itself gets a fresh snapshot; a second, independently
// paired device's own stream is untouched by any of it throughout.
func TestEventStreamSupersedesOnlySameDevice(t *testing.T) {
	engine := harness.RequireEngine(t)
	a, deviceA := readReadyAgent(t, "events-superseded-a")
	deviceB := harness.PairDevice(t, a, "events-superseded-b")

	harness.StartFixture(t, engine, harness.FixtureOptions{NameSuffix: "superseded"})

	first := harness.OpenStream(t, deviceA, "/v1/events/stream")
	if first.Status != http.StatusOK {
		t.Fatalf("open first event stream (device A): status = %d, want %d; body = %s", first.Status, http.StatusOK, first.Body)
	}
	collectFrames(t, first, 1, 10*time.Second) // drain the opening snapshot

	other := harness.OpenStream(t, deviceB, "/v1/events/stream")
	defer other.Close(t)
	if other.Status != http.StatusOK {
		t.Fatalf("open event stream (device B): status = %d, want %d; body = %s", other.Status, http.StatusOK, other.Body)
	}
	otherFrames := collectFrames(t, other, 1, 10*time.Second)
	if otherFrames[0].Event != "snapshot" {
		t.Fatalf("device B's first frame event = %q, want %q", otherFrames[0].Event, "snapshot")
	}

	second := harness.OpenStream(t, deviceA, "/v1/events/stream")
	defer second.Close(t)
	if second.Status != http.StatusOK {
		t.Fatalf("open second event stream (device A): status = %d, want %d; body = %s", second.Status, http.StatusOK, second.Body)
	}

	// The evicted first stream must receive exactly one terminal error frame
	// carrying the "do not retry" message, then close on its own.
	termFrames := collectFrames(t, first, 1, 10*time.Second)
	if termFrames[0].Event != "error" {
		t.Fatalf("first stream's terminal frame event = %q, want %q", termFrames[0].Event, "error")
	}
	var termBody wireEventStreamError
	if err := json.Unmarshal(termFrames[0].Data, &termBody); err != nil {
		t.Fatalf("decode first stream's terminal frame data: %v; data = %s", err, termFrames[0].Data)
	}
	const wantSupersededMsg = "event stream superseded"
	if termBody.Error != wantSupersededMsg {
		t.Errorf("first stream's terminal error = %q, want %q", termBody.Error, wantSupersededMsg)
	}
	first.WaitClosed(t, 10*time.Second)

	// The newer, second stream gets a fresh snapshot of its own.
	secondFrames := collectFrames(t, second, 1, 10*time.Second)
	if secondFrames[0].Event != "snapshot" {
		t.Errorf("second stream's first frame event = %q, want %q", secondFrames[0].Event, "snapshot")
	}

	// Device B's independent stream must be untouched by any of the above.
	if err := other.Err(); err != nil {
		t.Errorf("device B's stream ended (Err = %v) while only device A's streams were superseded", err)
	}
}

// TestEventStreamDoesNotConsumeLogStreamBudget asserts opening an event
// stream first still leaves a device able to open its full
// maxStreamsPerDevice worth of /logs/stream connections — the property D11
// and server.go's own route comment both promise: an event stream is
// registered through the `read` guard chain, never streamBudget, so it costs
// this device nothing against the log-stream ceiling.
func TestEventStreamDoesNotConsumeLogStreamBudget(t *testing.T) {
	engine := harness.RequireEngine(t)
	_, d := readReadyAgent(t, "events-log-budget")

	id := harness.StartFixture(t, engine, harness.FixtureOptions{
		NameSuffix: "log-budget",
		Cmd:        logLinesCmd,
	})

	ev := harness.OpenStream(t, d, "/v1/events/stream")
	defer ev.Close(t)
	if ev.Status != http.StatusOK {
		t.Fatalf("open event stream: status = %d, want %d; body = %s", ev.Status, http.StatusOK, ev.Body)
	}
	collectFrames(t, ev, 1, 10*time.Second) // drain the opening snapshot

	// maxStreamsPerDevice duplicates internal/httpapi/server.go's own
	// constant (D4, mirroring contract_logs_test.go's own duplication of it).
	const maxStreamsPerDevice = 3

	logStreams := make([]*harness.Stream, 0, maxStreamsPerDevice)
	defer func() {
		for _, s := range logStreams {
			s.Close(t)
		}
	}()

	for i := 0; i < maxStreamsPerDevice; i++ {
		s := harness.OpenStream(t, d, "/v1/containers/"+id+"/logs/stream")
		if s.Status != http.StatusOK {
			t.Fatalf("log stream %d/%d after opening an event stream: status = %d, want %d; body = %s",
				i+1, maxStreamsPerDevice, s.Status, http.StatusOK, s.Body)
		}
		logStreams = append(logStreams, s)
	}
}

// waitContainerUnhealthy polls the Engine (never the agent) until id reports
// health "unhealthy", bounded by a deadline rather than a fixed sleep —
// mirrors waitContainerRunning's own reasoning in contract_lifecycle_test.go.
func waitContainerUnhealthy(t *testing.T, engine *client.Client, id string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last string
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		res, err := engine.ContainerInspect(ctx, id, client.ContainerInspectOptions{})
		cancel()
		if err == nil && res.Container.State != nil && res.Container.State.Health != nil {
			last = string(res.Container.State.Health.Status)
			if last == string(container.Unhealthy) {
				return
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("container %s did not reach health %q within %s (last observed = %q)", id, container.Unhealthy, timeout, last)
}
