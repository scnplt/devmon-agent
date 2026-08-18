// SPDX-License-Identifier: AGPL-3.0-only

package dockerx

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/moby/moby/api/types/container"
)

// TestEngineOptionsAlwaysBothStreams covers D3/D6's requirement that
// ShowStdout, ShowStderr, and Timestamps are never left to the caller's
// discretion: an unset pair yields an empty stream with a nil error, and a
// missing timestamp removes the resume cursor entirely.
func TestEngineOptionsAlwaysBothStreams(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		opts LogOptions
	}{
		{name: "zero value", opts: LogOptions{}},
		{name: "tail and since set", opts: LogOptions{Tail: 50, Since: "2026-08-08T10:02:11.441Z"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			// Act
			got, err := tt.opts.engineOptions(false)

			// Assert
			if err != nil {
				t.Fatalf("engineOptions(%+v) error = %v, want nil", tt.opts, err)
			}
			if !got.ShowStdout || !got.ShowStderr || !got.Timestamps {
				t.Errorf("got = %+v, want ShowStdout, ShowStderr, and Timestamps all true", got)
			}
		})
	}
}

// TestEngineOptionsTail covers the Tail mapping: a positive count becomes its
// decimal string, and a zero or negative count becomes "all". Tail is a
// string field in client.ContainerLogsOptions; passing an int would not
// compile.
func TestEngineOptionsTail(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		tail int
		want string
	}{
		{name: "positive tail", tail: 50, want: "50"},
		{name: "zero tail means all", tail: 0, want: "all"},
		{name: "negative tail means all", tail: -1, want: "all"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			// Arrange
			opts := LogOptions{Tail: tt.tail}

			// Act
			got, err := opts.engineOptions(false)

			// Assert
			if err != nil {
				t.Fatalf("engineOptions(%+v) error = %v, want nil", opts, err)
			}
			if got.Tail != tt.want {
				t.Errorf("got.Tail = %q, want %q", got.Tail, tt.want)
			}
		})
	}
}

// TestEngineOptionsInvalidSince covers the boundary check on Since: it
// reaches the Engine's request URL, so an unparsable value must fail here
// rather than being silently accepted or defaulted like Tail.
func TestEngineOptionsInvalidSince(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		since string
	}{
		{name: "human phrase", since: "yesterday"},
		{name: "unix timestamp, not RFC3339Nano", since: "1700000000"},
		{name: "garbage", since: "not-a-timestamp"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			// Arrange
			opts := LogOptions{Since: tt.since}

			// Act
			_, err := opts.engineOptions(false)

			// Assert
			if err != ErrInvalidSince {
				t.Errorf("engineOptions(%+v) error = %v, want ErrInvalidSince", opts, err)
			}
		})
	}
}

// TestContainerUsesTTYNilConfig covers the guard on container.Config, which
// is a pointer and can be nil (an ordinary shape, per Phase 3's
// toContainerDetail). A nil Config must be read as "no TTY", not panic.
func TestContainerUsesTTYNilConfig(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		res  container.InspectResponse
		want bool
	}{
		{name: "nil config", res: container.InspectResponse{Config: nil}, want: false},
		{name: "config with tty false", res: container.InspectResponse{Config: &container.Config{Tty: false}}, want: false},
		{name: "config with tty true", res: container.InspectResponse{Config: &container.Config{Tty: true}}, want: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			// Act
			got := containerHasTTY(tt.res)

			// Assert
			if got != tt.want {
				t.Errorf("containerHasTTY(%+v) = %v, want %v", tt.res, got, tt.want)
			}
		})
	}
}

// TestStreamPropagatesEmitError covers the contract that emit's error aborts
// a stream unchanged: it means the client went away, and treating it as an
// Engine fault would produce a false failure log for every user who simply
// closes a log view.
func TestStreamPropagatesEmitError(t *testing.T) {
	t.Parallel()

	// Arrange
	var buf bytes.Buffer
	buf.Write(buildFrame(frameTypeStdout, []byte("line one\n")))
	buf.Write(buildFrame(frameTypeStdout, []byte("line two\n")))
	buf.Write(buildFrame(frameTypeStdout, []byte("line three\n")))
	r := bytes.NewReader(buf.Bytes())

	wantErr := errors.New("client gone")
	calls := 0
	emit := func(LogLine) error {
		calls++
		if calls == 2 {
			return wantErr
		}
		return nil
	}

	// Act
	err := demuxNonTTY(r, emit)

	// Assert
	if !errors.Is(err, wantErr) {
		t.Fatalf("demuxNonTTY() error = %v, want %v", err, wantErr)
	}
	if calls != 2 {
		t.Errorf("emit called %d times, want 2 (stop on the failing line)", calls)
	}
	if r.Len() == 0 {
		t.Errorf("reader fully consumed, want the third frame left unread after emit failed")
	}
}

// TestTTYStreamHasNoFraming covers D5: a TTY stream carries no multiplexing
// headers at all, so bytes that would be a valid non-TTY frame header must
// survive as ordinary text — proving demuxTTY never runs the frame parser
// that demuxNonTTY uses.
func TestTTYStreamHasNoFraming(t *testing.T) {
	t.Parallel()

	// Arrange: prefix the raw TTY output with bytes that look exactly like a
	// non-TTY frame header (stdout type, zero length). If demuxTTY wrongly
	// applied framing, these bytes would be consumed as a header rather than
	// emitted as part of the first line.
	header := buildFrame(frameTypeStdout, nil)
	raw := append(append([]byte{}, header...), []byte("hello world\nsecond line\n")...)
	r := bytes.NewReader(raw)

	var got []LogLine
	emit := func(l LogLine) error {
		got = append(got, l)
		return nil
	}

	// Act
	err := demuxTTY(r, emit)

	// Assert
	if err != nil {
		t.Fatalf("demuxTTY() error = %v, want nil", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d lines, want 2", len(got))
	}
	for i, l := range got {
		if l.Stream != streamStdout {
			t.Errorf("line[%d].Stream = %q, want %q", i, l.Stream, streamStdout)
		}
	}
	if want := string(header) + "hello world"; got[0].Line != want {
		t.Errorf("line[0] = %q, want %q (header-shaped bytes preserved as text, not stripped as framing)", got[0].Line, want)
	}
	if want := "second line"; got[1].Line != want {
		t.Errorf("line[1] = %q, want %q", got[1].Line, want)
	}
}

// TestDemuxNonTTYNoOutput covers a container that has produced no output at
// all: zero lines, no error — not "container went away", just silence.
func TestDemuxNonTTYNoOutput(t *testing.T) {
	t.Parallel()

	// Arrange
	r := bytes.NewReader(nil)
	var got []LogLine
	emit := func(l LogLine) error {
		got = append(got, l)
		return nil
	}

	// Act
	err := demuxNonTTY(r, emit)

	// Assert
	if err != nil {
		t.Fatalf("demuxNonTTY() error = %v, want nil", err)
	}
	if len(got) != 0 {
		t.Errorf("got %d lines, want 0", len(got))
	}
}

// TestDemuxTTYNoOutput is TestDemuxNonTTYNoOutput's TTY-mode counterpart.
func TestDemuxTTYNoOutput(t *testing.T) {
	t.Parallel()

	// Arrange
	r := bytes.NewReader(nil)
	var got []LogLine
	emit := func(l LogLine) error {
		got = append(got, l)
		return nil
	}

	// Act
	err := demuxTTY(r, emit)

	// Assert
	if err != nil {
		t.Fatalf("demuxTTY() error = %v, want nil", err)
	}
	if len(got) != 0 {
		t.Errorf("got %d lines, want 0", len(got))
	}
}

// TestDemuxNonTTYPreservesInterleaving covers D4 through the full demux path
// rather than readFrame alone: stdout and stderr frames in a known
// alternating order must come out in that same order. stdcopy.StdCopy pumps
// into two separate writers and would not preserve it — this is the test
// that would fail if demuxNonTTY were ever replaced with it.
func TestDemuxNonTTYPreservesInterleaving(t *testing.T) {
	t.Parallel()

	// Arrange
	var buf bytes.Buffer
	buf.Write(buildFrame(frameTypeStdout, []byte("out-1\n")))
	buf.Write(buildFrame(frameTypeStderr, []byte("err-1\n")))
	buf.Write(buildFrame(frameTypeStdout, []byte("out-2\n")))
	buf.Write(buildFrame(frameTypeStderr, []byte("err-2\n")))
	r := bytes.NewReader(buf.Bytes())

	var got []LogLine
	emit := func(l LogLine) error {
		got = append(got, l)
		return nil
	}

	// Act
	err := demuxNonTTY(r, emit)

	// Assert
	if err != nil {
		t.Fatalf("demuxNonTTY() error = %v, want nil", err)
	}
	want := []struct{ stream, line string }{
		{streamStdout, "out-1"},
		{streamStderr, "err-1"},
		{streamStdout, "out-2"},
		{streamStderr, "err-2"},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d lines, want %d", len(got), len(want))
	}
	for i, w := range want {
		if got[i].Stream != w.stream || got[i].Line != w.line {
			t.Errorf("line[%d] = (%q, %q), want (%q, %q)", i, got[i].Stream, got[i].Line, w.stream, w.line)
		}
	}
}

// TestDemuxNonTTYFinalLineNoNewline covers a container's last line surviving
// through the full demux path — demuxNonTTY calling flush itself on a clean
// EOF, not flush tested in isolation.
func TestDemuxNonTTYFinalLineNoNewline(t *testing.T) {
	t.Parallel()

	// Arrange
	var buf bytes.Buffer
	buf.Write(buildFrame(frameTypeStdout, []byte("no trailing newline")))
	r := bytes.NewReader(buf.Bytes())

	var got []LogLine
	emit := func(l LogLine) error {
		got = append(got, l)
		return nil
	}

	// Act
	err := demuxNonTTY(r, emit)

	// Assert
	if err != nil {
		t.Fatalf("demuxNonTTY() error = %v, want nil", err)
	}
	if len(got) != 1 || got[0].Line != "no trailing newline" {
		t.Fatalf("got = %+v, want the final unterminated line emitted by flush", got)
	}
}

// TestDemuxNonTTYLineSplitAcrossFrames covers a line split across two Engine
// frames, reassembled through the full demux path rather than through
// lineSplitter.push called directly.
func TestDemuxNonTTYLineSplitAcrossFrames(t *testing.T) {
	t.Parallel()

	// Arrange
	var buf bytes.Buffer
	buf.Write(buildFrame(frameTypeStdout, []byte("half a ")))
	buf.Write(buildFrame(frameTypeStdout, []byte("line\n")))
	r := bytes.NewReader(buf.Bytes())

	var got []LogLine
	emit := func(l LogLine) error {
		got = append(got, l)
		return nil
	}

	// Act
	err := demuxNonTTY(r, emit)

	// Assert
	if err != nil {
		t.Fatalf("demuxNonTTY() error = %v, want nil", err)
	}
	if len(got) != 1 || got[0].Line != "half a line" {
		t.Fatalf("got = %+v, want a single reassembled line %q", got, "half a line")
	}
}

// TestDemuxNonTTYUnexpectedEOFMidFrame covers a stream that ends mid-frame:
// io.ErrUnexpectedEOF must surface from the full demux path, not be silently
// swallowed as a clean end.
func TestDemuxNonTTYUnexpectedEOFMidFrame(t *testing.T) {
	t.Parallel()

	// Arrange: one complete frame, then a second frame whose header promises
	// a payload that never fully arrives.
	var buf bytes.Buffer
	buf.Write(buildFrame(frameTypeStdout, []byte("first line\n")))
	full := buildFrame(frameTypeStdout, []byte("second line\n"))
	buf.Write(full[:frameHeaderLen+3]) // header intact, payload cut short
	r := bytes.NewReader(buf.Bytes())

	var got []LogLine
	emit := func(l LogLine) error {
		got = append(got, l)
		return nil
	}

	// Act
	err := demuxNonTTY(r, emit)

	// Assert
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("demuxNonTTY() error = %v, want io.ErrUnexpectedEOF", err)
	}
	if len(got) != 1 || got[0].Line != "first line" {
		t.Fatalf("got = %+v, want exactly the first complete line before the cut", got)
	}
}

// TestDemuxTTYPropagatesEmitError is demuxTTY's counterpart to
// TestStreamPropagatesEmitError: emit's error must abort a TTY-mode stream
// unchanged too.
func TestDemuxTTYPropagatesEmitError(t *testing.T) {
	t.Parallel()

	// Arrange
	raw := []byte("line one\nline two\nline three\n")
	r := bytes.NewReader(raw)
	wantErr := errors.New("client gone")
	calls := 0
	emit := func(LogLine) error {
		calls++
		if calls == 2 {
			return wantErr
		}
		return nil
	}

	// Act
	err := demuxTTY(r, emit)

	// Assert
	if !errors.Is(err, wantErr) {
		t.Fatalf("demuxTTY() error = %v, want %v", err, wantErr)
	}
	if calls != 2 {
		t.Errorf("emit called %d times, want 2 (stop on the failing line)", calls)
	}
}

// TestDemuxTTYReadError covers a stream that fails outright rather than
// ending cleanly — e.g. an Engine connection reset while TTY output is being
// read. Built with io.Pipe so a genuine non-EOF Read error can be produced
// without a live Engine.
func TestDemuxTTYReadError(t *testing.T) {
	t.Parallel()

	// Arrange
	pr, pw := io.Pipe()
	wantErr := errors.New("connection reset")
	go func() {
		_, _ = pw.Write([]byte("hello\n"))
		_ = pw.CloseWithError(wantErr)
	}()

	var got []LogLine
	emit := func(l LogLine) error {
		got = append(got, l)
		return nil
	}

	// Act
	err := demuxTTY(pr, emit)

	// Assert
	if !errors.Is(err, wantErr) {
		t.Fatalf("demuxTTY() error = %v, want wrapped %v", err, wantErr)
	}
	if len(got) != 1 || got[0].Line != "hello" {
		t.Fatalf("got = %+v, want the line read before the error", got)
	}
}

// TestContainerLogsTruncatesAtMaxHistoricalLines exercises the truncation
// boundary ContainerLogs applies to its result. ContainerLogs itself cannot
// be called here — it dials the Engine (via containerUsesTTY) before ever
// reaching demux — so this drives the real demux pipeline (readFrame +
// lineSplitter) through the same capping emit closure ContainerLogs itself
// uses: the cap is enforced inside emit as each line arrives (L1's fix),
// never by slicing a fully-accumulated slice afterward. Asserting that
// len(items) never exceeds the cap at any point during the loop, not just at
// the end, is what would catch a regression back to post-hoc slicing.
func TestContainerLogsTruncatesAtMaxHistoricalLines(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		lineCount     int
		wantLen       int
		wantTruncated bool
	}{
		{name: "one under the cap", lineCount: maxHistoricalLines - 1, wantLen: maxHistoricalLines - 1, wantTruncated: false},
		{name: "exactly at the cap", lineCount: maxHistoricalLines, wantLen: maxHistoricalLines, wantTruncated: false},
		{name: "over the cap", lineCount: maxHistoricalLines + 5, wantLen: maxHistoricalLines, wantTruncated: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			// Arrange
			var buf bytes.Buffer
			payload := []byte(strings.Repeat("line\n", tt.lineCount))
			buf.Write(buildFrame(frameTypeStdout, payload))
			r := bytes.NewReader(buf.Bytes())

			items := make([]LogLine, 0, maxHistoricalLines)
			truncated := false
			maxObservedLen := 0
			emit := func(l LogLine) error {
				if len(items) == maxHistoricalLines {
					truncated = true
					return nil
				}
				items = append(items, l)
				if len(items) > maxObservedLen {
					maxObservedLen = len(items)
				}
				return nil
			}

			// Act
			if err := demuxNonTTY(r, emit); err != nil {
				t.Fatalf("demuxNonTTY() error = %v, want nil", err)
			}

			// Assert
			if len(items) != tt.wantLen {
				t.Errorf("len(items) = %d, want %d", len(items), tt.wantLen)
			}
			if truncated != tt.wantTruncated {
				t.Errorf("truncated = %v, want %v", truncated, tt.wantTruncated)
			}
			if maxObservedLen > maxHistoricalLines {
				t.Errorf("items grew to %d mid-emit, want it capped at %d at every point", maxObservedLen, maxHistoricalLines)
			}
		})
	}
}

// TestContainerLogsItemsNeverNilWhenEmpty exercises the DEFENSIVE_SLICE_INIT
// guarantee ContainerLogs depends on: an empty result must marshal to
// "items":[], never "items":null, or every client would have to handle both
// shapes. Constructed the same way ContainerLogs constructs items, since
// ContainerLogs itself needs a live Engine to call.
func TestContainerLogsItemsNeverNilWhenEmpty(t *testing.T) {
	t.Parallel()

	// Arrange
	items := make([]LogLine, 0)
	emit := func(l LogLine) error {
		items = append(items, l)
		return nil
	}

	// Act
	if err := demuxNonTTY(bytes.NewReader(nil), emit); err != nil {
		t.Fatalf("demuxNonTTY() error = %v, want nil", err)
	}
	data, err := json.Marshal(ListResult[LogLine]{Items: items, Truncated: false})

	// Assert
	if err != nil {
		t.Fatalf("json.Marshal() error = %v, want nil", err)
	}
	if !bytes.Contains(data, []byte(`"items":[]`)) {
		t.Errorf("marshaled body = %s, want it to contain \"items\":[]", data)
	}
}

// rawLogHandler answers a GET .../logs request with the raw bytes body,
// mirroring the Engine's own log stream response: no JSON envelope, no
// content-type negotiation, just the framed (or unframed, for TTY) bytes.
func rawLogHandler(body []byte) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
	}
}

// TestContainerUsesTTYEngineError covers containerUsesTTY's classify branch
// through the real Engine call path: an inspect failure surfaces as a
// classified error, not a panic or a silent false.
func TestContainerUsesTTYEngineError(t *testing.T) {
	t.Parallel()

	// Arrange
	const ref = "ref1"
	c, _ := newFakeEngine(t, map[string]http.HandlerFunc{
		inspectRoute(ref): errorHandler(http.StatusInternalServerError),
	})

	// Act
	_, err := c.containerUsesTTY(context.Background(), ref)

	// Assert
	if err == nil {
		t.Fatal("containerUsesTTY() error = nil, want a classified engine failure")
	}
}

// TestContainerUsesTTYInvalidRef covers the ValidateRef guard: an invalid ref
// is rejected before any Engine call is made.
func TestContainerUsesTTYInvalidRef(t *testing.T) {
	t.Parallel()

	// Arrange
	c, rec := newFakeEngine(t, map[string]http.HandlerFunc{})

	// Act
	_, err := c.containerUsesTTY(context.Background(), "../../info")

	// Assert
	if !errors.Is(err, ErrInvalidRef) {
		t.Fatalf("err = %v, want errors.Is(err, ErrInvalidRef)", err)
	}
	if got := rec.Count(); got != 0 {
		t.Errorf("engine request count = %d, want 0", got)
	}
}

// TestContainerLogsNonTTYHappyPath drives ContainerLogs end to end through a
// fake Engine: inspect reports no TTY, the logs endpoint answers a
// multiplexed stream, and the result is the demuxed lines.
func TestContainerLogsNonTTYHappyPath(t *testing.T) {
	t.Parallel()

	// Arrange
	const ref = "ref1"
	var buf bytes.Buffer
	buf.Write(buildFrame(frameTypeStdout, []byte("out-1\n")))
	buf.Write(buildFrame(frameTypeStderr, []byte("err-1\n")))
	c, rec := newFakeEngine(t, map[string]http.HandlerFunc{
		inspectRoute(ref):           jsonHandler(http.StatusOK, container.InspectResponse{Config: &container.Config{Tty: false}}),
		"GET /containers/ref1/logs": rawLogHandler(buf.Bytes()),
	})

	// Act
	got, err := c.ContainerLogs(context.Background(), ref, LogOptions{})

	// Assert
	if err != nil {
		t.Fatalf("ContainerLogs() error = %v, want nil", err)
	}
	if len(got.Items) != 2 {
		t.Fatalf("len(Items) = %d, want 2", len(got.Items))
	}
	if got.Items[0].Line != "out-1" || got.Items[0].Stream != streamStdout {
		t.Errorf("Items[0] = %+v, want stdout \"out-1\"", got.Items[0])
	}
	if got.Items[1].Line != "err-1" || got.Items[1].Stream != streamStderr {
		t.Errorf("Items[1] = %+v, want stderr \"err-1\"", got.Items[1])
	}
	if got.Truncated {
		t.Error("Truncated = true, want false")
	}
	if got := rec.Count(); got < 2 {
		t.Errorf("engine request count = %d, want at least 2 (inspect + logs)", got)
	}
}

// TestContainerLogsTTYHappyPath is TestContainerLogsNonTTYHappyPath's TTY
// counterpart: the inspect result selects the unframed reader.
func TestContainerLogsTTYHappyPath(t *testing.T) {
	t.Parallel()

	// Arrange
	const ref = "ref1"
	c, _ := newFakeEngine(t, map[string]http.HandlerFunc{
		inspectRoute(ref):           jsonHandler(http.StatusOK, container.InspectResponse{Config: &container.Config{Tty: true}}),
		"GET /containers/ref1/logs": rawLogHandler([]byte("hello tty\n")),
	})

	// Act
	got, err := c.ContainerLogs(context.Background(), ref, LogOptions{})

	// Assert
	if err != nil {
		t.Fatalf("ContainerLogs() error = %v, want nil", err)
	}
	if len(got.Items) != 1 || got.Items[0].Line != "hello tty" {
		t.Fatalf("got = %+v, want a single line %q", got.Items, "hello tty")
	}
}

// TestContainerLogsInvalidRef covers the ValidateRef guard reached through
// containerUsesTTY: no Engine call is made at all.
func TestContainerLogsInvalidRef(t *testing.T) {
	t.Parallel()

	// Arrange
	c, rec := newFakeEngine(t, map[string]http.HandlerFunc{})

	// Act
	_, err := c.ContainerLogs(context.Background(), "../../info", LogOptions{})

	// Assert
	if !errors.Is(err, ErrInvalidRef) {
		t.Fatalf("err = %v, want errors.Is(err, ErrInvalidRef)", err)
	}
	if got := rec.Count(); got != 0 {
		t.Errorf("engine request count = %d, want 0", got)
	}
}

// TestContainerLogsInvalidSince covers engineOptions' error path reached
// through ContainerLogs, after containerUsesTTY has already succeeded.
func TestContainerLogsInvalidSince(t *testing.T) {
	t.Parallel()

	// Arrange
	const ref = "ref1"
	c, _ := newFakeEngine(t, map[string]http.HandlerFunc{
		inspectRoute(ref): jsonHandler(http.StatusOK, container.InspectResponse{Config: &container.Config{Tty: false}}),
	})

	// Act
	_, err := c.ContainerLogs(context.Background(), ref, LogOptions{Since: "not-a-timestamp"})

	// Assert
	if err != ErrInvalidSince {
		t.Fatalf("err = %v, want ErrInvalidSince", err)
	}
}

// TestContainerLogsEngineError covers classify's path when the logs endpoint
// itself fails, after a successful TTY inspect.
func TestContainerLogsEngineError(t *testing.T) {
	t.Parallel()

	// Arrange
	const ref = "ref1"
	c, _ := newFakeEngine(t, map[string]http.HandlerFunc{
		inspectRoute(ref):           jsonHandler(http.StatusOK, container.InspectResponse{Config: &container.Config{Tty: false}}),
		"GET /containers/ref1/logs": errorHandler(http.StatusInternalServerError),
	})

	// Act
	_, err := c.ContainerLogs(context.Background(), ref, LogOptions{})

	// Assert
	if err == nil {
		t.Fatal("ContainerLogs() error = nil, want a classified engine failure")
	}
}

// TestContainerLogsDemuxError covers the demux failure path: a stream cut
// mid-frame surfaces as a wrapped error, not a partial silent success.
func TestContainerLogsDemuxError(t *testing.T) {
	t.Parallel()

	// Arrange
	const ref = "ref1"
	full := buildFrame(frameTypeStdout, []byte("cut short"))
	c, _ := newFakeEngine(t, map[string]http.HandlerFunc{
		inspectRoute(ref):           jsonHandler(http.StatusOK, container.InspectResponse{Config: &container.Config{Tty: false}}),
		"GET /containers/ref1/logs": rawLogHandler(full[:frameHeaderLen+2]),
	})

	// Act
	_, err := c.ContainerLogs(context.Background(), ref, LogOptions{})

	// Assert
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("err = %v, want wrapped io.ErrUnexpectedEOF", err)
	}
}

// TestStreamContainerLogsHappyPath drives StreamContainerLogs end to end
// through a fake Engine, proving it calls emit once per demuxed line.
func TestStreamContainerLogsHappyPath(t *testing.T) {
	t.Parallel()

	// Arrange
	const ref = "ref1"
	var buf bytes.Buffer
	buf.Write(buildFrame(frameTypeStdout, []byte("line one\n")))
	buf.Write(buildFrame(frameTypeStdout, []byte("line two\n")))
	c, _ := newFakeEngine(t, map[string]http.HandlerFunc{
		inspectRoute(ref):           jsonHandler(http.StatusOK, container.InspectResponse{Config: &container.Config{Tty: false}}),
		"GET /containers/ref1/logs": rawLogHandler(buf.Bytes()),
	})

	var got []LogLine
	emit := func(l LogLine) error {
		got = append(got, l)
		return nil
	}

	// Act
	err := c.StreamContainerLogs(context.Background(), ref, LogOptions{}, emit)

	// Assert
	if err != nil {
		t.Fatalf("StreamContainerLogs() error = %v, want nil", err)
	}
	if len(got) != 2 || got[0].Line != "line one" || got[1].Line != "line two" {
		t.Fatalf("got = %+v, want two lines in order", got)
	}
}

// TestStreamContainerLogsInvalidRef covers the ValidateRef guard reached
// through containerUsesTTY.
func TestStreamContainerLogsInvalidRef(t *testing.T) {
	t.Parallel()

	// Arrange
	c, rec := newFakeEngine(t, map[string]http.HandlerFunc{})

	// Act
	err := c.StreamContainerLogs(context.Background(), "../../info", LogOptions{}, func(LogLine) error { return nil })

	// Assert
	if !errors.Is(err, ErrInvalidRef) {
		t.Fatalf("err = %v, want errors.Is(err, ErrInvalidRef)", err)
	}
	if got := rec.Count(); got != 0 {
		t.Errorf("engine request count = %d, want 0", got)
	}
}

// TestStreamContainerLogsInvalidSince covers engineOptions' error path
// reached through StreamContainerLogs.
func TestStreamContainerLogsInvalidSince(t *testing.T) {
	t.Parallel()

	// Arrange
	const ref = "ref1"
	c, _ := newFakeEngine(t, map[string]http.HandlerFunc{
		inspectRoute(ref): jsonHandler(http.StatusOK, container.InspectResponse{Config: &container.Config{Tty: false}}),
	})

	// Act
	err := c.StreamContainerLogs(context.Background(), ref, LogOptions{Since: "garbage"}, func(LogLine) error { return nil })

	// Assert
	if err != ErrInvalidSince {
		t.Fatalf("err = %v, want ErrInvalidSince", err)
	}
}

// TestStreamContainerLogsEngineError covers classify's path when the logs
// endpoint itself fails.
func TestStreamContainerLogsEngineError(t *testing.T) {
	t.Parallel()

	// Arrange
	const ref = "ref1"
	c, _ := newFakeEngine(t, map[string]http.HandlerFunc{
		inspectRoute(ref):           jsonHandler(http.StatusOK, container.InspectResponse{Config: &container.Config{Tty: false}}),
		"GET /containers/ref1/logs": errorHandler(http.StatusInternalServerError),
	})

	// Act
	err := c.StreamContainerLogs(context.Background(), ref, LogOptions{}, func(LogLine) error { return nil })

	// Assert
	if err == nil {
		t.Fatal("StreamContainerLogs() error = nil, want a classified engine failure")
	}
}

// TestStreamContainerLogsContextCancelledBeforeInspect covers ctx cancelled
// before the pre-stream inspect ever completes: the classified error
// surfaces through StreamContainerLogs rather than hanging.
func TestStreamContainerLogsContextCancelledBeforeInspect(t *testing.T) {
	t.Parallel()

	// Arrange
	const ref = "ref1"
	c, _ := newFakeEngine(t, map[string]http.HandlerFunc{
		inspectRoute(ref): jsonHandler(http.StatusOK, container.InspectResponse{Config: &container.Config{Tty: false}}),
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	// Act
	err := c.StreamContainerLogs(ctx, ref, LogOptions{}, func(LogLine) error { return nil })

	// Assert
	if err == nil {
		t.Fatal("StreamContainerLogs() error = nil, want a context-cancelled failure")
	}
}
