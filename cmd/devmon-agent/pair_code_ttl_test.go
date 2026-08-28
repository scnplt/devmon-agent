// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"math"
	"strings"
	"testing"
	"time"
)

func TestResolvePairCodeTTLOmittedUsesDefaultCeiling(t *testing.T) {
	t.Parallel()

	// Act — --ttl omitted (set=false), ceiling is the config default (10m).
	// ttlMinutes is 0 here too, proving the omitted path is driven by set,
	// never by the raw value.
	got, err := resolvePairCodeTTL(0, false, 10*time.Minute)

	// Assert
	if err != nil {
		t.Fatalf("resolvePairCodeTTL() unexpected error: %v", err)
	}
	if got != 10*time.Minute {
		t.Errorf("resolvePairCodeTTL() = %v, want 10m", got)
	}
}

func TestResolvePairCodeTTLOmittedClampsToLoweredCeiling(t *testing.T) {
	t.Parallel()

	// Act — the ceiling is lowered below the 10-minute default, so the
	// effective TTL must never exceed it even with --ttl omitted.
	got, err := resolvePairCodeTTL(0, false, 7*time.Minute)

	// Assert
	if err != nil {
		t.Fatalf("resolvePairCodeTTL() unexpected error: %v", err)
	}
	if got != 7*time.Minute {
		t.Errorf("resolvePairCodeTTL() = %v, want 7m (the ceiling)", got)
	}
}

func TestResolvePairCodeTTLExplicitValueWithinRange(t *testing.T) {
	t.Parallel()

	// Act
	got, err := resolvePairCodeTTL(8, true, 15*time.Minute)

	// Assert
	if err != nil {
		t.Fatalf("resolvePairCodeTTL() unexpected error: %v", err)
	}
	if got != 8*time.Minute {
		t.Errorf("resolvePairCodeTTL() = %v, want 8m", got)
	}
}

func TestResolvePairCodeTTLAboveCeilingErrors(t *testing.T) {
	t.Parallel()

	// Act
	_, err := resolvePairCodeTTL(30, true, 15*time.Minute)

	// Assert
	if err == nil {
		t.Fatal("resolvePairCodeTTL() = nil error, want one for a TTL above the ceiling")
	}
	if !strings.Contains(err.Error(), "30") || !strings.Contains(err.Error(), "DEVMON_PAIR_TTL_MAX_MIN") || !strings.Contains(err.Error(), "15") {
		t.Errorf("resolvePairCodeTTL() error = %v, want it to name the requested value, the env var, and the ceiling", err)
	}
}

func TestResolvePairCodeTTLBelowMinimumErrors(t *testing.T) {
	t.Parallel()

	// Act
	_, err := resolvePairCodeTTL(4, true, 15*time.Minute)

	// Assert
	if err == nil {
		t.Fatal("resolvePairCodeTTL() = nil error, want one for a TTL below the 5-minute floor")
	}
	if !strings.Contains(err.Error(), "4") || !strings.Contains(err.Error(), "5") {
		t.Errorf("resolvePairCodeTTL() error = %v, want it to name the requested value and the 5-minute minimum", err)
	}
}

// TestResolvePairCodeTTLExplicitZeroErrors proves an explicit `--ttl 0` is a
// hard error rather than being silently redirected to the default TTL — the
// second security-review finding on issue #129. Only set=false (the flag
// truly omitted) takes the default path; set=true with ttlMinutes==0 must
// fail exactly like any other below-floor explicit value.
func TestResolvePairCodeTTLExplicitZeroErrors(t *testing.T) {
	t.Parallel()

	// Act
	got, err := resolvePairCodeTTL(0, true, 15*time.Minute)

	// Assert
	if err == nil {
		t.Fatal("resolvePairCodeTTL() = nil error, want one for an explicit --ttl 0")
	}
	if got != 0 {
		t.Errorf("resolvePairCodeTTL() = %v, want 0 on error", got)
	}
	if !strings.Contains(err.Error(), "0") || !strings.Contains(err.Error(), "5") {
		t.Errorf("resolvePairCodeTTL() error = %v, want it to name the requested value and the 5-minute minimum", err)
	}
}

// TestResolvePairCodeTTLExplicitNegativeErrors proves an explicit negative
// --ttl is a hard error, not clamped or treated as omitted.
func TestResolvePairCodeTTLExplicitNegativeErrors(t *testing.T) {
	t.Parallel()

	// Act
	got, err := resolvePairCodeTTL(-1, true, 15*time.Minute)

	// Assert
	if err == nil {
		t.Fatal("resolvePairCodeTTL() = nil error, want one for an explicit negative --ttl")
	}
	if got != 0 {
		t.Errorf("resolvePairCodeTTL() = %v, want 0 on error", got)
	}
	if !strings.Contains(err.Error(), "-1") || !strings.Contains(err.Error(), "5") {
		t.Errorf("resolvePairCodeTTL() error = %v, want it to name the requested value and the 5-minute minimum", err)
	}
}

// TestResolvePairCodeTTLOverflowingValuesAreHardErrors proves resolvePairCodeTTL
// range-checks the raw --ttl minute count before ever multiplying it by
// time.Minute. time.Duration is int64 nanoseconds, so multiplying first lets
// a huge minute count wrap via two's-complement overflow into an
// innocent-looking Duration that would slip past the ceiling check — see the
// security-review finding on issue #129. Every case here must be rejected
// with an error that names the literal value the caller passed, never
// silently accepted or clamped.
func TestResolvePairCodeTTLOverflowingValuesAreHardErrors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		minutes int
		ceiling time.Duration
		wantIn  []string // substrings the error message must contain
	}{
		{
			// 9007199254741022 minutes * time.Minute (60e9 ns) overflows
			// int64 and wraps around to exactly 30m — historically accepted
			// against a 60m ceiling despite being nowhere near it.
			name:    "wraps to exactly 30m under naive multiplication",
			minutes: 9007199254741022,
			ceiling: 60 * time.Minute,
			wantIn:  []string{"9007199254741022", pairTTLMaxEnvVar, "60"},
		},
		{
			name:    "math.MaxInt64",
			minutes: math.MaxInt64,
			ceiling: 60 * time.Minute,
			wantIn:  []string{"9223372036854775807", pairTTLMaxEnvVar, "60"},
		},
		{
			name:    "large negative value",
			minutes: -9007199254741022,
			ceiling: 60 * time.Minute,
			wantIn:  []string{"-9007199254741022", "5"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			// Act
			got, err := resolvePairCodeTTL(tt.minutes, true, tt.ceiling)

			// Assert
			if err == nil {
				t.Fatalf("resolvePairCodeTTL(%d, %v) = %v, nil error, want a hard error", tt.minutes, tt.ceiling, got)
			}
			if got != 0 {
				t.Errorf("resolvePairCodeTTL(%d, %v) = %v, want 0 on error", tt.minutes, tt.ceiling, got)
			}
			for _, want := range tt.wantIn {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("resolvePairCodeTTL(%d, %v) error = %q, want it to contain %q", tt.minutes, tt.ceiling, err.Error(), want)
				}
			}
		})
	}
}

func TestResolvePairCodeTTLAtBoundsSucceeds(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		minutes int
		ceiling time.Duration
	}{
		{"exactly the floor", 5, 60 * time.Minute},
		{"exactly the ceiling", 60, 60 * time.Minute},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			// Act
			got, err := resolvePairCodeTTL(tt.minutes, true, tt.ceiling)

			// Assert
			if err != nil {
				t.Fatalf("resolvePairCodeTTL(%d, %v) unexpected error: %v", tt.minutes, tt.ceiling, err)
			}
			if want := time.Duration(tt.minutes) * time.Minute; got != want {
				t.Errorf("resolvePairCodeTTL(%d, %v) = %v, want %v", tt.minutes, tt.ceiling, got, want)
			}
		})
	}
}
