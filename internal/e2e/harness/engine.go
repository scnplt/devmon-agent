//go:build e2e

package harness

import (
	"context"
	"fmt"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"
)

// Environment variables read by the harness itself (D13 of the phase plan).
// They share the DEVMON_ prefix for discoverability, but they are NOT part of
// the agent's own configuration surface: the agent's environment is built
// explicitly for every subprocess (see buildAgentEnv in agent.go) and never
// sees these.
const (
	envRequire    = "DEVMON_E2E_REQUIRE"     // turns a skip into a hard failure
	envDockerHost = "DEVMON_E2E_DOCKER_HOST" // overrides the Engine endpoint the harness probes
	envKeep       = "DEVMON_E2E_KEEP"        // skips fixture and state cleanup for post-mortem inspection
)

// defaultEngineHost is the only Docker Engine endpoint the harness assumes
// without being told otherwise. It matches internal/config's own default and
// its unix/tcp-only scheme check, which is why the harness never tries an
// npipe endpoint (D6).
const defaultEngineHost = "unix:///var/run/docker.sock"

// pingTimeout bounds how long the harness waits for an Engine to answer
// before treating it as unreachable.
const pingTimeout = 5 * time.Second

// Suite-wide fixture labels (D11, the LABELLED_FIXTURE pattern). Every
// container the harness creates carries both; cleanup only ever removes
// containers matching LabelSuite, so the suite can never touch a container it
// did not create.
const (
	LabelSuite = "com.devmon.e2e"
	LabelRun   = "com.devmon.e2e.run"
)

// defaultFixtureImage is the pinned image StartFixture uses when the caller
// does not name one. Pinning the tag keeps fixture behaviour identical across
// runs and hosts.
const defaultFixtureImage = "busybox:1.36.1"

// runID identifies this test binary invocation. It is embedded in the
// LabelRun value of every fixture this process creates, so a concurrent run
// on the same host cannot delete another run's containers.
var runID = fmt.Sprintf("%d-%d", os.Getpid(), time.Now().UnixNano())

// EngineHost returns the Docker Engine endpoint the harness will try. When
// running is not possible at all — Windows without an explicit
// DEVMON_E2E_DOCKER_HOST — it returns a skip reason instead: Windows-native
// has no documented path, since internal/config accepts only the unix and tcp
// schemes and Docker Desktop's default is npipe (D6).
func EngineHost() (host, skipReason string) {
	if h := strings.TrimSpace(os.Getenv(envDockerHost)); h != "" {
		return h, ""
	}
	if runtime.GOOS == "windows" {
		return "", "Windows-native running is not supported; run from WSL2 " +
			"(make e2e from a WSL shell), or set " + envDockerHost + " to a reachable tcp:// endpoint"
	}
	return defaultEngineHost, ""
}

// RequireEngine skips the calling test — or fails it, when DEVMON_E2E_REQUIRE=1
// — when no Docker Engine answers a ping. A suite that quietly does nothing
// is worse than no suite, because a green run is what flips a PRD row to
// complete (D5).
func RequireEngine(t *testing.T) *client.Client {
	t.Helper()

	host, skipReason := EngineHost()
	if skipReason != "" {
		skipOrFail(t, skipReason)
		return nil
	}

	cli, err := client.New(client.WithHost(host))
	if err != nil {
		skipOrFail(t, fmt.Sprintf("Docker Engine at %s: building client: %v", host, err))
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), pingTimeout)
	defer cancel()
	if _, err := cli.Ping(ctx, client.PingOptions{NegotiateAPIVersion: true}); err != nil {
		skipOrFail(t, fmt.Sprintf("Docker Engine at %s did not answer a ping: %v", host, err))
		return nil
	}

	t.Cleanup(func() { _ = cli.Close() })
	return cli
}

// skipOrFail implements D5: a missing Engine is a skip everywhere except when
// the suite's own DEVMON_E2E_REQUIRE=1 is set, which CI does.
func skipOrFail(t *testing.T, reason string) {
	t.Helper()
	if os.Getenv(envRequire) == "1" {
		t.Fatalf("%s (required by %s=1)", reason, envRequire)
	}
	t.Skip(reason)
}

// FixtureOptions describes one throwaway container the suite creates against
// the real Engine.
type FixtureOptions struct {
	Image       string
	NameSuffix  string
	Cmd         []string
	Env         []string
	Labels      map[string]string
	Healthcheck *container.HealthConfig
	Stopped     bool // create but do not start
}

// StartFixture creates (and, unless Stopped, starts) one labelled container
// and registers a t.Cleanup that removes it by ID. It never removes anything
// it did not just create (D11).
func StartFixture(t *testing.T, cli *client.Client, opts FixtureOptions) string {
	t.Helper()
	ctx := context.Background()

	image := opts.Image
	if image == "" {
		image = defaultFixtureImage
	}
	ensureImage(t, cli, image)

	cmd := opts.Cmd
	if len(cmd) == 0 {
		cmd = []string{"sleep", "300"}
	}

	labels := map[string]string{LabelSuite: "1", LabelRun: runID}
	for k, v := range opts.Labels {
		labels[k] = v
	}

	name := fmt.Sprintf("devmon-e2e-%s", runID)
	if opts.NameSuffix != "" {
		name += "-" + opts.NameSuffix
	}

	created, err := cli.ContainerCreate(ctx, client.ContainerCreateOptions{
		Name: name,
		Config: &container.Config{
			Image:       image,
			Cmd:         cmd,
			Env:         opts.Env,
			Labels:      labels,
			Healthcheck: opts.Healthcheck,
		},
	})
	if err != nil {
		t.Fatalf("create fixture container %s: %v", name, err)
	}
	t.Cleanup(func() { removeFixture(t, cli, created.ID) })

	if !opts.Stopped {
		if _, err := cli.ContainerStart(ctx, created.ID, client.ContainerStartOptions{}); err != nil {
			t.Fatalf("start fixture container %s: %v", name, err)
		}
	}

	return created.ID
}

// ensureImage pulls ref only when it is not already present, so a suite run
// against a warm cache does not pay a network round trip every time.
func ensureImage(t *testing.T, cli *client.Client, ref string) {
	t.Helper()
	ctx := context.Background()

	if _, err := cli.ImageInspect(ctx, ref); err == nil {
		return
	}

	rc, err := cli.ImagePull(ctx, ref, client.ImagePullOptions{})
	if err != nil {
		t.Fatalf("pull fixture image %s: %v", ref, err)
	}
	defer func() { _ = rc.Close() }()
	if err := rc.Wait(ctx); err != nil {
		t.Fatalf("pull fixture image %s: %v", ref, err)
	}
}

// removeFixture force-removes id, unless DEVMON_E2E_KEEP=1 asked the suite to
// leave fixtures in place for a failed run's post-mortem. That escape hatch
// must never be the default (D13's DEVMON_E2E_KEEP).
func removeFixture(t *testing.T, cli *client.Client, id string) {
	t.Helper()
	if os.Getenv(envKeep) == "1" {
		t.Logf("%s=1: leaving fixture container %s in place", envKeep, id)
		return
	}
	if _, err := cli.ContainerRemove(context.Background(), id, client.ContainerRemoveOptions{
		Force:         true,
		RemoveVolumes: true,
	}); err != nil {
		t.Logf("remove fixture container %s: %v", id, err)
	}
}
