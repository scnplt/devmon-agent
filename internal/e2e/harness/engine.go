// SPDX-License-Identifier: AGPL-3.0-only

//go:build e2e

package harness

import (
	"context"
	"fmt"
	"os"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"
)

// Environment variables read by the harness itself (D13).
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
// is worse than no suite, because a green run is what marks a phase
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

// RequireEngineAPIAtLeast skips the calling test when cli's negotiated
// Docker Engine API version is older than minVersion (a "major.minor"
// string, e.g. "1.52"). Comparison is numeric on the major and minor
// components, never a string comparison — "1.5" would otherwise sort after
// "1.48".
//
// cli must come from RequireEngine, called first by the caller (see
// RequireLinuxContainerEngine for the same composition pattern). This
// function never dials its own connection and never decides "no Engine
// answered" — that decision, and its DEVMON_E2E_REQUIRE=1 hard-failure path,
// belongs entirely to RequireEngine/skipOrFail. If cli is nil, the caller has
// already been skipped or failed by RequireEngine and this is a no-op.
//
// A Ping error here — on an Engine that already answered RequireEngine's own
// ping moments earlier — is not a capability gap, so it is routed through
// skipOrFail, not t.Skipf: DEVMON_E2E_REQUIRE=1 should still turn "Engine
// stopped answering" into a hard failure.
//
// Only the actual version comparison — and an unparseable version string —
// calls t.Skipf DIRECTLY, never skipOrFail: DEVMON_E2E_REQUIRE=1 exists to
// turn "no Engine answered" into a hard failure so a silently-passing suite
// cannot mark a phase complete (D5). An Engine that DID answer but predates the API
// version carrying the field under test is not a missing Engine — it is a
// genuine capability gap in the Engine itself, and no amount of
// DEVMON_E2E_REQUIRE=1 can make an old Engine speak a field it never sends.
// Forcing that case to a hard failure would make CI red on Engine versions
// the project never claimed to support, for a reason unrelated to the code
// under test.
func RequireEngineAPIAtLeast(t *testing.T, cli *client.Client, minVersion string) {
	t.Helper()

	if cli == nil {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), pingTimeout)
	defer cancel()
	result, err := cli.Ping(ctx, client.PingOptions{NegotiateAPIVersion: true})
	if err != nil {
		skipOrFail(t, fmt.Sprintf("Docker Engine did not answer a ping: %v", err))
		return
	}

	gotMajor, gotMinor, err := parseAPIVersion(result.APIVersion)
	if err != nil {
		t.Skipf("Docker Engine reported an unparseable API version %q: %v", result.APIVersion, err)
		return
	}
	wantMajor, wantMinor, err := parseAPIVersion(minVersion)
	if err != nil {
		t.Fatalf("RequireEngineAPIAtLeast: minVersion %q: %v", minVersion, err)
	}

	if gotMajor < wantMajor || (gotMajor == wantMajor && gotMinor < wantMinor) {
		t.Skipf("Docker Engine API version %s is older than the %s this test needs", result.APIVersion, minVersion)
	}
}

// parseAPIVersion splits a "major.minor" Docker API version string into its
// numeric components, so callers compare versions numerically rather than
// lexicographically ("1.5" < "1.48" as strings, but not as versions).
func parseAPIVersion(version string) (major, minor int, err error) {
	parts := strings.SplitN(version, ".", 2)
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("expected a \"major.minor\" version, got %q", version)
	}
	major, err = strconv.Atoi(parts[0])
	if err != nil {
		return 0, 0, fmt.Errorf("parse major version from %q: %w", version, err)
	}
	minor, err = strconv.Atoi(parts[1])
	if err != nil {
		return 0, 0, fmt.Errorf("parse minor version from %q: %w", version, err)
	}
	return major, minor, nil
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
