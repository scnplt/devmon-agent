//go:build e2e

package incontainer

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/moby/moby/client"

	"github.com/scnplt/devmon-agent/internal/e2e/harness"
)

// This file is the PRD's headline metric — "agent surviving a delete attempt
// in the most permissive mode, 100%" — and the reason self-exclusion needs a
// containerised agent to test at all: the rule (internal/dockerx/lifecycle.go
// resolveTarget) compares the request's resolved container ID against the ID
// the agent discovered for ITSELF through /proc/self/mountinfo
// (internal/dockerx/self.go), which is only non-empty when the agent
// genuinely runs as a container. A host-binary agent (internal/e2e/api) has
// no container of its own to protect, so it can never exercise this path.
// lifecycle-policy-and-audit.plan.md:1086-1126.
//
// What this file deliberately does NOT cover: hostname-override and
// unresolvable-override self-identification variants — proving the *right*
// container was found, rather than that finding it refuses the right
// requests — belong to contract_selfid_test.go (Task 11). This file assumes
// auto-detection resolved correctly and is entirely about the refusal.

// selfExclusionBody is the exact error body internal/httpapi/reads.go serves
// for dockerx.ErrSelfProtected. Declared locally rather than imported (D4):
// the suite must notice a wording change, which it cannot do if it shares the
// constant with the server that produces it.
const selfExclusionBody = "the agent cannot act on itself"

// policyForbiddenBody is httpapi's SEPARATE 403 body for a policy-mode
// refusal (internal/httpapi/policygate.go). TestSelfExclusionIsNotAPolicyTier
// asserts a self-refusal body never equals this one, even though both paths
// answer the same 403 status code — the two mechanisms are not the same
// thing wearing one status code.
const policyForbiddenBody = "operation not permitted by host policy"

// outcomeDeniedSelf is the audit outcome internal/httpapi/reads.go records
// for a self-protection refusal (internal/state/audit.go's OutcomeDeniedSelf
// constant). Declared locally for the same D4 reason as selfExclusionBody.
const outcomeDeniedSelf = "denied_self"

// selfExclusionImageTag is this file's own image tag, distinct from
// imageTag (main_test.go / TestContainerSmoke): BuildImage is not memoized
// (image.go), so every test in this package builds its own — cheaply, since
// Docker's own layer cache makes a repeat build of an unchanged context fast.
const selfExclusionImageTag = "devmon-agent:e2e-selfexclusion"

// refFormOrder fixes the iteration order over the three reference forms so
// subtest names are stable across runs.
var refFormOrder = []string{"name", "short", "full"}

// refForms returns id's three reference forms: its container name with
// Docker's leading "/" stripped (dockerx.ValidateRef's pattern
// (internal/dockerx/ref.go) rejects a leading slash, so the raw
// InspectResponse.Name would fail validation before self-exclusion ever
// runs), its 12-character short ID, and its 64-character full ID. Built from
// ContainerInspect, never from `docker ps` output or $HOSTNAME (Task 10's
// GOTCHA).
func refForms(t *testing.T, e *client.Client, id string) map[string]string {
	t.Helper()

	if len(id) != 64 {
		t.Fatalf("container ID %q is not a 64-character full ID", id)
	}
	inspect, err := e.ContainerInspect(context.Background(), id, client.ContainerInspectOptions{})
	if err != nil {
		t.Fatalf("inspect container %s: %v", id, err)
	}
	name := strings.TrimPrefix(inspect.Container.Name, "/")

	return map[string]string{
		"name":  name,
		"short": id[:12],
		"full":  id,
	}
}

// lifecycleRoute is one of the five mutating routes under test.
type lifecycleRoute struct {
	operation string // policy.Operation value, matched against the audit trail
	method    string
	path      func(ref string) string
}

var selfExclusionRoutes = []lifecycleRoute{
	{"start", http.MethodPost, func(ref string) string { return "/v1/containers/" + ref + "/start" }},
	{"restart", http.MethodPost, func(ref string) string { return "/v1/containers/" + ref + "/restart" }},
	{"stop", http.MethodPost, func(ref string) string { return "/v1/containers/" + ref + "/stop" }},
	{"kill", http.MethodPost, func(ref string) string { return "/v1/containers/" + ref + "/kill" }},
	{"delete", http.MethodDelete, func(ref string) string { return "/v1/containers/" + ref }},
}

// waitFixtureRunning polls the Engine (never the agent) until id reports a
// running state, mirroring internal/e2e/api/contract_lifecycle_test.go's
// waitContainerRunning: ContainerStart returns before the container has
// necessarily reached that state.
func waitFixtureRunning(t *testing.T, e *client.Client, id string) {
	t.Helper()
	deadline := time.Now().Add(containerReadinessTimeoutForFixtures)
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		res, err := e.ContainerInspect(ctx, id, client.ContainerInspectOptions{})
		cancel()
		if err == nil && res.Container.State != nil && res.Container.State.Running {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("fixture container %s did not reach running state within %s", id, containerReadinessTimeoutForFixtures)
}

// containerReadinessTimeoutForFixtures bounds waitFixtureRunning. Named
// distinctly from harness's own containerReadinessTimeout (image.go), which
// this package cannot reach (unexported) and does not need to: fixture
// containers are plain busybox processes, not the agent itself.
const containerReadinessTimeoutForFixtures = 20 * time.Second

// TestAgentRefusesToActOnItself is the PRD's headline metric: a full-mode
// agent — the most permissive policy tier there is — refuses all five
// lifecycle routes against its own container, by every one of the three
// reference forms a client could plausibly send, and is still running,
// unrestarted, and answering afterward. Fifteen cells, all the same answer.
func TestAgentRefusesToActOnItself(t *testing.T) {
	e := harness.RequireLinuxContainerEngine(t)
	harness.BuildImage(t, selfExclusionImageTag, nil)

	c := harness.RunAgentContainer(t, e, harness.ContainerAgentOptions{
		Image:      selfExclusionImageTag,
		PolicyMode: "full",
	})
	device := harness.PairDeviceInContainer(t, c, "self-exclusion-device")

	selfRefs := refForms(t, e, c.ID)

	for _, route := range selfExclusionRoutes {
		for _, form := range refFormOrder {
			ref := selfRefs[form]
			t.Run(route.operation+"/"+form, func(t *testing.T) {
				status, _, raw := device.Do(t, route.method, route.path(ref), nil)
				if status != http.StatusForbidden {
					t.Fatalf("%s %s (self, by %s): status = %d, want %d; body = %s",
						route.method, route.path(ref), form, status, http.StatusForbidden, raw)
				}

				var obj map[string]string
				if err := json.Unmarshal(raw, &obj); err != nil {
					t.Fatalf("decode self-protection body for %s %s: %v; body = %s", route.method, route.path(ref), err, raw)
				}
				if obj["error"] != selfExclusionBody {
					t.Errorf("%s %s (self, by %s): error = %q, want %q", route.method, route.path(ref), form, obj["error"], selfExclusionBody)
				}
			})
		}
	}

	// The PRD metric itself: still running, no restart, still answering —
	// asked of the ENGINE, never the agent (Task 10's GOTCHA). An agent that
	// answered 403 fifteen times and then died would pass a version of this
	// assertion that only asked the agent's own HTTP port.
	running, restartCount := c.IsRunning(t)
	if !running {
		t.Fatalf("agent container %s is not running after the fifteen self-exclusion attempts", c.ID)
	}
	if restartCount != 0 {
		t.Errorf("agent container %s restart count = %d, want 0", c.ID, restartCount)
	}
	if status, _, raw := device.Do(t, http.MethodGet, "/v1/status", nil); status != http.StatusOK {
		t.Errorf("GET /v1/status after the fifteen self-exclusion attempts: status = %d, want %d; body = %s", status, http.StatusOK, raw)
	}

	// Falsifiability (Task 10's GOTCHA): repeat the identical fifteen cells
	// against FIXTURE containers instead of the agent's own. If a client bug
	// made every mutating request fail regardless of target, the fifteen 403s
	// above would prove nothing about self-exclusion specifically. This half
	// pins the earlier refusals to the TARGET, by showing the exact same
	// client, the exact same routes, and the exact same reference forms
	// succeed the moment the target is not the agent.
	assertLifecycleRoutesSucceedAgainstFixtures(t, e, device)
}

// assertLifecycleRoutesSucceedAgainstFixtures runs the same fifteen
// (route, reference-form) cells TestAgentRefusesToActOnItself just ran
// against the agent, against a fresh fixture container per cell, and asserts
// every one succeeds. A fresh fixture per cell avoids one call's side effect
// (a kill, a delete) invalidating the next cell's expected starting state.
func assertLifecycleRoutesSucceedAgainstFixtures(t *testing.T, e *client.Client, device *harness.Device) {
	t.Helper()

	for _, route := range selfExclusionRoutes {
		for _, form := range refFormOrder {
			t.Run("falsifiability/"+route.operation+"/"+form, func(t *testing.T) {
				// start and delete need a container that begins stopped:
				// starting an already-running one is merely idempotent, and
				// deleting a running one conflicts (dockerx.ErrConflict) for a
				// reason that has nothing to do with self-exclusion.
				stopped := route.operation == "start" || route.operation == "delete"

				id := harness.StartFixture(t, e, harness.FixtureOptions{
					NameSuffix: "selfexcl-" + route.operation + "-" + form,
					Stopped:    stopped,
				})
				if !stopped {
					waitFixtureRunning(t, e, id)
				}
				ref := refForms(t, e, id)[form]

				status, _, raw := device.Do(t, route.method, route.path(ref), nil)
				if status != http.StatusNoContent {
					t.Fatalf("%s %s (fixture, by %s): status = %d, want %d; body = %s — "+
						"a refusal here would mean the earlier self-refusals proved nothing about the TARGET",
						route.method, route.path(ref), form, status, http.StatusNoContent, raw)
				}
			})
		}
	}
}

// TestAgentRowIsMarkedProtected asserts GET /v1/containers?all=true shows the
// agent's own row with "protected": true and every other row "protected":
// false, present in the JSON in both cases — dockerx/types.go's
// ContainerSummary.Protected field has no omitempty specifically so a client
// can never mistake an absent key for "not protected".
func TestAgentRowIsMarkedProtected(t *testing.T) {
	e := harness.RequireLinuxContainerEngine(t)
	harness.BuildImage(t, selfExclusionImageTag, nil)

	c := harness.RunAgentContainer(t, e, harness.ContainerAgentOptions{
		Image:      selfExclusionImageTag,
		PolicyMode: "full",
	})
	device := harness.PairDeviceInContainer(t, c, "protected-row-device")

	fixtureID := harness.StartFixture(t, e, harness.FixtureOptions{NameSuffix: "protected-row"})
	waitFixtureRunning(t, e, fixtureID)

	status, obj := device.JSON(t, http.MethodGet, "/v1/containers?all=true")
	if status != http.StatusOK {
		t.Fatalf("GET /v1/containers?all=true: status = %d, want %d", status, http.StatusOK)
	}
	harness.AssertExactKeys(t, obj, []string{"items", "truncated"})

	items, ok := obj["items"].([]any)
	if !ok {
		t.Fatalf("GET /v1/containers?all=true: items is not a JSON array: %#v", obj["items"])
	}

	agentRow := findRowByID(t, items, c.ID)
	if protected, ok := agentRow["protected"]; !ok {
		t.Errorf("agent's own row has no %q key at all", "protected")
	} else if protected != true {
		t.Errorf("agent's own row: protected = %v, want true", protected)
	}

	fixtureRow := findRowByID(t, items, fixtureID)
	if protected, ok := fixtureRow["protected"]; !ok {
		t.Errorf("fixture row has no %q key at all", "protected")
	} else if protected != false {
		t.Errorf("fixture row: protected = %v, want false", protected)
	}
}

// findRowByID scans a decoded "items" array for the object whose "id" field
// equals want, failing the test rather than returning a zero value if it is
// absent — a missing row is itself the finding for every caller here.
func findRowByID(t *testing.T, items []any, want string) map[string]any {
	t.Helper()
	for _, raw := range items {
		obj, ok := raw.(map[string]any)
		if !ok {
			t.Fatalf("item is not a JSON object: %#v", raw)
		}
		if obj["id"] == want {
			return obj
		}
	}
	t.Fatalf("no item with id = %q found among %d items", want, len(items))
	return nil
}

// TestSelfExclusionIsNotAPolicyTier proves the refusal is a fixed rule, not a
// mode a stricter policy happens to also deny: it runs against a DEFAULT-mode
// agent (DEVMON_POLICY_MODE unset), where policy itself explicitly PERMITS
// restart and stop (internal/policy/mode.go) — so a self-refusal here cannot
// be the policy gate rejecting the operation; only the self-exclusion rule,
// deeper in the handler, produces it. The 403 body is asserted to be
// selfExclusionBody, and explicitly NOT policyForbiddenBody, which is what a
// policy-mode denial (e.g. this same agent's kill or delete) would answer
// instead.
func TestSelfExclusionIsNotAPolicyTier(t *testing.T) {
	e := harness.RequireLinuxContainerEngine(t)
	harness.BuildImage(t, selfExclusionImageTag, nil)

	c := harness.RunAgentContainer(t, e, harness.ContainerAgentOptions{
		Image: selfExclusionImageTag, // PolicyMode "" -> DEVMON_POLICY_MODE unset, the default tier
	})
	device := harness.PairDeviceInContainer(t, c, "not-a-policy-tier-device")

	selfRefs := refForms(t, e, c.ID)

	// restart and stop are both permitted operations under the default
	// policy tier (internal/policy/mode.go's defaultOperations) — if this
	// refusal were policy doing the work, these two would succeed.
	for _, op := range []string{"restart", "stop"} {
		var route lifecycleRoute
		for _, r := range selfExclusionRoutes {
			if r.operation == op {
				route = r
			}
		}

		t.Run(op, func(t *testing.T) {
			status, _, raw := device.Do(t, route.method, route.path(selfRefs["full"]), nil)
			if status != http.StatusForbidden {
				t.Fatalf("%s %s under default policy (self): status = %d, want %d; body = %s",
					route.method, route.path(selfRefs["full"]), status, http.StatusForbidden, raw)
			}

			var obj map[string]string
			if err := json.Unmarshal(raw, &obj); err != nil {
				t.Fatalf("decode self-protection body: %v; body = %s", err, raw)
			}
			if obj["error"] != selfExclusionBody {
				t.Errorf("error = %q, want %q (the self-exclusion rule's own message)", obj["error"], selfExclusionBody)
			}
			if obj["error"] == policyForbiddenBody {
				t.Errorf("error = %q, which is the POLICY-DENIAL message; this operation is permitted by default policy, "+
					"so a policy-shaped refusal here would mean the wrong mechanism produced the 403", obj["error"])
			}
		})
	}
}

// TestAuditRecordsSelfRefusals asserts each self-exclusion refusal leaves
// exactly one audit row with outcome denied_self — the audit trail's own
// half of the same guarantee: an operator reviewing `audit list` must be able
// to see that a self-targeting attempt was made and refused, not silence.
func TestAuditRecordsSelfRefusals(t *testing.T) {
	e := harness.RequireLinuxContainerEngine(t)
	harness.BuildImage(t, selfExclusionImageTag, nil)

	c := harness.RunAgentContainer(t, e, harness.ContainerAgentOptions{
		Image:      selfExclusionImageTag,
		PolicyMode: "full",
	})
	device := harness.PairDeviceInContainer(t, c, "audit-self-refusal-device")

	selfRefs := refForms(t, e, c.ID)

	// One attempt per reference form is enough to prove the audit half of
	// the guarantee; TestAgentRefusesToActOnItself already proves the wire
	// contract for all five routes.
	const op = "kill"
	var route lifecycleRoute
	for _, r := range selfExclusionRoutes {
		if r.operation == op {
			route = r
		}
	}

	for _, form := range refFormOrder {
		ref := selfRefs[form]
		status, _, raw := device.Do(t, route.method, route.path(ref), nil)
		if status != http.StatusForbidden {
			t.Fatalf("%s %s (self, by %s): status = %d, want %d; body = %s",
				route.method, route.path(ref), form, status, http.StatusForbidden, raw)
		}
	}

	// The CLI's DEVMON_STATE_DIR requirement is all runCLI reads off the
	// *harness.Agent it is given (internal/e2e/harness/cli.go); the agent
	// container's state directory is the same host path RunAgentContainer
	// bind-mounted in, so a minimal Agent value carrying just that field lets
	// this test reuse harness.ListAudit verbatim instead of duplicating the
	// CLI subprocess and tabwriter-parsing logic here.
	target := &harness.Agent{StateDir: c.StateDir}
	rows := harness.ListAudit(t, target, len(refFormOrder)+5)

	found := 0
	for _, form := range refFormOrder {
		ref := selfRefs[form]
		row := findAuditRowByTarget(t, rows, ref)
		if row.Operation != op {
			t.Errorf("audit row for target %q: operation = %q, want %q", ref, row.Operation, op)
		}
		if row.Outcome != outcomeDeniedSelf {
			t.Errorf("audit row for target %q: outcome = %q, want %q", ref, row.Outcome, outcomeDeniedSelf)
		}
		found++
	}
	if found != len(refFormOrder) {
		t.Errorf("found %d of %d expected self-refusal audit rows", found, len(refFormOrder))
	}
}

// findAuditRowByTarget scans rows for the one whose Target column equals
// ref — the audit trail records the reference exactly as the device supplied
// it (internal/state/audit.go's AuditEntry.Target doc comment), which is what
// makes the three reference forms distinguishable in the trail at all.
func findAuditRowByTarget(t *testing.T, rows []harness.AuditRow, ref string) harness.AuditRow {
	t.Helper()
	for _, row := range rows {
		if row.Target == ref {
			return row
		}
	}
	t.Fatalf("no audit row found with target = %q among %d rows", ref, len(rows))
	return harness.AuditRow{}
}
