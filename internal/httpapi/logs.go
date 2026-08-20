// SPDX-License-Identifier: AGPL-3.0-only

package httpapi

import (
	"context"
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/scnplt/devmon-agent/internal/dockerx"
)

// LogReader is the container-log surface, named separately from the four read
// interfaces because a log is a stream rather than a projection: one of its
// two methods never returns until the caller stops it.
type LogReader interface {
	ContainerLogs(ctx context.Context, ref string, opts dockerx.LogOptions) (dockerx.ListResult[dockerx.LogLine], error)
	StreamContainerLogs(ctx context.Context, ref string, opts dockerx.LogOptions, emit func(dockerx.LogLine) error) error
}

const (
	// defaultHistoricalTail is used when ?tail= is absent, unparsable, or out
	// of bounds on the historical route.
	defaultHistoricalTail = 200

	// defaultStreamTail is used the same way on the stream route. It is
	// smaller than the historical default because it only seeds the initial
	// backlog before the stream starts following live output.
	defaultStreamTail = 100

	// minTail and maxTail bound ?tail= on both routes. A typo or a hostile
	// value must not fail a diagnostic request mid-incident (mirrors
	// listAllParam's default-on-parse-failure reasoning), so out-of-range
	// values fall back to the route's default rather than erroring.
	minTail = 1
	maxTail = 2000

	// msgTooManyStreams is served when the host's global stream ceiling is
	// exhausted, regardless of which devices hold the slots. Unlike the
	// other rejections this one is specific and actionable: the caller is an
	// authenticated device and the fix is on its side — close a log view.
	msgTooManyStreams = "too many concurrent log streams"

	// msgTooManyDeviceStreams is served when the caller's own device is at
	// maxStreamsPerDevice, distinct from msgTooManyStreams so a device that
	// hit its own cap is told that, rather than being told the host is full
	// when other devices may have slots to spare (issue #80).
	msgTooManyDeviceStreams = "too many concurrent log streams for this device"

	// msgInvalidSince is served for an unparsable resume cursor. Unlike tail,
	// since is not defaulted on a parse failure: it reaches the Engine's
	// request URL, which makes it a boundary input rather than a preference.
	msgInvalidSince = "invalid since timestamp"
)

// tailParam parses ?tail=, falling back to def when the value is absent,
// unparsable, or outside [minTail, maxTail].
func tailParam(r *http.Request, def int) int {
	raw := r.URL.Query().Get("tail")
	if raw == "" {
		return def
	}

	n, err := strconv.Atoi(raw)
	if err != nil || n < minTail || n > maxTail {
		return def
	}
	return n
}

// sinceParam parses ?since=, returning dockerx.ErrInvalidSince when the value
// is present but does not parse as RFC3339Nano. Unlike tailParam it never
// defaults on failure: the value is interpolated into the Engine's request
// URL, so it is a boundary input rather than a preference.
func sinceParam(r *http.Request) (string, error) {
	raw := r.URL.Query().Get("since")
	if raw == "" {
		return "", nil
	}

	if _, err := time.Parse(time.RFC3339Nano, raw); err != nil {
		return "", dockerx.ErrInvalidSince
	}
	return raw, nil
}

// handleContainerLogs returns a bounded slice of recent lines as JSON.
func (s *Server) handleContainerLogs(w http.ResponseWriter, r *http.Request) {
	if !s.requireDocker(w) {
		return
	}

	since, err := sinceParam(r)
	if err != nil {
		s.writeError(w, http.StatusBadRequest, msgInvalidSince)
		return
	}

	opts := dockerx.LogOptions{Tail: tailParam(r, defaultHistoricalTail), Since: since}

	resp, err := s.dc.ContainerLogs(r.Context(), r.PathValue("id"), opts)
	if err != nil {
		s.writeDockerError(w, r, "container logs", err)
		return
	}

	s.writeJSON(w, http.StatusOK, resp)
}

// handleStreamContainerLogs opens a live SSE stream of a container's output.
//
// Ordering matters: requireDocker and sinceParam run before the stream slot
// is acquired, so a request that was always going to fail never occupies a
// slot another client could have used. The slot is released with defer on
// every path, including the 400 and 404 ones — a slot leaked on an error path
// is permanent, and after eight bad requests the route would answer 503
// forever until a restart.
//
// The slot is acquired per device (issue #80): a missing device in the
// request context is a 500, the same reasoning withDeviceLimit gives — this
// handler only ever runs behind requireDevice, and falling back to an
// unkeyed slot would silently restore the bug being fixed.
func (s *Server) handleStreamContainerLogs(w http.ResponseWriter, r *http.Request) {
	if !s.requireDocker(w) {
		return
	}

	since, err := sinceParam(r)
	if err != nil {
		s.writeError(w, http.StatusBadRequest, msgInvalidSince)
		return
	}

	device, ok := DeviceFrom(r.Context())
	if !ok {
		s.log.Error("handleStreamContainerLogs ran without a resolved device in the request context")
		s.writeError(w, http.StatusInternalServerError, "internal error")
		return
	}

	release, outcome := s.streams.acquire(device.ID)
	switch outcome {
	case streamGranted:
		defer release()
	case streamDeviceFull:
		s.log.Warn("too many concurrent log streams for device",
			slog.String("device_id", device.ID), slog.Int("limit", maxStreamsPerDevice))
		s.writeError(w, http.StatusServiceUnavailable, msgTooManyDeviceStreams)
		return
	case streamHostFull:
		s.log.Warn("too many concurrent log streams for host",
			slog.String("device_id", device.ID), slog.Any("holders", s.streams.holders()))
		s.writeError(w, http.StatusServiceUnavailable, msgTooManyStreams)
		return
	}

	ref := r.PathValue("id")
	opts := dockerx.LogOptions{Tail: tailParam(r, defaultStreamTail), Since: since}

	sse := newSSEWriter(w)

	// The keepalive goroutine writes to the same ResponseWriter as emit
	// below, guarded by sseWriter's own mutex. It must exit when the handler
	// returns or every stream leaks a goroutine and a ticker for the
	// process's life: ticker.Stop() alone does not close the channel, so the
	// goroutine needs its own cancellation signal, not just the ticker's.
	//
	// cancel alone only signals the goroutine to stop; it does not wait for
	// it to observe that signal. Without wg, the goroutine could still be
	// inside sse.keepalive() — holding its mutex mid-write — after this
	// handler returns to ServeHTTP, which on a keep-alive connection may
	// already be serving the next request over the same ResponseWriter.
	// Registration order is the fix: wg.Wait() is deferred BEFORE cancel(),
	// so defer's LIFO order runs it AFTER cancel() closes ctx.Done(). The
	// reverse order would deadlock the handler for a full keepaliveInterval
	// on every request, since the goroutine would be waited on before it is
	// ever told to stop.
	ctx, cancel := context.WithCancel(r.Context())
	var wg sync.WaitGroup
	defer wg.Wait()
	defer cancel()

	ticker := time.NewTicker(keepaliveInterval)
	defer ticker.Stop()

	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				// A keepalive write failure means the client is gone; there
				// is nothing to do here but stop, which the emit path below
				// will also discover on its own next write.
				_ = sse.keepalive()
			}
		}
	}()

	emit := func(line dockerx.LogLine) error {
		return sse.event(line.Timestamp, sseEventLog, line)
	}

	streamErr := s.dc.StreamContainerLogs(ctx, ref, opts, emit)
	if streamErr == nil {
		return
	}

	if !sse.Started() {
		s.writeDockerError(w, r, "stream container logs", streamErr)
		return
	}

	// A stream that failed because the client went away has nothing left to
	// deliver and no one left to deliver it to: attempting the terminal
	// frame below would just be a second write failing for the same reason
	// the first one did, and logging that at ERROR would report, at the
	// agent's highest severity, that it could not tell a client something
	// the client is no longer there to hear (issue #9). See isClientGone's
	// doc comment for exactly what "gone" is checked against — a genuine
	// Engine fault with the client still connected falls through unchanged.
	if isClientGone(ctx, streamErr) {
		return
	}

	// Headers are already committed: the only way left to signal failure is
	// a terminal event frame. D16 forbids logging the ref alongside the
	// container's own output — this path never touches LogLine.Line, only
	// the error.
	if err := sse.event("", sseEventError, errorBody{Error: msgEngineUnavailable}); err != nil {
		// streamErr above was a genuine Engine fault, not a disconnect — so
		// this write was worth attempting. But the client can still have
		// vanished in the gap between the Engine dying and this frame going
		// out, and a failure for THAT reason is the same "no one to hear
		// this" case one step later: DEBUG, not ERROR. Any other failure to
		// send the frame is a distinct fault of its own and stays at ERROR.
		if isClientGone(ctx, err) {
			s.log.Debug("stream container logs: write terminal error frame", slog.Any("err", err))
		} else {
			s.log.Error("stream container logs: write terminal error frame", slog.Any("err", err))
		}
	}
}
