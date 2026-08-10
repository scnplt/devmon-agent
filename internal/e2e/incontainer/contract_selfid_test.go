//go:build e2e

package incontainer

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/scnplt/devmon-agent/internal/e2e/harness"
)

// This file covers the self-IDENTIFICATION half of Phase 5's outstanding
// checklist (lifecycle-policy-and-audit.plan.md:1086-1126): proving the
// agent finds its own container through /proc/self/mountinfo rather than
// $HOSTNAME, that an explicit DEVMON_SELF_CONTAINER_ID override is honoured
// ahead of that auto-detection, and that a well-formed but unresolvable
// override degrades the agent's lifecycle routes rather than crashing it or
// silently disabling self-protection.
//
// What this file deliberately does NOT cover: the refusal itself — that a
// resolved self ID makes every lifecycle route answer 403 against all three
// reference forms — is contract_selfexclusion_test.go's job (Task 10). This
// file assumes that refusal mechanism works and is entirely about which
// container ID it gets fed.

// selfIDImageTag is this file's own image tag (BuildImage is not memoized;
// image.go), distinct from imageTag (main_test.go) and selfExclusionImageTag
// (contract_selfexclusion_test.go).
const selfIDImageTag = "devmon-agent:e2e-selfid"

// selfUnknownBody is the exact error body internal/httpapi/reads.go serves
// for dockerx.ErrSelfUnknown. Declared locally rather than imported (D4),
// for the same reason contract_selfexclusion_test.go declares
// selfExclusionBody: the suite must notice a wording change, which it
// cannot do if it shares the constant with the server that produces it.
const selfUnknownBody = "agent cannot identify its own container"

// unresolvableSelfID is a well-formed 64-character hex string chosen from a
// fixed, readable pattern (not crypto/rand) so a failing assertion is
// reproducible, and picked with no plausible chance of colliding with a
// real Docker-assigned ID.
const unresolvableSelfID = "deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef"

// agentLogPath is the operational log file's path INSIDE the container:
// internal/config.Config.AgentLogPath() joined against image.go's
// containerStateDir ("/var/lib/devmon"). Reproduced here as a literal
// because containerStateDir is unexported in image.go and this task's
// instructions ask for harness additions to land in a new file rather than
// widen an existing one's exported surface for a single call site.
const agentLogPath = "/var/lib/devmon/logs/agent.log"

// TestSelfIDResolvesWithOverriddenHostname proves self-identification comes
// from mountinfo, not $HOSTNAME: the container's runtime hostname is set to
// something that is neither the container's own ID nor hex-shaped at all,
// so selfid.Detect's lowest-priority $HOSTNAME candidate
// (hostnameID12Pattern) cannot even match, let alone resolve. If mountinfo
// were NOT the thing doing the work, self-exclusion would never fire here —
// which is exactly the failure mode this test is built to catch.
func TestSelfIDResolvesWithOverriddenHostname(t *testing.T) {
	e := harness.RequireLinuxContainerEngine(t)
	harness.BuildImage(t, selfIDImageTag, nil)

	c := harness.RunAgentContainer(t, e, harness.ContainerAgentOptions{
		Image:      selfIDImageTag,
		PolicyMode: "full",
		Hostname:   "not-the-container-id",
	})
	device := harness.PairDeviceInContainer(t, c, "hostname-override-device")

	selfRefs := refForms(t, e, c.ID)

	// Falsifiability is built into the assertion itself here: a 204 instead
	// of a 403 would mean the agent never recognised the container as its
	// own — the only way that happens with $HOSTNAME overridden away from
	// the ID is if mountinfo detection is NOT what resolved it.
	status, _, raw := device.Do(t, http.MethodPost, "/v1/containers/"+selfRefs["full"]+"/restart", nil)
	if status != http.StatusForbidden {
		t.Fatalf("restart self with Config.Hostname overridden away from the container ID: status = %d, want %d; body = %s",
			status, http.StatusForbidden, raw)
	}
	var obj map[string]string
	if err := json.Unmarshal(raw, &obj); err != nil {
		t.Fatalf("decode self-protection body: %v; body = %s", err, raw)
	}
	if obj["error"] != selfExclusionBody {
		t.Errorf("error = %q, want %q", obj["error"], selfExclusionBody)
	}

	logText := string(c.ReadStateFile(t, agentLogPath))
	if !strings.Contains(logText, c.ID) {
		t.Errorf("agent.log does not contain the resolved container ID %s at startup", c.ID)
	}
	if strings.Contains(logText, "could not identify its own container") {
		t.Errorf("agent.log carries the self-UNRESOLVED error line despite self-exclusion demonstrably firing above")
	}
}

// TestExplicitSelfIDOverrideIsHonoured proves DEVMON_SELF_CONTAINER_ID is
// honoured ahead of mountinfo/cgroup auto-detection, the moment the Engine
// confirms it names a real container — WITHOUT needing the override to
// equal the agent's OWN Docker-assigned ID.
//
// That equality cannot be constructed at all: a container's ID does not
// exist until client.ContainerCreate returns it, and the SAME call must
// already carry the override inside Config.Env, so no test can ever learn a
// container's real ID in time to hand it back as that container's own
// override. (Verified in this session: client.ContainerUpdateOptions only
// covers Resources and RestartPolicy — Env cannot be changed after create
// either, so there is no two-step way around this.)
//
// Instead this proves the sharper, equally load-bearing property: the
// override is taken AS-IS the instant the Engine confirms it names SOME
// container — even one that is provably NOT the agent. A plain fixture
// container stands in as that "someone else". If the override were ignored
// in favour of auto-detection (the bug this test exists to catch), the
// agent would self-identify via mountinfo as its OWN container instead, and
// the "protected" row would land on the agent's own ID rather than the
// fixture's — the opposite of what is asserted below.
func TestExplicitSelfIDOverrideIsHonoured(t *testing.T) {
	e := harness.RequireLinuxContainerEngine(t)
	harness.BuildImage(t, selfIDImageTag, nil)

	bystander := harness.StartFixture(t, e, harness.FixtureOptions{NameSuffix: "override-target"})
	waitFixtureRunning(t, e, bystander)

	c := harness.RunAgentContainer(t, e, harness.ContainerAgentOptions{
		Image:      selfIDImageTag,
		PolicyMode: "full",
		Env:        map[string]string{"DEVMON_SELF_CONTAINER_ID": bystander},
	})
	device := harness.PairDeviceInContainer(t, c, "explicit-override-device")

	status, obj := device.JSON(t, http.MethodGet, "/v1/containers?all=true")
	if status != http.StatusOK {
		t.Fatalf("GET /v1/containers?all=true: status = %d, want %d", status, http.StatusOK)
	}
	items, ok := obj["items"].([]any)
	if !ok {
		t.Fatalf("GET /v1/containers?all=true: items is not a JSON array: %#v", obj["items"])
	}

	bystanderRow := findRowByID(t, items, bystander)
	if protected, ok := bystanderRow["protected"]; !ok || protected != true {
		t.Errorf("bystander row (the DEVMON_SELF_CONTAINER_ID target): protected = %v, want true — the override was not honoured", protected)
	}

	agentRow := findRowByID(t, items, c.ID)
	if protected, ok := agentRow["protected"]; !ok || protected != false {
		t.Errorf("agent's own real row: protected = %v, want false — "+
			"the agent self-identified as its OWN container despite the override naming a different one", protected)
	}
}

// TestUnresolvableSelfIDFailsClosed is the case where the agent starts
// successfully and DEGRADES, rather than refusing to start: a well-formed
// but non-existent DEVMON_SELF_CONTAINER_ID is not a configuration error
// (internal/config/config.go validates only the override's SHAPE — the
// Engine is not reachable yet at config-parse time). Per
// internal/dockerx/lifecycle.go's resolveTarget, once the agent ends up
// containerised but self-unresolved, EVERY lifecycle route answers 503
// unconditionally of its own target, because the agent cannot enforce the
// self-exclusion rule without knowing which container to protect.
//
// GOTCHA (Task 11): an agent that had refused to start would also produce
// 503s to a careless test, which is why this test asserts the container is
// still running and that reads/logs are 200 alongside the 503s — the
// unresolved state is a live, serving agent, not a crashed one.
//
// OPEN QUESTION this task could not settle by reading code alone:
// internal/dockerx/self.go's confirmSelf does not stop at the first
// candidate that fails Engine verification; it falls through in order to
// the mountinfo- and cgroup-derived candidates next. In this image's normal
// bridge-network setup those candidates DO resolve — that is exactly what
// TestSelfIDResolvesWithOverriddenHostname above depends on. If the same
// fallback fires here, the agent may recover silently via mountinfo and
// behave exactly like the no-override case (200/204 instead of the 503s
// asserted below) rather than genuinely failing closed. This suite could
// not be run against a live Engine in the environment that produced it (no
// Docker Engine was reachable — see the task's implementation report), so
// this test encodes the documented contract as specified rather than a
// weakened version of it; treat a red run here as a real finding to
// investigate, not a flaky test to retry.
func TestUnresolvableSelfIDFailsClosed(t *testing.T) {
	e := harness.RequireLinuxContainerEngine(t)
	harness.BuildImage(t, selfIDImageTag, nil)

	c := harness.RunAgentContainer(t, e, harness.ContainerAgentOptions{
		Image:      selfIDImageTag,
		PolicyMode: "full",
		Env:        map[string]string{"DEVMON_SELF_CONTAINER_ID": unresolvableSelfID},
	})
	device := harness.PairDeviceInContainer(t, c, "unresolvable-override-device")

	selfRefs := refForms(t, e, c.ID)

	for _, route := range selfExclusionRoutes {
		t.Run(route.operation, func(t *testing.T) {
			status, _, raw := device.Do(t, route.method, route.path(selfRefs["full"]), nil)
			if status != http.StatusServiceUnavailable {
				t.Fatalf("%s %s (unresolvable self ID): status = %d, want %d; body = %s",
					route.method, route.path(selfRefs["full"]), status, http.StatusServiceUnavailable, raw)
			}
			var obj map[string]string
			if err := json.Unmarshal(raw, &obj); err != nil {
				t.Fatalf("decode self-unknown body: %v; body = %s", err, raw)
			}
			if obj["error"] != selfUnknownBody {
				t.Errorf("error = %q, want %q", obj["error"], selfUnknownBody)
			}
		})
	}

	// Reads and logs must stay 200: an unresolved self ID degrades only the
	// self-exclusion guarantee, never the whole API surface.
	if status, obj := device.JSON(t, http.MethodGet, "/v1/containers"); status != http.StatusOK {
		t.Errorf("GET /v1/containers with an unresolvable self ID: status = %d, want %d; body = %v", status, http.StatusOK, obj)
	}
	if status, _, raw := device.Do(t, http.MethodGet, "/v1/containers/"+c.ID+"/logs?tail=1", nil); status != http.StatusOK {
		t.Errorf("GET .../logs with an unresolvable self ID: status = %d, want %d; body = %s", status, http.StatusOK, raw)
	}

	running, _ := c.IsRunning(t)
	if !running {
		t.Fatalf("agent container %s is not running after the unresolvable-self-ID lifecycle attempts", c.ID)
	}

	logText := string(c.ReadStateFile(t, agentLogPath))
	if !strings.Contains(logText, "DEVMON_SELF_CONTAINER_ID") {
		t.Errorf("agent.log does not name DEVMON_SELF_CONTAINER_ID anywhere; an operator debugging a stuck 503 would have no lead")
	}
	if !strings.Contains(logText, "level=ERROR") {
		t.Errorf("agent.log has no ERROR-level line recording the unresolved self ID")
	}
}
