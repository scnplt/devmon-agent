// SPDX-License-Identifier: AGPL-3.0-only

//go:build e2e

package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"

	"github.com/scnplt/devmon-agent/internal/e2e/harness"
)

// This file replays Phase 3's manual checklist: the eight read routes' wire
// shapes, the two secrets that must never leak (a container's environment,
// an image's baked-in ENV, a volume's driver options), the ?all= and health
// projections, freshness with no caching layer, invalid and unknown
// references, the Engine-unavailable 502 path with recovery (D16's proxy),
// and read-only mode permitting every one of them.
// read-operations.plan.md:894-907.
//
// What this file deliberately does NOT cover: revocation losing read access
// is TestRevokedDeviceLosesAccessImmediately in contract_identity_test.go —
// one behaviour, already exercised there, is not duplicated here.

// readFixtureRunID identifies this test binary invocation, mirrored from
// harness's own (unexported) runID so fixture names created directly in this
// file — networks, volumes, and committed images, none of which
// harness.StartFixture covers — stay unique across a concurrent run on the
// same host, the same way harness's own fixtures do (D11).
var readFixtureRunID = fmt.Sprintf("%d-%d", os.Getpid(), time.Now().UnixNano())

// readContractTimeout bounds a single read round trip in this file's tests;
// generous enough for a cold image pull on a first run, short enough that a
// wedged Engine still fails the test instead of hanging the suite.
const readContractTimeout = 30 * time.Second

// assertKeySet asserts obj's key set is exactly the union of required and
// optional, and that every required key is actually present. Optional keys
// (the DTO's own omitempty fields — dockerx/types.go) are asserted as a
// PERMITTED superset, never promised: a container with no healthcheck omits
// "health" entirely, one with a healthcheck does not. Writing down which list
// is which, rather than asserting one blanket set, is itself part of the
// contract a client needs (Task 5's gotcha in the phase plan).
func assertKeySet(t *testing.T, obj map[string]any, required, optional []string) {
	t.Helper()

	allowed := make(map[string]bool, len(required)+len(optional))
	for _, k := range required {
		allowed[k] = true
	}
	for _, k := range optional {
		allowed[k] = true
	}

	for k := range obj {
		if !allowed[k] {
			t.Errorf("response has an undocumented key %q; obj = %v", k, obj)
		}
	}
	for _, k := range required {
		if _, ok := obj[k]; !ok {
			t.Errorf("response is missing required key %q; obj = %v", k, obj)
		}
	}
}

// asObject asserts v decodes as a JSON object and returns it, failing the
// test with the offending value's Go type otherwise.
func asObject(t *testing.T, v any) map[string]any {
	t.Helper()
	obj, ok := v.(map[string]any)
	if !ok {
		t.Fatalf("value is not a JSON object: %#v", v)
	}
	return obj
}

// asArray asserts v decodes as a JSON array and returns it.
func asArray(t *testing.T, v any) []any {
	t.Helper()
	arr, ok := v.([]any)
	if !ok {
		t.Fatalf("value is not a JSON array: %#v", v)
	}
	return arr
}

// findByKey scans a decoded "items" array for the first object whose field
// key equals want, so a test can locate its own fixture inside a list that
// may also contain the developer's unrelated containers, images, networks,
// and volumes.
func findByKey(t *testing.T, items []any, key, want string) map[string]any {
	t.Helper()
	for _, raw := range items {
		obj := asObject(t, raw)
		if obj[key] == want {
			return obj
		}
	}
	t.Fatalf("no item with %s = %q found among %d items", key, want, len(items))
	return nil
}

// readEnvelope decodes a list route's response and asserts the envelope
// itself — items and truncated, and nothing else — before returning the
// decoded items for a per-item assertion. ListResult has no omitempty field
// (dockerx/types.go), so the envelope's key set has no optional half.
func readEnvelope(t *testing.T, d *harness.Device, path string) []any {
	t.Helper()

	status, obj := d.JSON(t, http.MethodGet, path)
	if status != http.StatusOK {
		t.Fatalf("GET %s status = %d, want %d", path, status, http.StatusOK)
	}
	harness.AssertExactKeys(t, obj, []string{"items", "truncated"})
	if obj["truncated"] != false {
		t.Errorf("GET %s truncated = %v, want false", path, obj["truncated"])
	}
	return asArray(t, obj["items"])
}

// readReadyAgent starts one agent, pairs one device, and returns both — the
// shared setup every test in this file that is not specifically about a
// second agent instance (the proxy tests, the policy test) needs.
func readReadyAgent(t *testing.T, name string) (*harness.Agent, *harness.Device) {
	t.Helper()
	a := harness.StartAgent(t, harness.AgentOptions{})
	d := harness.PairDevice(t, a, name)
	return a, d
}

// TestContainerReadContractKeys asserts the list and inspect projections for
// a container carry exactly the documented required keys, plus only the
// documented optional ones (dockerx/types.go's ContainerSummary and
// ContainerDetail).
func TestContainerReadContractKeys(t *testing.T) {
	t.Parallel()
	engine := harness.RequireEngine(t)
	_, d := readReadyAgent(t, "reads-container-keys")

	id := harness.StartFixture(t, engine, harness.FixtureOptions{NameSuffix: "keys"})

	items := readEnvelope(t, d, "/v1/containers")
	summary := findByKey(t, items, "id", id)
	assertKeySet(t, summary,
		[]string{"id", "names", "image", "image_id", "command", "created_at", "state", "status", "labels", "ports", "protected"},
		[]string{"health"},
	)

	status, detail := d.JSON(t, http.MethodGet, "/v1/containers/"+id)
	if status != http.StatusOK {
		t.Fatalf("GET /v1/containers/%s status = %d, want %d", id, status, http.StatusOK)
	}
	assertKeySet(t, detail,
		[]string{
			"id", "name", "image", "created_at", "state", "running", "paused", "restarting",
			"exit_code", "restart_count", "platform", "labels", "command", "args",
			"mounts", "networks", "ports", "protected",
		},
		[]string{"started_at", "finished_at", "health", "restart_policy", "entrypoint", "working_dir", "user"},
	)
	if detail["protected"] != false {
		t.Errorf("fixture container protected = %v, want false", detail["protected"])
	}
}

// TestImageReadContractKeys asserts the list and inspect projections for an
// image carry exactly the documented keys (dockerx/types.go's ImageSummary
// and ImageDetail).
func TestImageReadContractKeys(t *testing.T) {
	t.Parallel()
	engine := harness.RequireEngine(t)
	_, d := readReadyAgent(t, "reads-image-keys")

	imageID := pulledImageID(t, engine, defaultFixtureImageRef)

	items := readEnvelope(t, d, "/v1/images")
	summary := findByKey(t, items, "id", imageID)
	assertKeySet(t, summary,
		[]string{"id", "repo_tags", "repo_digests", "created_at", "size", "containers", "labels"},
		[]string{"parent_id"},
	)

	status, detail := d.JSON(t, http.MethodGet, "/v1/images/"+bareHex(imageID))
	if status != http.StatusOK {
		t.Fatalf("GET /v1/images/%s status = %d, want %d", bareHex(imageID), status, http.StatusOK)
	}
	assertKeySet(t, detail,
		[]string{"id", "repo_tags", "repo_digests", "size", "architecture", "os"},
		[]string{"created_at", "author", "comment"},
	)
}

// TestNetworkReadContractKeys asserts the list and inspect projections for a
// network carry exactly the documented keys (dockerx/types.go's
// NetworkSummary and NetworkDetail). Neither struct has an omitempty field,
// so both key sets are exact rather than required-plus-optional.
func TestNetworkReadContractKeys(t *testing.T) {
	t.Parallel()
	engine := harness.RequireEngine(t)
	_, d := readReadyAgent(t, "reads-network-keys")

	id := createTestNetwork(t, engine, "keys")

	items := readEnvelope(t, d, "/v1/networks")
	summary := findByKey(t, items, "id", id)
	harness.AssertExactKeys(t, summary, []string{"id", "name", "driver", "scope", "created_at", "internal", "enable_ipv6", "labels"})

	status, detail := d.JSON(t, http.MethodGet, "/v1/networks/"+id)
	if status != http.StatusOK {
		t.Fatalf("GET /v1/networks/%s status = %d, want %d", id, status, http.StatusOK)
	}
	harness.AssertExactKeys(t, detail, []string{"id", "name", "driver", "scope", "created_at", "internal", "enable_ipv6", "labels", "containers"})
}

// TestVolumeReadContractKeys asserts the list and inspect projections for a
// volume carry exactly the documented keys (dockerx/types.go's
// VolumeSummary — used unchanged as the inspect response too).
func TestVolumeReadContractKeys(t *testing.T) {
	t.Parallel()
	engine := harness.RequireEngine(t)
	_, d := readReadyAgent(t, "reads-volume-keys")

	name := createTestVolume(t, engine, "keys", nil)

	items := readEnvelope(t, d, "/v1/volumes")
	summary := findByKey(t, items, "name", name)
	assertKeySet(t, summary,
		[]string{"name", "driver", "mountpoint", "scope", "labels"},
		[]string{"created_at", "size_bytes"},
	)

	status, detail := d.JSON(t, http.MethodGet, "/v1/volumes/"+name)
	if status != http.StatusOK {
		t.Fatalf("GET /v1/volumes/%s status = %d, want %d", name, status, http.StatusOK)
	}
	assertKeySet(t, detail,
		[]string{"name", "driver", "mountpoint", "scope", "labels"},
		[]string{"created_at", "size_bytes"},
	)
}

// TestContainerInspectOmitsEnvironment asserts a container started with a
// secret-shaped environment variable never surfaces it: container.Config.Env
// has no field in ContainerDetail (dockerx/types.go's D2), so a leak would
// have to come back embedded in some other value entirely — this test looks
// for the raw secret text anywhere in the response body, not just in a named
// field.
func TestContainerInspectOmitsEnvironment(t *testing.T) {
	t.Parallel()
	engine := harness.RequireEngine(t)
	_, d := readReadyAgent(t, "reads-container-env")

	const secret = "hunter2devmone2e"
	id := harness.StartFixture(t, engine, harness.FixtureOptions{
		NameSuffix: "env-secret",
		Env:        []string{"DB_PASSWORD=" + secret},
	})

	status, hdr, raw := d.Do(t, http.MethodGet, "/v1/containers/"+id, nil)
	if status != http.StatusOK {
		t.Fatalf("GET /v1/containers/%s status = %d, want %d", id, status, http.StatusOK)
	}
	_ = hdr
	if strings.Contains(string(raw), secret) || strings.Contains(string(raw), "DB_PASSWORD") {
		t.Errorf("container inspect response leaks the environment: %s", raw)
	}
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err == nil {
		if _, ok := obj["env"]; ok {
			t.Errorf("container inspect response has an \"env\" key at all: %v", obj)
		}
	}
}

// TestImageInspectOmitsEnvironment asserts an image whose Dockerfile baked in
// ENV API_KEY=... never surfaces it: image.InspectResponse.Config, which
// carries that value, has no field in ImageDetail (dockerx/types.go's D2).
// The fixture image is built by committing a container that had the variable
// set at runtime, which produces the same Config.Env entry a real
// `ENV API_KEY=...` Dockerfile instruction would.
func TestImageInspectOmitsEnvironment(t *testing.T) {
	t.Parallel()
	engine := harness.RequireEngine(t)
	_, d := readReadyAgent(t, "reads-image-env")

	const secret = "supersecretapikey123"
	imageID := commitImageWithEnv(t, engine, "API_KEY="+secret)

	status, _, raw := d.Do(t, http.MethodGet, "/v1/images/"+bareHex(imageID), nil)
	if status != http.StatusOK {
		t.Fatalf("GET /v1/images/%s status = %d, want %d", bareHex(imageID), status, http.StatusOK)
	}
	if strings.Contains(string(raw), secret) || strings.Contains(string(raw), "API_KEY") {
		t.Errorf("image inspect response leaks Config.Env: %s", raw)
	}
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err == nil {
		if _, ok := obj["config"]; ok {
			t.Errorf("image inspect response has a \"config\" key at all: %v", obj)
		}
	}
}

// TestVolumeInspectOmitsDriverOptions asserts a volume created with driver
// options — the shape that routinely carries tmpfs/CIFS/NFS credentials —
// never surfaces them: volume.Volume.Options has no field in VolumeSummary
// (dockerx/types.go's D2).
func TestVolumeInspectOmitsDriverOptions(t *testing.T) {
	t.Parallel()
	engine := harness.RequireEngine(t)
	_, d := readReadyAgent(t, "reads-volume-options")

	const optionValue = "size=1m"
	name := createTestVolume(t, engine, "driver-opts", map[string]string{"o": optionValue, "device": "tmpfs", "type": "tmpfs"})

	status, _, raw := d.Do(t, http.MethodGet, "/v1/volumes/"+name, nil)
	if status != http.StatusOK {
		t.Fatalf("GET /v1/volumes/%s status = %d, want %d", name, status, http.StatusOK)
	}
	if strings.Contains(string(raw), optionValue) {
		t.Errorf("volume inspect response leaks a driver option value: %s", raw)
	}
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err == nil {
		if _, ok := obj["options"]; ok {
			t.Errorf("volume inspect response has an \"options\" key at all: %v", obj)
		}
	}
}

// TestContainerListAllParameter asserts a stopped container is absent from
// the default list and present, as "exited", only with ?all=true.
func TestContainerListAllParameter(t *testing.T) {
	t.Parallel()
	engine := harness.RequireEngine(t)
	_, d := readReadyAgent(t, "reads-list-all")

	id := harness.StartFixture(t, engine, harness.FixtureOptions{
		NameSuffix: "stopped",
		Cmd:        []string{"true"},
	})
	waitContainerExited(t, engine, id)

	defaultItems := readEnvelope(t, d, "/v1/containers")
	for _, raw := range defaultItems {
		if asObject(t, raw)["id"] == id {
			t.Fatalf("stopped container %s appears in the default (all=false) list", id)
		}
	}

	allItems := readEnvelope(t, d, "/v1/containers?all=true")
	found := findByKey(t, allItems, "id", id)
	if found["state"] != "exited" {
		t.Errorf("stopped container state = %v, want %q", found["state"], "exited")
	}
}

// TestContainerListReportsHealth asserts a container with a failing
// healthcheck eventually surfaces health: "unhealthy" in the list projection.
// Docker needs a few checks before it reports the container unhealthy, so
// this polls with a deadline rather than sleeping a fixed duration.
//
// Requires Engine 29+ / API v1.52: per the vendored swagger spec,
// ContainerSummary.Health was added in API v1.52 — before that version the
// list projection (/containers/json) never sends the field at all, so an
// older Engine would leave "health" perpetually absent no matter how the
// fixture container behaves. That is a genuine Engine capability floor, not
// a bug in the projection, so the test skips below that version rather than
// failing.
func TestContainerListReportsHealth(t *testing.T) {
	t.Parallel()
	engine := harness.RequireEngine(t)
	harness.RequireEngineAPIAtLeast(t, engine, "1.52")
	_, d := readReadyAgent(t, "reads-health")

	id := harness.StartFixture(t, engine, harness.FixtureOptions{
		NameSuffix: "unhealthy",
		Healthcheck: &container.HealthConfig{
			Test:        []string{"CMD-SHELL", "exit 1"},
			Interval:    500 * time.Millisecond,
			Timeout:     500 * time.Millisecond,
			Retries:     1,
			StartPeriod: 0,
		},
	})

	deadline := time.Now().Add(20 * time.Second)
	var lastHealth any
	for time.Now().Before(deadline) {
		items := readEnvelope(t, d, "/v1/containers")
		found := findByKey(t, items, "id", id)
		lastHealth = found["health"]
		if lastHealth == "unhealthy" {
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("container health never reached \"unhealthy\" within the deadline; last observed = %v", lastHealth)
}

// TestReadsAreNotCached asserts a network and a volume created mid-test
// appear on the very next list call — there is no caching layer between the
// agent and the Engine to go stale.
func TestReadsAreNotCached(t *testing.T) {
	t.Parallel()
	engine := harness.RequireEngine(t)
	_, d := readReadyAgent(t, "reads-not-cached")

	before := readEnvelope(t, d, "/v1/networks")
	netID := createTestNetwork(t, engine, "not-cached")
	after := readEnvelope(t, d, "/v1/networks")
	if len(after) <= len(before) {
		t.Errorf("network list length after creating one = %d, want more than before (%d)", len(after), len(before))
	}
	findByKey(t, after, "id", netID)

	beforeVols := readEnvelope(t, d, "/v1/volumes")
	volName := createTestVolume(t, engine, "not-cached", nil)
	afterVols := readEnvelope(t, d, "/v1/volumes")
	if len(afterVols) <= len(beforeVols) {
		t.Errorf("volume list length after creating one = %d, want more than before (%d)", len(afterVols), len(beforeVols))
	}
	findByKey(t, afterVols, "name", volName)
}

// TestReadsRejectInvalidAndUnknownRefs asserts every inspect route answers
// 400 for a reference that fails dockerx.ValidateRef (never reaching the
// Engine) and 404 for a well-formed but nonexistent one.
//
// The invalid-ref probes are percent-encoded ("%2e%2e", not a literal ".."),
// mirroring what Phase 3's manual validation confirmed
// (read-operations-report.md's End-to-End Validation section): a literal
// ".." in the request path is collapsed by net/http.ServeMux's own path
// cleaning BEFORE the pattern ever matches, which answers with a redirect,
// not the documented 400 — a probe that never reaches this agent's own
// validation is not a test of it. Go's mux cleans http.Request.URL's escaped
// path, and a percent-escaped dot is not a literal dot to that cleaner, so
// the encoded form is what reaches dockerx.ValidateRef as ".." after this
// agent's own decoding.
func TestReadsRejectInvalidAndUnknownRefs(t *testing.T) {
	t.Parallel()
	harness.RequireEngine(t)
	_, d := readReadyAgent(t, "reads-invalid-refs")

	const unknown = "devmone2enosuchobject000000000000"

	tests := []struct {
		name string
		path string
	}{
		{name: "container invalid ref", path: "/v1/containers/%2e%2e"},
		{name: "container unknown ref", path: "/v1/containers/" + unknown},
		{name: "image invalid ref", path: "/v1/images/sha256:ab"},
		{name: "image unknown ref", path: "/v1/images/" + unknown},
		{name: "network invalid ref", path: "/v1/networks/a%2fb"},
		{name: "network unknown ref", path: "/v1/networks/" + unknown},
		{name: "volume invalid ref", path: "/v1/volumes/-leading"},
		{name: "volume unknown ref", path: "/v1/volumes/" + unknown},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			status, _, raw := d.Do(t, http.MethodGet, tt.path, nil)
			want := http.StatusNotFound
			if strings.Contains(tt.name, "invalid") {
				want = http.StatusBadRequest
			}
			if status != want {
				t.Errorf("GET %s status = %d, want %d; body = %s", tt.path, status, want, raw)
			}
		})
	}
}

// TestReadOnlyModePermitsAllReads asserts all eight read routes answer 200
// under a read-only-mode agent, and that reads/inspects behave identically to
// the default-mode case — read-only restricts mutation, not visibility.
func TestReadOnlyModePermitsAllReads(t *testing.T) {
	t.Parallel()
	engine := harness.RequireEngine(t)

	a := harness.StartAgent(t, harness.AgentOptions{PolicyMode: "read-only"})
	d := harness.PairDevice(t, a, "reads-read-only-mode")

	id := harness.StartFixture(t, engine, harness.FixtureOptions{NameSuffix: "read-only"})
	imageID := pulledImageID(t, engine, defaultFixtureImageRef)
	netID := createTestNetwork(t, engine, "read-only")
	volName := createTestVolume(t, engine, "read-only", nil)

	routes := []string{
		"/v1/containers",
		"/v1/containers/" + id,
		"/v1/images",
		"/v1/images/" + bareHex(imageID),
		"/v1/networks",
		"/v1/networks/" + netID,
		"/v1/volumes",
		"/v1/volumes/" + volName,
	}
	for _, path := range routes {
		t.Run(path, func(t *testing.T) {
			t.Parallel()
			status, _, raw := d.Do(t, http.MethodGet, path, nil)
			if status != http.StatusOK {
				t.Errorf("GET %s under read-only mode: status = %d, want %d; body = %s", path, status, http.StatusOK, raw)
			}
		})
	}
}

// TestReadsAnswer502WhenEngineIsGone is D16's proxy in action: every read
// route answers 502 with the agent's own Engine-unavailable body while the
// proxy is severed, the agent itself stays reachable throughout (proven by
// asking the Engine, not the agent, and by /v1/status continuing to answer),
// and every route recovers with no agent restart once the proxy reopens.
//
// This test owns its own agent and its own proxy (never shared with a test
// that runs t.Parallel()): severing a proxy another test's assertions depend
// on would make that test's outcome depend on scheduling order, which is
// exactly the bug class this suite exists to catch, not to introduce.
func TestReadsAnswer502WhenEngineIsGone(t *testing.T) {
	harness.RequireEngine(t)

	realHost, skipReason := harness.EngineHost()
	if skipReason != "" {
		t.Skip(skipReason)
	}

	// The proxy must be listening BEFORE the agent starts: dockerx.New pings
	// the Engine at startup, and a dead endpoint there is a fatal startup
	// error, not the 502 this test is about (D16's gotcha).
	proxy := harness.NewProxy(t, realHost)

	a := harness.StartAgent(t, harness.AgentOptions{DockerHost: proxy.Addr()})
	d := harness.PairDevice(t, a, "reads-engine-gone")

	routes := []string{
		"/v1/containers", "/v1/images", "/v1/networks", "/v1/volumes",
	}

	// Falsifiability / sanity: with the proxy intact, every route is 200.
	// If this pre-check itself failed, the 502 assertions below would prove
	// nothing about the proxy — they would be indistinguishable from a
	// misconfigured agent that always answers 502.
	for _, path := range routes {
		status, _, raw := d.Do(t, http.MethodGet, path, nil)
		if status != http.StatusOK {
			t.Fatalf("sanity check before severing the proxy: GET %s status = %d, want %d; body = %s", path, status, http.StatusOK, raw)
		}
	}

	proxy.Sever(t)

	for _, path := range routes {
		status, obj := d.JSON(t, http.MethodGet, path)
		if status != http.StatusBadGateway {
			t.Errorf("GET %s with the Engine severed: status = %d, want %d", path, status, http.StatusBadGateway)
			continue
		}
		if obj["error"] != "docker engine unavailable" {
			t.Errorf("GET %s error = %v, want %q", path, obj["error"], "docker engine unavailable")
		}
	}

	// The agent itself is still up: /v1/status does not depend on the
	// Engine, and asking the real Docker Engine (not the agent) confirms the
	// agent's own process — not just its HTTP port — survived.
	if status, _, _ := d.Do(t, http.MethodGet, "/v1/status", nil); status != http.StatusOK {
		t.Errorf("GET /v1/status with the Engine severed: status = %d, want %d", status, http.StatusOK)
	}

	proxy.Restore(t)

	for _, path := range routes {
		status, _, raw := d.Do(t, http.MethodGet, path, nil)
		if status != http.StatusOK {
			t.Errorf("GET %s after Restore: status = %d, want %d; body = %s", path, status, http.StatusOK, raw)
		}
	}
}

// TestRequestLogCarriesNoObjectReferences documents the read-operations
// checklist item "agent.log request lines carry no container names or refs"
// against what internal/httpapi/middleware.go's withRequestLog actually logs.
//
// It does NOT assert the checklist's literal claim: withRequestLog logs
// r.URL.Path verbatim (method/path/status/duration only), and a read route's
// path IS the object reference the caller supplied — a GET to
// /v1/containers/<ref> logs <ref> as part of "path". This was confirmed
// during Phase 3's manual validation (read-operations-report.md, "Confirmed
// review finding M4 with evidence") and is a recorded, un-fixed observation,
// not something this task may correct (D19: no production code changes).
//
// What the checklist item's underlying security concern actually protects
// against — and what this test asserts instead — is that no SECRET ever
// reaches agent.log: not an environment value, not a driver option, not a
// pairing code, not key material. That guarantee holds.
func TestRequestLogCarriesNoObjectReferences(t *testing.T) {
	t.Parallel()
	engine := harness.RequireEngine(t)

	a := harness.StartAgent(t, harness.AgentOptions{Env: map[string]string{"DEVMON_LOG_LEVEL": "debug"}})
	d := harness.PairDevice(t, a, "reads-request-log")

	const secret = "hunter2requestlog"
	id := harness.StartFixture(t, engine, harness.FixtureOptions{
		NameSuffix: "request-log-env",
		Env:        []string{"DB_PASSWORD=" + secret},
	})

	if status, _, _ := d.Do(t, http.MethodGet, "/v1/containers/"+id, nil); status != http.StatusOK {
		t.Fatalf("GET /v1/containers/%s status = %d, want %d", id, status, http.StatusOK)
	}

	a.Stop(t)
	logText := a.LogText(t)

	if !strings.Contains(logText, "request served") {
		t.Fatalf("agent.log has no \"request served\" line at DEVMON_LOG_LEVEL=debug; the assertions below would be vacuous")
	}
	if strings.Contains(logText, secret) {
		t.Errorf("agent.log contains the environment secret value: a real leak, not the recorded M4 finding")
	}
	if strings.Contains(logText, "DB_PASSWORD") {
		t.Errorf("agent.log contains the environment variable name %q", "DB_PASSWORD")
	}
}

// --- Fixture helpers local to this file. Every Engine object they create is
// labelled with harness.LabelSuite and cleaned up by ID, mirroring
// harness.StartFixture's own contract for containers (D11): nothing without
// the label is ever created here, and every t.Cleanup removes exactly the
// one object it made.

// defaultFixtureImageRef is the same pinned image harness.StartFixture pulls
// when no image is named, reused here so image-route tests do not need a
// second pull.
const defaultFixtureImageRef = "busybox:1.36.1"

// pulledImageID ensures ref is present locally and returns its full
// "sha256:..." ID, pulling it if necessary.
func pulledImageID(t *testing.T, engine *client.Client, ref string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), readContractTimeout)
	defer cancel()

	res, err := engine.ImageInspect(ctx, ref)
	if err == nil {
		return res.ID
	}

	rc, err := engine.ImagePull(ctx, ref, client.ImagePullOptions{})
	if err != nil {
		t.Fatalf("pull image %s: %v", ref, err)
	}
	defer func() { _ = rc.Close() }()
	if err := rc.Wait(ctx); err != nil {
		t.Fatalf("pull image %s: %v", ref, err)
	}

	res, err = engine.ImageInspect(ctx, ref)
	if err != nil {
		t.Fatalf("inspect freshly pulled image %s: %v", ref, err)
	}
	return res.ID
}

// bareHex strips a "sha256:" prefix so an image ID is safe to interpolate
// into an agent API path: dockerx.ValidateRef deliberately rejects ":", so
// the digest form is never accepted by the route under test — only the bare
// hex is.
func bareHex(id string) string {
	return strings.TrimPrefix(id, "sha256:")
}

// commitImageWithEnv creates a throwaway, never-started container with env
// set, commits it into a new local image (which bakes env into the image's
// own Config.Env — exactly what a Dockerfile's ENV instruction produces),
// removes the source container, and returns the new image's full ID. Both
// the source container and the resulting image are labelled and cleaned up.
func commitImageWithEnv(t *testing.T, engine *client.Client, env string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), readContractTimeout)
	defer cancel()

	created, err := engine.ContainerCreate(ctx, client.ContainerCreateOptions{
		Config: &container.Config{
			Image:  defaultFixtureImageRef,
			Env:    []string{env},
			Cmd:    []string{"true"},
			Labels: fixtureLabels(),
		},
	})
	if err != nil {
		t.Fatalf("create source container for image commit: %v", err)
	}
	t.Cleanup(func() {
		_, _ = engine.ContainerRemove(context.Background(), created.ID, client.ContainerRemoveOptions{Force: true})
	})

	commitRes, err := engine.ContainerCommit(ctx, created.ID, client.ContainerCommitOptions{
		Config: &container.Config{Image: defaultFixtureImageRef, Env: []string{env}, Labels: fixtureLabels()},
	})
	if err != nil {
		t.Fatalf("commit image with env %q: %v", env, err)
	}
	t.Cleanup(func() {
		_, _ = engine.ImageRemove(context.Background(), commitRes.ID, client.ImageRemoveOptions{Force: true})
	})

	res, err := engine.ImageInspect(context.Background(), commitRes.ID)
	if err != nil {
		t.Fatalf("inspect committed image %s: %v", commitRes.ID, err)
	}
	return res.ID
}

// createTestNetwork creates a labelled bridge network, registers its
// removal, and returns its ID.
func createTestNetwork(t *testing.T, engine *client.Client, suffix string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), readContractTimeout)
	defer cancel()

	name := "devmon-e2e-net-" + readFixtureRunID + "-" + suffix
	res, err := engine.NetworkCreate(ctx, name, client.NetworkCreateOptions{
		Driver: "bridge",
		Labels: fixtureLabels(),
	})
	if err != nil {
		t.Fatalf("create network %s: %v", name, err)
	}
	t.Cleanup(func() {
		_, _ = engine.NetworkRemove(context.Background(), res.ID, client.NetworkRemoveOptions{})
	})
	return res.ID
}

// createTestVolume creates a labelled local volume, optionally with driver
// options (for the D2 leak test), registers its removal, and returns its
// name — volumes are addressed by name, not ID.
func createTestVolume(t *testing.T, engine *client.Client, suffix string, driverOpts map[string]string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), readContractTimeout)
	defer cancel()

	name := "devmon-e2e-vol-" + readFixtureRunID + "-" + suffix
	res, err := engine.VolumeCreate(ctx, client.VolumeCreateOptions{
		Name:       name,
		Driver:     "local",
		DriverOpts: driverOpts,
		Labels:     fixtureLabels(),
	})
	if err != nil {
		t.Fatalf("create volume %s: %v", name, err)
	}
	t.Cleanup(func() {
		_, _ = engine.VolumeRemove(context.Background(), name, client.VolumeRemoveOptions{Force: true})
	})
	return res.Volume.Name
}

// fixtureLabels returns the suite-wide labels every object this file creates
// carries, matching harness.StartFixture's own contract (D11): cleanup here
// is always by the ID or name this file itself just received back from the
// Engine, never by a name guess, but the labels still mark these objects as
// the suite's own for a human inspecting the host mid-run.
func fixtureLabels() map[string]string {
	return map[string]string{harness.LabelSuite: "1", harness.LabelRun: readFixtureRunID}
}

// waitContainerExited polls the Engine (never the agent) until id reports a
// non-running state, bounded by a deadline rather than a fixed sleep.
func waitContainerExited(t *testing.T, engine *client.Client, id string) {
	t.Helper()
	deadline := time.Now().Add(readContractTimeout)
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		res, err := engine.ContainerInspect(ctx, id, client.ContainerInspectOptions{})
		cancel()
		if err == nil && res.Container.State != nil && !res.Container.State.Running {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("container %s did not exit within %s", id, readContractTimeout)
}
