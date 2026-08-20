// SPDX-License-Identifier: AGPL-3.0-only

package httpapi

import "sync"

// streamOutcome names why an acquire succeeded or failed, so the caller can
// answer with the right message and log the right thing.
type streamOutcome int

const (
	streamGranted streamOutcome = iota
	streamDeviceFull
	streamHostFull
)

// streamBudget bounds concurrent live log streams with a global ceiling and
// a per-device cap under it, so one device holding streams cannot starve
// every other paired device (issue #80). It replaces the single buffered
// channel that previously played this role.
//
// Live entries in byDevice are bounded by maxTotal, since every acquire
// that grows the map also consumes a global slot; deleting an entry once
// its count reaches zero is therefore the whole eviction story — there is
// no separate maxKeys cap of the kind internal/ratelimit needs.
type streamBudget struct {
	mu           sync.Mutex
	byDevice     map[string]int
	total        int
	maxTotal     int
	maxPerDevice int
}

// newStreamBudget builds a streamBudget with a global ceiling of maxTotal
// slots and a per-device cap of maxPerDevice.
func newStreamBudget(maxTotal, maxPerDevice int) *streamBudget {
	return &streamBudget{
		byDevice:     make(map[string]int),
		maxTotal:     maxTotal,
		maxPerDevice: maxPerDevice,
	}
}

// acquire takes one slot for deviceID. On streamGranted the returned release
// must be called exactly once; on any other outcome release is nil.
//
// The per-device cap is checked before the global ceiling, so a device
// already at its own cap is told that rather than being told the host is
// full — the two refusals are deliberately distinguishable (see the plan's
// Design section).
func (b *streamBudget) acquire(deviceID string) (release func(), outcome streamOutcome) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.byDevice[deviceID] >= b.maxPerDevice {
		return nil, streamDeviceFull
	}
	if b.total >= b.maxTotal {
		return nil, streamHostFull
	}

	b.byDevice[deviceID]++
	b.total++

	var once sync.Once
	release = func() {
		once.Do(func() {
			b.mu.Lock()
			defer b.mu.Unlock()

			b.total--
			b.byDevice[deviceID]--
			if b.byDevice[deviceID] <= 0 {
				delete(b.byDevice, deviceID)
			}
		})
	}
	return release, streamGranted
}

// holders returns a snapshot of live stream counts by device ID, for
// logging and tests.
func (b *streamBudget) holders() map[string]int {
	b.mu.Lock()
	defer b.mu.Unlock()

	snapshot := make(map[string]int, len(b.byDevice))
	for id, count := range b.byDevice {
		snapshot[id] = count
	}
	return snapshot
}
