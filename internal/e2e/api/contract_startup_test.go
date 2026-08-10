//go:build e2e

package api

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/scnplt/devmon-agent/internal/e2e/harness"
)

// This file replays the remainder of Phase 1's manual checklist: the
// configuration-fault contract (exit 2, every problem reported at once), the
// unreachable-Engine startup path, graceful shutdown, the "certs/ deleted
// under an intact database" identity fault, and the no-credential-material
// sweep of the whole state directory.
// secure-foundation-and-persistence.plan.md:1216-1229.

// TestConfigFaultsReportedTogether asserts internal/config's aggregation
// contract from the outside: an operator who typos three variables at once
// sees all three named in one run, not one per restart, and exits with the
// documented configuration-fault code (main.go's exitConfig = 2, distinct
// from a generic failure). A second, independent case covers the one
// variable with no default: an absent DEVMON_PUBLIC_ADDR must be named on
// its own.
func TestConfigFaultsReportedTogether(t *testing.T) {
	t.Parallel()
	harness.RequireEngine(t)

	t.Run("three bad variables are all reported together", func(t *testing.T) {
		t.Parallel()

		a := harness.StartAgent(t, harness.AgentOptions{
			PolicyMode: "bogus", // invalid DEVMON_POLICY_MODE
			Env: map[string]string{
				"DEVMON_LOG_MAX_AGE_DAYS":  "x",               // not an integer
				"DEVMON_SELF_CONTAINER_ID": "not-a-hex-id!!!", // matches neither container ID pattern
			},
			ExpectFailure: true,
		})

		exitCode, stderr := a.Wait(t)
		if exitCode != 2 {
			t.Errorf("exit code = %d, want 2 (config fault)", exitCode)
		}
		for _, name := range []string{"DEVMON_POLICY_MODE", "DEVMON_LOG_MAX_AGE_DAYS", "DEVMON_SELF_CONTAINER_ID"} {
			if !strings.Contains(stderr, name) {
				t.Errorf("stderr does not name %s; stderr = %s", name, stderr)
			}
		}
	})

	t.Run("missing DEVMON_PUBLIC_ADDR is named on its own", func(t *testing.T) {
		t.Parallel()

		a := harness.StartAgent(t, harness.AgentOptions{
			// AgentOptions always sets a default DEVMON_PUBLIC_ADDR; an empty
			// override here makes config.Load see the variable as genuinely
			// unset (its raw() helper treats a blank value as absent).
			Env:           map[string]string{"DEVMON_PUBLIC_ADDR": ""},
			ExpectFailure: true,
		})

		exitCode, stderr := a.Wait(t)
		if exitCode != 2 {
			t.Errorf("exit code = %d, want 2 (config fault)", exitCode)
		}
		if !strings.Contains(stderr, "DEVMON_PUBLIC_ADDR") {
			t.Errorf("stderr does not name DEVMON_PUBLIC_ADDR; stderr = %s", stderr)
		}
	})

	// A zero DEVMON_RATE_GUARDED_PER_SEC is a configuration fault, not "no
	// limit": internal/config's minRatePerX floor is 1 (hardening-and-oss-
	// release.plan.md, Task 2's gotcha) precisely so no value can disable the
	// limiter, and this proves that floor from the outside.
	t.Run("zero DEVMON_RATE_GUARDED_PER_SEC is a configuration fault", func(t *testing.T) {
		t.Parallel()

		a := harness.StartAgent(t, harness.AgentOptions{
			Env:           map[string]string{"DEVMON_RATE_GUARDED_PER_SEC": "0"},
			ExpectFailure: true,
		})

		exitCode, stderr := a.Wait(t)
		if exitCode != 2 {
			t.Errorf("exit code = %d, want 2 (config fault)", exitCode)
		}
		if !strings.Contains(stderr, "DEVMON_RATE_GUARDED_PER_SEC") {
			t.Errorf("stderr does not name DEVMON_RATE_GUARDED_PER_SEC; stderr = %s", stderr)
		}
	})
}

// TestStartupFailsWhenEngineUnreachable asserts an agent pointed at a Docker
// socket that does not exist fails fast with a specific, non-panicking
// error, rather than starting and answering every request with a confusing
// runtime failure later.
func TestStartupFailsWhenEngineUnreachable(t *testing.T) {
	t.Parallel()
	harness.RequireEngine(t)

	a := harness.StartAgent(t, harness.AgentOptions{
		DockerHost:    "unix:///nonexistent/docker.sock",
		ExpectFailure: true,
	})

	exitCode, stderr := a.Wait(t)
	if exitCode == 0 {
		t.Fatalf("exit code = 0, want non-zero when the Docker Engine is unreachable")
	}
	if strings.Contains(stderr, "panic:") {
		t.Errorf("stderr contains a panic, want a handled error: %s", stderr)
	}
	if !strings.Contains(stderr, "ping docker engine") {
		t.Errorf("stderr does not name the ping failure; stderr = %s", stderr)
	}
}

// TestGracefulShutdown asserts SIGTERM to a healthy agent exits 0 well
// within the 5s drain budget (internal/httpapi/server.go's shutdownGrace),
// not the slower path Docker falls back to when a container ignores
// SIGTERM and gets SIGKILLed after its full stop timeout. The log's last
// line must be the shutdown message: neither the rotator nor the pruner
// logs anything on context cancellation (internal/logging/rotator.go,
// internal/state/pruner.go), so "shutting down http server" is the only
// candidate for the final line.
func TestGracefulShutdown(t *testing.T) {
	t.Parallel()
	harness.RequireEngine(t)

	a := harness.StartAgent(t, harness.AgentOptions{})

	// Agent.Stop already asserts a clean (0) exit within its own
	// shutdownTimeout (5s, matching server.go's own drain budget) — a
	// timeout or a non-zero exit fails the test right here.
	exitCode := a.Stop(t)
	if exitCode != 0 {
		t.Fatalf("exit code = %d, want 0", exitCode)
	}

	logText := strings.TrimRight(a.LogText(t), "\n")
	lines := strings.Split(logText, "\n")
	last := lines[len(lines)-1]
	if !strings.Contains(last, "shutting down http server") {
		t.Errorf("last log line = %q, want it to contain %q", last, "shutting down http server")
	}
}

// TestMissingCertsDirIsLoudNotSilent asserts the D9 identity-consistency
// check from the outside: a state database that survived alongside a
// destroyed certs/ directory is the signature of a partial restore, and the
// agent must refuse to start rather than mint a fresh, silently different
// identity that every paired device would trust without knowing it changed.
func TestMissingCertsDirIsLoudNotSilent(t *testing.T) {
	t.Parallel()
	harness.RequireEngine(t)

	stateDir := t.TempDir()

	a1 := harness.StartAgent(t, harness.AgentOptions{StateDir: stateDir})
	harness.PairDevice(t, a1, "missing-certs-dir")
	a1.Stop(t)

	certsDir := filepath.Join(stateDir, "certs")
	if err := os.RemoveAll(certsDir); err != nil {
		t.Fatalf("remove %s: %v", certsDir, err)
	}

	a2 := harness.StartAgent(t, harness.AgentOptions{StateDir: stateDir, ExpectFailure: true})
	exitCode, stderr := a2.Wait(t)
	if exitCode == 0 {
		t.Fatalf("exit code = 0, want non-zero when certs/ is missing but the state database is intact")
	}
	if !strings.Contains(stderr, "agent identity is incomplete") {
		t.Errorf("stderr does not name the identity fault; stderr = %s", stderr)
	}
	if !strings.Contains(stderr, "certificate authority") {
		t.Errorf("stderr does not explain which half is missing; stderr = %s", stderr)
	}

	// The claim is that no NEW CA was minted — not that the directory is
	// absent. The agent recreates the state directory's subdirectories
	// (cmd/devmon-agent/main.go's prepareStateDir) before it ever reaches the
	// identity check, so an EMPTY certs/ after a refused start is expected and
	// harmless. What must not exist is CA material inside it: that would be
	// the silent new identity every paired device would trust without knowing
	// it changed. Asserting the directory's absence instead would fail against
	// correct behaviour, which is why this checks the contents.
	entries, err := os.ReadDir(certsDir)
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("read %s after the failed restart: %v", certsDir, err)
	}
	for _, e := range entries {
		t.Errorf("certs dir holds %q after the failed restart; the CA must not be silently regenerated", e.Name())
	}
}

// credentialMarkers are byte sequences that must never appear outside
// certs/: PEM headers for a private key or a certificate, and — once
// substituted per test run — the plaintext of a pairing code minted during
// the run. internal/state/pairing.go persists only the code's SHA-256, and
// internal/state's devices table persists only a certificate serial, never
// PEM (internal/state/schema.go); this test is the black-box proof of both.
var credentialMarkers = []string{"PRIVATE KEY", "BEGIN CERTIFICATE"}

// TestStateDirCarriesNoCredentialMaterial walks the whole state directory
// after a full pair-and-drive cycle and asserts no file outside certs/
// contains a private key, a certificate, or the plaintext of a pairing code
// that was minted during the run. certs/ legitimately holds PEM — the claim
// under test is that key material never leaks OUT of there, into the
// database or the operational log.
func TestStateDirCarriesNoCredentialMaterial(t *testing.T) {
	t.Parallel()
	harness.RequireEngine(t)

	a := harness.StartAgent(t, harness.AgentOptions{})

	// An unused pairing code: minted while the agent runs, deliberately not
	// redeemed, so its plaintext exists only in this test's memory and
	// (hashed) in the database — never on disk in the clear.
	unusedCode := harness.MintPairingCode(t, a, "credential-sweep-unused")

	d := harness.PairDevice(t, a, "credential-sweep-device")
	status, _, _ := d.Do(t, "GET", "/v1/containers", nil)
	if status != 200 && status != 502 {
		// 502 is acceptable here: this test only needs one authenticated
		// round trip to exercise the request path, not a live Engine
		// response, since the assertion below is about the state
		// directory, not the read contract.
		t.Fatalf("GET /v1/containers status = %d, want 200 or 502", status)
	}

	// Flush everything to disk before reading it back.
	a.Stop(t)

	certsDir := filepath.Join(a.StateDir, "certs")
	err := filepath.WalkDir(a.StateDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if path == certsDir {
				return filepath.SkipDir
			}
			return nil
		}

		// #nosec G304 -- path comes from WalkDir over this test's own
		// t.TempDir()-allocated state directory, never from external input.
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		content := string(data)

		for _, marker := range credentialMarkers {
			if strings.Contains(content, marker) {
				t.Errorf("%s contains %q outside certs/", path, marker)
			}
		}
		if strings.Contains(content, unusedCode) {
			t.Errorf("%s contains the plaintext of a minted pairing code", path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk state dir %s: %v", a.StateDir, err)
	}
}
