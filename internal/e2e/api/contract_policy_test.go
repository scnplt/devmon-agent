// SPDX-License-Identifier: AGPL-3.0-only

//go:build e2e

package api

import (
	"net/http"
	"testing"

	"github.com/scnplt/devmon-agent/internal/e2e/harness"
)

// This file replays the policy-mode half of Phase 5's manual checklist: the
// three named tiers (read-only, default, full) each answer a fixed status
// per lifecycle operation, and reads/logs stay at 200 regardless of tier —
// read-only restricts mutation, never visibility.
// lifecycle-policy-and-audit.plan.md:1086-1126, and specifically the two
// checklist rows "Start the agent with DEVMON_POLICY_MODE=read-only; all
// five routes answer 403 and reads still answer 200" and "Start with
// DEVMON_POLICY_MODE unset; restart/stop/start work, kill/delete are
// refused".
//
// What this file deliberately does NOT cover: the exact curl-script sequence
// (idempotent start, the delete-while-running conflict) is
// contract_lifecycle_test.go; self-exclusion — a fixed rule, not a policy
// tier — is internal/e2e/incontainer (Tasks 9-11).

// TestKillIsPermittedOnlyInFullMode is the single clearest cell of the
// matrix on its own: kill is refused in default mode and permitted in full
// mode, nothing in between.
func TestKillIsPermittedOnlyInFullMode(t *testing.T) {
	t.Parallel()
	engine := harness.RequireEngine(t)

	tests := []struct {
		name       string
		suffix     string
		policyMode string
		want       int
	}{
		{name: "default mode refuses kill", suffix: "default", policyMode: "", want: http.StatusForbidden},
		{name: "full mode permits kill", suffix: "full", policyMode: "full", want: http.StatusNoContent},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			a := harness.StartAgent(t, harness.AgentOptions{PolicyMode: tt.policyMode})
			d := harness.PairDevice(t, a, "lifecycle-kill-"+tt.suffix)

			id := harness.StartFixture(t, engine, harness.FixtureOptions{NameSuffix: "kill-" + tt.suffix})
			waitContainerRunning(t, engine, id)

			status, _, raw := d.Do(t, http.MethodPost, "/v1/containers/"+id+"/kill", nil)
			if status != tt.want {
				t.Fatalf("POST .../kill status = %d, want %d; body = %s", status, tt.want, raw)
			}
			if status == http.StatusNoContent {
				assertNoBody(t, raw)
			}
		})
	}
}

// policyCell is one (operation, expected status) pair inside one mode's row
// of TestPolicyMatrix.
type policyCell struct {
	op     string // used only for the fixture name suffix and subtest name
	method string
	suffix string // appended to /v1/containers/{id}; "" for DELETE /v1/containers/{id} itself
	want   int
}

// TestPolicyMatrix asserts the exact status per (mode, operation) cell —
// three modes times five mutating operations, plus a read and a logs check
// per mode that must answer 200 regardless of tier. Every cell gets its own
// freshly created, labelled fixture (Task 7's gotcha: no cell may share
// mutable state with another, or an earlier cell's mutation would make a
// later cell's outcome depend on run order).
func TestPolicyMatrix(t *testing.T) {
	t.Parallel()
	engine := harness.RequireEngine(t)

	matrix := []struct {
		label string
		mode  string // AgentOptions.PolicyMode; "" leaves DEVMON_POLICY_MODE unset
		cells []policyCell
	}{
		{
			label: "read-only",
			mode:  "read-only",
			cells: []policyCell{
				{op: "start", method: http.MethodPost, suffix: "/start", want: http.StatusForbidden},
				{op: "restart", method: http.MethodPost, suffix: "/restart", want: http.StatusForbidden},
				{op: "stop", method: http.MethodPost, suffix: "/stop", want: http.StatusForbidden},
				{op: "kill", method: http.MethodPost, suffix: "/kill", want: http.StatusForbidden},
				{op: "delete", method: http.MethodDelete, suffix: "", want: http.StatusForbidden},
			},
		},
		{
			label: "default",
			mode:  "",
			cells: []policyCell{
				{op: "start", method: http.MethodPost, suffix: "/start", want: http.StatusNoContent},
				{op: "restart", method: http.MethodPost, suffix: "/restart", want: http.StatusNoContent},
				{op: "stop", method: http.MethodPost, suffix: "/stop", want: http.StatusNoContent},
				{op: "kill", method: http.MethodPost, suffix: "/kill", want: http.StatusForbidden},
				{op: "delete", method: http.MethodDelete, suffix: "", want: http.StatusForbidden},
			},
		},
		{
			label: "full",
			mode:  "full",
			cells: []policyCell{
				{op: "start", method: http.MethodPost, suffix: "/start", want: http.StatusNoContent},
				{op: "restart", method: http.MethodPost, suffix: "/restart", want: http.StatusNoContent},
				{op: "stop", method: http.MethodPost, suffix: "/stop", want: http.StatusNoContent},
				{op: "kill", method: http.MethodPost, suffix: "/kill", want: http.StatusNoContent},
				{op: "delete", method: http.MethodDelete, suffix: "", want: http.StatusNoContent},
			},
		},
	}

	for _, m := range matrix {
		t.Run(m.label, func(t *testing.T) {
			t.Parallel()

			a := harness.StartAgent(t, harness.AgentOptions{PolicyMode: m.mode})
			d := harness.PairDevice(t, a, "policy-matrix-"+m.label)

			for _, c := range m.cells {
				t.Run(c.op, func(t *testing.T) {
					t.Parallel()

					// A cell whose operation is a PERMITTED delete needs a
					// stopped fixture (RemoveContainer conflicts on a
					// running one — see TestDeleteRunningContainerConflicts
					// in contract_lifecycle_test.go); every other cell needs
					// a genuinely running one, including a DENIED delete,
					// where the container's state is irrelevant to the
					// policy gate but must still exist to be addressed.
					wantsStoppedDelete := c.op == "delete" && c.want == http.StatusNoContent
					id := harness.StartFixture(t, engine, harness.FixtureOptions{
						NameSuffix: "matrix-" + m.label + "-" + c.op,
						Stopped:    wantsStoppedDelete,
					})
					if !wantsStoppedDelete {
						waitContainerRunning(t, engine, id)
					}

					status, _, raw := d.Do(t, c.method, "/v1/containers/"+id+c.suffix, nil)
					if status != c.want {
						t.Fatalf("%s policy, %s: status = %d, want %d; body = %s", m.label, c.op, status, c.want, raw)
					}
					if status == http.StatusNoContent {
						assertNoBody(t, raw)
					}
				})
			}

			// Reads and logs answer 200 under every policy mode — visibility
			// is never restricted, only mutation (mirrors
			// TestReadOnlyModePermitsAllReads in contract_reads_test.go,
			// repeated here for default and full so the claim is checked
			// against all three tiers, not just the most restrictive one).
			readID := harness.StartFixture(t, engine, harness.FixtureOptions{NameSuffix: "matrix-" + m.label + "-read"})
			if status, _, raw := d.Do(t, http.MethodGet, "/v1/containers", nil); status != http.StatusOK {
				t.Errorf("%s policy, read: GET /v1/containers status = %d, want %d; body = %s", m.label, status, http.StatusOK, raw)
			}
			if status, _, raw := d.Do(t, http.MethodGet, "/v1/containers/"+readID+"/logs", nil); status != http.StatusOK {
				t.Errorf("%s policy, logs: GET .../logs status = %d, want %d; body = %s", m.label, status, http.StatusOK, raw)
			}
		})
	}
}

// The "protected" field on a container row is Phase 5's other outstanding
// checklist item this task owns a slice of. The host-binary group's own
// fixtures are never self-excluded, so the only value observable here is
// "false", already asserted (both list and inspect projections) by
// TestContainerReadContractKeys in contract_reads_test.go — repeating an
// identical assertion in this file would add no falsifiability. The value
// this field takes for the agent's OWN row ("true", the PRD's headline
// signal) needs a containerised agent to observe at all and is
// TestAgentRowIsMarkedProtected, internal/e2e/incontainer, Task 10.
