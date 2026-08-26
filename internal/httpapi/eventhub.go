// SPDX-License-Identifier: AGPL-3.0-only

package httpapi

import (
	"context"
	"errors"
	"log/slog"
	"sync"

	"github.com/scnplt/devmon-agent/internal/dockerx"
)

// eventCloseReason names why the hub closed a subscriber, so the handler can
// send the right terminal frame and log at the right level.
type eventCloseReason int

const (
	eventClosedEngine eventCloseReason = iota // the shared feed failed or ended
	eventClosedLagged                         // this subscriber could not keep up
)

// eventClientBuffer bounds how far one subscriber may fall behind before it is
// dropped. 64 is roughly two seconds of a very busy host's container churn: high
// enough that an ordinary client on a slow link never trips it, low enough that
// the hub's total buffering stays trivial.
//
// A package variable rather than a constant so a test can shrink it to 1 and
// exercise the overflow path deterministically, exactly as keepaliveInterval is
// a variable so a test can shorten it.
var eventClientBuffer = 64

// errEventHubUnavailable is returned by attach when the shared subscription's
// run ended before it ever became ready and without a more specific Engine
// error to report — a defensive fallback so attach never returns a nil
// subscriber alongside a nil error.
var errEventHubUnavailable = errors.New("event stream subscription unavailable")

// eventSubscriber is one attached client's view of the hub.
type eventSubscriber struct {
	events chan dockerx.ContainerEvent
	closed chan eventCloseReason // buffered 1; receives at most one value
	once   sync.Once
}

// close records reason as this subscriber's terminal state. It is
// sync.Once-guarded and sends on a buffered-1 channel via select/default, so a
// second close (for example a lagged drop racing the hub's own feed-death
// teardown) can never block or panic.
func (s *eventSubscriber) close(reason eventCloseReason) {
	s.once.Do(func() {
		select {
		case s.closed <- reason:
		default:
		}
	})
}

// eventHub owns the agent's single Docker events subscription and fans it out
// to every attached client.
//
// Exactly one subscription per process, not one per client: GET /events is a
// long-lived Engine connection, and N clients receiving a byte-identical feed
// through N connections is redundant file-descriptor pressure on the host this
// agent exists to protect.
//
// Lazy: the subscription starts when the first client attaches and stops when
// the last one detaches, so an agent nobody is watching holds no Engine
// connection and needs no reconnect-forever machinery.
//
// The hub NEVER reconnects. When the feed fails or ends, every subscriber is
// closed with eventClosedEngine and the hub stops. A hub that reconnected would
// resume a feed with a hole in it and — because this feature has no replay —
// no way to tell its clients what fell in. Turning every gap into a disconnect
// makes the opening snapshot the single recovery path for every failure here.
type eventHub struct {
	dc  EventReader
	log *slog.Logger

	mu      sync.Mutex
	subs    map[*eventSubscriber]struct{}
	running bool
	ready   chan struct{} // closed once the Engine subscription is live
	failed  chan struct{} // closed if the run ends before ready
	runErr  error         // the run's error, valid once failed is closed
	cancel  context.CancelFunc
	wg      sync.WaitGroup
}

// newEventHub builds an eventHub with no subscription running.
func newEventHub(dc EventReader, log *slog.Logger) *eventHub {
	return &eventHub{dc: dc, log: log, subs: make(map[*eventSubscriber]struct{})}
}

// attach registers a subscriber and blocks until the shared subscription is
// live, starting it if this is the first client.
//
// The subscriber is registered BEFORE the wait, so its buffer is already
// collecting by the time the caller takes its snapshot — that ordering is the
// whole reason an event cannot fall between the two (see the handler).
//
// parent is the server's lifetime context, not the request's: the shared
// subscription must outlive whichever client happened to start it. reqCtx
// bounds only how long this call waits for readiness — cancelling it while the
// Engine is unreachable must not hang the caller.
func (h *eventHub) attach(parent, reqCtx context.Context) (*eventSubscriber, error) {
	sub := &eventSubscriber{
		events: make(chan dockerx.ContainerEvent, eventClientBuffer),
		closed: make(chan eventCloseReason, 1),
	}

	h.mu.Lock()
	h.subs[sub] = struct{}{}

	if !h.running {
		h.running = true
		h.runErr = nil
		h.ready = make(chan struct{})
		h.failed = make(chan struct{})

		ctx, cancel := context.WithCancel(parent)
		h.cancel = cancel

		h.wg.Add(1)
		go func() {
			defer h.wg.Done()
			h.run(ctx)
		}()
	}
	ready := h.ready
	failed := h.failed
	h.mu.Unlock()

	select {
	case <-ready:
		return sub, nil
	case <-failed:
		h.detach(sub)
		h.mu.Lock()
		err := h.runErr
		h.mu.Unlock()
		if err == nil {
			err = errEventHubUnavailable
		}
		return nil, err
	case <-reqCtx.Done():
		h.detach(sub)
		return nil, reqCtx.Err()
	}
}

// detach removes sub and stops the subscription if it was the last one.
func (h *eventHub) detach(sub *eventSubscriber) {
	h.mu.Lock()
	if _, ok := h.subs[sub]; !ok {
		h.mu.Unlock()
		return
	}
	delete(h.subs, sub)
	last := len(h.subs) == 0 && h.running
	cancel := h.cancel
	h.mu.Unlock()

	if last {
		cancel()
	}
}

// stop cancels the subscription and waits for its goroutine, bounded so it can
// never consume the server's shutdownGrace window. Idempotent: a second call
// finds no cancel to call and an already-finished wg.
func (h *eventHub) stop() {
	h.mu.Lock()
	cancel := h.cancel
	h.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	h.wg.Wait()
}

// run drives the shared Engine subscription for one lifecycle: start, fan
// out until it ends, then tear every subscriber down. It never reconnects
// (see eventHub's doc comment).
func (h *eventHub) run(ctx context.Context) {
	err := h.dc.StreamContainerEvents(ctx, h.markReady, h.fanout)

	h.mu.Lock()
	h.runErr = err
	failed := h.failed
	subs := make([]*eventSubscriber, 0, len(h.subs))
	for sub := range h.subs {
		subs = append(subs, sub)
	}
	h.subs = make(map[*eventSubscriber]struct{})
	h.running = false
	h.cancel = nil
	h.mu.Unlock()

	// Unblock any attach still waiting on this run's readiness, whether it
	// already succeeded (harmless — select already chose the ready branch)
	// or never got the chance to.
	close(failed)

	for _, sub := range subs {
		sub.close(eventClosedEngine)
	}

	if err == nil {
		return
	}
	// The error is an Engine transport error, never an event payload, so it
	// is safe to log. Warn for an ordinary feed end (a daemon restart);
	// Error for anything else.
	if errors.Is(err, dockerx.ErrEventFeedClosed) {
		h.log.Warn("container event feed closed", slog.Any("err", err))
	} else {
		h.log.Error("container event feed failed", slog.Any("err", err))
	}
}

// markReady closes the current run's ready channel, signalling every
// attach call waiting on it that the Engine subscription is live.
func (h *eventHub) markReady() {
	h.mu.Lock()
	ready := h.ready
	h.mu.Unlock()
	close(ready)
}

// fanout is the emit callback passed to StreamContainerEvents. It must never
// block: the Engine's message channel is unbuffered, so a stalled fan-out
// stalls the Engine decoder goroutine for every subscriber, not just a slow
// one.
func (h *eventHub) fanout(ev dockerx.ContainerEvent) error {
	h.mu.Lock()
	subs := make([]*eventSubscriber, 0, len(h.subs))
	for sub := range h.subs {
		subs = append(subs, sub)
	}
	h.mu.Unlock()

	for _, sub := range subs {
		select {
		case sub.events <- ev:
		default:
			// Bounded buffer full: drop the CLIENT, never the event. A dropped
			// event leaves the client believing it has a complete picture with
			// nothing on the wire to say otherwise; a dropped client reconnects
			// and re-snapshots, which is self-correcting.
			h.log.Warn("event subscriber fell behind, dropping it",
				slog.String("container_id", ev.ID), slog.String("event", ev.Event))
			sub.close(eventClosedLagged)
			h.detach(sub)
		}
	}
	return nil
}
