//go:build e2e

package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/scnplt/devmon-agent/internal/e2e/harness"
)

// This file replays Phase 4's outstanding half — the two items
// logs-and-live-streaming-report.md:178-190 left unverified against a real
// host: the 30-minute endurance run (owned by Task 12's
// contract_endurance_test.go, gated by DEVMON_E2E_ENDURANCE per D14 — no
// long-running test belongs here) and the Wi-Fi <-> mobile-data handover's
// agent-side half. That half is two properties an app's reconnect logic
// depends on and can be asserted without a radio: the stream tears down
// cleanly after an RST (D15), and ?since=<last id> resumes with at most one
// repeated line, never a gap. The client half — actually performing a
// network handover — is named as belonging to the Android app's own suite
// (the phase plan's Coverage Map).
//
// It also covers the historical log route's bounds (?tail=, ?since=) and the
// stream route's keepalive and slot-exhaustion behaviour, none of which had
// an owner outside this file.
//
// What this file deliberately does NOT assert: logs-and-live-streaming-report.md
// records that an abandoned stream logs at ERROR in agent.log. That is a
// known, recorded, unfixed observation (D19 — no production code changes in
// this phase); asserting its absence would make the eventual fix look like a
// regression, so it is left alone.
//
// Every test in this file requires a real Docker Engine (harness.RequireEngine)
// and was not executed in the environment this file was written in — no
// Engine was reachable from that shell (D6). What compiled and what a table
// test could prove without one (stream.go's SSE parser) was proved
// separately; the round trips below are unexercised until run against a real
// host, and that is stated here rather than left implicit.

// logLinesCmd is the fixture Cmd every test in this file that needs live,
// ongoing output uses: numbered lines on a short, steady interval, forever —
// so one fixture container can serve a historical ?tail= request, a live
// stream, and a resume test without racing its own startup.
var logLinesCmd = []string{"sh", "-c", `i=0; while true; do echo "line $i"; i=$((i+1)); sleep 0.2; done`}

// wireLogLine mirrors one historical or streamed log line's JSON shape
// (internal/dockerx/types.go's LogLine), declared here rather than imported
// (D4): the suite must notice a renamed field, which it cannot do sharing a
// struct with the code that produces it.
type wireLogLine struct {
	Timestamp string `json:"ts"`
	Stream    string `json:"stream"`
	Line      string `json:"line"`
}

// decodeLogLine unmarshals one SSE frame's data into wireLogLine, failing the
// test on a shape it cannot parse — a parse failure here means the frame
// contract itself broke, not that this test's assertion was wrong.
func decodeLogLine(t *testing.T, data []byte) wireLogLine {
	t.Helper()
	var line wireLogLine
	if err := json.Unmarshal(data, &line); err != nil {
		t.Fatalf("decode log line frame data: %v; data = %s", err, data)
	}
	return line
}

// lineIndex parses the numeric suffix off a logLinesCmd line ("line 41" ->
// 41), so TestStreamResumeRepeatsAtMostOneLine can tell "repeated" from
// "skipped ahead" instead of only comparing opaque strings.
func lineIndex(s string) (int, error) {
	var n int
	if _, err := fmt.Sscanf(s, "line %d", &n); err != nil {
		return 0, fmt.Errorf("parse line index from %q: %w", s, err)
	}
	return n, nil
}

// collectFrames reads exactly n real event frames off s.Frames, failing the
// test if the stream closes early or the deadline elapses first. A closed
// channel before n frames arrived is itself the failure — it means the
// server ended the stream (or the connection broke) before the fixture
// produced enough output, which every caller in this file needs to be able
// to tell apart from a slow-but-healthy stream.
func collectFrames(t *testing.T, s *harness.Stream, n int, deadline time.Duration) []harness.SSEFrame {
	t.Helper()
	frames := make([]harness.SSEFrame, 0, n)
	timeout := time.After(deadline)
	for len(frames) < n {
		select {
		case f, ok := <-s.Frames:
			if !ok {
				t.Fatalf("stream closed after %d of %d expected frames (Err = %v)", len(frames), n, s.Err())
			}
			frames = append(frames, f)
		case <-timeout:
			t.Fatalf("timed out waiting for %d frames; got %d within %s", n, len(frames), deadline)
		}
	}
	return frames
}

// TestHistoricalLogsBounded asserts ?tail=20 against a fixture producing far
// more than 20 lines returns exactly 20 items, truncated: false, and every
// item carries the documented ts/stream/line keys (Timestamp extracted into
// its own field, as the phase plan's Coverage Map requires).
func TestHistoricalLogsBounded(t *testing.T) {
	t.Parallel()
	engine := harness.RequireEngine(t)
	_, d := readReadyAgent(t, "logs-historical-bounded")

	id := harness.StartFixture(t, engine, harness.FixtureOptions{
		NameSuffix: "historical-bounded",
		Cmd:        logLinesCmd,
	})

	const wantTail = 20
	obj := waitForLogCount(t, d, fmt.Sprintf("/v1/containers/%s/logs?tail=%d", id, wantTail), wantTail, 15*time.Second)

	harness.AssertExactKeys(t, obj, []string{"items", "truncated"})
	if obj["truncated"] != false {
		t.Errorf("historical logs truncated = %v, want false", obj["truncated"])
	}

	items := asArray(t, obj["items"])
	if len(items) != wantTail {
		t.Fatalf("len(items) = %d, want exactly %d (?tail=%d)", len(items), wantTail, wantTail)
	}
	for i, raw := range items {
		item := asObject(t, raw)
		assertKeySet(t, item, []string{"ts", "stream", "line"}, []string{"truncated"})
		if item["ts"] == "" {
			t.Errorf("item %d: ts is empty, want an RFC3339Nano timestamp", i)
		}
		if item["stream"] != "stdout" && item["stream"] != "stderr" {
			t.Errorf("item %d: stream = %v, want \"stdout\" or \"stderr\"", i, item["stream"])
		}
	}
}

// TestHistoricalLogsInvalidSince asserts ?since=nonsense answers 400 with the
// documented error, and a well-formed ?since= in the future answers 200 with
// an empty items array — since is never defaulted on a parse failure
// (logs.go's sinceParam), unlike tail.
func TestHistoricalLogsInvalidSince(t *testing.T) {
	t.Parallel()
	engine := harness.RequireEngine(t)
	_, d := readReadyAgent(t, "logs-invalid-since")

	id := harness.StartFixture(t, engine, harness.FixtureOptions{
		NameSuffix: "invalid-since",
		Cmd:        logLinesCmd,
	})

	status, obj := d.JSON(t, http.MethodGet, "/v1/containers/"+id+"/logs?since=nonsense")
	if status != http.StatusBadRequest {
		t.Errorf("GET .../logs?since=nonsense status = %d, want %d", status, http.StatusBadRequest)
	}
	harness.AssertExactKeys(t, obj, []string{"error"})
	if obj["error"] != "invalid since timestamp" {
		t.Errorf(`error = %v, want "invalid since timestamp"`, obj["error"])
	}

	future := url.QueryEscape(time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano))
	status2, obj2 := d.JSON(t, http.MethodGet, "/v1/containers/"+id+"/logs?since="+future)
	if status2 != http.StatusOK {
		t.Fatalf("GET .../logs?since=<future> status = %d, want %d", status2, http.StatusOK)
	}
	harness.AssertExactKeys(t, obj2, []string{"items", "truncated"})
	items := asArray(t, obj2["items"])
	if len(items) != 0 {
		t.Errorf("GET .../logs?since=<future> returned %d items, want 0", len(items))
	}
}

// TestStreamDeliversLinesLive asserts lines the fixture writes after the
// stream opens arrive within a short deadline, framed exactly as sse.go
// documents: the frame's id equals the line's own ts field.
func TestStreamDeliversLinesLive(t *testing.T) {
	t.Parallel()
	engine := harness.RequireEngine(t)
	_, d := readReadyAgent(t, "logs-stream-live")

	id := harness.StartFixture(t, engine, harness.FixtureOptions{
		NameSuffix: "stream-live",
		Cmd:        logLinesCmd,
	})

	s := harness.OpenStream(t, d, "/v1/containers/"+id+"/logs/stream")
	defer s.Close(t)
	if s.Status != http.StatusOK {
		t.Fatalf("open stream: status = %d, want %d; body = %s", s.Status, http.StatusOK, s.Body)
	}

	frames := collectFrames(t, s, 3, 10*time.Second)
	for i, f := range frames {
		line := decodeLogLine(t, f.Data)
		if line.Stream != "stdout" {
			t.Errorf("frame %d: data.stream = %q, want %q", i, line.Stream, "stdout")
		}
		if f.ID == "" {
			t.Errorf("frame %d: id is empty, want the line's RFC3339Nano timestamp", i)
		}
		if f.ID != line.Timestamp {
			t.Errorf("frame %d: id = %q, data.ts = %q; the SSE id must equal the line's own timestamp field (logs.go)", i, f.ID, line.Timestamp)
		}
	}
}

// TestStreamKeepaliveOnSilentContainer asserts a container that writes
// nothing keeps its stream open past the server's 30s WriteTimeout by
// receiving keepalive frames on the documented 20s interval
// (internal/httpapi/sse.go's keepaliveInterval) — this test waits out the
// real production interval rather than a shortened one, since the harness
// has no way to override an unexported package variable in a different
// process.
func TestStreamKeepaliveOnSilentContainer(t *testing.T) {
	t.Parallel()
	engine := harness.RequireEngine(t)
	_, d := readReadyAgent(t, "logs-stream-keepalive")

	id := harness.StartFixture(t, engine, harness.FixtureOptions{NameSuffix: "silent"})

	s := harness.OpenStream(t, d, "/v1/containers/"+id+"/logs/stream")
	defer s.Close(t)
	if s.Status != http.StatusOK {
		t.Fatalf("open stream on a silent container: status = %d, want %d; body = %s", s.Status, http.StatusOK, s.Body)
	}

	deadline := time.Now().Add(35 * time.Second)
	for time.Now().Before(deadline) {
		if s.Keepalives() > 0 {
			return
		}
		select {
		case f, ok := <-s.Frames:
			if ok {
				t.Fatalf("unexpected log frame from a silent container: %+v", f)
			}
		case <-time.After(time.Second):
		}
	}
	t.Fatalf("no keepalive observed within 35s of a silent container's stream (server interval is 20s, WriteTimeout is 30s); Err = %v", s.Err())
}

// TestStreamSurvivesAbruptConnectionLoss is D15's primitive in use: an RST
// mid-stream (not the orderly FIN Phase 4 already verified) must not take
// down the agent, and the stream slot it held must be released — proven by
// repeating ten times and confirming an eleventh stream still succeeds. A
// slot leaked on every abrupt loss would exhaust maxConcurrentStreams (8)
// well before the tenth repetition and this test would fail loudly rather
// than silently, which is what makes it falsifiable: removing the deferred
// slot release in handleStreamContainerLogs (a hypothetical regression, not
// touched here per D19) would turn this red.
func TestStreamSurvivesAbruptConnectionLoss(t *testing.T) {
	t.Parallel()
	engine := harness.RequireEngine(t)
	_, d := readReadyAgent(t, "logs-stream-abrupt-loss")

	id := harness.StartFixture(t, engine, harness.FixtureOptions{
		NameSuffix: "abrupt-loss",
		Cmd:        logLinesCmd,
	})

	const repetitions = 10
	for i := 0; i < repetitions; i++ {
		s := harness.OpenStream(t, d, "/v1/containers/"+id+"/logs/stream")
		if s.Status != http.StatusOK {
			t.Fatalf("iteration %d/%d: open stream status = %d, want %d", i+1, repetitions, s.Status, http.StatusOK)
		}
		collectFrames(t, s, 1, 10*time.Second)

		s.AbruptClose(t)
		s.WaitClosed(t, 10*time.Second)
		if s.Err() == nil {
			t.Errorf("iteration %d/%d: Stream.Err() = nil after AbruptClose, want a connection-reset-shaped error", i+1, repetitions)
		}
	}

	// The agent process itself must still be answering — not just the TCP
	// port, which a hung process would still hold open.
	if status, _, raw := d.Do(t, http.MethodGet, "/v1/status", nil); status != http.StatusOK {
		t.Fatalf("GET /v1/status after %d abrupt stream losses: status = %d, want %d; body = %s", repetitions, status, http.StatusOK, raw)
	}

	// Every one of the ten slots must have been released: an eleventh stream
	// still succeeds.
	final := harness.OpenStream(t, d, "/v1/containers/"+id+"/logs/stream")
	defer final.Close(t)
	if final.Status != http.StatusOK {
		t.Fatalf("stream after %d abrupt losses: status = %d, want %d (a non-200 here would mean a leaked slot)", repetitions, final.Status, http.StatusOK)
	}
}

// TestStreamResumeRepeatsAtMostOneLine is the agent-side half of the Wi-Fi
// <-> mobile-data handover item (Coverage Map): a client that reconnects
// with ?since=<last id> must see the boundary line repeat AT MOST once —
// this is a documented at-least-once contract, not exactly-once, so
// asserting zero repeats would fail against correct behaviour — and nothing
// between the boundary and the reconnect may be missing.
//
// Falsifiability: this assertion is deliberately NOT "no repeats". Omitting
// the ?since= parameter entirely (reconnecting plain) would make the second
// stream restart from the server's default backlog (defaultStreamTail=100
// lines back) and repeat many lines, not at most one — turning the
// at-most-one assertion below red. That inversion was not run in this
// environment (no Engine was reachable, D6); it is recorded here as the
// falsification a future run against a real host should perform once before
// trusting this test.
func TestStreamResumeRepeatsAtMostOneLine(t *testing.T) {
	t.Parallel()
	engine := harness.RequireEngine(t)
	_, d := readReadyAgent(t, "logs-stream-resume")

	id := harness.StartFixture(t, engine, harness.FixtureOptions{
		NameSuffix: "resume",
		Cmd:        logLinesCmd,
	})

	first := harness.OpenStream(t, d, "/v1/containers/"+id+"/logs/stream")
	if first.Status != http.StatusOK {
		t.Fatalf("open first stream: status = %d, want %d; body = %s", first.Status, http.StatusOK, first.Body)
	}
	firstFrames := collectFrames(t, first, 5, 10*time.Second)
	first.Close(t)
	first.WaitClosed(t, 5*time.Second)

	boundary := firstFrames[len(firstFrames)-1]
	boundaryLine := decodeLogLine(t, boundary.Data)
	boundaryIdx, err := lineIndex(boundaryLine.Line)
	if err != nil {
		t.Fatalf("parse boundary line index: %v", err)
	}

	second := harness.OpenStream(t, d, "/v1/containers/"+id+"/logs/stream?since="+url.QueryEscape(boundary.ID))
	defer second.Close(t)
	if second.Status != http.StatusOK {
		t.Fatalf("open resumed stream: status = %d, want %d; body = %s", second.Status, http.StatusOK, second.Body)
	}
	resumedFrames := collectFrames(t, second, 3, 10*time.Second)

	resumedLine0 := decodeLogLine(t, resumedFrames[0].Data)
	repeatedOnce := resumedFrames[0].ID == boundary.ID && resumedLine0.Line == boundaryLine.Line

	if repeatedOnce {
		if len(resumedFrames) > 1 {
			resumedLine1 := decodeLogLine(t, resumedFrames[1].Data)
			if resumedLine1.Line == boundaryLine.Line {
				t.Errorf("boundary line %q repeated twice after resume (frames 0 and 1), want at most once", boundaryLine.Line)
			}
		}
		return
	}

	// Did not repeat: it must not have skipped ahead of the boundary either.
	resumedIdx, err := lineIndex(resumedLine0.Line)
	if err != nil {
		t.Fatalf("parse resumed line index: %v", err)
	}
	if resumedIdx > boundaryIdx+1 {
		t.Errorf("resume skipped lines: boundary was %q, first line after resume was %q (gap of %d)",
			boundaryLine.Line, resumedLine0.Line, resumedIdx-boundaryIdx-1)
	}
}

// TestStreamSlotExhaustion asserts the ninth concurrent stream against the
// same agent answers 503 with the documented body, and that closing one of
// the eight frees a slot a subsequent stream can use. It does not run
// t.Parallel(): it deliberately exhausts a resource (maxConcurrentStreams,
// internal/httpapi/server.go) shared by every stream this agent instance
// serves, and a sibling test's own stream would make the count
// non-deterministic — exactly the ordering bug this suite exists to catch,
// not to introduce (mirrors TestReadsAnswer502WhenEngineIsGone's same
// reasoning for its own single-owner proxy).
func TestStreamSlotExhaustion(t *testing.T) {
	engine := harness.RequireEngine(t)
	_, d := readReadyAgent(t, "logs-stream-slots")

	id := harness.StartFixture(t, engine, harness.FixtureOptions{
		NameSuffix: "slots",
		Cmd:        logLinesCmd,
	})

	// maxConcurrentStreams duplicates internal/httpapi/server.go's own
	// constant (D4): the suite must notice a change to the server's limit,
	// which it cannot do importing the value it is supposed to be checking.
	const maxConcurrentStreams = 8

	streams := make([]*harness.Stream, 0, maxConcurrentStreams)
	defer func() {
		for _, s := range streams {
			s.Close(t)
		}
	}()

	for i := 0; i < maxConcurrentStreams; i++ {
		s := harness.OpenStream(t, d, "/v1/containers/"+id+"/logs/stream")
		if s.Status != http.StatusOK {
			t.Fatalf("stream %d/%d: status = %d, want %d; body = %s", i+1, maxConcurrentStreams, s.Status, http.StatusOK, s.Body)
		}
		streams = append(streams, s)
	}

	ninth := harness.OpenStream(t, d, "/v1/containers/"+id+"/logs/stream")
	if ninth.Status != http.StatusServiceUnavailable {
		t.Fatalf("ninth concurrent stream: status = %d, want %d; body = %s", ninth.Status, http.StatusServiceUnavailable, ninth.Body)
	}
	var obj map[string]any
	if err := json.Unmarshal(ninth.Body, &obj); err != nil {
		t.Fatalf("decode ninth stream's rejection body: %v; body = %s", err, ninth.Body)
	}
	if obj["error"] != "too many concurrent log streams" {
		t.Errorf(`ninth stream error = %v, want "too many concurrent log streams"`, obj["error"])
	}

	// Free one slot and confirm it becomes usable again. The release happens
	// when the server's own write to the closed connection fails, which is
	// not instantaneous, so this polls rather than asserting immediately.
	streams[0].Close(t)
	streams[0].WaitClosed(t, 10*time.Second)
	streams = streams[1:]

	deadline := time.Now().Add(15 * time.Second)
	var freed *harness.Stream
	for time.Now().Before(deadline) {
		freed = harness.OpenStream(t, d, "/v1/containers/"+id+"/logs/stream")
		if freed.Status == http.StatusOK {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	if freed == nil || freed.Status != http.StatusOK {
		t.Fatalf("a new stream still failed within 15s of freeing one of %d slots; want the freed slot reused", maxConcurrentStreams)
	}
	streams = append(streams, freed)
}

// TestStreamAgainstUnknownContainer asserts a stream request against a
// well-formed but nonexistent container ID answers 404 before any SSE
// header is committed — the response's Content-Type must never be
// text/event-stream on this path, which is the client-observable proof that
// sse.go's lazy header commit (start on first event, not on handler entry)
// held for the one status the stream route can still fail on before writing
// anything.
func TestStreamAgainstUnknownContainer(t *testing.T) {
	t.Parallel()
	harness.RequireEngine(t)
	_, d := readReadyAgent(t, "logs-stream-unknown")

	const unknown = "devmone2enosuchcontainer00000000000000000000000000000000000000"
	s := harness.OpenStream(t, d, "/v1/containers/"+unknown+"/logs/stream")
	if s.Status != http.StatusNotFound {
		t.Fatalf("stream against an unknown container: status = %d, want %d; body = %s", s.Status, http.StatusNotFound, s.Body)
	}
	if ct := s.Header.Get("Content-Type"); strings.HasPrefix(ct, "text/event-stream") {
		t.Errorf("Content-Type = %q on a 404, want the SSE headers never to have been committed", ct)
	}

	var obj map[string]any
	if err := json.Unmarshal(s.Body, &obj); err != nil {
		t.Fatalf("decode 404 body: %v; body = %s", err, s.Body)
	}
	harness.AssertExactKeys(t, obj, []string{"error"})
	if obj["error"] != "not found" {
		t.Errorf(`error = %v, want "not found"`, obj["error"])
	}
}

// waitForLogCount polls the historical logs route until it returns at least
// want items or timeout elapses, so a fresh fixture's log accumulation is
// bounded by a deadline rather than a fixed, possibly-flaky sleep — mirrors
// TestContainerListReportsHealth's own polling reasoning in
// contract_reads_test.go.
func waitForLogCount(t *testing.T, d *harness.Device, path string, want int, timeout time.Duration) map[string]any {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last map[string]any
	for time.Now().Before(deadline) {
		status, obj := d.JSON(t, http.MethodGet, path)
		if status != http.StatusOK {
			t.Fatalf("GET %s status = %d, want %d", path, status, http.StatusOK)
		}
		last = obj
		if items, ok := obj["items"].([]any); ok && len(items) >= want {
			return obj
		}
		time.Sleep(300 * time.Millisecond)
	}
	t.Fatalf("GET %s never returned >= %d items within %s; last response = %v", path, want, timeout, last)
	return nil
}
