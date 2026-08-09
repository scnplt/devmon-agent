//go:build e2e

// Package api is the host-binary group: it builds and runs the real
// devmon-agent binary, pairs through the documented host-side command path
// (device pair-code), and drives every route over mTLS. Every file in this
// package replays one section of a manual checklist from Phases 1-5 (PRD
// Phase 6) so it runs unattended against a real Docker Engine instead of a
// human with curl.
//
// What this package deliberately does NOT cover: anything that needs a
// containerised agent — self-identification via mountinfo, self-exclusion,
// crash-and-restart across a real `docker kill` — lives in the sibling
// internal/e2e/incontainer package instead, because only a real container
// gives those properties something to observe.
package api

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/moby/moby/client"

	"github.com/scnplt/devmon-agent/internal/e2e/harness"
)

// TestMain sweeps any container this package's own previous, possibly
// crashed run left behind before any test runs, then runs the suite. It
// intentionally does not itself decide "no Engine, skip everything": that
// decision belongs to each test through harness.RequireEngine (D5), because
// only a *testing.T can turn a missing Engine into either a skip or a hard
// failure under DEVMON_E2E_REQUIRE=1. TestMain has no *testing.T to give it,
// so the sweep here is best-effort and silent when the Engine is absent.
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
		fmt.Fprintf(os.Stderr, "e2e/api: list orphaned fixture containers: %v\n", err)
		return
	}
	for _, item := range result.Items {
		if _, err := cli.ContainerRemove(context.Background(), item.ID, client.ContainerRemoveOptions{
			Force:         true,
			RemoveVolumes: true,
		}); err != nil {
			fmt.Fprintf(os.Stderr, "e2e/api: remove orphaned fixture container %s: %v\n", item.ID, err)
		}
	}
}
