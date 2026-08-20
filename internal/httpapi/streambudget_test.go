// SPDX-License-Identifier: AGPL-3.0-only

package httpapi

import (
	"fmt"
	"sync"
	"testing"
)

// TestStreamBudgetPerDeviceCap proves a device may hold up to maxPerDevice
// slots, and the next acquire for the same device is refused as
// streamDeviceFull rather than consuming a global slot.
func TestStreamBudgetPerDeviceCap(t *testing.T) {
	t.Parallel()

	// Arrange
	const maxPerDevice = 3
	b := newStreamBudget(8, maxPerDevice)

	// Act — fill the device's own cap
	for i := 0; i < maxPerDevice; i++ {
		_, outcome := b.acquire("device-a")
		if outcome != streamGranted {
			t.Fatalf("acquire %d: outcome = %v, want streamGranted", i, outcome)
		}
	}

	// Assert — the next acquire for the same device is refused
	release, outcome := b.acquire("device-a")
	if outcome != streamDeviceFull {
		t.Fatalf("acquire past per-device cap: outcome = %v, want streamDeviceFull", outcome)
	}
	if release != nil {
		t.Fatal("acquire past per-device cap: release is not nil, want nil")
	}
}

// TestStreamBudgetSecondDeviceStillGranted is the regression the issue is
// about: a second device still gets a slot while the first is pinned at its
// own per-device cap.
func TestStreamBudgetSecondDeviceStillGranted(t *testing.T) {
	t.Parallel()

	// Arrange
	const maxPerDevice = 3
	b := newStreamBudget(8, maxPerDevice)
	for i := 0; i < maxPerDevice; i++ {
		if _, outcome := b.acquire("device-a"); outcome != streamGranted {
			t.Fatalf("seed acquire %d: outcome = %v, want streamGranted", i, outcome)
		}
	}

	// Act
	release, outcome := b.acquire("device-b")

	// Assert
	if outcome != streamGranted {
		t.Fatalf("acquire for device-b: outcome = %v, want streamGranted", outcome)
	}
	if release == nil {
		t.Fatal("acquire for device-b: release is nil, want non-nil")
	}
}

// TestStreamBudgetGlobalCeiling proves enough devices at their own caps
// exhaust the global ceiling, and the refusal is streamHostFull — not
// streamDeviceFull — for the device that triggers it.
func TestStreamBudgetGlobalCeiling(t *testing.T) {
	t.Parallel()

	// Arrange — global ceiling of 8, per-device cap of 3: three devices at
	// their own cap sum to 9, past the global ceiling, so the third device's
	// last acquire must find the host full rather than its own cap hit.
	const maxTotal = 8
	const maxPerDevice = 3
	b := newStreamBudget(maxTotal, maxPerDevice)

	devices := []string{"device-a", "device-b", "device-c"}
	granted := 0
	for _, id := range devices {
		for i := 0; i < maxPerDevice; i++ {
			_, outcome := b.acquire(id)
			if outcome == streamGranted {
				granted++
				continue
			}
			// Assert — the refusal must be streamHostFull, since this
			// device has not yet reached its own cap of 3.
			if outcome != streamHostFull {
				t.Fatalf("acquire for %s (granted so far: %d): outcome = %v, want streamHostFull", id, granted, outcome)
			}
			if granted != maxTotal {
				t.Fatalf("streamHostFull reached after %d grants, want %d", granted, maxTotal)
			}
			return
		}
	}
	t.Fatalf("never hit streamHostFull after granting %d slots across %d devices", granted, len(devices))
}

// TestStreamBudgetReleaseReturnsSlot proves releasing a slot returns it to
// both the device's own count and the global total, so a later acquire can
// reuse it.
func TestStreamBudgetReleaseReturnsSlot(t *testing.T) {
	t.Parallel()

	// Arrange
	b := newStreamBudget(1, 1)
	release, outcome := b.acquire("device-a")
	if outcome != streamGranted {
		t.Fatalf("seed acquire: outcome = %v, want streamGranted", outcome)
	}
	if _, outcome := b.acquire("device-b"); outcome != streamHostFull {
		t.Fatalf("acquire while budget is full: outcome = %v, want streamHostFull", outcome)
	}

	// Act
	release()

	// Assert — the freed slot is usable by another device
	if _, outcome := b.acquire("device-b"); outcome != streamGranted {
		t.Fatalf("acquire after release: outcome = %v, want streamGranted", outcome)
	}
}

// TestStreamBudgetDeviceEntryDeletedAtZero proves the map entry for a
// device disappears once its last stream is released — the whole eviction
// story per the plan's Design section.
func TestStreamBudgetDeviceEntryDeletedAtZero(t *testing.T) {
	t.Parallel()

	// Arrange
	b := newStreamBudget(8, 3)
	release, outcome := b.acquire("device-a")
	if outcome != streamGranted {
		t.Fatalf("acquire: outcome = %v, want streamGranted", outcome)
	}
	if got := b.holders(); len(got) != 1 || got["device-a"] != 1 {
		t.Fatalf("holders before release = %v, want {device-a: 1}", got)
	}

	// Act
	release()

	// Assert
	if got := b.holders(); len(got) != 0 {
		t.Fatalf("holders after release = %v, want empty", got)
	}
}

// TestStreamBudgetDoubleReleaseDoesNotCorruptCounts proves a double
// release() is a no-op rather than driving a count negative.
func TestStreamBudgetDoubleReleaseDoesNotCorruptCounts(t *testing.T) {
	t.Parallel()

	// Arrange
	b := newStreamBudget(8, 3)
	release, outcome := b.acquire("device-a")
	if outcome != streamGranted {
		t.Fatalf("acquire: outcome = %v, want streamGranted", outcome)
	}

	// Act — release twice
	release()
	release()

	// Assert — the budget is back to fully empty, not negative
	if got := b.holders(); len(got) != 0 {
		t.Fatalf("holders after double release = %v, want empty", got)
	}
	release2, outcome := b.acquire("device-a")
	if outcome != streamGranted {
		t.Fatalf("acquire after double release: outcome = %v, want streamGranted", outcome)
	}
	if got := b.holders(); got["device-a"] != 1 {
		t.Fatalf("holders after re-acquire = %v, want {device-a: 1}", got)
	}
	release2()
}

// TestStreamBudgetConcurrentAcquireRelease drives many goroutines through
// acquire/release at once and proves neither the per-device cap nor the
// global ceiling is ever exceeded. Run with -race.
func TestStreamBudgetConcurrentAcquireRelease(t *testing.T) {
	t.Parallel()

	// Arrange
	const maxTotal = 8
	const maxPerDevice = 3
	const devices = 6
	const attemptsPerDevice = 50

	b := newStreamBudget(maxTotal, maxPerDevice)

	var wg sync.WaitGroup
	var violations int32
	var mu sync.Mutex

	for d := 0; d < devices; d++ {
		deviceID := fmt.Sprintf("device-%d", d)
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < attemptsPerDevice; i++ {
				release, outcome := b.acquire(deviceID)
				if outcome != streamGranted {
					continue
				}

				// Assert — neither cap is ever exceeded while this slot is
				// held. Reading holders() takes the same mutex acquire and
				// release use, so this is a consistent snapshot, not a race.
				holders := b.holders()
				total := 0
				for _, count := range holders {
					total += count
					if count > maxPerDevice {
						mu.Lock()
						violations++
						mu.Unlock()
					}
				}
				if total > maxTotal {
					mu.Lock()
					violations++
					mu.Unlock()
				}

				release()
			}
		}()
	}
	wg.Wait()

	if violations != 0 {
		t.Fatalf("observed %d cap violations during concurrent acquire/release", violations)
	}
}
