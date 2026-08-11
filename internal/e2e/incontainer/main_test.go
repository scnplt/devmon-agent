// SPDX-License-Identifier: AGPL-3.0-only

//go:build e2e

// Package incontainer is the image group: it builds the shipped
// devmon-agent image the same way the Makefile and CI do (D22), runs it as a
// container the way compose.example.yaml documents, and drives it through
// docker exec and the published mTLS port. This is the only group that can
// exercise self-identification through mountinfo and the self-exclusion
// guarantee, because both depend on the agent actually running inside the
// container whose lifecycle it protects — a host-binary process has no
// container of its own to protect.
//
// What this package deliberately does NOT cover: everything that does not
// need a real container — the route-by-route contract, policy matrix, audit
// trail, and streaming — lives in the sibling internal/e2e/api package,
// which starts an order of magnitude faster and does not need a Linux
// Engine or the docker CLI.
package incontainer

import (
	"net/http"
	"testing"
	"time"

	"github.com/scnplt/devmon-agent/internal/e2e/harness"
)

// imageTag is the tag TestContainerSmoke builds once, reused by every
// test in the package. Tasks that need a second, differently-built image
// (the upgrade rehearsal) build their own tag rather than reusing this one.
const imageTag = "devmon-agent:e2e-incontainer"

// There is deliberately no TestMain here, and no startup sweep. An implicit
// "remove every container carrying the suite label" pass cannot tell a
// crashed previous run's leftovers from a CONCURRENT run's live containers,
// so it would force-remove the latter - and in this package that includes
// another run's running AGENT container, which carries the same label
// (harness/image.go). Every container is removed by the t.Cleanup that
// created it, by ID; cleaning up after a run that crashed hard enough to
// skip its own cleanups is an explicit operator action, `make e2e-clean`.

// TestContainerSmoke is this task's own falsifiability check as well as the
// group's acceptance test: it proves the whole chain the later,
// assertion-bearing tasks build on actually works — build, run, exec, read
// — before any self-exclusion or self-identification claim is layered on
// top of it. Tasks 10 and 11 own those claims; this test owns the plumbing.
func TestContainerSmoke(t *testing.T) {
	e := harness.RequireLinuxContainerEngine(t)
	harness.BuildImage(t, imageTag, nil)

	c := harness.RunAgentContainer(t, e, harness.ContainerAgentOptions{Image: imageTag})

	device := harness.PairDeviceInContainer(t, c, "smoke-test-device")

	status, obj := device.JSON(t, "GET", "/v1/containers")
	if status != 200 {
		t.Fatalf("GET /v1/containers through the published port: status = %d, want 200", status)
	}
	if _, ok := obj["items"]; !ok {
		t.Fatalf("GET /v1/containers response has no items key: %v", obj)
	}

	// Falsifiability: a client trusting the same pinned CA but presenting no
	// client certificate must NOT get the 200 the paired device above got —
	// proving the prior assertion actually depended on the mTLS pairing that
	// just happened, rather than the route being open by accident of test
	// ordering. Deriving trust from the real pairing response (rather than
	// InsecureSkipVerify) keeps this off D7's forbidden path: after pairing,
	// no client in the suite may skip TLS verification again.
	unauthTLS := device.TLSConfig()
	unauthTLS.Certificates = nil
	unauthClient := &http.Client{
		Timeout:   10 * time.Second,
		Transport: &http.Transport{TLSClientConfig: unauthTLS},
	}
	resp, err := unauthClient.Get(c.BaseURL + "/v1/containers")
	if err != nil {
		t.Fatalf("unauthenticated GET /v1/containers: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		t.Fatalf("unauthenticated GET /v1/containers: status = 200, want anything but 200 (pairing would be proven meaningless)")
	}

	running, restartCount := c.IsRunning(t)
	if !running {
		t.Fatalf("agent container %s is not running after the smoke sequence", c.ID)
	}
	if restartCount != 0 {
		t.Fatalf("agent container %s restart count = %d, want 0", c.ID, restartCount)
	}
}
