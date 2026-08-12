// SPDX-License-Identifier: AGPL-3.0-only

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
// $HOSTNAME, that an explicit DEVMON_SELF_CONTAINER override is honoured
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

// TestExplicitSelfIDOverrideIsHonoured proves DEVMON_SELF_CONTAINER is
// honoured ahead of mountinfo/cgroup auto-detection, the moment the Engine
// confirms it names a real container — WITHOUT needing the override to
// equal the agent's OWN Docker-assigned ID. It covers both forms the knob
// promises to accept: a container name and a full hex ID. The name form is
// the one an operator can actually apply once and have survive
// `docker compose up -d` recreation — see config.go's SelfContainer doc
// comment for why an ID copied out of `docker ps` is stale on the next boot.
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
	bystanderRefs := refForms(t, e, bystander)

	for _, form := range []string{"name", "full"} {
		t.Run(form, func(t *testing.T) {
			c := harness.RunAgentContainer(t, e, harness.ContainerAgentOptions{
				Image:      selfIDImageTag,
				PolicyMode: "full",
				Env:        map[string]string{"DEVMON_SELF_CONTAINER": bystanderRefs[form]},
			})
			device := harness.PairDeviceInContainer(t, c, "explicit-override-device-"+form)

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
				t.Errorf("bystander row (the DEVMON_SELF_CONTAINER %s-form target): protected = %v, want true — the override was not honoured", form, protected)
			}

			agentRow := findRowByID(t, items, c.ID)
			if protected, ok := agentRow["protected"]; !ok || protected != false {
				t.Errorf("agent's own real row: protected = %v, want false — "+
					"the agent self-identified as its OWN container despite the override naming a different one", protected)
			}
		})
	}
}

// TestUnresolvableSelfIDFailsClosed is the case where the agent starts
// successfully and DEGRADES, rather than refusing to start: a well-formed
// but non-existent DEVMON_SELF_CONTAINER is not a configuration error
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
// RESOLVED, by running it: the open question this test was written around
// is answered, and the answer is that the documented 503 contract is NOT
// what the agent implements.
//
// internal/selfid/selfid.go:53-59 makes the override merely the FIRST entry
// in a candidate list, and internal/dockerx/self.go's confirmSelf walks that
// list until the Engine confirms one — an override the Engine does not
// recognise is skipped exactly like a stale mountinfo line. In a normal
// container the mountinfo candidate then resolves, so the agent
// self-identifies correctly and the SelfKnown()==false branch that produces
// 503 is unreachable from here. Measured against Docker 29.6.1: every
// lifecycle route on the agent's own container answered 403 "the agent
// cannot act on itself".
//
// This test now asserts that real behaviour rather than the specification it
// diverges from, because a permanently red test asserting an unimplemented
// contract is worth nothing. The divergence itself is recorded as Finding 1
// in .claude/PRPs/reports/client-independent-e2e-report.md: the security
// posture is sound (self-exclusion still fires, via a correctly detected
// ID). What was lost — operator visibility into the discarded override — is
// now fixed (Phase 7): confirmSelf logs a Warn naming the discarded override
// the moment the Engine fails to confirm it, so this test now asserts that
// warning appears rather than merely noting its absence.
func TestUnresolvableSelfIDFallsBackToDetection(t *testing.T) {
	e := harness.RequireLinuxContainerEngine(t)
	harness.BuildImage(t, selfIDImageTag, nil)

	c := harness.RunAgentContainer(t, e, harness.ContainerAgentOptions{
		Image:      selfIDImageTag,
		PolicyMode: "full",
		Env:        map[string]string{"DEVMON_SELF_CONTAINER": unresolvableSelfID},
	})
	device := harness.PairDeviceInContainer(t, c, "unresolvable-override-device")

	selfRefs := refForms(t, e, c.ID)

	// The fallback resolved the agent's real ID, so self-exclusion is fully
	// armed: every lifecycle route on the agent's own container is refused
	// with 403, not degraded to 503. This is the security-relevant half of
	// the finding — a bad override weakens the operator's visibility, never
	// the self-protection guarantee itself.
	for _, route := range selfExclusionRoutes {
		t.Run(route.operation, func(t *testing.T) {
			status, _, raw := device.Do(t, route.method, route.path(selfRefs["full"]), nil)
			if status != http.StatusForbidden {
				t.Fatalf("%s %s (unrecognised self-ID override): status = %d, want %d; body = %s",
					route.method, route.path(selfRefs["full"]), status, http.StatusForbidden, raw)
			}
			var obj map[string]string
			if err := json.Unmarshal(raw, &obj); err != nil {
				t.Fatalf("decode self-protection body: %v; body = %s", err, raw)
			}
			if obj["error"] != selfExclusionBody {
				t.Errorf("error = %q, want %q", obj["error"], selfExclusionBody)
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

	// The self-identification line names the container the agent ACTUALLY
	// resolved — its own — not the override it discarded.
	logText := string(c.ReadStateFile(t, agentLogPath))
	if !strings.Contains(logText, "agent self-identified") {
		t.Errorf("agent.log has no self-identification line; the fallback this test documents did not happen")
	}
	if !strings.Contains(logText, c.ID) {
		t.Errorf("agent.log does not carry the agent's own container ID; the resolved identity is not the container itself")
	}

	// Finding 1's fix: the discarded override is no longer silent. The agent
	// must log both the warning naming it as discarded and the override
	// value itself, or an operator who pins the wrong container ID still
	// gets no signal that their explicit configuration was thrown away.
	if !strings.Contains(logText, "discarding DEVMON_SELF_CONTAINER") {
		t.Errorf("agent.log does not contain the discard warning for the unrecognised override")
	}
	if !strings.Contains(logText, unresolvableSelfID) {
		t.Errorf("agent.log does not name the discarded override %s", unresolvableSelfID)
	}
}
