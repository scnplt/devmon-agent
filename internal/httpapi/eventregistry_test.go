// SPDX-License-Identifier: AGPL-3.0-only

package httpapi

import (
	"context"
	"sync"
	"testing"
)

func TestEventRegistrySecondConnectionCancelsFirst(t *testing.T) {
	t.Parallel()
	// Arrange
	r := newEventRegistry()
	firstCtx, firstCancel := context.WithCancel(context.Background())
	secondCtx, secondCancel := context.WithCancel(context.Background())
	defer secondCancel()

	// Act
	_, firstSuperseded := r.register("device-a", firstCancel)
	_, secondSuperseded := r.register("device-a", secondCancel)

	// Assert
	select {
	case <-firstCtx.Done():
	default:
		t.Fatal("first stream's context should have been cancelled")
	}
	select {
	case <-firstSuperseded:
	default:
		t.Fatal("first slot's superseded channel should be closed")
	}
	select {
	case <-secondCtx.Done():
		t.Fatal("second stream's context should not have been cancelled")
	default:
	}
	select {
	case <-secondSuperseded:
		t.Fatal("second slot's superseded channel should not be closed")
	default:
	}
}

func TestEventRegistryDevicesAreIndependent(t *testing.T) {
	t.Parallel()
	// Arrange
	r := newEventRegistry()
	aCtx, aCancel := context.WithCancel(context.Background())
	defer aCancel()
	_, aSuperseded := r.register("device-a", aCancel)

	// Act
	_, bCancel := context.WithCancel(context.Background())
	defer bCancel()
	r.register("device-b", bCancel)

	// Assert
	select {
	case <-aCtx.Done():
		t.Fatal("registering device-b should not cancel device-a's stream")
	default:
	}
	select {
	case <-aSuperseded:
		t.Fatal("registering device-b should not supersede device-a's slot")
	default:
	}
	if got := r.count(); got != 2 {
		t.Fatalf("count() = %d, want 2", got)
	}
}

func TestEventRegistryReleaseAfterSupersededDoesNotEvictNewer(t *testing.T) {
	t.Parallel()
	// Arrange
	r := newEventRegistry()
	_, firstCancel := context.WithCancel(context.Background())
	firstRelease, _ := r.register("device-a", firstCancel)
	_, secondCancel := context.WithCancel(context.Background())
	defer secondCancel()
	secondRelease, _ := r.register("device-a", secondCancel)
	defer secondRelease()

	// Act: the superseded (first) slot releases on its way out.
	firstRelease()

	// Assert: the newer slot is still live.
	if got := r.count(); got != 1 {
		t.Fatalf("count() = %d, want 1", got)
	}
}

func TestEventRegistryReleaseDeletesEntry(t *testing.T) {
	t.Parallel()
	// Arrange
	r := newEventRegistry()
	_, cancel := context.WithCancel(context.Background())
	defer cancel()
	release, _ := r.register("device-a", cancel)

	// Act
	release()

	// Assert
	if got := r.count(); got != 0 {
		t.Fatalf("count() = %d, want 0", got)
	}
}

func TestEventRegistryDoubleReleaseIsSafe(t *testing.T) {
	t.Parallel()
	// Arrange
	r := newEventRegistry()
	_, cancel := context.WithCancel(context.Background())
	defer cancel()
	release, _ := r.register("device-a", cancel)

	// Act
	release()
	release()

	// Assert
	if got := r.count(); got != 0 {
		t.Fatalf("count() = %d, want 0", got)
	}
}

func TestEventRegistryConcurrent(t *testing.T) {
	t.Parallel()
	// Arrange
	r := newEventRegistry()
	const devices = 8
	const attemptsPerDevice = 20

	var wg sync.WaitGroup
	var maxObserved int
	var maxMu sync.Mutex

	// Act
	for d := 0; d < devices; d++ {
		deviceID := deviceIDFor(d)
		for a := 0; a < attemptsPerDevice; a++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, cancel := context.WithCancel(context.Background())
				release, _ := r.register(deviceID, cancel)

				maxMu.Lock()
				if got := r.count(); got > maxObserved {
					maxObserved = got
				}
				maxMu.Unlock()

				release()
			}()
		}
	}
	wg.Wait()

	// Assert
	if maxObserved > devices {
		t.Fatalf("count() reached %d, want <= %d distinct device IDs", maxObserved, devices)
	}
	if got := r.count(); got != 0 {
		t.Fatalf("count() = %d after all releases, want 0", got)
	}
}

func deviceIDFor(i int) string {
	return "device-" + string(rune('a'+i))
}
