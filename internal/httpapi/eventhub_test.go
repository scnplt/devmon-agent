// SPDX-License-Identifier: AGPL-3.0-only

package httpapi

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/scnplt/devmon-agent/internal/dockerx"
)

// fakeEventReader implements EventReader with a single function field, so
// each test injects exactly the StreamContainerEvents behaviour it needs.
// ContainerStates is unused by eventHub and is not exercised here.
type fakeEventReader struct {
	streamContainerEventsFn func(ctx context.Context, onReady func(), emit func(dockerx.ContainerEvent) error) error
}

var _ EventReader = (*fakeEventReader)(nil)

func (f *fakeEventReader) ContainerStates(ctx context.Context) ([]dockerx.ContainerStateSummary, error) {
	return nil, nil
}

func (f *fakeEventReader) StreamContainerEvents(ctx context.Context, onReady func(), emit func(dockerx.ContainerEvent) error) error {
	if f.streamContainerEventsFn == nil {
		if onReady != nil {
			onReady()
		}
		<-ctx.Done()
		return nil
	}
	return f.streamContainerEventsFn(ctx, onReady, emit)
}

// blockingEventReader is a fakeEventReader whose default behaviour is "become
// ready, then block on ctx until cancelled" — the shape most of the tests
// below need, since they only care about lazy start/stop and fan-out.
func blockingEventReader(onStart func()) *fakeEventReader {
	return &fakeEventReader{
		streamContainerEventsFn: func(ctx context.Context, onReady func(), emit func(dockerx.ContainerEvent) error) error {
			if onStart != nil {
				onStart()
			}
			if onReady != nil {
				onReady()
			}
			<-ctx.Done()
			return nil
		},
	}
}

func TestEventHubStartsLazily(t *testing.T) {
	t.Parallel()

	var started atomic.Bool
	fer := blockingEventReader(func() { started.Store(true) })
	h := newEventHub(fer, testLogger())
	t.Cleanup(h.stop)

	if started.Load() {
		t.Fatal("StreamContainerEvents called before any attach")
	}

	sub, err := h.attach(context.Background(), context.Background())
	if err != nil {
		t.Fatalf("attach: %v", err)
	}
	t.Cleanup(func() { h.detach(sub) })

	if !started.Load() {
		t.Fatal("StreamContainerEvents never called after the first attach")
	}
}

func TestEventHubStopsAfterLastDetach(t *testing.T) {
	t.Parallel()

	var starts atomic.Int32
	fer := blockingEventReader(func() { starts.Add(1) })
	h := newEventHub(fer, testLogger())
	t.Cleanup(h.stop)

	sub1, err := h.attach(context.Background(), context.Background())
	if err != nil {
		t.Fatalf("first attach: %v", err)
	}

	h.detach(sub1)

	waitForCondition(t, time.Second, 5*time.Millisecond,
		func() bool { h.mu.Lock(); defer h.mu.Unlock(); return !h.running },
		func() string { return "hub still reports running after the last detach" })

	sub2, err := h.attach(context.Background(), context.Background())
	if err != nil {
		t.Fatalf("second attach: %v", err)
	}
	t.Cleanup(func() { h.detach(sub2) })

	if got := starts.Load(); got != 2 {
		t.Fatalf("StreamContainerEvents call count = %d, want 2 (a fresh subscription per run)", got)
	}
}

func TestEventHubOneSubscriptionForManyClients(t *testing.T) {
	t.Parallel()

	var starts atomic.Int32
	fer := blockingEventReader(func() { starts.Add(1) })
	h := newEventHub(fer, testLogger())
	t.Cleanup(h.stop)

	subs := make([]*eventSubscriber, 0, 3)
	for i := 0; i < 3; i++ {
		sub, err := h.attach(context.Background(), context.Background())
		if err != nil {
			t.Fatalf("attach %d: %v", i, err)
		}
		subs = append(subs, sub)
	}
	t.Cleanup(func() {
		for _, sub := range subs {
			h.detach(sub)
		}
	})

	if got := starts.Load(); got != 1 {
		t.Fatalf("StreamContainerEvents call count = %d, want 1", got)
	}
}

func TestEventHubFansOutToEverySubscriber(t *testing.T) {
	t.Parallel()

	emitCh := make(chan func(dockerx.ContainerEvent) error, 1)
	fer := &fakeEventReader{
		streamContainerEventsFn: func(ctx context.Context, onReady func(), emit func(dockerx.ContainerEvent) error) error {
			emitCh <- emit
			if onReady != nil {
				onReady()
			}
			<-ctx.Done()
			return nil
		},
	}
	h := newEventHub(fer, testLogger())
	t.Cleanup(h.stop)

	subs := make([]*eventSubscriber, 0, 3)
	for i := 0; i < 3; i++ {
		sub, err := h.attach(context.Background(), context.Background())
		if err != nil {
			t.Fatalf("attach %d: %v", i, err)
		}
		subs = append(subs, sub)
	}
	t.Cleanup(func() {
		for _, sub := range subs {
			h.detach(sub)
		}
	})

	emit := <-emitCh
	ev := dockerx.ContainerEvent{ID: "c1", Event: "die"}
	if err := emit(ev); err != nil {
		t.Fatalf("emit: %v", err)
	}

	for i, sub := range subs {
		select {
		case got := <-sub.events:
			if got != ev {
				t.Fatalf("subscriber %d got %+v, want %+v", i, got, ev)
			}
		case <-time.After(time.Second):
			t.Fatalf("subscriber %d never received the event", i)
		}
	}
}

func TestEventHubAttachFailsWhenEngineUnreachable(t *testing.T) {
	t.Parallel()

	wantErr := errors.New("engine down")
	fer := &fakeEventReader{
		streamContainerEventsFn: func(ctx context.Context, onReady func(), emit func(dockerx.ContainerEvent) error) error {
			return wantErr // never calls onReady
		},
	}
	h := newEventHub(fer, testLogger())
	t.Cleanup(h.stop)

	done := make(chan struct{})
	var sub *eventSubscriber
	var err error
	go func() {
		sub, err = h.attach(context.Background(), context.Background())
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("attach hung instead of returning the engine error")
	}

	if sub != nil {
		t.Fatalf("sub = %v, want nil", sub)
	}
	if !errors.Is(err, wantErr) {
		t.Fatalf("err = %v, want %v", err, wantErr)
	}
}

func TestEventHubAttachRespectsRequestCancellation(t *testing.T) {
	t.Parallel()

	fer := &fakeEventReader{
		streamContainerEventsFn: func(ctx context.Context, onReady func(), emit func(dockerx.ContainerEvent) error) error {
			<-ctx.Done() // never calls onReady
			return nil
		},
	}
	h := newEventHub(fer, testLogger())
	t.Cleanup(h.stop)

	reqCtx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	var sub *eventSubscriber
	var err error
	go func() {
		sub, err = h.attach(context.Background(), reqCtx)
		close(done)
	}()

	cancel()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("attach hung after the request context was cancelled")
	}

	if sub != nil {
		t.Fatalf("sub = %v, want nil", sub)
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

// TestEventHubSlowSubscriberIsDropped is the "must not block the fan-out"
// proof: with eventClientBuffer shrunk to 1, a subscriber that is never
// drained is dropped on overflow, while a subscriber that IS drained keeps
// receiving every event fanout sends after it.
func TestEventHubSlowSubscriberIsDropped(t *testing.T) {
	// Not t.Parallel(): mutates the package-level eventClientBuffer.
	original := eventClientBuffer
	eventClientBuffer = 1
	t.Cleanup(func() { eventClientBuffer = original })

	emitCh := make(chan func(dockerx.ContainerEvent) error, 1)
	fer := &fakeEventReader{
		streamContainerEventsFn: func(ctx context.Context, onReady func(), emit func(dockerx.ContainerEvent) error) error {
			emitCh <- emit
			if onReady != nil {
				onReady()
			}
			<-ctx.Done()
			return nil
		},
	}
	h := newEventHub(fer, testLogger())
	t.Cleanup(h.stop)

	stalled, err := h.attach(context.Background(), context.Background())
	if err != nil {
		t.Fatalf("attach stalled: %v", err)
	}
	reading, err := h.attach(context.Background(), context.Background())
	if err != nil {
		t.Fatalf("attach reading: %v", err)
	}
	t.Cleanup(func() {
		h.detach(stalled)
		h.detach(reading)
	})

	emit := <-emitCh

	// First event: both buffers (size 1) have room, so both receive it.
	// stalled is deliberately never drained from here on.
	if err := emit(dockerx.ContainerEvent{ID: "c1", Event: "die"}); err != nil {
		t.Fatalf("first emit: %v", err)
	}
	select {
	case <-reading.events:
	case <-time.After(time.Second):
		t.Fatal("reading subscriber missed the first event")
	}

	// Second event: stalled's single slot is still full, so this overflows
	// it and the hub must drop stalled rather than block fanout.
	if err := emit(dockerx.ContainerEvent{ID: "c1", Event: "start"}); err != nil {
		t.Fatalf("second emit: %v", err)
	}

	select {
	case reason := <-stalled.closed:
		if reason != eventClosedLagged {
			t.Fatalf("stalled close reason = %v, want eventClosedLagged", reason)
		}
	case <-time.After(time.Second):
		t.Fatal("stalled subscriber was never closed")
	}

	select {
	case <-reading.events:
	case <-time.After(time.Second):
		t.Fatal("reading subscriber never received the second event; the stalled subscriber stalled fanout")
	}
}

func TestEventHubFeedDeathClosesEverySubscriber(t *testing.T) {
	t.Parallel()

	endFeed := make(chan struct{})
	fer := &fakeEventReader{
		streamContainerEventsFn: func(ctx context.Context, onReady func(), emit func(dockerx.ContainerEvent) error) error {
			if onReady != nil {
				onReady()
			}
			<-endFeed
			return dockerx.ErrEventFeedClosed
		},
	}
	h := newEventHub(fer, testLogger())
	t.Cleanup(h.stop)

	sub1, err := h.attach(context.Background(), context.Background())
	if err != nil {
		t.Fatalf("attach1: %v", err)
	}
	sub2, err := h.attach(context.Background(), context.Background())
	if err != nil {
		t.Fatalf("attach2: %v", err)
	}

	close(endFeed)

	for i, sub := range []*eventSubscriber{sub1, sub2} {
		select {
		case reason := <-sub.closed:
			if reason != eventClosedEngine {
				t.Fatalf("subscriber %d close reason = %v, want eventClosedEngine", i, reason)
			}
		case <-time.After(time.Second):
			t.Fatalf("subscriber %d was never closed", i)
		}
	}
}

func TestEventHubStopIsIdempotent(t *testing.T) {
	t.Parallel()

	h := newEventHub(blockingEventReader(nil), testLogger())

	sub, err := h.attach(context.Background(), context.Background())
	if err != nil {
		t.Fatalf("attach: %v", err)
	}
	_ = sub

	h.stop()
	h.stop() // must not panic or block
}

func TestEventHubConcurrentAttachDetach(t *testing.T) {
	t.Parallel()

	h := newEventHub(blockingEventReader(nil), testLogger())
	t.Cleanup(h.stop)

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sub, err := h.attach(context.Background(), context.Background())
			if err != nil {
				return
			}
			h.detach(sub)
		}()
	}
	wg.Wait()
}

// eventHubRunGoroutineFrame is the stack-trace frame of the anonymous
// goroutine attach starts to run the shared subscription. A renamed or moved
// goroutine must fail TestEventHubGoroutineDoesNotLeak loudly rather than
// pass vacuously — see countGoroutinesMatching (logs_test.go) for why a stack
// frame is used instead of runtime.NumGoroutine().
const eventHubRunGoroutineFrame = "internal/httpapi.(*eventHub).attach.func1"

// TestEventHubGoroutineDoesNotLeak attaches and detaches 20 times and asserts
// the count of the hub's run goroutine returns to zero, mirroring Phase 4's
// TestStreamGoroutineDoesNotLeak.
func TestEventHubGoroutineDoesNotLeak(t *testing.T) {
	t.Parallel()

	h := newEventHub(blockingEventReader(nil), testLogger())
	t.Cleanup(h.stop)

	for i := 0; i < 20; i++ {
		sub, err := h.attach(context.Background(), context.Background())
		if err != nil {
			t.Fatalf("attach %d: %v", i, err)
		}
		h.detach(sub)
	}

	waitForCondition(t, 2*time.Second, 5*time.Millisecond,
		func() bool { return countGoroutinesMatching(eventHubRunGoroutineFrame) == 0 },
		func() string {
			return fmt.Sprintf("event hub run goroutine count = %d, want 0",
				countGoroutinesMatching(eventHubRunGoroutineFrame))
		})
}
