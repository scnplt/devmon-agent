// SPDX-License-Identifier: AGPL-3.0-only

// Package ratelimit bounds how often a caller may be served. It hands out one
// token bucket per key behind a hard cap on how many keys it will hold, so
// the registry itself cannot become an unbounded-memory attack surface. When
// the registry is full and cannot admit a new key, the caller is expected to
// fall back to a coarser, shared bucket rather than treat the key as
// unlimited or as permanently refused.
package ratelimit

import (
	"sync"
	"time"

	"golang.org/x/time/rate"
)

// Registry hands out one token bucket per key, with a hard cap on how many
// keys it will hold.
type Registry struct {
	mu      sync.Mutex
	buckets map[string]*rate.Limiter
	limit   rate.Limit
	burst   int
	maxKeys int
}

// NewRegistry builds a Registry whose limiters each allow limit tokens per
// second with the given burst, holding at most maxKeys keys at once.
func NewRegistry(limit rate.Limit, burst, maxKeys int) *Registry {
	return &Registry{
		buckets: make(map[string]*rate.Limiter),
		limit:   limit,
		burst:   burst,
		maxKeys: maxKeys,
	}
}

// Allow reports whether key may be served at now. The second return is false
// when the registry is at capacity and could not admit key — the caller must
// then fall back to its global bucket (see the package comment).
func (r *Registry) Allow(key string, now time.Time) (allowed, keyed bool) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if lim, ok := r.buckets[key]; ok {
		return lim.AllowN(now, 1), true
	}

	if len(r.buckets) >= r.maxKeys {
		r.sweepIdleLocked(now)
	}
	if len(r.buckets) >= r.maxKeys {
		// No room could be made: every bucket is still actively throttled.
		// Refuse the key rather than admit it unbounded or lock the
		// registry up; the caller falls back to its global bucket (D9).
		return false, false
	}

	lim := rate.NewLimiter(r.limit, r.burst)
	r.buckets[key] = lim
	return lim.AllowN(now, 1), true
}

// sweepIdleLocked deletes every bucket whose token count at now has refilled
// back to its burst — an idle key, since a limiter starts full and only a
// full bucket can have that many tokens again. Callers must hold r.mu.
func (r *Registry) sweepIdleLocked(now time.Time) {
	for key, lim := range r.buckets {
		if lim.TokensAt(now) >= float64(r.burst) {
			delete(r.buckets, key)
		}
	}
}

// Len reports how many keys are currently held. Test and diagnostic use only.
func (r *Registry) Len() int {
	r.mu.Lock()
	defer r.mu.Unlock()

	return len(r.buckets)
}
