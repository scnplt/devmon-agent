// SPDX-License-Identifier: AGPL-3.0-only

package httpapi

import (
	"context"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/scnplt/devmon-agent/internal/dockerx"
)

// EventReader is the container event surface, named separately from the read
// interfaces because one of its two methods never returns until the caller
// stops it — the same reason LogReader is its own interface.
type EventReader interface {
	ContainerStates(ctx context.Context) ([]dockerx.ContainerStateSummary, error)
	StreamContainerEvents(ctx context.Context, onReady func(), emit func(dockerx.ContainerEvent) error) error
}

const (
	// msgEventStreamLagged is the terminal frame served to a client the hub
	// dropped for falling behind. Distinct from the Engine failure below so
	// the client can tell "consume faster" from "the host's Docker died" —
	// the same argument that keeps 401, 403, 429 and 502 distinct on the
	// request itself.
	msgEventStreamLagged = "event stream fell behind"

	// msgEventStreamSuperseded is the terminal frame served to a device's
	// older event stream when that device opens a newer one. It is the one
	// terminal error a client must NOT retry: reconnecting would fight the
	// newer stream.
	msgEventStreamSuperseded = "event stream superseded"
)

// handleEventStream opens the container health event stream: one
// event: snapshot frame with the current state of every container, then one
// event: health frame per allowlisted Engine transition, until the client
// disconnects, this device's stream is superseded by a newer one, or the
// shared Engine subscription fails.
//
// Ordering mirrors handleStreamContainerLogs (logs.go) step for step; the
// comments here call out only where this handler differs.
//
// The heartbeat goroutine below also re-checks the device's revocation
// status on every tick (GHSA-qrxm-qm54-xc44): requireDevice only checks it
// once, at request entry, and this stream can otherwise run indefinitely
// after that — see streamRevoked's doc comment (logs.go) for the fail-open
// and deleted-device reasoning.
//
// At most one terminal event: error frame is ever sent, enforced by the
// terminalFrame/sync.Once pair declared below rather than by an ordering
// assumption: select among the heartbeat goroutine's cancel(), <-superseded,
// and <-sub.closed is pseudo-random when more than one is ready in the same
// window, so "the frame that fires first wins" is the only sound invariant.
func (s *Server) handleEventStream(w http.ResponseWriter, r *http.Request) {
	if !s.requireDocker(w) {
		return
	}

	// A missing device in the request context is a 500, not a silent
	// fallback: this handler only ever runs behind requireDevice, and
	// degrading to an unkeyed stream would silently remove the
	// one-stream-per-device rule (mirrors withDeviceLimit's reasoning).
	device, ok := DeviceFrom(r.Context())
	if !ok {
		s.log.Error("handleEventStream ran without a resolved device in the request context")
		s.writeError(w, http.StatusInternalServerError, "internal error")
		return
	}

	// wg.Wait() is deferred BEFORE cancel(), so defer's LIFO order runs
	// cancel() first. Reversing these deadlocks the handler for a full
	// eventHeartbeatInterval on every request (see logs.go's doc comment for
	// the full argument).
	ctx, cancel := context.WithCancel(r.Context())
	var wg sync.WaitGroup
	defer wg.Wait()
	defer cancel()

	// Registering before attaching is deliberate: a device reconnecting must
	// evict its own stale stream first. If attach then fails below, the only
	// way this device could have had a live stream is if the hub was already
	// running — in which case attach cannot fail — so no working stream is
	// ever destroyed by a failure here.
	release, superseded := s.eventStreams.register(device.ID, cancel)
	defer release()

	// parent is the server's lifetime context, not the request's: the shared
	// subscription must outlive whichever client happened to start it.
	sub, err := s.events.attach(s.lifecycleCtx, ctx)
	if err != nil {
		// Nothing has been written to w yet, so this is a real 502/404-style
		// mapping through writeDockerError, not a terminal frame.
		s.writeDockerError(w, r, "attach event stream", err)
		return
	}
	defer s.events.detach(sub)

	// D7's ordering pays off here: sub has been buffering since attach
	// returned, so an event that fires during this call is already sitting
	// in sub.events by the time the snapshot below is sent.
	states, err := s.dc.ContainerStates(ctx)
	if err != nil {
		s.writeDockerError(w, r, "container states", err)
		return
	}

	sse := newSSEWriter(w)

	// The snapshot is the first thing on the wire, before anything buffered
	// in sub.events is drained.
	if err := sse.event("", sseEventSnapshot, states); err != nil {
		return
	}

	// terminalFrame enforces "at most one terminal event: error frame per
	// stream" as a structural invariant, not an ordering assumption
	// (GHSA-qrxm-qm54-xc44 follow-up). The heartbeat goroutine below sends
	// its own frame (revocation) then cancels ctx expecting the main
	// select's <-ctx.Done() case to win the race — but select picks
	// pseudo-randomly among channels that are simultaneously ready, so
	// <-superseded or <-sub.closed can become ready in the same scheduling
	// window and write a SECOND terminal frame after the first. Routing
	// every terminal write through the same sync.Once makes that impossible
	// regardless of which branch the scheduler happens to pick.
	//
	// This is built via a helper method, not an inline func literal here,
	// so the heartbeat goroutine below stays handleEventStream.func1 —
	// TestEventStreamGoroutineDoesNotLeak asserts that exact stack frame
	// name, and an extra func literal ahead of it in source order would
	// renumber it to func2.
	terminalFrame := s.terminalEventFrameSender(r, sse)

	// Started only after the snapshot, mirroring handleStreamContainerLogs's
	// keepalive goroutine verbatim — see its doc comment for the wg/cancel
	// ordering argument that also applies here.
	ticker := time.NewTicker(eventHeartbeatInterval)
	defer ticker.Stop()

	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				// A failed heartbeat write means the client is gone; the
				// main loop's next send will discover the same thing on its
				// own next write.
				_ = sse.comment("heartbeat")

				// The revocation re-check runs after the heartbeat write, on
				// the same tick, mirroring handleStreamContainerLogs's
				// keepalive goroutine (GHSA-qrxm-qm54-xc44): a revoked
				// device still gets a live connection until this check
				// catches up, rather than stalling the heartbeat on an
				// extra DB round trip first.
				if !s.streamRevoked(ctx, device.ID) {
					continue
				}

				// Frame first, cancel second: writeTerminalEventError
				// gates on r.Context(), not this handler's derived ctx (see
				// its own doc comment for why), so the frame is still
				// writable right up to the moment cancel() below fires.
				// cancel() then makes the main select's <-ctx.Done() case
				// fire, which returns silently in the common case — but
				// terminalFrame's Once is what actually guarantees the main
				// select cannot also send its own frame if <-superseded or
				// <-sub.closed happens to be ready at the same time.
				terminalFrame(msgStreamRevoked)
				cancel()
				return
			}
		}
	}()

	for {
		select {
		case <-superseded:
			// A newer stream from the same device evicted this one. This is
			// the one terminal error the client must not retry, so it gets
			// its own message distinct from an ordinary Engine failure.
			// terminalFrame's Once means this loses silently if the
			// heartbeat goroutine's revocation frame already won the race.
			terminalFrame(msgEventStreamSuperseded)
			return
		case <-ctx.Done():
			// Client gone, the server is shutting down, or the heartbeat
			// goroutine's revocation re-check just cancelled ctx after
			// already sending its own msgStreamRevoked terminal frame.
			// Nothing to send here in any of those cases: a plain
			// disconnect has no reason to attempt one, and the revocation
			// case already sent its one and only terminal frame.
			return
		case ev := <-sub.events:
			if err := sse.event("", sseEventHealth, ev); err != nil {
				return
			}
		case reason := <-sub.closed:
			msg := msgEngineUnavailable
			if reason == eventClosedLagged {
				msg = msgEventStreamLagged
			}
			// terminalFrame's Once means this loses silently if the
			// heartbeat goroutine's revocation frame already won the race.
			terminalFrame(msg)
			return
		}
	}
}

// terminalEventFrameSender returns a function that writes at most one
// terminal event: error frame for this request, via writeTerminalEventError,
// regardless of how many times or from how many goroutines it is called.
// Every terminal-frame call site in handleEventStream routes through the
// same sender so "at most one terminal frame per stream" holds structurally
// rather than by an assumption about select's scheduling order.
func (s *Server) terminalEventFrameSender(r *http.Request, sse *sseWriter) func(msg string) {
	var once sync.Once
	return func(msg string) {
		once.Do(func() { s.writeTerminalEventError(r, sse, msg) })
	}
}

// writeTerminalEventError sends the terminal event: error frame carrying
// msg, mirroring handleStreamContainerLogs's terminal-frame path in spirit
// but NOT in the context it checks for "is the client gone": it gates on
// r.Context(), not the handler's own derived ctx.
//
// That distinction is load-bearing. handleStreamContainerLogs's ctx is
// cancelled only by a real client disconnect or server shutdown, so
// isClientGone(ctx, ctx.Err()) there really does mean "the client is gone".
// handleEventStream's derived ctx is ALSO cancelled by
// eventRegistry.register when this device opens a newer stream (D11) — the
// very case msgEventStreamSuperseded exists to report. Gating on that same
// ctx would make the superseded frame unreachable: by the time this
// function runs for that case, ctx.Err() is already non-nil because OF the
// eviction, not because the client left, so isClientGone would report "gone"
// on every single supersede and silently drop the one terminal frame the
// feature spec requires. r.Context() carries no such internal signal — it is
// cancelled only by the underlying connection actually closing or the
// server shutting down — so it is what genuinely answers "is anyone still
// listening".
func (s *Server) writeTerminalEventError(r *http.Request, sse *sseWriter, msg string) {
	if isClientGone(r.Context(), r.Context().Err()) {
		return
	}

	if err := sse.event("", sseEventError, errorBody{Error: msg}); err != nil {
		if isClientGone(r.Context(), err) {
			s.log.Debug("stream container events: write terminal error frame", slog.Any("err", err))
		} else {
			s.log.Error("stream container events: write terminal error frame", slog.Any("err", err))
		}
	}
}
