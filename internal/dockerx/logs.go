// SPDX-License-Identifier: AGPL-3.0-only

package dockerx

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"
)

// LogOptions is the validated, agent-side view of a log request. Both fields
// are bounded by the caller in httpapi before they arrive here; this struct
// re-validates Since because it is what reaches the Engine's request URL.
type LogOptions struct {
	Tail  int    // lines to seed with; <= 0 means "all"
	Since string // RFC3339Nano cursor; "" means from the beginning
}

// engineOptions maps LogOptions onto the SDK's option struct.
//
// ShowStdout and ShowStderr both default to false, and an unset pair yields
// an empty stream with a nil error — a silent no-output bug with nothing in
// it to trace. Timestamps is likewise mandatory rather than optional: the
// per-line ts field and the client's resume cursor are both derived from it.
func (o LogOptions) engineOptions(follow bool) (client.ContainerLogsOptions, error) {
	if o.Since != "" {
		if _, err := time.Parse(time.RFC3339Nano, o.Since); err != nil {
			return client.ContainerLogsOptions{}, ErrInvalidSince
		}
	}

	tail := "all"
	if o.Tail > 0 {
		tail = strconv.Itoa(o.Tail)
	}

	return client.ContainerLogsOptions{
		ShowStdout: true,
		ShowStderr: true,
		Timestamps: true,
		Follow:     follow,
		Since:      o.Since,
		Tail:       tail,
	}, nil
}

// containerUsesTTY reports whether ref was created with a TTY. A TTY stream
// carries no multiplexing headers at all, so the answer selects between two
// incompatible readers; there is no way to detect it from the stream itself.
func (c *Client) containerUsesTTY(ctx context.Context, ref string) (bool, error) {
	if err := ValidateRef(ref); err != nil {
		return false, err
	}

	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()

	res, err := c.api.ContainerInspect(ctx, ref, client.ContainerInspectOptions{})
	if err != nil {
		return false, classify("inspect container for tty", err)
	}

	return containerHasTTY(res.Container), nil
}

// containerHasTTY reads the TTY flag from an inspected container. Config is a
// *container.Config and can be nil — Phase 3's toContainerDetail guards the
// same field for the same reason. A nil Config means "assume no TTY", the
// common case, not a panic.
func containerHasTTY(r container.InspectResponse) bool {
	if r.Config == nil {
		return false
	}
	return r.Config.Tty
}

// maxHistoricalLines caps the bounded fetch. Mirrors maxListItems' reasoning:
// a phone on a mobile connection must not receive a body the size of a log
// file, and the cap is server-side because startup configuration — not the
// client — is this agent's security boundary.
const maxHistoricalLines = 2000

// ContainerLogs returns a bounded slice of recent lines.
func (c *Client) ContainerLogs(ctx context.Context, ref string, opts LogOptions) (ListResult[LogLine], error) {
	tty, err := c.containerUsesTTY(ctx, ref)
	if err != nil {
		return ListResult[LogLine]{}, err
	}

	engineOpts, err := opts.engineOptions(false)
	if err != nil {
		return ListResult[LogLine]{}, err
	}

	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()

	res, err := c.api.ContainerLogs(ctx, ref, engineOpts)
	if err != nil {
		return ListResult[LogLine]{}, classify("container logs", err)
	}
	// ContainerLogsResult is an interface (interface{ io.ReadCloser }), unlike
	// every other v29 result type: there is no .Items field, only the reader
	// itself. Leaking this Close holds an Engine connection for the process's
	// life.
	defer func() { _ = res.Close() }()

	// items is capped at maxHistoricalLines inside emit itself, not sliced
	// after the read completes: opts.Tail is bounded on the network path
	// (tailParam clamps to [1,2000]), but ContainerLogs is exported, and a
	// future caller passing Tail<=0 ("all") would otherwise accumulate the
	// whole container log in agent memory before this function ever gets a
	// chance to truncate it — the line-count analogue of the OOM
	// maxLogLineBytes exists to prevent for a single line. truncated means
	// "there was more than we returned"; the demux loop still runs to
	// completion (draining, not buffering, the rest of the stream), it just
	// stops appending once the cap is hit.
	items := make([]LogLine, 0, maxHistoricalLines)
	truncated := false
	emit := func(l LogLine) error {
		if len(items) == maxHistoricalLines {
			truncated = true
			return nil
		}
		items = append(items, l)
		return nil
	}

	demux := demuxNonTTY
	if tty {
		demux = demuxTTY
	}
	if err := demux(res, emit); err != nil {
		return ListResult[LogLine]{}, fmt.Errorf("container logs: %w", err)
	}

	return ListResult[LogLine]{Items: items, Truncated: truncated}, nil
}

// StreamContainerLogs follows ref's output, calling emit once per line until
// the container stops, ctx is cancelled, or emit returns an error.
//
// emit is a callback rather than a returned channel deliberately: the caller
// needs to flush after every line and to abort the stream the instant a
// client write fails. A channel would need a producer goroutine, a second
// error channel, and close semantics careful enough to survive a client that
// vanishes mid-line — three places to leak a goroutine instead of none.
func (c *Client) StreamContainerLogs(ctx context.Context, ref string, opts LogOptions, emit func(LogLine) error) error {
	tty, err := c.containerUsesTTY(ctx, ref)
	if err != nil {
		return err
	}

	engineOpts, err := opts.engineOptions(true)
	if err != nil {
		return err
	}

	// No callTimeout here, deliberately: it bounds the pre-stream inspect
	// above and the historical fetch in ContainerLogs only. A 15s timeout on
	// this call would kill every stream at 15 seconds — the stream's lifetime
	// is bounded by ctx and nothing else, and the success signal for this
	// route is a 30-minute stream.
	res, err := c.api.ContainerLogs(ctx, ref, engineOpts)
	if err != nil {
		return classify("stream container logs", err)
	}
	defer func() { _ = res.Close() }()

	demux := demuxNonTTY
	if tty {
		demux = demuxTTY
	}
	// demux's error is returned unchanged: it is either a clean nil (the
	// container stopped or ctx ended), or emit's own error propagated
	// verbatim, which means the client went away and must not be reported as
	// an Engine fault.
	return demux(res, emit)
}

// demuxNonTTY reads a non-TTY log stream — Docker's 8-byte-header framing —
// via readFrame, splitting each frame's payload into per-stream lines. A
// clean io.EOF at a frame boundary ends the read normally: a container that
// exits ends its own log stream, which is not a failure.
func demuxNonTTY(r io.Reader, emit func(LogLine) error) error {
	splitter := newLineSplitter()
	for {
		stream, payload, err := readFrame(r)
		if err != nil {
			if errors.Is(err, io.EOF) {
				return splitter.flush(emit)
			}
			return fmt.Errorf("read log frame: %w", err)
		}
		if err := splitter.push(stream, payload, emit); err != nil {
			return err
		}
	}
}

// demuxTTY reads a TTY log stream, which carries no multiplexing headers at
// all — every byte belongs to a single ordered output, labelled streamStdout.
// It uses the same lineSplitter as demuxNonTTY so line accumulation, the
// 8 KiB cap, and timestamp extraction behave identically for both stream
// kinds.
func demuxTTY(r io.Reader, emit func(LogLine) error) error {
	splitter := newLineSplitter()
	buf := make([]byte, 32*1024)
	for {
		n, readErr := r.Read(buf)
		if n > 0 {
			if err := splitter.push(streamStdout, buf[:n], emit); err != nil {
				return err
			}
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				return splitter.flush(emit)
			}
			return fmt.Errorf("read tty log stream: %w", readErr)
		}
	}
}
