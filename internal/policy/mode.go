// Package policy defines the agent's named permission tiers.
//
// The tier is fixed by the operator at startup via DEVMON_POLICY_MODE and is
// immutable for the life of the process. A client can never widen what the
// operator granted — that is the agent's core security property.
package policy

import (
	"fmt"
	"strings"
)

// Mode is a permission tier. Tiers are strict supersets of one another, which is
// what lets Allows be a single comparison rather than a set membership test.
type Mode int

const (
	// ModeReadOnly permits inspection only: list, inspect, logs.
	ModeReadOnly Mode = iota
	// ModeDefault adds non-destructive lifecycle control: start, restart, stop.
	// This is the tier chosen when DEVMON_POLICY_MODE is unset — useful out of
	// the box, but incapable of destroying anything.
	ModeDefault
	// ModeFull adds destructive operations: kill, delete.
	ModeFull
)

// Operation is a single agent capability that a Mode either permits or denies.
type Operation string

const (
	OpRead    Operation = "read"
	OpLogs    Operation = "logs"
	OpStart   Operation = "start"
	OpRestart Operation = "restart"
	OpStop    Operation = "stop"
	OpKill    Operation = "kill"
	OpDelete  Operation = "delete"
)

// Mode names as they appear in DEVMON_POLICY_MODE and in the /v1/status payload.
const (
	nameReadOnly = "read-only"
	nameDefault  = "default"
	nameFull     = "full"
)

// minMode is the lowest tier that permits each operation. An operation absent
// from this map is denied by every tier, so a later phase that adds a capability
// without registering it here fails closed rather than open.
var minMode = map[Operation]Mode{
	OpRead:    ModeReadOnly,
	OpLogs:    ModeReadOnly,
	OpStart:   ModeDefault,
	OpRestart: ModeDefault,
	OpStop:    ModeDefault,
	OpKill:    ModeFull,
	OpDelete:  ModeFull,
}

// Allows reports whether this tier permits op. Unknown operations are denied.
func (m Mode) Allows(op Operation) bool {
	min, ok := minMode[op]
	return ok && m >= min
}

// String returns the wire name of the tier.
func (m Mode) String() string {
	switch m {
	case ModeReadOnly:
		return nameReadOnly
	case ModeDefault:
		return nameDefault
	case ModeFull:
		return nameFull
	default:
		return fmt.Sprintf("unknown(%d)", int(m))
	}
}

// ParseMode converts a DEVMON_POLICY_MODE value into a Mode.
//
// The empty string yields ModeDefault, not ModeReadOnly: an unconfigured agent
// should be useful without being able to destroy anything. Parsing is exact —
// no case folding — so a typo surfaces as a startup error rather than as a
// silently different permission tier.
func ParseMode(s string) (Mode, error) {
	switch s {
	case "":
		return ModeDefault, nil
	case nameReadOnly:
		return ModeReadOnly, nil
	case nameDefault:
		return ModeDefault, nil
	case nameFull:
		return ModeFull, nil
	default:
		return ModeDefault, fmt.Errorf(
			"%q is not a valid policy mode (want one of: %s)",
			s, strings.Join([]string{nameReadOnly, nameDefault, nameFull}, ", "),
		)
	}
}
