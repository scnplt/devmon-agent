// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"strings"
	"testing"
	"time"
)

func TestResolvePairCodeTTLOmittedUsesDefaultCeiling(t *testing.T) {
	t.Parallel()

	// Act — --ttl omitted (sentinel 0), ceiling is the config default (10m).
	got, err := resolvePairCodeTTL(0, 10*time.Minute)

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
	got, err := resolvePairCodeTTL(0, 7*time.Minute)

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
	got, err := resolvePairCodeTTL(8, 15*time.Minute)

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
	_, err := resolvePairCodeTTL(30, 15*time.Minute)

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
	_, err := resolvePairCodeTTL(4, 15*time.Minute)

	// Assert
	if err == nil {
		t.Fatal("resolvePairCodeTTL() = nil error, want one for a TTL below the 5-minute floor")
	}
	if !strings.Contains(err.Error(), "4") || !strings.Contains(err.Error(), "5") {
		t.Errorf("resolvePairCodeTTL() error = %v, want it to name the requested value and the 5-minute minimum", err)
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
			got, err := resolvePairCodeTTL(tt.minutes, tt.ceiling)

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
