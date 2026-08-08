package dockerx

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"strings"
	"time"
)

// Framing constants. Docker's non-TTY log stream is a sequence of
// [8]byte{STREAM_TYPE, 0, 0, 0, SIZE1, SIZE2, SIZE3, SIZE4} headers followed
// by SIZE bytes of payload, size big-endian.
const (
	frameHeaderLen = 8
	frameTypeIdx   = 0
	frameSizeOff   = 4

	streamStdout = "stdout"
	streamStderr = "stderr"

	// maxLogLineBytes bounds a single line. A container emitting one
	// enormous line — a stack dump, a base64 blob, a minified bundle — would
	// otherwise be accumulated whole in agent memory before any newline
	// arrived, which is the agent OOM-killing itself while reading logs.
	maxLogLineBytes = 8 << 10

	// maxFramePayloadBytes bounds one Engine frame. The size field is
	// attacker-influenced only via container output, but a corrupt or
	// truncated stream can present a 4 GiB length that make() would honour.
	maxFramePayloadBytes = 1 << 20
)

// readFrame reads one multiplexed frame. It returns io.EOF cleanly at a frame
// boundary and io.ErrUnexpectedEOF for a stream cut mid-frame.
func readFrame(r io.Reader) (stream string, payload []byte, err error) {
	header := make([]byte, frameHeaderLen)
	if _, err := io.ReadFull(r, header); err != nil {
		// io.ReadFull returns io.EOF when zero bytes were read (a clean
		// boundary) and io.ErrUnexpectedEOF for a partial header. Both are
		// propagated as-is: only the caller knows whether an EOF here is
		// expected.
		return "", nil, err
	}

	switch header[frameTypeIdx] {
	case 1:
		stream = streamStdout
	case 2:
		stream = streamStderr
	default:
		return "", nil, fmt.Errorf("dockerx: unknown frame stream type %d", header[frameTypeIdx])
	}

	size := binary.BigEndian.Uint32(header[frameSizeOff:])
	// Bound the payload BEFORE any allocation so a corrupt or truncated
	// stream cannot make() a multi-gigabyte slice.
	if size > maxFramePayloadBytes {
		return "", nil, fmt.Errorf("dockerx: frame payload of %d bytes exceeds max %d", size, maxFramePayloadBytes)
	}
	// size is now provably <= maxFramePayloadBytes (1 MiB), so the
	// uint32 -> int conversion below cannot overflow on any supported
	// platform.
	n := int(size)

	payload = make([]byte, n)
	if n > 0 {
		if _, err := io.ReadFull(r, payload); err != nil {
			if err == io.EOF {
				// The header promised n bytes of payload and the stream
				// ended before delivering any of them: that is a cut
				// mid-frame, not a clean boundary.
				err = io.ErrUnexpectedEOF
			}
			return "", nil, err
		}
	}

	return stream, payload, nil
}

// lineState is the per-stream accumulator lineSplitter keeps for stdout and
// stderr independently, so an interleaved stdout/stderr byte sequence never
// merges into the wrong logical line.
type lineState struct {
	buf []byte
	// discarding is true once buf has been cut at maxLogLineBytes and the
	// truncated LogLine already emitted; the remaining bytes of the
	// over-length line are dropped until the real newline arrives.
	discarding bool
}

// lineSplitter accumulates payload bytes per stream and emits whole lines.
// Docker's json-file driver usually frames one line at a time, but that is
// not guaranteed by any driver contract: a frame may hold several lines or
// half of one, so partial lines must be buffered across frames.
type lineSplitter struct {
	states map[string]*lineState
}

// newLineSplitter returns a lineSplitter ready to accept frames from any
// number of distinct streams.
func newLineSplitter() *lineSplitter {
	return &lineSplitter{states: make(map[string]*lineState)}
}

func (s *lineSplitter) state(stream string) *lineState {
	st, ok := s.states[stream]
	if !ok {
		st = &lineState{}
		s.states[stream] = st
	}
	return st
}

// push splits payload on '\n', trims a trailing '\r', extracts the timestamp
// prefix, and calls emit per complete line. Partial lines are buffered on the
// stream's state until a future push or flush completes them.
func (s *lineSplitter) push(stream string, payload []byte, emit func(LogLine) error) error {
	st := s.state(stream)
	data := payload

	for len(data) > 0 {
		idx := bytes.IndexByte(data, '\n')
		var chunk []byte
		hasNewline := idx >= 0
		if hasNewline {
			chunk = data[:idx]
			data = data[idx+1:]
		} else {
			chunk = data
			data = nil
		}

		if st.discarding {
			// Bytes belonging to an already-truncated-and-emitted line are
			// dropped outright; only the arrival of the real newline
			// resets state for the next logical line.
			if hasNewline {
				st.discarding = false
			}
			continue
		}

		// Enforce maxLogLineBytes on the ACCUMULATING buffer, not after a
		// newline arrives — checking afterwards means the unbounded growth
		// has already happened.
		space := maxLogLineBytes - len(st.buf)
		if space < 0 {
			space = 0
		}
		take := chunk
		overflowed := false
		if len(take) > space {
			take = take[:space]
			overflowed = true
		}
		st.buf = append(st.buf, take...)

		if overflowed {
			if err := emitLine(stream, st.buf, true, emit); err != nil {
				return err
			}
			st.buf = st.buf[:0]
			if !hasNewline {
				// The real end of this line has not been seen yet; keep
				// discarding until it arrives, possibly in a later push.
				st.discarding = true
			}
			continue
		}

		if hasNewline {
			if err := emitLine(stream, st.buf, false, emit); err != nil {
				return err
			}
			st.buf = st.buf[:0]
		}
		// Otherwise: no newline yet, keep accumulating for the next push.
	}

	return nil
}

// flush emits any buffered remainder when the stream ends — a container's
// final line frequently has no trailing newline, and dropping it loses the
// panic message.
func (s *lineSplitter) flush(emit func(LogLine) error) error {
	for stream, st := range s.states {
		if len(st.buf) == 0 {
			continue
		}
		if err := emitLine(stream, st.buf, false, emit); err != nil {
			return err
		}
		st.buf = st.buf[:0]
	}
	return nil
}

// emitLine trims a trailing '\r', extracts the RFC3339Nano timestamp prefix
// (if any), and calls emit with the resulting LogLine. line is converted to a
// string here — which copies it — before the caller reuses its backing array.
func emitLine(stream string, line []byte, truncated bool, emit func(LogLine) error) error {
	text := strings.TrimSuffix(string(line), "\r")
	ts, rest := extractTimestamp(text)
	return emit(LogLine{Timestamp: ts, Stream: stream, Line: rest, Truncated: truncated})
}

// extractTimestamp splits Docker's "<RFC3339Nano> <text>" prefix off line. If
// the prefix does not parse as RFC3339Nano, the whole original line is kept
// and the timestamp is left empty: a line is never dropped because its
// timestamp looked odd, since the malformed line is often the interesting
// one.
func extractTimestamp(line string) (ts, text string) {
	prefix, rest, found := strings.Cut(line, " ")
	if !found {
		return "", line
	}
	if _, err := time.Parse(time.RFC3339Nano, prefix); err != nil {
		return "", line
	}
	return prefix, rest
}
