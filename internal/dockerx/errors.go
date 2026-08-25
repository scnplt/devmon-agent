// SPDX-License-Identifier: AGPL-3.0-only

package dockerx

import (
	"errors"
	"fmt"

	cerrdefs "github.com/containerd/errdefs"
)

// ErrNotFound is returned when the Engine reports no such object. It is the
// only Engine failure the API distinguishes: the caller is an authenticated
// device, so telling it "that container is gone" is information it is
// entitled to, while every other Engine fault is an upstream problem the
// client can only retry.
var ErrNotFound = errors.New("docker object not found")

// ErrInvalidRef is returned by ValidateRef. It never reaches the Engine.
var ErrInvalidRef = errors.New("invalid docker object reference")

// ErrInvalidSince is returned when a caller-supplied resume cursor does not
// parse as a timestamp. Like ErrInvalidRef it never reaches the Engine: the
// value is interpolated into the Engine's request URL, so it is validated at
// the agent's boundary rather than trusted to an upstream parser.
var ErrInvalidSince = errors.New("invalid since timestamp")

// ErrSelfProtected is returned when a lifecycle operation targets the agent's
// own container. This is a fixed rule, not a policy tier: stopping or deleting
// the agent from the app would destroy the operator's only remote access, and
// no configuration may opt into that (D1).
var ErrSelfProtected = errors.New("the agent cannot act on its own container")

// ErrSelfUnknown is returned when the agent is running in a container but
// could not determine which one. Lifecycle operations fail closed rather than
// proceed with the self-exclusion rule unenforceable (D3).
var ErrSelfUnknown = errors.New("agent cannot identify its own container")

// ErrConflict is returned when the Engine refuses an operation because of the
// object's current state — in practice, deleting a running container (D10).
var ErrConflict = errors.New("docker object is in a conflicting state")

// ErrNotModified is returned when the Engine reports the object was already in
// the requested state. Callers treat it as success (D9); it exists as a
// distinct sentinel so the audit detail can record that nothing changed.
var ErrNotModified = errors.New("docker object already in the requested state")

// ErrEventFeedClosed is returned when the Engine's event feed ends rather than
// fails — an io.EOF on the error channel, or a closed channel. It is distinct
// from a transport failure because the caller's response is the same either way
// (tear the subscribers down so they reconnect and re-snapshot) while the log
// level is not: an ended feed is ordinary on a daemon restart, a transport
// failure is not.
var ErrEventFeedClosed = errors.New("docker event feed closed")

// classify maps a raw Engine error onto the package's sentinel errors,
// wrapping the result with op so a caller can name the failing operation
// without inspecting the underlying error type.
func classify(op string, err error) error {
	if err == nil {
		return nil
	}
	if cerrdefs.IsNotFound(err) {
		return fmt.Errorf("%s: %w", op, ErrNotFound)
	}
	if cerrdefs.IsNotModified(err) {
		return fmt.Errorf("%s: %w", op, ErrNotModified)
	}
	if cerrdefs.IsConflict(err) {
		return fmt.Errorf("%s: %w", op, ErrConflict)
	}
	return fmt.Errorf("%s: %w", op, err)
}
