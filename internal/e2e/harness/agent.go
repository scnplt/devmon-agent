// SPDX-License-Identifier: AGPL-3.0-only

//go:build e2e

package harness

import (
	"bytes"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// Timing budgets for the agent subprocess. readinessTimeout is generous: the
// state store and CA are built on first start, which is slower than a warm
// restart.
//
// shutdownTimeout must strictly exceed the agent's own drain grace
// (shutdownGrace in internal/httpapi/server.go, currently 5s — unexported, so
// it can't be imported here and the two values are kept aligned by this
// comment) plus roughly 2s of scheduling margin. Setting it equal to the
// drain grace put SIGTERM-to-SIGKILL on a knife edge where even a correct
// full-grace drain got killed as if it were a hang (issue #117).
const (
	readinessTimeout = 15 * time.Second
	readinessPoll    = 100 * time.Millisecond
	shutdownTimeout  = 7 * time.Second
	killTimeout      = 10 * time.Second
)

var (
	buildOnce sync.Once
	buildPath string
	buildErr  error
)

// BuildBinary compiles the real devmon-agent once per test binary run and
// returns the path to the resulting executable. Every agent process the
// suite starts runs this exact artifact, built with CGO_ENABLED=0 like the
// shipped one (D17) — only the test binary itself runs with -race.
func BuildBinary(t *testing.T) string {
	t.Helper()
	buildOnce.Do(func() {
		buildPath, buildErr = buildBinary()
	})
	if buildErr != nil {
		t.Fatalf("build devmon-agent: %v", buildErr)
	}
	return buildPath
}

// buildBinary resolves the module root by walking up from the test's working
// directory, not by hardcoding a relative depth — both api/ and incontainer/
// call BuildBinary from different depths.
func buildBinary() (string, error) {
	root, err := moduleRoot()
	if err != nil {
		return "", err
	}

	out := binaryOutputPath()
	// #nosec G204 -- "go" and its arguments are fixed by this function, not by
	// any caller-supplied input; this test harness's entire purpose is to
	// build and run the repository's own binary.
	cmd := exec.Command("go", "build", "-o", out, "./cmd/devmon-agent")
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")

	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("go build ./cmd/devmon-agent: %w: %s", err, stderr.String())
	}
	return out, nil
}

func moduleRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("getwd: %w", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("go.mod not found above %s", dir)
		}
		dir = parent
	}
}

func binaryOutputPath() string {
	name := fmt.Sprintf("devmon-agent-e2e-%d", os.Getpid())
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	return filepath.Join(os.TempDir(), name)
}

// AgentOptions configures one devmon-agent process.
type AgentOptions struct {
	PolicyMode string            // "" leaves DEVMON_POLICY_MODE unset — the default-mode case
	DockerHost string            // "" uses the suite's own Engine endpoint
	PublicAddr string            // "" defaults to 127.0.0.1
	StateDir   string            // "" allocates a fresh t.TempDir
	Env        map[string]string // extra DEVMON_* for this case only

	// ExpectFailure marks a case where the process is expected to exit
	// non-zero at (or shortly after) startup; StartAgent does not wait for
	// readiness and the caller drives it through Wait instead.
	ExpectFailure bool
}

// Agent is one running (or, for ExpectFailure cases, exited) devmon-agent
// process.
type Agent struct {
	BaseURL  string // https://127.0.0.1:<port>
	StateDir string
	Port     int

	cmd    *exec.Cmd
	output *bytes.Buffer
	done   chan struct{}

	mu       sync.Mutex
	exitCode int
	waitErr  error
}

// StartAgent builds (once per package) and starts one devmon-agent process
// with a curated environment, and registers a t.Cleanup that stops it. The
// port allocation-then-bind sequence is a real race (another process can grab
// the port between allocation and the agent's own bind); on that specific
// failure the whole allocation is retried once rather than assumed not to
// happen.
func StartAgent(t *testing.T, opts AgentOptions) *Agent {
	t.Helper()

	a, err := startAgentOnce(t, opts)
	if err != nil && isPortRaceError(err) {
		t.Logf("retrying agent start after an apparent port allocation race: %v", err)
		a, err = startAgentOnce(t, opts)
	}
	if err != nil {
		t.Fatalf("start devmon-agent: %v", err)
	}
	return a
}

func startAgentOnce(t *testing.T, opts AgentOptions) (*Agent, error) {
	t.Helper()

	bin := BuildBinary(t)

	stateDir := opts.StateDir
	if stateDir == "" {
		stateDir = t.TempDir()
	}

	port, err := allocatePort()
	if err != nil {
		return nil, fmt.Errorf("allocate port: %w", err)
	}

	env := agentEnvVars(t, opts, stateDir, port)

	// #nosec G204 -- bin is the path this same package just built via
	// buildBinary; it is never derived from test input.
	cmd := exec.Command(bin)
	cmd.Env = buildAgentEnv(env)
	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &output

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("exec %s: %w", bin, err)
	}

	a := &Agent{
		BaseURL:  fmt.Sprintf("https://127.0.0.1:%d", port),
		StateDir: stateDir,
		Port:     port,
		cmd:      cmd,
		output:   &output,
		done:     make(chan struct{}),
	}
	go a.waitProcess()

	t.Cleanup(func() { a.cleanup(t, opts.ExpectFailure) })

	if opts.ExpectFailure {
		return a, nil
	}
	if err := a.waitReady(readinessTimeout); err != nil {
		return nil, err
	}
	return a, nil
}

// agentEnvVars builds the DEVMON_* set for one agent instance. Every value
// comes from opts or a documented default — never from the ambient
// environment (D12).
func agentEnvVars(t *testing.T, opts AgentOptions, stateDir string, port int) map[string]string {
	t.Helper()

	publicAddr := opts.PublicAddr
	if publicAddr == "" {
		publicAddr = "127.0.0.1"
	}

	dockerHost := opts.DockerHost
	if dockerHost == "" {
		host, skipReason := EngineHost()
		if skipReason != "" {
			t.Fatalf("no Engine endpoint available for the agent under test: %s", skipReason)
		}
		dockerHost = host
	}

	env := map[string]string{
		"DEVMON_STATE_DIR":   stateDir,
		"DEVMON_LISTEN_ADDR": "127.0.0.1:" + strconv.Itoa(port),
		"DEVMON_PUBLIC_ADDR": publicAddr,
		"DEVMON_DOCKER_HOST": dockerHost,
	}
	if opts.PolicyMode != "" {
		env["DEVMON_POLICY_MODE"] = opts.PolicyMode
	}
	for k, v := range opts.Env {
		env[k] = v
	}
	return env
}

// buildAgentEnv builds the child process's environment explicitly — never
// os.Environ() plus overrides (D12, the SUBPROCESS_ENVIRONMENT pattern). A
// developer with DEVMON_POLICY_MODE=full exported in their shell would
// otherwise silently invalidate every read-only assertion in the suite. PATH
// and HOME are passed through because the agent binary itself needs neither,
// but a well-formed process environment on every platform expects them.
func buildAgentEnv(vars map[string]string) []string {
	env := []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + os.Getenv("HOME"),
	}
	for k, v := range vars {
		env = append(env, k+"="+v)
	}
	return env
}

// allocatePort finds a free TCP port by listening on 127.0.0.1:0 and closing
// immediately. The window between close and the agent's own bind is a real
// race, handled by StartAgent's single retry rather than pretended away.
func allocatePort() (int, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	addr, ok := ln.Addr().(*net.TCPAddr)
	if !ok {
		_ = ln.Close()
		return 0, fmt.Errorf("unexpected listener address type %T", ln.Addr())
	}
	if err := ln.Close(); err != nil {
		return 0, err
	}
	return addr.Port, nil
}

func isPortRaceError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "address already in use") || strings.Contains(msg, "bind:")
}

func (a *Agent) waitProcess() {
	err := a.cmd.Wait()
	a.mu.Lock()
	a.waitErr = err
	a.exitCode = exitCodeFrom(err)
	a.mu.Unlock()
	close(a.done)
}

func exitCodeFrom(err error) int {
	if err == nil {
		return 0
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode()
	}
	return -1
}

// hasExited reports whether the process has already been reaped, without
// blocking.
func (a *Agent) hasExited() bool {
	select {
	case <-a.done:
		return true
	default:
		return false
	}
}

func (a *Agent) waitedExitCode() int {
	<-a.done
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.exitCode
}

// waitReady polls GET /v1/status over TLS with InsecureSkipVerify — the one
// place besides pairing's bootstrap request where that is permitted (D7) —
// until it answers 200 or timeout elapses. It deliberately does not just dial
// the TCP port: the listener comes up before the state store and CA do, and a
// test that starts pairing at that moment fails intermittently in a way that
// would be blamed on the agent rather than the probe.
func (a *Agent) waitReady(timeout time.Duration) error {
	probe := &http.Client{
		Timeout: 2 * time.Second,
		Transport: &http.Transport{
			// Bootstrap readiness probe only; see the package comment on why
			// this is one of exactly two permitted uses (D7).
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, // #nosec G402 -- bootstrap readiness probe only, before any CA is pinned (D7)
		},
	}

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if a.hasExited() {
			return fmt.Errorf("devmon-agent exited before it became ready:\n%s", a.output.String())
		}
		resp, err := probe.Get(a.BaseURL + "/v1/status")
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
		}
		time.Sleep(readinessPoll)
	}
	return fmt.Errorf("devmon-agent did not answer /v1/status within %s:\n%s", timeout, a.output.String())
}

// Stop sends SIGTERM and asserts a clean (0) exit within shutdownTimeout,
// falling back to SIGKILL — and a test failure — if the agent does not honor
// it.
func (a *Agent) Stop(t *testing.T) (exitCode int) {
	t.Helper()
	if a.hasExited() {
		return a.waitedExitCode()
	}

	if err := a.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("signal devmon-agent (SIGTERM): %v", err)
	}

	select {
	case <-a.done:
	case <-time.After(shutdownTimeout):
		_ = a.cmd.Process.Kill()
		<-a.done
		t.Fatalf("devmon-agent did not exit within %s of SIGTERM; killed:\n%s", shutdownTimeout, a.output.String())
	}

	code := a.waitedExitCode()
	if code != 0 {
		t.Fatalf("devmon-agent exited %d after SIGTERM, want 0:\n%s", code, a.output.String())
	}
	return code
}

// Kill sends SIGKILL without waiting for a graceful shutdown, for
// crash-survival tests that need the agent to disappear the way `docker kill`
// makes it disappear.
func (a *Agent) Kill(t *testing.T) {
	t.Helper()
	if a.hasExited() {
		return
	}
	if err := a.cmd.Process.Kill(); err != nil {
		t.Fatalf("kill devmon-agent: %v", err)
	}
	<-a.done
}

// Wait blocks until an ExpectFailure agent exits (or killTimeout elapses,
// which is treated as the test failing to see the expected exit) and returns
// its exit code and combined stdout/stderr.
func (a *Agent) Wait(t *testing.T) (exitCode int, stderr string) {
	t.Helper()
	select {
	case <-a.done:
	case <-time.After(killTimeout):
		_ = a.cmd.Process.Kill()
		<-a.done
		t.Fatalf("devmon-agent was expected to exit but did not within %s:\n%s", killTimeout, a.output.String())
	}
	return a.waitedExitCode(), a.output.String()
}

// LogText reads the agent's operational log file. It fails the test rather
// than returning an error: every caller needs the log to make an assertion,
// and a missing file is itself the finding.
func (a *Agent) LogText(t *testing.T) string {
	t.Helper()
	path := filepath.Join(a.StateDir, "logs", "agent.log")
	// #nosec G304 -- a.StateDir is this agent's own temp state directory,
	// allocated by StartAgent, not caller-supplied input.
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read agent log %s: %v", path, err)
	}
	return string(data)
}

// cleanup runs at test end for every agent StartAgent produced, whether or
// not the test itself ever called Stop, Kill, or Wait.
func (a *Agent) cleanup(t *testing.T, expectFailure bool) {
	t.Helper()
	if a.hasExited() {
		return
	}
	if expectFailure {
		_ = a.cmd.Process.Kill()
		<-a.done
		return
	}
	a.Stop(t)
}
