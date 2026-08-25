// SPDX-License-Identifier: AGPL-3.0-only

package httpapi

import (
	"context"
	"sync"
)

// eventRegistry enforces one live event stream per paired device. A second
// connection from the same device cancels the first and takes its place.
//
// This is deliberately NOT streamBudget. A log stream costs an Engine
// connection, so it is rationed against a host-wide ceiling; an event stream
// costs a channel and a goroutine against a subscription every stream shares,
// so the only limit worth enforcing is "one per device" and no global ceiling
// is needed — the count is bounded by the number of paired devices. Charging an
// event stream to the log budget would let a client watching health lose its
// ability to open a log view.
//
// Newest-wins rather than newest-refused: a second connection from one device is
// in practice a client that reconnected before its old socket was reaped, and
// refusing the new one would leave the device attached to a socket it has
// already forgotten.
type eventRegistry struct {
	mu     sync.Mutex
	active map[string]*eventSlot
}

// eventSlot is one device's live stream. The pointer is the identity token:
// release deletes the map entry only when it still holds THIS slot, so a stream
// that was superseded cannot, on its way out, evict the stream that superseded
// it.
type eventSlot struct {
	cancel     context.CancelFunc
	superseded chan struct{}
	once       sync.Once
}

// newEventRegistry builds an empty eventRegistry.
func newEventRegistry() *eventRegistry {
	return &eventRegistry{active: make(map[string]*eventSlot)}
}

// register makes cancel the live stream for deviceID, cancelling and replacing
// any stream that device already had. The returned release must be called
// exactly once, and is safe to call after this slot has already been
// superseded.
//
// superseded is closed when — and only when — this slot is later evicted by a
// newer registration for the same device, so the handler that owns it can
// distinguish "superseded" from an ordinary client disconnect and write the
// right terminal frame.
func (r *eventRegistry) register(deviceID string, cancel context.CancelFunc) (release func(), superseded <-chan struct{}) {
	slot := &eventSlot{cancel: cancel, superseded: make(chan struct{})}

	r.mu.Lock()
	old := r.active[deviceID]
	r.active[deviceID] = slot
	r.mu.Unlock()

	// Cancel the old slot only after releasing the mutex: the cancelled
	// handler's unwind calls release, which takes r.mu, so cancelling while
	// holding it is a lock-ordering hazard.
	//
	// superseded is closed before cancel is called (rather than after) so
	// the evicted handler's select never has to observe ctx.Done() without
	// superseded already being visible too — belt and braces alongside the
	// handler-side fix (events.go's writeTerminalEventError) that is what
	// actually makes the superseded frame reach the wire: gating "is the
	// client gone" on the handler's own derived ctx, which this cancel call
	// deliberately closes, made the terminal frame unreachable regardless of
	// this ordering.
	if old != nil {
		close(old.superseded)
		old.cancel()
	}

	release = func() {
		slot.once.Do(func() {
			r.mu.Lock()
			defer r.mu.Unlock()

			if r.active[deviceID] == slot {
				delete(r.active, deviceID)
			}
		})
	}
	return release, slot.superseded
}

// count returns the number of devices with a live stream, for tests and for the
// shutdown log line.
func (r *eventRegistry) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.active)
}
