//go:build e2e

package incontainer

import (
	"bytes"
	"context"
	"net/http"
	"testing"

	"github.com/moby/moby/client"

	"github.com/scnplt/devmon-agent/internal/e2e/harness"
)

// This file covers the state-survival half of Task 11 (the earlier phases'
// persistence items, lifecycle-policy-and-audit.plan.md:1086-1126 and
// secure-foundation-and-persistence.plan.md:1216-1229): pre-kill log lines
// readable after a crash restart, both paired devices surviving a genuine
// image rebuild and container recreation against the same bind mount, and
// the negative case — a fresh state directory yields a different CA
// identity — that makes the positive case's claim meaningful rather than
// coincidental.

// stateImageTag is this file's own image tag (BuildImage is not memoized;
// image.go), distinct from every other *_test.go file's tag in this
// package.
const stateImageTag = "devmon-agent:e2e-state"

// TestStateSurvivesCrashRestart is Phase 1's outstanding manual item: log
// lines written before a `docker kill` are still readable after the
// container starts again, new lines append below them, and the paired
// device does not need to re-pair.
//
// GOTCHA (Task 11): KillContainer sends SIGKILL, the same signal `docker
// kill` sends — the agent gets no chance to flush anything on the way down.
// Softening this to a graceful stop would prove nothing about crash
// survival.
func TestStateSurvivesCrashRestart(t *testing.T) {
	e := harness.RequireLinuxContainerEngine(t)
	harness.BuildImage(t, stateImageTag, nil)

	c := harness.RunAgentContainer(t, e, harness.ContainerAgentOptions{Image: stateImageTag})
	device := harness.PairDeviceInContainer(t, c, "crash-survivor")

	// Drive a little traffic so there is something in the log before the
	// kill, and so the "still authenticates" assertion below is not vacuous.
	if status, obj := device.JSON(t, http.MethodGet, "/v1/containers"); status != http.StatusOK {
		t.Fatalf("pre-kill GET /v1/containers: status = %d, want %d; body = %v", status, http.StatusOK, obj)
	}
	preLog := c.ReadStateFile(t, agentLogPath)
	if len(preLog) == 0 {
		t.Fatalf("agent.log is empty before the kill; the rest of this test would be vacuous")
	}

	c.KillContainer(t)
	c.Restart(t) // ContainerRestart on an already-stopped container starts it in place.

	postLog := c.ReadStateFile(t, agentLogPath)
	if !bytes.HasPrefix(postLog, preLog) {
		t.Fatalf("post-restart agent.log (%d bytes) does not start with the pre-kill content (%d bytes): "+
			"pre-kill lines were lost rather than surviving on the bind mount", len(postLog), len(preLog))
	}
	if len(postLog) <= len(preLog) {
		t.Errorf("post-restart agent.log is not longer than the pre-kill log (%d vs %d bytes); no new lines appended below the old ones", len(postLog), len(preLog))
	}

	// The pre-kill device certificate must still work: same CA, same device
	// table, no restart-induced re-pair. Only the ADDRESS moves — the Engine
	// hands the restarted container a fresh ephemeral host port — so the
	// device is re-pointed while keeping its pinned CA and its certificate,
	// which is what the claim under test is about. A re-pair here would make
	// the assertion vacuous; RebindToURL deliberately cannot do that.
	device = harness.RebindToURL(device, c.BaseURL)
	if status, obj := device.JSON(t, http.MethodGet, "/v1/containers"); status != http.StatusOK {
		t.Errorf("post-restart GET /v1/containers with the pre-kill device certificate: status = %d, want %d; body = %v", status, http.StatusOK, obj)
	}

	rows := harness.ListDevices(t, &harness.Agent{StateDir: c.StateDir})
	found := false
	for _, row := range rows {
		if row.ID == device.ID {
			found = true
		}
	}
	if !found {
		t.Errorf("device %s is missing from `device list` after the crash restart", device.ID)
	}
}

// TestPairingsSurviveImageUpgrade is the PRD's "pairings surviving a
// restart and an image upgrade: 100%" success signal. It rebuilds the image
// with a different VERSION build arg and RECREATES the container — not a
// restart, which shares the writable layer and would prove nothing about
// the bind mount (Task 11's GOTCHA) — against the SAME host state
// directory, and asserts both previously paired devices still authenticate
// with no re-pair, the CA fingerprint is unchanged, and /v1/status reports
// the new version.
func TestPairingsSurviveImageUpgrade(t *testing.T) {
	e := harness.RequireLinuxContainerEngine(t)

	const tagV1 = "devmon-agent:e2e-state-upgrade-v1"
	const tagV2 = "devmon-agent:e2e-state-upgrade-v2"
	const versionV2 = "e2e-upgrade-v2"

	harness.BuildImage(t, tagV1, map[string]string{"VERSION": "e2e-upgrade-v1"})
	c1 := harness.RunAgentContainer(t, e, harness.ContainerAgentOptions{Image: tagV1})

	deviceA := harness.PairDeviceInContainer(t, c1, "upgrade-device-a")
	deviceB := harness.PairDeviceInContainer(t, c1, "upgrade-device-b")

	statusBefore, objBefore := deviceA.JSON(t, http.MethodGet, "/v1/status")
	if statusBefore != http.StatusOK {
		t.Fatalf("pre-upgrade GET /v1/status: status = %d, want %d", statusBefore, http.StatusOK)
	}
	fingerprintBefore, _ := objBefore["ca_fingerprint"].(string)
	if fingerprintBefore == "" {
		t.Fatalf("pre-upgrade ca_fingerprint is empty: %v", objBefore)
	}

	// Recreate: image.go's RunAgentContainer names every agent container it
	// creates identically within one test-binary run, so the old one must
	// be removed before a new one can reuse that name — independent of the
	// bind-mount claim this test exists to check.
	if _, err := e.ContainerRemove(context.Background(), c1.ID, client.ContainerRemoveOptions{Force: true}); err != nil {
		t.Fatalf("remove pre-upgrade agent container %s: %v", c1.ID, err)
	}

	harness.BuildImage(t, tagV2, map[string]string{"VERSION": versionV2})
	c2 := harness.RunAgentContainer(t, e, harness.ContainerAgentOptions{Image: tagV2, StateDir: c1.StateDir})

	deviceARebound := harness.RebindToURL(deviceA, c2.BaseURL)
	deviceBRebound := harness.RebindToURL(deviceB, c2.BaseURL)

	for name, d := range map[string]*harness.Device{"a": deviceARebound, "b": deviceBRebound} {
		if status, obj := d.JSON(t, http.MethodGet, "/v1/containers"); status != http.StatusOK {
			t.Errorf("post-upgrade GET /v1/containers for device %s: status = %d, want %d; body = %v", name, status, http.StatusOK, obj)
		}
	}

	statusAfter, objAfter := deviceARebound.JSON(t, http.MethodGet, "/v1/status")
	if statusAfter != http.StatusOK {
		t.Fatalf("post-upgrade GET /v1/status: status = %d, want %d", statusAfter, http.StatusOK)
	}
	if got, _ := objAfter["ca_fingerprint"].(string); got != fingerprintBefore {
		t.Errorf("post-upgrade ca_fingerprint = %q, want unchanged %q", got, fingerprintBefore)
	}
	if got, _ := objAfter["agent_version"].(string); got != versionV2 {
		t.Errorf("post-upgrade agent_version = %q, want %q — this is the recreated-image signal, not just a restart", got, versionV2)
	}
}

// TestStateBindMountIsTheOnlyDurableState is the negative case
// TestPairingsSurviveImageUpgrade's positive case needs to mean anything: a
// container recreated on a FRESH state directory gets a different CA
// identity. If the two fingerprints ever matched, the bind mount would not
// actually be the thing anchoring identity across a recreation — something
// else (the image, the container name, the Engine) would be, and the
// upgrade test's "unchanged fingerprint" assertion would not be evidence of
// what it claims to be evidence of.
func TestStateBindMountIsTheOnlyDurableState(t *testing.T) {
	e := harness.RequireLinuxContainerEngine(t)
	harness.BuildImage(t, stateImageTag, nil)

	c1 := harness.RunAgentContainer(t, e, harness.ContainerAgentOptions{Image: stateImageTag})
	device1 := harness.PairDeviceInContainer(t, c1, "bind-mount-baseline")

	status1, obj1 := device1.JSON(t, http.MethodGet, "/v1/status")
	if status1 != http.StatusOK {
		t.Fatalf("baseline GET /v1/status: status = %d, want %d", status1, http.StatusOK)
	}
	fingerprint1, _ := obj1["ca_fingerprint"].(string)
	if fingerprint1 == "" {
		t.Fatalf("baseline ca_fingerprint is empty: %v", obj1)
	}

	if _, err := e.ContainerRemove(context.Background(), c1.ID, client.ContainerRemoveOptions{Force: true}); err != nil {
		t.Fatalf("remove first agent container %s: %v", c1.ID, err)
	}

	// opts.StateDir left "" allocates a FRESH t.TempDir — never the same
	// bind mount as c1.
	c2 := harness.RunAgentContainer(t, e, harness.ContainerAgentOptions{Image: stateImageTag})
	device2 := harness.PairDeviceInContainer(t, c2, "bind-mount-fresh")

	status2, obj2 := device2.JSON(t, http.MethodGet, "/v1/status")
	if status2 != http.StatusOK {
		t.Fatalf("fresh-directory GET /v1/status: status = %d, want %d", status2, http.StatusOK)
	}
	fingerprint2, _ := obj2["ca_fingerprint"].(string)
	if fingerprint2 == "" {
		t.Fatalf("fresh-directory ca_fingerprint is empty: %v", obj2)
	}

	if fingerprint1 == fingerprint2 {
		t.Errorf("ca_fingerprint is identical (%s) across two containers on two different state directories; "+
			"identity is meant to be reprovisioned per state directory, and the fresh directory should have produced a new CA", fingerprint1)
	}
}
