package httpapi

import (
	"log/slog"
	"math"
	"net"
	"net/http"
	"strconv"
	"time"

	"golang.org/x/time/rate"

	"github.com/scnplt/devmon-agent/internal/ratelimit"
)

// Rate-limiting constants. Each carries its own reasoning, per the Rate-
// Limiting Contract section of the hardening plan.
const (
	// msgRateLimited is the single rejection body for every tier. Which
	// bucket a caller hit is operator information — it goes to the log, not
	// the response (mirrors msgClientCertRequired in middleware.go).
	msgRateLimited = "rate limit exceeded"

	// headerRetryAfter is the header name a 429 carries its delay in, per
	// RFC 9110 section 10.2.3.
	headerRetryAfter = "Retry-After"

	// unauthGlobalPerSec and unauthGlobalBurst are the global pre-auth
	// backstop (D8). Per-IP alone does not stop a distributed scan. This is
	// a constant, not a DEVMON_RATE_* knob: deriving it from the per-IP
	// variables would mean an operator who raises one silently removes the
	// other, and a fourth env var is install surface nobody tunes.
	unauthGlobalPerSec = 50
	unauthGlobalBurst  = 100

	// guardedBurstMultiplier sizes the guarded tier's burst at 2x its
	// per-second rate: opening the container list fires one list call plus
	// one inspect per visible container, and a phone that shows twelve
	// containers must not trip the limiter on its first screen.
	guardedBurstMultiplier = 2

	// rateLimitMaxKeys bounds every per-key registry (status, pair, device)
	// so the limiter itself cannot become an unbounded-memory attack
	// surface (see internal/ratelimit's package comment).
	rateLimitMaxKeys = 4096

	// secondsPerMinute converts the DEVMON_RATE_STATUS_PER_MIN and
	// DEVMON_RATE_PAIR_PER_MIN per-minute knobs into a per-second
	// rate.Limit, which is the unit golang.org/x/time/rate works in.
	secondsPerMinute = 60

	// defaultRateStatusPerMin, defaultRatePairPerMin, and
	// defaultRateGuardedPerSec mirror internal/config's own unexported
	// defaults of the same name. NewServer floors a config value below 1
	// to its matching constant here so a zero-value config.Config — the
	// shape cmd/devmon-agent/main.go's and this package's own tests build —
	// never yields a limiter that refuses everything. config.Load itself
	// can never produce a value below 1 (its own minRatePerX floor), so
	// this floor exists purely for zero-value Config literals in tests.
	defaultRateStatusPerMin  = 30
	defaultRatePairPerMin    = 5
	defaultRateGuardedPerSec = 20
)

// clientIP extracts the caller's address from r.RemoteAddr, which net/http
// sets from the accepted TCP connection and a client cannot forge.
//
// X-Forwarded-For is deliberately never consulted (D5): the documented
// deployment is direct inbound with no reverse proxy, and honouring a
// client-supplied forwarding header would let any caller mint a fresh
// limiter key per request and bypass the limiter entirely — worse than no
// limiter at all, because it would look like protection.
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// tooManyRequests writes a 429 with an integer Retry-After.
//
// Retry-After is ceil(1/ratePerSecond) seconds, floored at 1 — an integer,
// per RFC 9110 section 10.2.3, which allows no fractional seconds. It is
// derived from the configured rate rather than from a Reservation, because
// Reserve consumes a token and this request has already been refused.
//
// The header is set before writeError, which commits the status line and
// headers by calling WriteHeader.
func (s *Server) tooManyRequests(w http.ResponseWriter, limit rate.Limit) {
	retryAfter := int(math.Ceil(1 / float64(limit)))
	if retryAfter < 1 {
		retryAfter = 1
	}
	w.Header().Set(headerRetryAfter, strconv.Itoa(retryAfter))
	s.writeError(w, http.StatusTooManyRequests, msgRateLimited)
}

// withGlobalUnauthLimit enforces the global pre-authentication backstop
// (D8), checked before either IP-keyed tier. One shared bucket: unlike
// withIPLimit, there is no per-key registry here, because the whole point
// of this tier is to bound the unauthenticated surface in aggregate.
func (s *Server) withGlobalUnauthLimit(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.unauthGlobal.AllowN(time.Now(), 1) {
			s.log.Warn("rate limit exceeded", slog.String("tier", "global-unauth"))
			s.tooManyRequests(w, s.unauthGlobal.Limit())
			return
		}
		next.ServeHTTP(w, r)
	})
}

// withIPLimit enforces one of the two per-IP pre-authentication tiers
// (status or pair), keyed on clientIP. limit is the tier's configured
// per-second rate, used only to compute Retry-After on a rejection — the
// registry itself does not expose it.
//
// When the registry is at capacity and cannot admit a new key, Allow
// reports keyed == false; the request then falls back to the shared global
// bucket rather than being admitted unconditionally or refused outright
// (D9), which is strictly tighter than no limiter and still serves a
// legitimate caller whenever the global bucket has tokens.
//
// Logging names the tier and nothing else: the IP is attacker-controlled
// input on this port, and echoing it into a size-bounded log would let a
// scanner write arbitrary volume into the operator's own diagnostics.
func (s *Server) withIPLimit(reg *ratelimit.Registry, limit rate.Limit, tier string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		now := time.Now()
		allowed, keyed := reg.Allow(clientIP(r), now)
		if !keyed {
			allowed = s.unauthGlobal.AllowN(now, 1)
		}
		if !allowed {
			s.log.Warn("rate limit exceeded", slog.String("tier", tier))
			s.tooManyRequests(w, limit)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// withDeviceLimit enforces the guarded tier, keyed on device ID rather than
// IP (D6): a phone roams across mobile IPs mid-incident, and a device ID is
// the stronger identifier anyway — proven by a client certificate rather
// than asserted by a packet header.
//
// It sits after requireDevice and before withAudit (D7): a throttled call
// must never write an audit row, or an authenticated device could inflate
// the audit table past DEVMON_AUDIT_MAX_ROWS and push real history out.
// Throttling is logged at Warn to the operational log instead.
//
// If DeviceFrom finds no device in the request context, withDeviceLimit
// rejects with 500 rather than passing the request through unlimited. This
// middleware only ever runs behind requireDevice; a missing device here
// means the chain was mis-composed, and a rate limiter that silently
// degrades to no limiter the moment it is mis-composed is worse than
// having none at all.
func (s *Server) withDeviceLimit(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		device, ok := DeviceFrom(r.Context())
		if !ok {
			s.log.Error("withDeviceLimit ran without a resolved device in the request context")
			s.writeError(w, http.StatusInternalServerError, "internal error")
			return
		}

		allowed, _ := s.deviceLimits.Allow(device.ID, time.Now())
		if !allowed {
			s.log.Warn("rate limit exceeded", slog.String("device_id", device.ID))
			s.tooManyRequests(w, s.deviceLimit)
			return
		}
		next.ServeHTTP(w, r)
	})
}
