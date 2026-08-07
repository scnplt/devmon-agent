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
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s = %v, want %v", c.name, c.got, c.want)
		}
	}
	if len(cfg.PublicAddrs) != 1 || cfg.PublicAddrs[0] != "vps.example.com" {
		t.Errorf("PublicAddrs = %v, want [vps.example.com]", cfg.PublicAddrs)
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
