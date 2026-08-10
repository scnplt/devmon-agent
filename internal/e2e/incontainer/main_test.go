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
	"context"
	"fmt"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/moby/moby/client"

	"github.com/scnplt/devmon-agent/internal/e2e/harness"
)

// imageTag is the tag this package's TestMain builds once, reused by every
// test in the package. Tasks that need a second, differently-built image
// (the upgrade rehearsal) build their own tag rather than reusing this one.
const imageTag = "devmon-agent:e2e-incontainer"

// TestMain sweeps any container a previous, possibly crashed run of this
// package left behind, then runs the suite. It intentionally does not decide
// "no Engine, no docker CLI, skip everything" itself: that decision belongs
// to each test through harness.RequireLinuxContainerEngine (D5, D6),
// because only a *testing.T can turn a missing prerequisite into either a
// skip or a hard failure under DEVMON_E2E_REQUIRE=1. TestMain has no
// *testing.T to give it, so the sweep here is best-effort and silent when
// the Engine is absent — the same shape internal/e2e/api's TestMain uses.
func TestMain(m *testing.M) {
	sweepOrphansBeforeRun()
	os.Exit(m.Run())
}

// sweepOrphansBeforeRun removes every container carrying the suite's label
// (harness.LabelSuite), regardless of which run created it — the same
// filter harness.SweepOrphans uses, reimplemented here without a *testing.T
// because TestMain has none to give it. Nothing without that label is ever
// listed, let alone removed (D11); a container this package did not create
// is never touched.
func sweepOrphansBeforeRun() {
	host, skipReason := harness.EngineHost()
	if skipReason != "" {
		return // no Engine reachable here; every test skips for itself (D5)
	}

	cli, err := client.New(client.WithHost(host))
	if err != nil {
		return
	}
	defer func() { _ = cli.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := cli.Ping(ctx, client.PingOptions{NegotiateAPIVersion: true}); err != nil {
		return
	}

	filters := client.Filters{}.Add("label", harness.LabelSuite+"=1")
	result, err := cli.ContainerList(context.Background(), client.ContainerListOptions{All: true, Filters: filters})
	if err != nil {
		fmt.Fprintf(os.Stderr, "e2e/incontainer: list orphaned fixture containers: %v\n", err)
		return
	}
	for _, item := range result.Items {
		if _, err := cli.ContainerRemove(context.Background(), item.ID, client.ContainerRemoveOptions{
			Force:         true,
			RemoveVolumes: true,
		}); err != nil {
			fmt.Fprintf(os.Stderr, "e2e/incontainer: remove orphaned fixture container %s: %v\n", item.ID, err)
		}
	}
}

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
