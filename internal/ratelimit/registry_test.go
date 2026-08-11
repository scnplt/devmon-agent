// SPDX-License-Identifier: AGPL-3.0-only

package ratelimit_test

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"golang.org/x/time/rate"

	"github.com/scnplt/devmon-agent/internal/ratelimit"
)

// TestRegistryAllowWithinBurst covers the single-key path: up to burst calls
// at the same instant are all admitted, and the next one is refused.
func TestRegistryAllowWithinBurst(t *testing.T) {
	t.Parallel()

	// Arrange
	const burst = 5
	reg := ratelimit.NewRegistry(rate.Every(time.Minute), burst, 10)
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	tests := []struct {
		name string
		call int
		want bool
	}{
		{name: "call 1 within burst", call: 1, want: true},
		{name: "call 2 within burst", call: 2, want: true},
		{name: "call 3 within burst", call: 3, want: true},
		{name: "call 4 within burst", call: 4, want: true},
		{name: "call 5 within burst", call: 5, want: true},
		{name: "call 6 past burst is refused", call: 6, want: false},
	}

	// Act / Assert — sequential calls against the same key and instant
	for _, tt := range tests {
		allowed, keyed := reg.Allow("k", now)
		if !keyed {
			t.Fatalf("%s: Allow keyed = false, want true", tt.name)
		}
		if allowed != tt.want {
			t.Errorf("%s: Allow = %v, want %v", tt.name, allowed, tt.want)
		}
	}
}

// TestRegistryAllowRefillsOverTime proves a refusal clears once the refill
// interval has elapsed, using a synthetic clock — never a real sleep.
func TestRegistryAllowRefillsOverTime(t *testing.T) {
	t.Parallel()

	// Arrange
	const burst = 1
	interval := time.Second
	reg := ratelimit.NewRegistry(rate.Every(interval), burst, 10)
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	// Act / Assert — first call at start exhausts the single-token burst
	allowed, keyed := reg.Allow("k", start)
	if !keyed || !allowed {
		t.Fatalf("first call = (%v, %v), want (true, true)", allowed, keyed)
	}

	// immediately after, the bucket is empty
	allowed, keyed = reg.Allow("k", start)
	if !keyed || allowed {
		t.Fatalf("second call = (%v, %v), want (true, false)", allowed, keyed)
	}

	// after the refill interval, a token is available again
	allowed, keyed = reg.Allow("k", start.Add(interval))
	if !keyed || !allowed {
		t.Fatalf("call after refill = (%v, %v), want (true, true)", allowed, keyed)
	}
}

// TestRegistryAllowKeyIsolation proves two keys never share tokens.
func TestRegistryAllowKeyIsolation(t *testing.T) {
	t.Parallel()

	// Arrange
	reg := ratelimit.NewRegistry(rate.Every(time.Minute), 1, 10)
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	// Act — exhaust key "a"
	allowed, keyed := reg.Allow("a", now)
	if !keyed || !allowed {
		t.Fatalf("first call for a = (%v, %v), want (true, true)", allowed, keyed)
	}
	allowed, keyed = reg.Allow("a", now)
	if !keyed || allowed {
		t.Fatalf("second call for a = (%v, %v), want (true, false)", allowed, keyed)
	}

	// Assert — key "b" is unaffected
	allowed, keyed = reg.Allow("b", now)
	if !keyed || !allowed {
		t.Fatalf("first call for b = (%v, %v), want (true, true)", allowed, keyed)
	}
}

// TestRegistryAllowEvictsIdleBucketAtCapacity proves that when the registry is
// full of idle (full-token) buckets, a new key sweeps room for itself rather
// than being refused.
func TestRegistryAllowEvictsIdleBucketAtCapacity(t *testing.T) {
	t.Parallel()

	// Arrange — fill the registry to maxKeys. Each seed call spends exactly
	// one of its five tokens, so by the time the newcomer arrives one refill
	// interval later, every bucket has refilled back to a full (idle) burst.
	const maxKeys = 3
	const burst = 5
	interval := time.Minute
	reg := ratelimit.NewRegistry(rate.Every(interval), burst, maxKeys)
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	for i := 0; i < maxKeys; i++ {
		key := fmt.Sprintf("idle-%d", i)
		allowed, keyed := reg.Allow(key, now)
		if !keyed || !allowed {
			t.Fatalf("seeding key %q = (%v, %v), want (true, true)", key, allowed, keyed)
		}
	}
	if got := reg.Len(); got != maxKeys {
		t.Fatalf("Len() = %d, want %d", got, maxKeys)
	}

	// Act — a new key arrives one refill interval later, once every existing
	// bucket has become idle (full) again, so the sweep must make room.
	allowed, keyed := reg.Allow("newcomer", now.Add(interval))

	// Assert
	if !keyed {
		t.Fatalf("Allow keyed = false, want true (sweep should have made room)")
	}
	if !allowed {
		t.Errorf("Allow allowed = false, want true (fresh bucket starts full)")
	}
	if got := reg.Len(); got > maxKeys {
		t.Errorf("Len() = %d, want <= %d", got, maxKeys)
	}
}

// TestRegistryAllowFallsBackWhenCapacityDrained proves that when the registry
// is full and every bucket is still actively throttled (not idle), a new key
// is refused rather than admitted — the caller must fall back to its global
// bucket (D9).
func TestRegistryAllowFallsBackWhenCapacityDrained(t *testing.T) {
	t.Parallel()

	// Arrange — fill the registry to maxKeys and drain every bucket so none
	// qualifies as idle at the sweep.
	const maxKeys = 2
	const burst = 1
	reg := ratelimit.NewRegistry(rate.Every(time.Hour), burst, maxKeys)
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	for i := 0; i < maxKeys; i++ {
		key := fmt.Sprintf("drained-%d", i)
		allowed, keyed := reg.Allow(key, now)
		if !keyed || !allowed {
			t.Fatalf("seeding key %q = (%v, %v), want (true, true)", key, allowed, keyed)
		}
	}

	// Act — every bucket above is now empty (burst was 1), so none is idle.
	allowed, keyed := reg.Allow("newcomer", now)

	// Assert — the registry could not admit the new key.
	if allowed || keyed {
		t.Errorf("Allow = (%v, %v), want (false, false)", allowed, keyed)
	}
	if got := reg.Len(); got != maxKeys {
		t.Errorf("Len() = %d, want unchanged at %d", got, maxKeys)
	}
}

// TestRegistryAllowConcurrent drives 100 goroutines through Allow to prove the
// registry's map access is race-free under -race.
func TestRegistryAllowConcurrent(t *testing.T) {
	t.Parallel()

	// Arrange
	reg := ratelimit.NewRegistry(rate.Every(time.Millisecond), 10, 50)
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	const workers = 100
	var wg sync.WaitGroup
	wg.Add(workers)

	// Act — concurrent calls across a handful of shared keys and instants.
	for i := 0; i < workers; i++ {
		go func(i int) {
			defer wg.Done()
			key := fmt.Sprintf("key-%d", i%5)
			reg.Allow(key, now.Add(time.Duration(i)*time.Microsecond))
		}(i)
	}
	wg.Wait()

	// Assert — no panic, no race (checked by the race detector), and the
	// registry never exceeds its key cap.
	if got := reg.Len(); got > 5 {
		t.Errorf("Len() = %d, want <= 5", got)
	}
}

// TestRegistryAllowClockGoingBackwardsDoesNotPanic covers a `now` earlier
// than a previous call for the same key — the underlying limiter must handle
// this without panicking.
func TestRegistryAllowClockGoingBackwardsDoesNotPanic(t *testing.T) {
	t.Parallel()

	// Arrange
	reg := ratelimit.NewRegistry(rate.Every(time.Minute), 3, 10)
	later := time.Date(2026, 1, 1, 0, 10, 0, 0, time.UTC)
	earlier := later.Add(-time.Hour)

	// Act — first call establishes state at `later`, second call goes
	// backwards in time.
	reg.Allow("k", later)

	// Assert — must not panic.
	reg.Allow("k", earlier)
}
