// SPDX-License-Identifier: AGPL-3.0-only

//go:build e2e

package harness

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"
)

// The in-container group's infrastructure (D22): building the product's own
// image the same way the Makefile and CI do, running it as a container the
// way compose.example.yaml documents, and reaching into it for the
// host-side subcommands and the log files nothing outside the container can
// read directly. Everything past the image build itself goes through the
// SDK, which is precise and already a dependency.

// agentContainerPort is the port the image EXPOSEs and the agent listens on
// by default inside the container (Dockerfile, compose.example.yaml).
const agentContainerPort = "8443/tcp"

// containerStateDir is the default DEVMON_STATE_DIR baked into the image;
// bind-mounting a host directory at this exact path, as compose.example.yaml
// does, means no DEVMON_STATE_DIR override is needed for the in-container
// group either.
const containerStateDir = "/var/lib/devmon"

// containerReadinessTimeout and containerReadinessPoll mirror agent.go's own
// budgets for the host-binary group; a containerised start is not
// meaningfully slower.
const (
	containerReadinessTimeout = 15 * time.Second
	containerReadinessPoll    = 200 * time.Millisecond
)

// RequireDockerCLI skips the calling test (or fails it, under
// DEVMON_E2E_REQUIRE=1, via the same skipOrFail path RequireEngine uses)
// when the `docker` CLI is not on PATH. BuildImage needs it (D22).
func RequireDockerCLI(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("docker"); err != nil {
		skipOrFail(t, "docker CLI not found on PATH; the in-container group needs it to build the image: "+err.Error())
	}
}

// RequireLinuxContainerEngine composes RequireEngine with the extra
// conditions the in-container group needs beyond what the host-binary group
// requires (D6): the docker CLI must be on PATH, and the Engine itself must
// report a Linux OSType — the image this repository ships
// (distroless/static-debian12) cannot run on anything else, and a remote or
// Windows daemon answering the ping would otherwise fail much later, inside
// ContainerCreate, with a far less obvious message.
func RequireLinuxContainerEngine(t *testing.T) *client.Client {
	t.Helper()

	RequireDockerCLI(t)
	cli := RequireEngine(t)
	if cli == nil {
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), pingTimeout)
	defer cancel()
	result, err := cli.Ping(ctx, client.PingOptions{NegotiateAPIVersion: true})
	if err != nil {
		skipOrFail(t, fmt.Sprintf("Docker Engine ping failed: %v", err))
		return nil
	}
	if result.OSType != "linux" {
		skipOrFail(t, fmt.Sprintf(
			"Docker Engine reports OSType=%q; the in-container group needs a Linux Engine (D6)", result.OSType))
		return nil
	}
	return cli
}

// BuildImage runs `docker build -t tag [--build-arg k=v ...] <module root>`
// — the exact command the Makefile's `image` target and CI's `image` job
// run. Assembling a tar build context in Go to reach the SDK's ImageBuild
// would be real work with no payoff when the same shelled-out command is
// already exercised elsewhere (D22). Call RequireDockerCLI (directly, or
// through RequireLinuxContainerEngine) before this: a missing docker CLI is
// a skip, and that distinction belongs to the caller, which has the
// *testing.T context to make it.
//
// It is not memoized: the upgrade-rehearsal test rebuilds the image a
// second time with a different VERSION build arg against the same tag
// family, and a cache here would silently answer that call with the first
// build's image.
func BuildImage(t *testing.T, tag string, buildArgs map[string]string) {
	t.Helper()

	root, err := moduleRoot()
	if err != nil {
		t.Fatalf("resolve module root for docker build: %v", err)
	}

	args := []string{"build", "-t", tag}
	for k, v := range buildArgs {
		args = append(args, "--build-arg", k+"="+v)
	}
	args = append(args, root)

	// #nosec G204 -- args are a fixed "build"/"-t"/"--build-arg" prefix plus
	// this package's own callers' build-arg values; never derived from
	// external or user-supplied input.
	cmd := exec.Command("docker", args...)
	cmd.Dir = root
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Run(); err != nil {
		t.Fatalf("docker build -t %s: %v\n%s", tag, err, out.String())
	}
}

// ContainerAgentOptions configures one containerised devmon-agent, run the
// way compose.example.yaml documents: a bind-mounted state directory, the
// Docker socket bind-mounted read-only, group_add for the socket's GID, and
// one published port.
type ContainerAgentOptions struct {
	Image      string            // required: an image tag BuildImage produced
	StateDir   string            // "" allocates a fresh t.TempDir, widened to 0o777 (see the GOTCHA on RunAgentContainer)
	PublicAddr string            // "" defaults to 127.0.0.1
	PolicyMode string            // "" leaves DEVMON_POLICY_MODE unset
	Hostname   string            // "" leaves the container runtime's own hostname; set to prove self-ID does not depend on it
	Env        map[string]string // extra DEVMON_* for this case only (D12 applies here too)
}

// ContainerAgent is one running containerised devmon-agent.
type ContainerAgent struct {
	ID       string
	BaseURL  string // https://127.0.0.1:<published port>
	StateDir string // host path, the bind mount source

	engine *client.Client
}

// RunAgentContainer creates and starts one labelled (D11) agent container
// and registers a t.Cleanup that removes it, unless DEVMON_E2E_KEEP=1 asked
// the harness to leave it for inspection (the same escape hatch
// removeFixture honours for ordinary fixtures).
//
// GOTCHA: the state directory is a host temp dir that must be world-writable
// before the container starts — the image runs as UID 65532 and
// prepareStateDir fails with "permission denied" otherwise. That is a
// TEST-ONLY widening of a directory StartAgent (well, here, RunAgentContainer)
// deletes at cleanup; it is exactly the mistake the README warns real
// operators against, done here on purpose and only here.
//
// GOTCHA (native Linux Engine only): the pre-start 0o777 above only covers
// the directory itself. Everything the agent creates UNDERNEATH it while
// running — logs/ (0700) and its log files (0600), both owned by UID 65532 —
// is still narrow, and on a native Linux Engine those ownerships are real on
// the host filesystem. When opts.StateDir == "" this function's stateDir is
// a t.TempDir(), and Go's own t.TempDir() cleanup then fails to RemoveAll a
// tree the host test user cannot traverse or delete. See
// widenStateDirForCleanup for how the fix works and why it is a container,
// not a host-side chmod.
func RunAgentContainer(t *testing.T, e *client.Client, opts ContainerAgentOptions) *ContainerAgent {
	t.Helper()
	ctx := context.Background()

	if opts.Image == "" {
		t.Fatalf("RunAgentContainer: opts.Image is required")
	}

	stateDir := opts.StateDir
	if stateDir == "" {
		stateDir = t.TempDir()
	}
	// #nosec G302 -- test-only widening of a throwaway temp directory so the
	// container's UID 65532 can write to it; see the GOTCHA above.
	if err := os.Chmod(stateDir, 0o777); err != nil {
		t.Fatalf("chmod state dir %s: %v", stateDir, err)
	}
	// Registered AFTER t.TempDir()'s own cleanup (above, when opts.StateDir ==
	// ""): t.Cleanup funcs run LIFO, so this one runs BEFORE t.TempDir()'s
	// RemoveAll gets a chance to trip over files it cannot delete. See the
	// GOTCHA above and widenStateDirForCleanup's doc comment.
	t.Cleanup(func() { widenStateDirForCleanup(t, e, stateDir) })

	socketPath := dockerSocketPathOrFail(t)
	binds := []string{
		stateDir + ":" + containerStateDir,
		socketPath + ":/var/run/docker.sock:ro",
	}

	hostConfig := &container.HostConfig{
		Binds: binds,
		PortBindings: network.PortMap{
			network.MustParsePort(agentContainerPort): {{
				HostIP:   netip.MustParseAddr("127.0.0.1"),
				HostPort: "0",
			}},
		},
	}
	if gid, ok := DockerSocketGID(socketPath); ok {
		hostConfig.GroupAdd = []string{gid}
	} else {
		t.Logf("could not determine the docker socket's GID at %s; running without group_add, matching the socket's own permissions", socketPath)
	}

	labels := map[string]string{LabelSuite: "1", LabelRun: runID}
	name := fmt.Sprintf("devmon-e2e-agent-%s", runID)

	created, err := e.ContainerCreate(ctx, client.ContainerCreateOptions{
		Name: name,
		Config: &container.Config{
			Image:        opts.Image,
			Hostname:     opts.Hostname,
			Env:          containerAgentEnv(t, opts),
			Labels:       labels,
			ExposedPorts: network.PortSet{network.MustParsePort(agentContainerPort): struct{}{}},
		},
		HostConfig: hostConfig,
	})
	if err != nil {
		t.Fatalf("create agent container %s: %v", name, err)
	}
	t.Cleanup(func() { removeFixture(t, e, created.ID) })

	if _, err := e.ContainerStart(ctx, created.ID, client.ContainerStartOptions{}); err != nil {
		t.Fatalf("start agent container %s: %v", name, err)
	}

	port := publishedPort(t, e, created.ID)
	c := &ContainerAgent{
		ID:       created.ID,
		BaseURL:  fmt.Sprintf("https://127.0.0.1:%d", port),
		StateDir: stateDir,
		engine:   e,
	}
	waitContainerReady(t, c)
	return c
}

// widenStateDirForCleanup makes everything the agent created under stateDir
// host-removable, by running a throwaway root container that chmods the
// whole bind-mounted tree from the inside.
//
// The host test user cannot do this itself: the agent runs as UID 65532 and
// creates logs/ (0700) and its log files (0600) under stateDir
// (internal/logging/logging.go); on a native Linux Engine those ownerships
// are real on the host filesystem, and a user can never chmod or remove
// files it does not own, no matter what mode the PARENT directory carries.
// Docker Desktop hides this entirely — it presents the bind mount through
// its own VM, where the host user appears to own everything the container
// wrote regardless of the in-container UID — which is why this asymmetry
// only surfaces on a native Linux Engine such as the CI runner's.
//
// This is a cleanup helper, called from a t.Cleanup: failure is reported
// with t.Logf, never t.Fatalf, so a permission problem here can never fail
// the test it is cleaning up after.
func widenStateDirForCleanup(t *testing.T, e *client.Client, stateDir string) {
	t.Helper()

	if os.Getenv(envKeep) == "1" {
		t.Logf("%s=1: leaving %s ownership as-is for inspection", envKeep, stateDir)
		return
	}

	ctx := context.Background()
	if _, err := e.ImageInspect(ctx, defaultFixtureImage); err != nil {
		rc, pullErr := e.ImagePull(ctx, defaultFixtureImage, client.ImagePullOptions{})
		if pullErr != nil {
			t.Logf("widen state dir %s: pull %s: %v", stateDir, defaultFixtureImage, pullErr)
			return
		}
		waitErr := rc.Wait(ctx)
		_ = rc.Close()
		if waitErr != nil {
			t.Logf("widen state dir %s: pull %s: %v", stateDir, defaultFixtureImage, waitErr)
			return
		}
	}

	name := fmt.Sprintf("devmon-e2e-widen-%s-%d", runID, time.Now().UnixNano())
	created, err := e.ContainerCreate(ctx, client.ContainerCreateOptions{
		Name: name,
		Config: &container.Config{
			Image:  defaultFixtureImage,
			Cmd:    []string{"chmod", "-R", "a+rwX", "/s"},
			Labels: map[string]string{LabelSuite: "1", LabelRun: runID},
		},
		HostConfig: &container.HostConfig{
			Binds: []string{stateDir + ":/s"},
		},
	})
	if err != nil {
		t.Logf("widen state dir %s: create widening container: %v", stateDir, err)
		return
	}
	defer removeFixture(t, e, created.ID)

	if _, err := e.ContainerStart(ctx, created.ID, client.ContainerStartOptions{}); err != nil {
		t.Logf("widen state dir %s: start widening container: %v", stateDir, err)
		return
	}

	wait := e.ContainerWait(ctx, created.ID, client.ContainerWaitOptions{Condition: container.WaitConditionNotRunning})
	select {
	case <-wait.Result:
	case err := <-wait.Error:
		t.Logf("widen state dir %s: wait for widening container: %v", stateDir, err)
	case <-time.After(10 * time.Second):
		t.Logf("widen state dir %s: widening container did not finish within 10s", stateDir)
	}
}

// containerAgentEnv builds the DEVMON_* set for one containerised agent,
// mirroring compose.example.yaml's layout. Like agentEnvVars in agent.go, it
// is built from opts alone (D12) — nothing here reads the harness process's
// own environment.
func containerAgentEnv(t *testing.T, opts ContainerAgentOptions) []string {
	t.Helper()

	publicAddr := opts.PublicAddr
	if publicAddr == "" {
		publicAddr = "127.0.0.1"
	}

	vars := map[string]string{"DEVMON_PUBLIC_ADDR": publicAddr}
	if opts.PolicyMode != "" {
		vars["DEVMON_POLICY_MODE"] = opts.PolicyMode
	}
	for k, v := range opts.Env {
		vars[k] = v
	}

	env := make([]string, 0, len(vars))
	for k, v := range vars {
		env = append(env, k+"="+v)
	}
	return env
}

// dockerSocketPathOrFail resolves the harness's configured Engine endpoint
// (EngineHost) down to the raw unix socket path RunAgentContainer bind-mounts
// into the container. The in-container group only ever runs against a local
// unix socket (D6); a tcp:// endpoint has no local file to bind-mount and is
// a hard failure here rather than a silent skip, because reaching this point
// already means RequireLinuxContainerEngine passed.
func dockerSocketPathOrFail(t *testing.T) string {
	t.Helper()
	host, skipReason := EngineHost()
	if skipReason != "" {
		t.Fatalf("resolve docker socket path: %s", skipReason)
	}
	scheme, path, err := parseEngineHost(host)
	if err != nil {
		t.Fatalf("resolve docker socket path: %v", err)
	}
	if scheme != "unix" {
		t.Fatalf("the in-container group needs a unix:// Docker Engine endpoint to bind-mount; got %q", host)
	}
	return path
}

// publishedPort reads back the host port the Engine assigned when
// RunAgentContainer asked for "0" — picking a port in the test itself would
// race with everything else on the developer's machine.
func publishedPort(t *testing.T, e *client.Client, id string) int {
	t.Helper()

	inspect, err := e.ContainerInspect(context.Background(), id, client.ContainerInspectOptions{})
	if err != nil {
		t.Fatalf("inspect agent container %s: %v", id, err)
	}
	if inspect.Container.NetworkSettings == nil {
		t.Fatalf("agent container %s has no network settings", id)
	}
	bindings := inspect.Container.NetworkSettings.Ports[network.MustParsePort(agentContainerPort)]
	if len(bindings) == 0 {
		t.Fatalf("agent container %s has no published binding for %s", id, agentContainerPort)
	}
	port, err := strconv.Atoi(bindings[0].HostPort)
	if err != nil {
		t.Fatalf("parse published port %q for agent container %s: %v", bindings[0].HostPort, id, err)
	}
	return port
}

// waitContainerReady polls GET /v1/status the same way Agent.waitReady does
// for the host-binary group (D7's readiness exception applies here too),
// until it answers 200 or containerReadinessTimeout elapses.
func waitContainerReady(t *testing.T, c *ContainerAgent) {
	t.Helper()

	probe := &http.Client{
		Timeout: 2 * time.Second,
		Transport: &http.Transport{
			// Bootstrap readiness probe only; see client.go's package comment
			// on why this is one of the few permitted uses (D7).
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, // #nosec G402 -- bootstrap readiness probe only, before any CA is pinned (D7)
		},
	}

	deadline := time.Now().Add(containerReadinessTimeout)
	for time.Now().Before(deadline) {
		running, _ := c.IsRunning(t)
		if !running {
			t.Fatalf("agent container %s exited before it became ready", c.ID)
		}
		resp, err := probe.Get(c.BaseURL + "/v1/status")
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(containerReadinessPoll)
	}
	t.Fatalf("agent container %s did not answer /v1/status within %s", c.ID, containerReadinessTimeout)
}

// Exec runs cmd inside the agent container and returns its combined
// stdout/stderr and exit code. It uses TTY: true so the output stream is not
// Docker-multiplexed and needs no 8-byte frame header stripped — with
// TTY: false a pairing code parsed from the raw stream would come back with
// binary noise in it.
func (c *ContainerAgent) Exec(t *testing.T, cmd ...string) (stdout string, exitCode int) {
	t.Helper()
	ctx := context.Background()

	created, err := c.engine.ExecCreate(ctx, c.ID, client.ExecCreateOptions{
		Cmd:          cmd,
		TTY:          true,
		AttachStdout: true,
		AttachStderr: true,
	})
	if err != nil {
		t.Fatalf("exec create %v in container %s: %v", cmd, c.ID, err)
	}

	attached, err := c.engine.ExecAttach(ctx, created.ID, client.ExecAttachOptions{TTY: true})
	if err != nil {
		t.Fatalf("exec attach %v in container %s: %v", cmd, c.ID, err)
	}
	out, err := io.ReadAll(attached.Reader)
	attached.Close()
	if err != nil {
		t.Fatalf("exec read output %v in container %s: %v", cmd, c.ID, err)
	}

	code := waitExecExitCode(t, c.engine, created.ID)
	return string(out), code
}

// waitExecExitCode polls ExecInspect until the exec has stopped running.
// ExecAttach's Reader reaching EOF means the command's output is fully
// drained, but the exec's own Running flag can lag that by a moment; polling
// with a deadline avoids a fixed sleep either way.
func waitExecExitCode(t *testing.T, e *client.Client, execID string) int {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)
	for {
		result, err := e.ExecInspect(context.Background(), execID, client.ExecInspectOptions{})
		if err != nil {
			t.Fatalf("exec inspect %s: %v", execID, err)
		}
		if !result.Running {
			return result.ExitCode
		}
		if time.Now().After(deadline) {
			t.Fatalf("exec %s did not finish within 5s", execID)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// ReadStateFile reads one file out of the container's state directory via
// CopyFromContainer, decoding the tar stream the daemon produces. Log files
// under the state directory are 0600, owned by UID 65532
// (internal/logging/logging.go); the host test user usually cannot read them
// off the bind mount directly, but the daemon reads them as root and streams
// the bytes back regardless of ownership.
func (c *ContainerAgent) ReadStateFile(t *testing.T, path string) []byte {
	t.Helper()
	ctx := context.Background()

	result, err := c.engine.CopyFromContainer(ctx, c.ID, client.CopyFromContainerOptions{SourcePath: path})
	if err != nil {
		t.Fatalf("copy %s from container %s: %v", path, c.ID, err)
	}
	defer func() { _ = result.Content.Close() }()

	tr := tar.NewReader(result.Content)
	hdr, err := tr.Next()
	if err != nil {
		t.Fatalf("read tar entry for %s from container %s: %v", path, c.ID, err)
	}
	if hdr.Typeflag == tar.TypeDir {
		t.Fatalf("%s in container %s is a directory, not a file", path, c.ID)
	}

	data, err := io.ReadAll(tr)
	if err != nil {
		t.Fatalf("read tar content for %s from container %s: %v", path, c.ID, err)
	}
	return data
}

// Restart asks the Engine to stop and start the container again, in place —
// unlike an image upgrade rehearsal, which must recreate the container
// against the same bind mount instead (see internal/e2e/incontainer).
// The container keeps its ID and its bind mount, but NOT its published host
// port: the port was requested as "0", and the Engine allocates an ephemeral
// one at every start, so a restarted container comes back on a different one
// (measured: 32770 before, 32771 after). BaseURL is therefore re-read here,
// and a Device paired before the restart is still pointed at the old port —
// callers must re-point it with RebindToURL(device, c.BaseURL).
func (c *ContainerAgent) Restart(t *testing.T) {
	t.Helper()
	if _, err := c.engine.ContainerRestart(context.Background(), c.ID, client.ContainerRestartOptions{}); err != nil {
		t.Fatalf("restart agent container %s: %v", c.ID, err)
	}
	c.BaseURL = fmt.Sprintf("https://127.0.0.1:%d", publishedPort(t, c.engine, c.ID))
	waitContainerReady(t, c)
}

// KillContainer sends SIGKILL to the container's process, the same signal
// `docker kill` sends, for the crash-survival claim: the agent gets no
// chance to flush anything on the way down.
func (c *ContainerAgent) KillContainer(t *testing.T) {
	t.Helper()
	if _, err := c.engine.ContainerKill(context.Background(), c.ID, client.ContainerKillOptions{}); err != nil {
		t.Fatalf("kill agent container %s: %v", c.ID, err)
	}
}

// IsRunning asks the Engine — never the agent itself — whether the container
// is still running, and its restart count. Asking the agent would let a
// process that answered 403 and then died pass the naive version of a
// self-exclusion test; the Engine is the only source of truth for "is this
// container still up".
func (c *ContainerAgent) IsRunning(t *testing.T) (running bool, restartCount int) {
	t.Helper()

	inspect, err := c.engine.ContainerInspect(context.Background(), c.ID, client.ContainerInspectOptions{})
	if err != nil {
		t.Fatalf("inspect agent container %s: %v", c.ID, err)
	}
	if inspect.Container.State == nil {
		return false, inspect.Container.RestartCount
	}
	return inspect.Container.State.Running, inspect.Container.RestartCount
}

// MintPairingCodeInContainer runs `device pair-code --name <name>` via Exec
// inside the running agent container instead of a local subprocess (D8,
// D22): the shipped image is distroless/static, with no shell and no way for
// the host test process to reach a devmon-agent binary except the one
// already running inside it. It parses the same "Pairing code: " prefix
// cli.go's MintPairingCode does, and never logs the parsed value, including
// on a parse failure.
func MintPairingCodeInContainer(t *testing.T, c *ContainerAgent, name string) string {
	t.Helper()

	out, exitCode := c.Exec(t, "/usr/local/bin/devmon-agent", "device", "pair-code", "--name", name)
	if exitCode != 0 {
		t.Fatalf("device pair-code (in container): exit code %d", exitCode)
	}

	lines := strings.Split(out, "\n")
	for _, line := range lines {
		if strings.HasPrefix(line, pairingCodePrefix) {
			return strings.TrimSpace(strings.TrimPrefix(line, pairingCodePrefix))
		}
	}
	t.Fatalf("device pair-code (in container): did not find a %q line in %d lines of output", pairingCodePrefix, len(lines))
	return ""
}

// devmonAgentBinPath is the path the shipped image installs the devmon-agent
// binary at (Dockerfile); every in-container Exec of a `device`/`audit`
// subcommand runs it from here.
const devmonAgentBinPath = "/usr/local/bin/devmon-agent"

// ListDevicesInContainer runs `device list` INSIDE the agent container via
// Exec and parses it with the same parseDeviceRows helper ListDevices uses
// on the host, so the two paths' assertions cannot drift apart.
//
// This exists because devmon.db is 0600, owned by UID 65532
// (internal/state/store.go) — on a native Linux Engine the host test process
// cannot open it directly, unlike Docker Desktop, which presents the bind
// mount through its own VM where the host user appears to own everything.
// Running the CLI where the file was written, inside the container, is the
// same fix MintPairingCodeInContainer already applies to pairing.
func ListDevicesInContainer(t *testing.T, c *ContainerAgent) []DeviceRow {
	t.Helper()

	out, exitCode := c.Exec(t, devmonAgentBinPath, "device", "list")
	if exitCode != 0 {
		t.Fatalf("device list (in container): exit code %d", exitCode)
	}
	return parseDeviceRows(t, out)
}

// ListAuditInContainer runs `audit list --limit <limit>` INSIDE the agent
// container via Exec and parses it with the same parseAuditRows helper
// ListAudit uses on the host. Same rationale as ListDevicesInContainer:
// devmon.db is unreadable by the host test process on a native Linux Engine.
func ListAuditInContainer(t *testing.T, c *ContainerAgent, limit int) []AuditRow {
	t.Helper()

	out, exitCode := c.Exec(t, devmonAgentBinPath, "audit", "list", "--limit", strconv.Itoa(limit))
	if exitCode != 0 {
		t.Fatalf("audit list (in container): exit code %d", exitCode)
	}
	return parseAuditRows(t, out)
}

// PairDeviceInContainer performs the same documented pairing sequence
// PairDevice does for the host-binary group (D7), but mints the code via
// Exec instead. TryPairDevice and deviceFromPairResponse (client.go) read
// only Agent.BaseURL, so a minimal Agent value carrying just that field lets
// this function reuse their response parsing verbatim instead of duplicating
// it, without editing client.go itself.
func PairDeviceInContainer(t *testing.T, c *ContainerAgent, name string) *Device {
	t.Helper()

	code := MintPairingCodeInContainer(t, c, name)
	key := GenerateDeviceKey(t)
	csrPEM := DeviceCSRPEM(t, key, "devmon-e2e-device")

	target := &Agent{BaseURL: c.BaseURL}
	status, raw := TryPairDevice(t, target, code, csrPEM)
	if status != http.StatusCreated {
		t.Fatalf("POST /v1/pair (in container): status = %d, want %d; body = %s", status, http.StatusCreated, redact(raw))
	}

	var resp pairResponseBody
	if err := json.Unmarshal(raw, &resp); err != nil {
		t.Fatalf("decode /v1/pair response: %v; body = %s", err, redact(raw))
	}
	return deviceFromPairResponse(t, target, key, resp)
}
