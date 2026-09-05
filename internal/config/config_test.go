// SPDX-License-Identifier: AGPL-3.0-only

package config

import (
	"errors"
	"log/slog"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/scnplt/devmon-agent/internal/policy"
)

// fakeEnv returns a getenv func backed by a map, so tests never mutate process
// state and can therefore run in parallel.
func fakeEnv(kv map[string]string) func(string) string {
	return func(k string) string { return kv[k] }
}

// minimalEnv is the smallest environment that loads successfully: only the one
// variable with no default.
func minimalEnv() map[string]string {
	return map[string]string{envPublicAddr: "vps.example.com"}
}

func TestLoadDefaults(t *testing.T) {
	t.Parallel()

	// Arrange
	env := minimalEnv()

	// Act
	cfg, err := Load(fakeEnv(env))

	// Assert
	if err != nil {
		t.Fatalf("Load() unexpected error: %v", err)
	}
	checks := []struct {
		name string
		got  any
		want any
	}{
		{"StateDir", cfg.StateDir, defaultStateDir},
		{"ListenAddr", cfg.ListenAddr, defaultListenAddr},
		{"PolicyMode", cfg.PolicyMode, policy.ModeDefault},
		{"DockerHost", cfg.DockerHost, defaultDockerHost},
		{"LogLevel", cfg.LogLevel, slog.LevelInfo},
		{"LogMaxAge", cfg.LogMaxAge, 24 * time.Hour},
		{"LogMaxTotalMB", cfg.LogMaxTotalMB, defaultLogMaxTotalMB},
		{"AuditMaxAge", cfg.AuditMaxAge, 365 * 24 * time.Hour},
		{"AuditMaxRows", cfg.AuditMaxRows, defaultAuditMaxRows},
		{"RateStatusPerMin", cfg.RateStatusPerMin, defaultRateStatusPerMin},
		{"RatePairPerMin", cfg.RatePairPerMin, defaultRatePairPerMin},
		{"RateGuardedPerSec", cfg.RateGuardedPerSec, defaultRateGuardedPerSec},
		{"PairTTLMax", cfg.PairTTLMax, defaultPairTTLMaxMin * time.Minute},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s = %v, want %v", c.name, c.got, c.want)
		}
	}
	if len(cfg.PublicAddrs) != 1 || cfg.PublicAddrs[0] != "vps.example.com" {
		t.Errorf("PublicAddrs = %v, want [vps.example.com]", cfg.PublicAddrs)
	}
	if len(cfg.ProtectedContainers) != 0 {
		t.Errorf("ProtectedContainers = %v, want empty", cfg.ProtectedContainers)
	}
}

func TestLoadOverrides(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		env   map[string]string
		check func(t *testing.T, cfg Config)
	}{
		{
			name: "state dir override",
			env:  map[string]string{envStateDir: "/srv/devmon"},
			check: func(t *testing.T, cfg Config) {
				if cfg.StateDir != "/srv/devmon" {
					t.Errorf("StateDir = %q, want /srv/devmon", cfg.StateDir)
				}
			},
		},
		{
			name: "listen addr override",
			env:  map[string]string{envListenAddr: "127.0.0.1:9443"},
			check: func(t *testing.T, cfg Config) {
				if cfg.ListenAddr != "127.0.0.1:9443" {
					t.Errorf("ListenAddr = %q, want 127.0.0.1:9443", cfg.ListenAddr)
				}
			},
		},
		{
			name: "multiple public addrs including an IP",
			env:  map[string]string{envPublicAddr: "vps.example.com, 203.0.113.7 ,alt.example.com"},
			check: func(t *testing.T, cfg Config) {
				want := []string{"vps.example.com", "203.0.113.7", "alt.example.com"}
				if strings.Join(cfg.PublicAddrs, ",") != strings.Join(want, ",") {
					t.Errorf("PublicAddrs = %v, want %v", cfg.PublicAddrs, want)
				}
			},
		},
		{
			name: "policy mode override",
			env:  map[string]string{envPolicyMode: "full"},
			check: func(t *testing.T, cfg Config) {
				if cfg.PolicyMode != policy.ModeFull {
					t.Errorf("PolicyMode = %v, want full", cfg.PolicyMode)
				}
			},
		},
		{
			name: "docker host over tcp",
			env:  map[string]string{envDockerHost: "tcp://10.0.0.5:2376"},
			check: func(t *testing.T, cfg Config) {
				if cfg.DockerHost != "tcp://10.0.0.5:2376" {
					t.Errorf("DockerHost = %q, want tcp://10.0.0.5:2376", cfg.DockerHost)
				}
			},
		},
		{
			name: "log level override",
			env:  map[string]string{envLogLevel: "debug"},
			check: func(t *testing.T, cfg Config) {
				if cfg.LogLevel != slog.LevelDebug {
					t.Errorf("LogLevel = %v, want debug", cfg.LogLevel)
				}
			},
		},
		{
			name: "retention overrides",
			env: map[string]string{
				envLogMaxAgeDays:   "7",
				envLogMaxTotalMB:   "128",
				envAuditMaxAgeDays: "30",
				envAuditMaxRows:    "5000",
			},
			check: func(t *testing.T, cfg Config) {
				if cfg.LogMaxAge != 7*24*time.Hour {
					t.Errorf("LogMaxAge = %v, want 168h", cfg.LogMaxAge)
				}
				if cfg.LogMaxTotalMB != 128 {
					t.Errorf("LogMaxTotalMB = %d, want 128", cfg.LogMaxTotalMB)
				}
				if cfg.AuditMaxAge != 30*24*time.Hour {
					t.Errorf("AuditMaxAge = %v, want 720h", cfg.AuditMaxAge)
				}
				if cfg.AuditMaxRows != 5000 {
					t.Errorf("AuditMaxRows = %d, want 5000", cfg.AuditMaxRows)
				}
			},
		},
		{
			name: "twenty SANs are accepted",
			env:  map[string]string{envPublicAddr: manySANs(20)},
			check: func(t *testing.T, cfg Config) {
				if len(cfg.PublicAddrs) != 20 {
					t.Errorf("len(PublicAddrs) = %d, want 20", len(cfg.PublicAddrs))
				}
			},
		},
		{
			name: "self container override, name form",
			env:  map[string]string{envSelfContainer: "devmon-agent"},
			check: func(t *testing.T, cfg Config) {
				if cfg.SelfContainer != "devmon-agent" {
					t.Errorf("SelfContainer = %q, want devmon-agent", cfg.SelfContainer)
				}
			},
		},
		{
			name: "self container override, short hex ID form",
			env:  map[string]string{envSelfContainer: "0123456789ab"},
			check: func(t *testing.T, cfg Config) {
				if cfg.SelfContainer != "0123456789ab" {
					t.Errorf("SelfContainer = %q, want 0123456789ab", cfg.SelfContainer)
				}
			},
		},
		{
			name: "self container override, full hex ID form",
			env: map[string]string{
				envSelfContainer: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
			},
			check: func(t *testing.T, cfg Config) {
				want := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
				if cfg.SelfContainer != want {
					t.Errorf("SelfContainer = %q, want %s", cfg.SelfContainer, want)
				}
			},
		},
		{
			name: "self container absent is the normal path",
			env:  map[string]string{},
			check: func(t *testing.T, cfg Config) {
				if cfg.SelfContainer != "" {
					t.Errorf("SelfContainer = %q, want empty", cfg.SelfContainer)
				}
			},
		},
		{
			name: "pair TTL max empty string falls back to the default",
			env:  map[string]string{envPairTTLMax: ""},
			check: func(t *testing.T, cfg Config) {
				if cfg.PairTTLMax != defaultPairTTLMaxMin*time.Minute {
					t.Errorf("PairTTLMax = %v, want %dm (default)", cfg.PairTTLMax, defaultPairTTLMaxMin)
				}
			},
		},
		{
			name: "pair TTL max override, lower bound",
			env:  map[string]string{envPairTTLMax: "5"},
			check: func(t *testing.T, cfg Config) {
				if cfg.PairTTLMax != 5*time.Minute {
					t.Errorf("PairTTLMax = %v, want 5m", cfg.PairTTLMax)
				}
			},
		},
		{
			name: "pair TTL max override, upper bound",
			env:  map[string]string{envPairTTLMax: "60"},
			check: func(t *testing.T, cfg Config) {
				if cfg.PairTTLMax != 60*time.Minute {
					t.Errorf("PairTTLMax = %v, want 60m", cfg.PairTTLMax)
				}
			},
		},
		{
			name: "protected containers, single name",
			env:  map[string]string{envProtectedContainers: "reverse-proxy"},
			check: func(t *testing.T, cfg Config) {
				want := []string{"reverse-proxy"}
				if strings.Join(cfg.ProtectedContainers, ",") != strings.Join(want, ",") {
					t.Errorf("ProtectedContainers = %v, want %v", cfg.ProtectedContainers, want)
				}
			},
		},
		{
			name: "protected containers, mixed name and IDs with ragged spaces",
			env: map[string]string{
				envProtectedContainers: "proxy, 0123456789ab ,0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
			},
			check: func(t *testing.T, cfg Config) {
				want := []string{
					"proxy",
					"0123456789ab",
					"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
				}
				if strings.Join(cfg.ProtectedContainers, ",") != strings.Join(want, ",") {
					t.Errorf("ProtectedContainers = %v, want %v", cfg.ProtectedContainers, want)
				}
			},
		},
		{
			name: "protected containers, duplicates deduped preserving order",
			env:  map[string]string{envProtectedContainers: "a1,b2,a1"},
			check: func(t *testing.T, cfg Config) {
				want := []string{"a1", "b2"}
				if strings.Join(cfg.ProtectedContainers, ",") != strings.Join(want, ",") {
					t.Errorf("ProtectedContainers = %v, want %v", cfg.ProtectedContainers, want)
				}
			},
		},
		{
			name: "protected containers, all commas yields empty with no error",
			env:  map[string]string{envProtectedContainers: " , ,"},
			check: func(t *testing.T, cfg Config) {
				if len(cfg.ProtectedContainers) != 0 {
					t.Errorf("ProtectedContainers = %v, want empty", cfg.ProtectedContainers)
				}
			},
		},
		{
			name: "protected containers, case preserved",
			env:  map[string]string{envProtectedContainers: "MyDb"},
			check: func(t *testing.T, cfg Config) {
				want := []string{"MyDb"}
				if strings.Join(cfg.ProtectedContainers, ",") != strings.Join(want, ",") {
					t.Errorf("ProtectedContainers = %v, want %v", cfg.ProtectedContainers, want)
				}
			},
		},
		{
			name: "rate limit overrides",
			env: map[string]string{
				envRateStatusPerMin:  "60",
				envRatePairPerMin:    "10",
				envRateGuardedPerSec: "40",
			},
			check: func(t *testing.T, cfg Config) {
				if cfg.RateStatusPerMin != 60 {
					t.Errorf("RateStatusPerMin = %d, want 60", cfg.RateStatusPerMin)
				}
				if cfg.RatePairPerMin != 10 {
					t.Errorf("RatePairPerMin = %d, want 10", cfg.RatePairPerMin)
				}
				if cfg.RateGuardedPerSec != 40 {
					t.Errorf("RateGuardedPerSec = %d, want 40", cfg.RateGuardedPerSec)
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			// Arrange
			env := minimalEnv()
			for k, v := range tt.env {
				env[k] = v
			}

			// Act
			cfg, err := Load(fakeEnv(env))

			// Assert
			if err != nil {
				t.Fatalf("Load() unexpected error: %v", err)
			}
			tt.check(t, cfg)
		})
	}
}

func TestLoadRejections(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		env     map[string]string
		wantKey string
	}{
		{"relative state dir", map[string]string{envStateDir: "var/lib/devmon"}, envStateDir},
		{"listen addr without a port", map[string]string{envListenAddr: "8443"}, envListenAddr},
		{"listen addr port out of range", map[string]string{envListenAddr: ":70000"}, envListenAddr},
		{"listen addr non-numeric port", map[string]string{envListenAddr: ":https"}, envListenAddr},
		{"public addr with a port", map[string]string{envPublicAddr: "vps.example.com:8443"}, envPublicAddr},
		{"public addr with a scheme", map[string]string{envPublicAddr: "https://vps.example.com"}, envPublicAddr},
		{"public addr with a bad label", map[string]string{envPublicAddr: "-bad.example.com"}, envPublicAddr},
		{"unknown policy mode", map[string]string{envPolicyMode: "admin"}, envPolicyMode},
		{"mixed-case policy mode", map[string]string{envPolicyMode: "Full"}, envPolicyMode},
		{"unsupported docker scheme", map[string]string{envDockerHost: "http://10.0.0.5:2375"}, envDockerHost},
		{"unknown log level", map[string]string{envLogLevel: "trace"}, envLogLevel},
		{"non-integer log age", map[string]string{envLogMaxAgeDays: "x"}, envLogMaxAgeDays},
		{"zero log age", map[string]string{envLogMaxAgeDays: "0"}, envLogMaxAgeDays},
		{"non-integer log size", map[string]string{envLogMaxTotalMB: "big"}, envLogMaxTotalMB},
		{"log size below the floor", map[string]string{envLogMaxTotalMB: "4"}, envLogMaxTotalMB},
		{"non-integer audit age", map[string]string{envAuditMaxAgeDays: "y"}, envAuditMaxAgeDays},
		{"non-integer audit rows", map[string]string{envAuditMaxRows: "z"}, envAuditMaxRows},
		{"audit rows below the floor", map[string]string{envAuditMaxRows: "10"}, envAuditMaxRows},
		{
			name:    "audit age shorter than log age",
			env:     map[string]string{envLogMaxAgeDays: "7", envAuditMaxAgeDays: "1"},
			wantKey: envAuditMaxAgeDays,
		},
		{
			name:    "self container leading hyphen",
			env:     map[string]string{envSelfContainer: "-devmon-agent"},
			wantKey: envSelfContainer,
		},
		{
			name:    "self container leading period",
			env:     map[string]string{envSelfContainer: ".devmon-agent"},
			wantKey: envSelfContainer,
		},
		{
			name:    "self container with embedded slash",
			env:     map[string]string{envSelfContainer: "devmon/agent"},
			wantKey: envSelfContainer,
		},
		{
			name:    "self container with a space",
			env:     map[string]string{envSelfContainer: "devmon agent"},
			wantKey: envSelfContainer,
		},
		{
			name:    "self container single character",
			env:     map[string]string{envSelfContainer: "a"},
			wantKey: envSelfContainer,
		},
		{
			name:    "protected containers leading hyphen",
			env:     map[string]string{envProtectedContainers: "-bad"},
			wantKey: envProtectedContainers,
		},
		{
			name:    "protected containers embedded slash",
			env:     map[string]string{envProtectedContainers: "a/b"},
			wantKey: envProtectedContainers,
		},
		{
			name:    "protected containers inner space",
			env:     map[string]string{envProtectedContainers: "bad name"},
			wantKey: envProtectedContainers,
		},
		{
			name:    "protected containers single character",
			env:     map[string]string{envProtectedContainers: "x"},
			wantKey: envProtectedContainers,
		},
		{"non-integer status rate", map[string]string{envRateStatusPerMin: "many"}, envRateStatusPerMin},
		{"zero status rate", map[string]string{envRateStatusPerMin: "0"}, envRateStatusPerMin},
		{"negative status rate", map[string]string{envRateStatusPerMin: "-1"}, envRateStatusPerMin},
		{"non-integer pair rate", map[string]string{envRatePairPerMin: "many"}, envRatePairPerMin},
		{"zero pair rate", map[string]string{envRatePairPerMin: "0"}, envRatePairPerMin},
		{"negative pair rate", map[string]string{envRatePairPerMin: "-1"}, envRatePairPerMin},
		{"non-integer guarded rate", map[string]string{envRateGuardedPerSec: "many"}, envRateGuardedPerSec},
		{"zero guarded rate", map[string]string{envRateGuardedPerSec: "0"}, envRateGuardedPerSec},
		{"negative guarded rate", map[string]string{envRateGuardedPerSec: "-1"}, envRateGuardedPerSec},
		{"pair TTL max below the floor", map[string]string{envPairTTLMax: "4"}, envPairTTLMax},
		{"pair TTL max above the ceiling", map[string]string{envPairTTLMax: "61"}, envPairTTLMax},
		{"non-integer pair TTL max", map[string]string{envPairTTLMax: "many"}, envPairTTLMax},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			// Arrange
			env := minimalEnv()
			for k, v := range tt.env {
				env[k] = v
			}

			// Act
			_, err := Load(fakeEnv(env))

			// Assert
			var vErr *ValidationError
			if !errors.As(err, &vErr) {
				t.Fatalf("Load() error = %v, want *ValidationError", err)
			}
			if !strings.Contains(vErr.Error(), tt.wantKey) {
				t.Errorf("error %q does not name %s", vErr.Error(), tt.wantKey)
			}
		})
	}
}

func TestLoadEmptyEnvironmentNamesPublicAddr(t *testing.T) {
	t.Parallel()

	// Arrange / Act
	_, err := Load(fakeEnv(map[string]string{}))

	// Assert
	var vErr *ValidationError
	if !errors.As(err, &vErr) {
		t.Fatalf("Load() error = %v, want *ValidationError", err)
	}
	if len(vErr.Problems) != 1 {
		t.Fatalf("Problems = %v, want exactly the missing SAN", vErr.Problems)
	}
	if !strings.Contains(vErr.Problems[0], envPublicAddr) {
		t.Errorf("problem %q does not name %s", vErr.Problems[0], envPublicAddr)
	}
}

func TestLoadAggregatesEveryProblem(t *testing.T) {
	t.Parallel()

	// Arrange — three simultaneous faults, as an operator would realistically
	// produce by copying a stale docker run line.
	env := map[string]string{
		envPublicAddr:    "vps.example.com",
		envPolicyMode:    "admin",
		envLogMaxAgeDays: "x",
		envLogLevel:      "trace",
	}

	// Act
	_, err := Load(fakeEnv(env))

	// Assert
	var vErr *ValidationError
	if !errors.As(err, &vErr) {
		t.Fatalf("Load() error = %v, want *ValidationError", err)
	}
	if len(vErr.Problems) != 3 {
		t.Fatalf("Problems = %#v, want 3", vErr.Problems)
	}
	for _, key := range []string{envPolicyMode, envLogMaxAgeDays, envLogLevel} {
		if !strings.Contains(vErr.Error(), key) {
			t.Errorf("aggregated error does not name %s:\n%s", key, vErr.Error())
		}
	}
}

func TestLoadReportsPublicAddrProblemEvenAfterAnEarlierFault(t *testing.T) {
	t.Parallel()

	// Arrange — a relative DEVMON_STATE_DIR fails first, and DEVMON_PUBLIC_ADDR
	// is present but empty after trimming its comma-only entries. Both faults
	// must surface in the same pass: an operator fixing DEVMON_STATE_DIR must
	// not restart only to discover DEVMON_PUBLIC_ADDR was broken all along.
	env := map[string]string{
		envStateDir:   "relative",
		envPublicAddr: ",",
	}

	// Act
	_, err := Load(fakeEnv(env))

	// Assert
	var vErr *ValidationError
	if !errors.As(err, &vErr) {
		t.Fatalf("Load() error = %v, want *ValidationError", err)
	}
	if len(vErr.Problems) != 2 {
		t.Fatalf("Problems = %#v, want 2 (state dir and public addr)", vErr.Problems)
	}
	for _, key := range []string{envStateDir, envPublicAddr} {
		if !strings.Contains(vErr.Error(), key) {
			t.Errorf("aggregated error does not name %s:\n%s", key, vErr.Error())
		}
	}
}

func TestLoadAggregatesRateLimitProblems(t *testing.T) {
	t.Parallel()

	// Arrange — three simultaneous rate faults, as an operator would produce by
	// copying a stale env file with the wrong shell quoting.
	env := map[string]string{
		envPublicAddr:        "vps.example.com",
		envRateStatusPerMin:  "0",
		envRatePairPerMin:    "-1",
		envRateGuardedPerSec: "many",
	}

	// Act
	_, err := Load(fakeEnv(env))

	// Assert
	var vErr *ValidationError
	if !errors.As(err, &vErr) {
		t.Fatalf("Load() error = %v, want *ValidationError", err)
	}
	if len(vErr.Problems) != 3 {
		t.Fatalf("Problems = %#v, want 3", vErr.Problems)
	}
	for _, key := range []string{envRateStatusPerMin, envRatePairPerMin, envRateGuardedPerSec} {
		if !strings.Contains(vErr.Error(), key) {
			t.Errorf("aggregated error does not name %s:\n%s", key, vErr.Error())
		}
	}
}

func TestLoadProtectedContainersGoodEntrySurvivesAlongsideABadOne(t *testing.T) {
	t.Parallel()

	// Arrange — one well-formed entry and one malformed entry in the same list,
	// as an operator would produce by fat-fingering a single name in a longer
	// comma list rather than typing the whole thing wrong.
	env := minimalEnv()
	env[envProtectedContainers] = "good, bad name"

	// Act
	_, err := Load(fakeEnv(env))

	// Assert — the malformed entry is the only problem; the well-formed entry
	// beside it does not itself produce a fault.
	var vErr *ValidationError
	if !errors.As(err, &vErr) {
		t.Fatalf("Load() error = %v, want *ValidationError", err)
	}
	if len(vErr.Problems) != 1 {
		t.Fatalf("Problems = %#v, want exactly 1", vErr.Problems)
	}
	if !strings.Contains(vErr.Problems[0], envProtectedContainers) {
		t.Errorf("problem %q does not name %s", vErr.Problems[0], envProtectedContainers)
	}
}

func TestLoadReturnsZeroConfigOnFailure(t *testing.T) {
	t.Parallel()

	// Arrange / Act
	cfg, err := Load(fakeEnv(map[string]string{envPublicAddr: "ok.example.com", envPolicyMode: "admin"}))

	// Assert — a partially populated Config must never escape, or a caller that
	// ignores the error would start with a silently wrong policy tier.
	if err == nil {
		t.Fatal("Load() error = nil, want *ValidationError")
	}
	if !reflect.DeepEqual(cfg, Config{}) {
		t.Errorf("Load() cfg = %+v, want zero Config", cfg)
	}
}

func TestDerivedPaths(t *testing.T) {
	t.Parallel()

	// Arrange
	cfg := Config{StateDir: "/var/lib/devmon"}

	// Act / Assert
	tests := []struct {
		name string
		got  string
		want string
	}{
		{"DBPath", cfg.DBPath(), filepath.Join("/var/lib/devmon", "devmon.db")},
		{"CertsDir", cfg.CertsDir(), filepath.Join("/var/lib/devmon", "certs")},
		{"LogsDir", cfg.LogsDir(), filepath.Join("/var/lib/devmon", "logs")},
		{"AgentLogPath", cfg.AgentLogPath(), filepath.Join("/var/lib/devmon", "logs", "agent.log")},
	}
	for _, tt := range tests {
		if tt.got != tt.want {
			t.Errorf("%s = %q, want %q", tt.name, tt.got, tt.want)
		}
	}
}

func manySANs(n int) string {
	parts := make([]string, n)
	for i := range parts {
		parts[i] = "host" + string(rune('a'+i)) + ".example.com"
	}
	return strings.Join(parts, ",")
}
