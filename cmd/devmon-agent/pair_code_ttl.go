// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"fmt"
	"time"

	"github.com/scnplt/devmon-agent/internal/state"
)

// pairCodeTTLFlag is the --ttl flag on `device pair-code`.
const pairCodeTTLFlag = "ttl"

// pairCodeMinTTLMinutes is the floor for --ttl. It mirrors
// internal/config's minPairTTLMaxMin: that package validates
// DEVMON_PAIR_TTL_MAX_MIN against the same floor, so a code minted below it
// would expire before an operator can plausibly read and enter one.
const pairCodeMinTTLMinutes = 5

// pairTTLMaxEnvVar names the env var that sets the ceiling --ttl is checked
// against, so a rejected --ttl points the operator at the knob that controls
// it instead of just a bare number.
const pairTTLMaxEnvVar = "DEVMON_PAIR_TTL_MAX_MIN"

// resolvePairCodeTTL turns the raw --ttl flag value into the Duration to mint
// a pairing code with, or rejects it outright.
//
// set reports whether --ttl was actually passed on the command line — the
// caller determines this with flag.FlagSet.Visit, never by inspecting
// ttlMinutes itself. flag.Int's zero value cannot double as a "not set"
// sentinel here: doing so would make an explicit `--ttl 0` indistinguishable
// from an omitted flag and silently redirect it to the default TTL instead
// of hard-failing it like every other below-floor value (the second
// security-review finding on issue #129). When set is false the effective
// TTL is min(state.DefaultPairingCodeTTL, ceiling): never above the ceiling,
// with no cross-field validation error for a default that happens to exceed
// a lowered ceiling.
//
// When set is true, ttlMinutes outside [pairCodeMinTTLMinutes, ceiling] —
// including 0 and negative values — is a hard error; it is never clamped, so
// an operator's script can trust that "no error" means the code was minted
// with the exact TTL requested.
//
// The range check happens on the raw minute count, before ttlMinutes is ever
// multiplied by time.Minute. time.Duration is int64 nanoseconds, so
// multiplying an out-of-range minute count first can overflow and wrap
// around to an innocent-looking Duration that would slip past a Duration
// comparison against ceiling — mirrors internal/config's rangedInt, which
// bounds-checks the raw integer before any Duration conversion for the same
// reason.
func resolvePairCodeTTL(ttlMinutes int, set bool, ceiling time.Duration) (time.Duration, error) {
	if !set {
		def := state.DefaultPairingCodeTTL
		if def > ceiling {
			def = ceiling
		}
		return def, nil
	}

	ceilingMinutes := int(ceiling / time.Minute)
	if ttlMinutes < pairCodeMinTTLMinutes {
		return 0, fmt.Errorf("device pair-code: --%s %d is below the %d-minute minimum",
			pairCodeTTLFlag, ttlMinutes, pairCodeMinTTLMinutes)
	}
	if ttlMinutes > ceilingMinutes {
		return 0, fmt.Errorf("device pair-code: --%s %d exceeds %s (%d)",
			pairCodeTTLFlag, ttlMinutes, pairTTLMaxEnvVar, ceilingMinutes)
	}
	return time.Duration(ttlMinutes) * time.Minute, nil
}
