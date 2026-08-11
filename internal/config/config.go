// SPDX-License-Identifier: AGPL-3.0-only

// Package config parses and validates the agent's entire startup surface.
//
// Every knob is an environment variable read exactly once, at start, and
// immutable thereafter. This is the agent's core security property: its powers
// are fixed by the operator's startup configuration, never by a client. There is
// deliberately no API, file, or signal that can change any value here.
package config

import (
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/scnplt/devmon-agent/internal/policy"
)

// Environment variable keys. Declared as constants so a typo is a compile error
// and so error messages can name the exact variable the operator must fix.
const (
	envStateDir        = "DEVMON_STATE_DIR"
	envListenAddr      = "DEVMON_LISTEN_ADDR"
	envPublicAddr      = "DEVMON_PUBLIC_ADDR"
	envPolicyMode      = "DEVMON_POLICY_MODE"
	envDockerHost      = "DEVMON_DOCKER_HOST"
	envLogLevel        = "DEVMON_LOG_LEVEL"
	envLogMaxAgeDays   = "DEVMON_LOG_MAX_AGE_DAYS"
	envLogMaxTotalMB   = "DEVMON_LOG_MAX_TOTAL_MB"
	envAuditMaxAgeDays = "DEVMON_AUDIT_MAX_AGE_DAYS"
	envAuditMaxRows    = "DEVMON_AUDIT_MAX_ROWS"
	envSelfContainerID = "DEVMON_SELF_CONTAINER_ID"

	envRateStatusPerMin  = "DEVMON_RATE_STATUS_PER_MIN"
	envRatePairPerMin    = "DEVMON_RATE_PAIR_PER_MIN"
	envRateGuardedPerSec = "DEVMON_RATE_GUARDED_PER_SEC"
)

// Defaults. DEVMON_PUBLIC_ADDR deliberately has none — a server certificate with
// no SAN is useless, and the failure would otherwise surface weeks later as an
// opaque TLS error on the operator's phone rather than at startup.
const (
	defaultStateDir        = "/var/lib/devmon"
	defaultListenAddr      = ":8443"
	defaultDockerHost      = "unix:///var/run/docker.sock"
	defaultLogLevel        = "info"
	defaultLogMaxAgeDays   = 1
	defaultLogMaxTotalMB   = 64
	defaultAuditMaxAgeDays = 365
	defaultAuditMaxRows    = 100000

	defaultRateStatusPerMin  = 30
	defaultRatePairPerMin    = 5
	defaultRateGuardedPerSec = 20
)

// Validation bounds.
const (
	minPort = 1
	maxPort = 65535

	// minLogMaxTotalMB floors the operational log budget. The total is divided
	// across (backups+1) files by internal/logging because lumberjack's MaxSize
	// is per file; below this floor the integer division yields 0, which
	// lumberjack reads as "unlimited" and the size cap disappears entirely.
	minLogMaxTotalMB = 8

	minDays         = 1
	minAuditMaxRows = 1000

	// minRatePerX is the floor for every rate-limit knob. It is 1, not 0: a
	// value of 0 would read as "no requests permitted", which bricks the agent
	// in a way that looks like a network fault from the client side, and there
	// must be no value that disables the limiter — an operator who wants no
	// ceiling raises the number instead.
	minRatePerX = 1

	maxDNSLabelLen = 63
	maxDNSNameLen  = 253
)

const hoursPerDay = 24

// shortContainerIDPattern and fullContainerIDPattern match Docker's short
// (12-character) and full (64-character) hex container IDs. Docker always
// lower-cases these; an uppercase value is treated as a typo rather than
// silently normalized, so the operator finds out at start.
var (
	shortContainerIDPattern = regexp.MustCompile(`^[0-9a-f]{12}$`)
	fullContainerIDPattern  = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// Config is the fully parsed, validated startup configuration.
type Config struct {
	StateDir      string
	ListenAddr    string
	PublicAddrs   []string
	PolicyMode    policy.Mode
	DockerHost    string
	LogLevel      slog.Level
	LogMaxAge     time.Duration
	LogMaxTotalMB int
	AuditMaxAge   time.Duration
	AuditMaxRows  int

	RateStatusPerMin  int
	RatePairPerMin    int
	RateGuardedPerSec int

	// SelfContainerID is an operator-supplied override for the agent's own
	// container ID, used when the agent cannot detect it automatically. Empty
	// is the normal case; it has no default.
	SelfContainerID string
}

// Derived paths live here as methods so no other package ever concatenates a
// state path by hand and drifts from the documented layout.

// DBPath is the SQLite state store.
func (c Config) DBPath() string { return filepath.Join(c.StateDir, "devmon.db") }

// CertsDir holds the server keypair (and, from Phase 2, the CA).
func (c Config) CertsDir() string { return filepath.Join(c.StateDir, "certs") }

// LogsDir holds the rotated operational logs.
func (c Config) LogsDir() string { return filepath.Join(c.StateDir, "logs") }

// AgentLogPath is the current operational log file.
func (c Config) AgentLogPath() string { return filepath.Join(c.LogsDir(), "agent.log") }

// ValidationError reports every problem found in one pass.
//
// It aggregates rather than failing on the first fault: an operator correcting a
// `docker run` line one variable at a time, one restart at a time, is a bad first
// experience and the exact thing the PRD asks this package to avoid.
type ValidationError struct{ Problems []string }

func (e *ValidationError) Error() string {
	return "invalid configuration:\n  " + strings.Join(e.Problems, "\n  ")
}

// Load reads the configuration from getenv, applies defaults, and validates.
//
// getenv is a parameter rather than a direct call to os.Getenv so tests can
// supply a fake map without mutating process state — t.Setenv forbids
// t.Parallel, and this package's table test is the largest in the repository.
//
// On failure it returns a *ValidationError listing every problem, never just the
// first, and a zero Config that callers must not use.
func Load(getenv func(string) string) (Config, error) {
	l := &loader{getenv: getenv}

	cfg := Config{
		StateDir:      l.stateDir(),
		ListenAddr:    l.listenAddr(),
		PublicAddrs:   l.publicAddrs(),
		PolicyMode:    l.policyMode(),
		DockerHost:    l.dockerHost(),
		LogLevel:      l.logLevel(),
		LogMaxAge:     l.days(envLogMaxAgeDays, defaultLogMaxAgeDays),
		LogMaxTotalMB: l.boundedInt(envLogMaxTotalMB, defaultLogMaxTotalMB, minLogMaxTotalMB),
		AuditMaxAge:   l.days(envAuditMaxAgeDays, defaultAuditMaxAgeDays),
		AuditMaxRows:  l.boundedInt(envAuditMaxRows, defaultAuditMaxRows, minAuditMaxRows),

		RateStatusPerMin:  l.boundedInt(envRateStatusPerMin, defaultRateStatusPerMin, minRatePerX),
		RatePairPerMin:    l.boundedInt(envRatePairPerMin, defaultRatePairPerMin, minRatePerX),
		RateGuardedPerSec: l.boundedInt(envRateGuardedPerSec, defaultRateGuardedPerSec, minRatePerX),

		SelfContainerID: l.selfContainerID(),
	}

	l.checkRetentionOrder(cfg)

	if len(l.problems) > 0 {
		return Config{}, &ValidationError{Problems: l.problems}
	}
	return cfg, nil
}

// loader threads the environment accessor and the accumulating problem list
// through the per-field parsers so each one can record a fault and continue.
type loader struct {
	getenv   func(string) string
	problems []string
}

func (l *loader) fail(key, format string, args ...any) {
	l.problems = append(l.problems, key+": "+fmt.Sprintf(format, args...))
}

// raw returns the trimmed value of key, or def when it is unset or blank.
func (l *loader) raw(key, def string) string {
	if v := strings.TrimSpace(l.getenv(key)); v != "" {
		return v
	}
	return def
}

func (l *loader) stateDir() string {
	dir := l.raw(envStateDir, defaultStateDir)
	if !isAbsPath(dir) {
		l.fail(envStateDir, "%q is not an absolute path", dir)
	}
	return dir
}

func (l *loader) listenAddr() string {
	addr := l.raw(envListenAddr, defaultListenAddr)
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		l.fail(envListenAddr, "%q is not a valid host:port (%v)", addr, err)
		return addr
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < minPort || n > maxPort {
		l.fail(envListenAddr, "port %q is not in %d-%d", port, minPort, maxPort)
	}
	return addr
}

func (l *loader) publicAddrs() []string {
	raw := l.raw(envPublicAddr, "")
	if raw == "" {
		l.fail(envPublicAddr, "is required (server certificate SANs; no default is safe)")
		return nil
	}

	addrs := make([]string, 0, strings.Count(raw, ",")+1)
	for _, part := range strings.Split(raw, ",") {
		e := strings.TrimSpace(part)
		if e == "" {
			continue
		}
		if !isValidSAN(e) {
			l.fail(envPublicAddr, "%q is not a valid DNS name or IP address", e)
			continue
		}
		addrs = append(addrs, e)
	}
	if len(addrs) == 0 && len(l.problems) == 0 {
		l.fail(envPublicAddr, "must list at least one address")
	}
	return addrs
}

func (l *loader) policyMode() policy.Mode {
	mode, err := policy.ParseMode(l.raw(envPolicyMode, ""))
	if err != nil {
		l.fail(envPolicyMode, "%v", err)
	}
	return mode
}

func (l *loader) dockerHost() string {
	host := l.raw(envDockerHost, defaultDockerHost)
	u, err := url.Parse(host)
	if err != nil {
		l.fail(envDockerHost, "%q is not a valid URL (%v)", host, err)
		return host
	}
	if u.Scheme != "unix" && u.Scheme != "tcp" {
		l.fail(envDockerHost, "scheme %q is not supported (want one of: unix, tcp)", u.Scheme)
	}
	return host
}

// selfContainerID parses the optional self-identification override. Absent is
// the normal path: the agent detects its own container ID by other means and
// this variable exists only as the documented fallback. A value that is
// present but malformed is a startup configuration error, not a warning — an
// operator who typos it must find out at start rather than when the delete
// button stays greyed out.
func (l *loader) selfContainerID() string {
	id := l.raw(envSelfContainerID, "")
	if id == "" {
		return ""
	}
	if !shortContainerIDPattern.MatchString(id) && !fullContainerIDPattern.MatchString(id) {
		l.fail(envSelfContainerID, "%q is not a valid container ID (want 12 or 64 lowercase hex characters)", id)
	}
	return id
}

func (l *loader) logLevel() slog.Level {
	name := l.raw(envLogLevel, defaultLogLevel)
	switch name {
	case "debug":
		return slog.LevelDebug
	case "info":
		return slog.LevelInfo
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		l.fail(envLogLevel, "%q is not a valid level (want one of: debug, info, warn, error)", name)
		return slog.LevelInfo
	}
}

// days parses an integer day count of at least minDays and returns it as a
// Duration, so no caller has to remember which unit the variable was in.
func (l *loader) days(key string, def int) time.Duration {
	return time.Duration(l.boundedInt(key, def, minDays)) * hoursPerDay * time.Hour
}

// boundedInt parses key as an integer no smaller than min. On any fault it
// records the problem and returns def, so parsing continues and every remaining
// variable is still checked.
func (l *loader) boundedInt(key string, def, min int) int {
	raw := l.raw(key, "")
	if raw == "" {
		return def
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		l.fail(key, "%q is not an integer", raw)
		return def
	}
	if n < min {
		l.fail(key, "%d is below the minimum of %d", n, min)
		return def
	}
	return n
}

// checkRetentionOrder enforces the PRD's separate-retention-budgets rule: the
// security record must outlive debug output. One shared short budget would
// quietly destroy the audit trail to make room for operational logs.
func (l *loader) checkRetentionOrder(cfg Config) {
	if cfg.AuditMaxAge < cfg.LogMaxAge {
		l.fail(envAuditMaxAgeDays,
			"%d days is shorter than %s (%d days); the audit record must outlive operational logs",
			int(cfg.AuditMaxAge.Hours()/hoursPerDay),
			envLogMaxAgeDays,
			int(cfg.LogMaxAge.Hours()/hoursPerDay),
		)
	}
}

// isAbsPath reports whether p is an absolute path. A leading slash counts even
// on Windows: the agent's paths are container paths, and the check must give the
// same answer on an operator's machine as it does in the image.
func isAbsPath(p string) bool {
	return strings.HasPrefix(p, "/") || filepath.IsAbs(p)
}

// isValidSAN reports whether e may be used as a certificate subject alternative
// name. Entries carrying a port or a path are rejected outright — "host:8443" is
// the most likely operator mistake here, and accepting it would produce a
// certificate that matches nothing.
func isValidSAN(e string) bool {
	if strings.ContainsAny(e, ":/") {
		return false
	}
	if net.ParseIP(e) != nil {
		return true
	}
	return isValidDNSName(e)
}

func isValidDNSName(s string) bool {
	if s == "" || len(s) > maxDNSNameLen {
		return false
	}
	for _, label := range strings.Split(strings.TrimSuffix(s, "."), ".") {
		if !isValidDNSLabel(label) {
			return false
		}
	}
	return true
}

func isValidDNSLabel(label string) bool {
	if label == "" || len(label) > maxDNSLabelLen {
		return false
	}
	if label[0] == '-' || label[len(label)-1] == '-' {
		return false
	}
	for i := 0; i < len(label); i++ {
		c := label[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '-':
		default:
			return false
		}
	}
	return true
}
