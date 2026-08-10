//go:build e2e

package api

import (
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/scnplt/devmon-agent/internal/e2e/harness"
)

// These two tests are compiled on every e2e run and skipped unless
// DEVMON_E2E_ENDURANCE=1 (D14). A separate build tag would keep them out of
// every ordinary compile, and code that compiles only in a target nobody runs
// weekly is code that rots. `make e2e-endurance` sets the variable and the
// longer -timeout (45m) both tests need room inside.
//
// TestStreamEnduranceThirtyMinutes closes the PRD's Phase 4 success signal:
// logs-and-live-streaming-report.md:178-190 records that only a 70-second run
// was ever performed against a real host, proving the write-deadline fix but
// not the 30-minute claim itself. TestLogRetentionBoundsDiskUse closes the
// PRD's Phase 1 retention signal: that the agent's own log volume stays
// bounded by DEVMON_LOG_MAX_TOTAL_MB rather than growing without limit, and
// that this bound is enforced with no client-facing way to change it — every
// knob here is a DEVMON_* startup variable read once by the agent process,
// never a route this suite's mTLS client can reach.
//
// Neither test was executed in the environment this file was written in: no
// Docker Engine was reachable from that shell (D6), and a 30-minute assertion
// cannot be exercised in this session regardless. What compiled and what a
// pure-function check could prove without a live agent (the skip gate itself,
// below) was proved separately; the round trips in both tests are
// unexercised until run with DEVMON_E2E_ENDURANCE=1 against a real host.

// enduranceEnvVar is named in every skip message so a reader of `go test`
// output learns how to un-skip these two tests without reading this file
// (D14's gotcha: a skip nobody knows how to lift is a deleted test with extra
// steps).
const enduranceEnvVar = "DEVMON_E2E_ENDURANCE"

// requireEndurance skips the calling test unless DEVMON_E2E_ENDURANCE=1,
// naming both the variable and the make target that sets it.
func requireEndurance(t *testing.T) {
	t.Helper()
	if os.Getenv(enduranceEnvVar) != "1" {
		t.Skipf("skipping long-running test: set %s=1 to run it (or `make e2e-endurance`, which also raises -timeout to 45m)", enduranceEnvVar)
	}
}

// enduranceLogLinesCmd writes one numbered line per second, forever — the
// PRD's 30-minute stream needs a fixture that keeps producing for the whole
// window without racing its own startup the way logLinesCmd's 0.2s interval
// (contract_logs_test.go) would at this scale.
var enduranceLogLinesCmd = []string{"sh", "-c", `i=0; while true; do echo "line $i"; i=$((i+1)); sleep 1; done`}

// TestStreamEnduranceThirtyMinutes holds a single live log stream open for 30
// minutes against a fixture writing one line per second, and asserts every
// line arrived, in order, with no gap and no reconnect — the PRD's Phase 4
// success signal that logs-and-live-streaming-report.md left unticked.
//
// The sequence is tracked by the fixture's own printed counter (decoded out
// of each frame's data.line field), never by wall-clock arithmetic: a
// 30-minute assertion built on timing would be flaky forever (Task 12's
// gotcha). The first frame received seeds the expected counter rather than
// assuming it starts at 0, because the stream route replays a short backlog
// (defaultStreamTail, documented in contract_logs_test.go) and the fixture
// may have already written one or two lines before OpenStream's request
// lands.
//
// Falsifiability: the sequence check compares got to next and fails on any
// mismatch, not only a decrease — a fixture that skipped a number (a real
// gap) or repeated one (a duplicate delivery) both turn this red. Not
// executed in this session (D6, no reachable Engine); a future run against a
// real host should confirm this once by pointing the assertion at a stream
// deliberately interrupted mid-run (e.g. killing and restarting the fixture)
// and observing the test fail with a sequence-break message, then restoring
// the real fixture.
func TestStreamEnduranceThirtyMinutes(t *testing.T) {
	requireEndurance(t)
	engine := harness.RequireEngine(t)
	_, d := readReadyAgent(t, "endurance-stream")

	id := harness.StartFixture(t, engine, harness.FixtureOptions{
		NameSuffix: "endurance",
		Cmd:        enduranceLogLinesCmd,
	})

	s := harness.OpenStream(t, d, "/v1/containers/"+id+"/logs/stream")
	defer s.Close(t)
	if s.Status != http.StatusOK {
		t.Fatalf("open endurance stream: status = %d, want %d; body = %s", s.Status, http.StatusOK, s.Body)
	}

	const (
		window = 30 * time.Minute
		// wantLines is a floor below the nominal 1800 (one line/second for 30
		// minutes): the fixture may have written a line or two before the
		// stream opened, and the loop below stops at the wall-clock deadline
		// rather than waiting for one more line that has not arrived yet.
		// Neither is a data-loss signal; both are startup and boundary slack,
		// not a gap in the sequence itself, which the per-line check below
		// verifies separately and strictly.
		wantLines = 1750
		idlePoll  = 5 * time.Second
	)

	deadline := time.Now().Add(window)
	var (
		count    int
		next     int
		haveNext bool
	)
	for time.Now().Before(deadline) {
		select {
		case f, ok := <-s.Frames:
			if !ok {
				t.Fatalf("stream closed after %d lines and before the %s endurance window elapsed (Err = %v) — a live stream must survive the whole window with no reconnect", count, window, s.Err())
			}
			line := decodeLogLine(t, f.Data)
			got, err := lineIndex(line.Line)
			if err != nil {
				t.Fatalf("parse endurance line index after %d lines: %v", count, err)
			}
			if !haveNext {
				next = got
				haveNext = true
			}
			if got != next {
				t.Fatalf("line sequence broke after %d lines: got index %d, want %d (a gap or a repeat, neither permitted over a held-open stream)", count, got, next)
			}
			next++
			count++
		case <-time.After(idlePoll):
			// No line arrived this tick; the fixture may be a little behind
			// its own schedule. Keep waiting up to the overall deadline
			// rather than failing on a single slow tick.
		}
	}

	if err := s.Err(); err != nil {
		t.Fatalf("stream reported an error before the %s window elapsed: %v (%d lines received)", window, err, count)
	}
	if count < wantLines {
		t.Fatalf("received %d lines over %s, want at least %d (one per second, with headroom for startup and boundary slack) — a gap this large means the connection was not actually held open", count, window, wantLines)
	}

	// The read loop never returned (s.Err() == nil, checked above), which
	// means this exact TCP connection was never torn down and reopened. A
	// restarted agent process cannot inherit an existing connection, so the
	// stream staying open for the entire window IS the observable proof that
	// the agent's restart count stayed at 0 — the harness exposes no separate
	// counter to read, and the connection's own continuity is the signal.
	if status, _, raw := d.Do(t, http.MethodGet, "/v1/status", nil); status != http.StatusOK {
		t.Fatalf("GET /v1/status after the %s stream: status = %d, want %d; body = %s", window, status, http.StatusOK, raw)
	}
}

// retentionMaxBackups mirrors internal/logging's unexported maxBackups
// constant (D4-style duplication, the same reasoning contract_logs_test.go's
// maxConcurrentStreams applies to a server-side limit): the suite must be
// able to notice a change to how many rotated files are kept, which it
// cannot do by importing the value it exists to check.
const retentionMaxBackups = 3

// retentionBudgetMB is DEVMON_LOG_MAX_TOTAL_MB for this test's agent — the
// floor internal/config enforces (minLogMaxTotalMB), chosen so a rotation is
// reachable within a few minutes of driven traffic rather than needing a
// realistic production budget.
const retentionBudgetMB = 8

// TestLogRetentionBoundsDiskUse drives enough authenticated traffic at
// DEVMON_LOG_LEVEL=debug to force the agent's own log file to rotate more
// than once, then asserts the logs directory stays within a generous
// headroom of its configured budget and that compressed backups exist —
// the PRD's Phase 1 retention success signal, and proof that retention is
// enforced with no client-facing knob: DEVMON_LOG_MAX_TOTAL_MB is read once
// at agent startup (internal/config), and nothing this suite's mTLS client
// can reach changes it afterward.
//
// Falsifiability: not executed in this session (D6, no reachable Engine). A
// future run against a real host should confirm this once by setting
// DEVMON_LOG_MAX_TOTAL_MB far above what driveTimeout's traffic could ever
// produce (so no rotation fires) and observing the wantBackups assertion
// fail — proving the check is not vacuously true — then restoring the real
// 8MB budget.
func TestLogRetentionBoundsDiskUse(t *testing.T) {
	requireEndurance(t)
	harness.RequireEngine(t)

	a := harness.StartAgent(t, harness.AgentOptions{
		Env: map[string]string{
			"DEVMON_LOG_MAX_TOTAL_MB": strconv.Itoa(retentionBudgetMB),
			"DEVMON_LOG_LEVEL":        "debug",
		},
	})
	d := harness.PairDevice(t, a, "retention")

	const (
		wantBackups     = 2
		driveTimeout    = 5 * time.Minute
		driveWorkers    = 24
		pollInterval    = 2 * time.Second
		perRequestLimit = 5 * time.Second
	)

	// A plain (no client certificate) TLS client pinned to the agent's own
	// CA — obtained via the paired device rather than InsecureSkipVerify,
	// which is permitted nowhere in this suite outside harness's own
	// pairing bootstrap and readiness probe (D7). /v1/status answers with no
	// client certificate at all (contract_status_test.go's
	// TestStatusAllowlist), so this is a legitimate, cheap way to generate
	// volume in the agent's own "request served" debug line without
	// touching the Docker Engine on every call.
	trafficTLS := d.TLSConfig()
	trafficTLS.Certificates = nil
	trafficClient := &http.Client{
		Timeout:   perRequestLimit,
		Transport: &http.Transport{TLSClientConfig: trafficTLS},
	}
	statusURL := a.BaseURL + "/v1/status"

	stop := make(chan struct{})
	var requests int64
	var wg sync.WaitGroup
	for i := 0; i < driveWorkers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			driveStatusTraffic(stop, trafficClient, statusURL, &requests)
		}()
	}

	deadline := time.Now().Add(driveTimeout)
	var backups int
	for time.Now().Before(deadline) {
		backups = countGzBackups(t, a)
		if backups >= wantBackups {
			break
		}
		time.Sleep(pollInterval)
	}
	close(stop)
	wg.Wait()

	if atomic.LoadInt64(&requests) == 0 {
		t.Fatalf("drove zero successful GET /v1/status requests in %s; the traffic generator itself is broken", driveTimeout)
	}
	if backups < wantBackups {
		t.Fatalf("only %d .gz backup(s) after %s of debug-level traffic at DEVMON_LOG_MAX_TOTAL_MB=%d (%d requests sent), want at least %d — rotation may not be triggering", backups, driveTimeout, retentionBudgetMB, atomic.LoadInt64(&requests), wantBackups)
	}
	if backups > retentionMaxBackups {
		t.Errorf(".gz backup count = %d, want at most %d (internal/logging's own retention cap) — growth beyond the cap means old backups are not being pruned", backups, retentionMaxBackups)
	}

	// "Roughly its budget", not exactly at it: internal/logging divides the
	// operator's total across (retentionMaxBackups+1) files, so lumberjack
	// rotates each file at a per-file size below the configured total, and a
	// write already in flight when a rotation fires can push the on-disk
	// total briefly past that per-file line. An exact-equality assertion
	// here would fail on a boundary that is itself correct (Task 12's
	// gotcha); the headroom multiplier below is deliberately generous.
	totalBytes := logsDirSize(t, a)
	const headroomMultiplier = 2.0
	budgetBytes := int64(retentionBudgetMB) * 1024 * 1024
	limitBytes := int64(float64(budgetBytes) * headroomMultiplier)
	if totalBytes > limitBytes {
		t.Errorf("logs directory is %d bytes after rotation, want at most %d (%.0fx the %dMB budget, with headroom for the per-file division and in-flight writes) — the on-disk total is not actually bounded", totalBytes, limitBytes, headroomMultiplier, retentionBudgetMB)
	}
}

// driveStatusTraffic issues GET requests against url until stop is closed,
// incrementing requests on every response the server actually answered. It
// never calls a *testing.T method: it runs on worker goroutines, and the Go
// testing package requires Fatal/FailNow to be called only from the test's
// own goroutine. A failed individual request is silently retried by the next
// loop iteration — this loop's job is generating log volume, not asserting
// per-request success, which the caller checks afterward via the requests
// counter.
func driveStatusTraffic(stop <-chan struct{}, client *http.Client, url string, requests *int64) {
	for {
		select {
		case <-stop:
			return
		default:
		}
		resp, err := client.Get(url)
		if err != nil {
			continue
		}
		_ = resp.Body.Close()
		atomic.AddInt64(requests, 1)
	}
}

// countGzBackups counts rotated, compressed log files under the agent's own
// logs directory — lumberjack's Compress:true (internal/logging.go) names
// them with a ".gz" suffix.
func countGzBackups(t *testing.T, a *harness.Agent) int {
	t.Helper()
	entries := readLogsDir(t, a)
	count := 0
	for _, e := range entries {
		if !e.IsDir() && filepath.Ext(e.Name()) == ".gz" {
			count++
		}
	}
	return count
}

// logsDirSize sums the size of every file under the agent's own logs
// directory — the current log file plus any rotated backups — which is the
// quantity DEVMON_LOG_MAX_TOTAL_MB bounds.
func logsDirSize(t *testing.T, a *harness.Agent) int64 {
	t.Helper()
	entries := readLogsDir(t, a)
	var total int64
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		info, err := e.Info()
		if err != nil {
			t.Fatalf("stat logs dir entry %s: %v", e.Name(), err)
		}
		total += info.Size()
	}
	return total
}

// readLogsDir reads the agent's <StateDir>/logs directory directly off the
// filesystem. This is the host-binary group: the agent runs as a local
// process under the test's own user, on a real host path, the same one
// Agent.LogText (harness/agent.go) reads agent.log from — there is no
// container boundary here to go through CopyFromContainer for, unlike the
// in-container group.
func readLogsDir(t *testing.T, a *harness.Agent) []os.DirEntry {
	t.Helper()
	dir := filepath.Join(a.StateDir, "logs")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read logs dir %s: %v", dir, err)
	}
	return entries
}
