// SPDX-License-Identifier: AGPL-3.0-only

package dockerx

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"testing"
)

// frameTypeStdout and frameTypeStderr are the Docker multiplexed stream type
// byte values, used only by the test fixture helper below.
const (
	frameTypeStdout = 1
	frameTypeStderr = 2
)

// buildFrame writes one 8-byte multiplexed header followed by payload, using
// an explicit helper so an off-by-one in the size offset cannot hide behind a
// hand-typed hex literal.
func buildFrame(typ byte, payload []byte) []byte {
	header := make([]byte, frameHeaderLen)
	header[frameTypeIdx] = typ
	binary.BigEndian.PutUint32(header[frameSizeOff:], uint32(len(payload)))
	return append(header, payload...)
}

func TestReadFrame(t *testing.T) {
	t.Parallel()

	t.Run("two frame stdout stderr interleave", func(t *testing.T) {
		t.Parallel()

		// Arrange
		var buf bytes.Buffer
		buf.Write(buildFrame(frameTypeStdout, []byte("out line\n")))
		buf.Write(buildFrame(frameTypeStderr, []byte("err line\n")))

		// Act
		stream1, payload1, err1 := readFrame(&buf)
		stream2, payload2, err2 := readFrame(&buf)

		// Assert
		if err1 != nil {
			t.Fatalf("readFrame() #1 error = %v, want nil", err1)
		}
		if stream1 != streamStdout || string(payload1) != "out line\n" {
			t.Errorf("readFrame() #1 = (%q, %q), want (%q, %q)", stream1, payload1, streamStdout, "out line\n")
		}
		if err2 != nil {
			t.Fatalf("readFrame() #2 error = %v, want nil", err2)
		}
		if stream2 != streamStderr || string(payload2) != "err line\n" {
			t.Errorf("readFrame() #2 = (%q, %q), want (%q, %q)", stream2, payload2, streamStderr, "err line\n")
		}
	})

	t.Run("header cut after 3 bytes", func(t *testing.T) {
		t.Parallel()

		// Arrange
		full := buildFrame(frameTypeStdout, []byte("hello\n"))
		r := bytes.NewReader(full[:3])

		// Act
		_, _, err := readFrame(r)

		// Assert
		if !errors.Is(err, io.ErrUnexpectedEOF) {
			t.Fatalf("readFrame() err = %v, want io.ErrUnexpectedEOF", err)
		}
	})

	t.Run("payload shorter than declared size", func(t *testing.T) {
		t.Parallel()

		// Arrange
		full := buildFrame(frameTypeStdout, []byte("hello world\n"))
		r := bytes.NewReader(full[:frameHeaderLen+3]) // header intact, only 3 of the payload bytes present

		// Act
		_, _, err := readFrame(r)

		// Assert
		if !errors.Is(err, io.ErrUnexpectedEOF) {
			t.Fatalf("readFrame() err = %v, want io.ErrUnexpectedEOF", err)
		}
	})

	t.Run("clean EOF at a frame boundary", func(t *testing.T) {
		t.Parallel()

		// Arrange
		r := bytes.NewReader(nil)

		// Act
		_, _, err := readFrame(r)

		// Assert
		if !errors.Is(err, io.EOF) {
			t.Fatalf("readFrame() err = %v, want io.EOF", err)
		}
	})

	t.Run("unsupported stream type byte", func(t *testing.T) {
		t.Parallel()

		// Arrange
		header := make([]byte, frameHeaderLen)
		header[frameTypeIdx] = 9 // neither stdout (1) nor stderr (2)
		binary.BigEndian.PutUint32(header[frameSizeOff:], 0)
		r := bytes.NewReader(header)

		// Act
		_, payload, err := readFrame(r)

		// Assert
		if err == nil {
			t.Fatal("readFrame() error = nil, want an error for an unknown stream type")
		}
		if payload != nil {
			t.Errorf("readFrame() payload = %v, want nil", payload)
		}
	})

	t.Run("header claims 4 GiB payload", func(t *testing.T) {
		t.Parallel()

		// Arrange
		header := make([]byte, frameHeaderLen)
		header[frameTypeIdx] = frameTypeStdout
		binary.BigEndian.PutUint32(header[frameSizeOff:], 0xFFFFFFFF) // ~4 GiB, exceeds maxFramePayloadBytes
		r := bytes.NewReader(header)

		// Act
		_, payload, err := readFrame(r)

		// Assert
		if err == nil {
			t.Fatal("readFrame() error = nil, want an oversized-payload error")
		}
		if payload != nil {
			t.Errorf("readFrame() payload = %v, want nil (no giant allocation)", payload)
		}
	})
}

func TestLineSplitter(t *testing.T) {
	t.Parallel()

	t.Run("frame carrying three lines", func(t *testing.T) {
		t.Parallel()

		// Arrange
		s := newLineSplitter()
		var got []LogLine
		emit := func(l LogLine) error {
			got = append(got, l)
			return nil
		}

		// Act
		err := s.push(streamStdout, []byte("one\ntwo\nthree\n"), emit)

		// Assert
		if err != nil {
			t.Fatalf("push() error = %v, want nil", err)
		}
		wantLines := []string{"one", "two", "three"}
		if len(got) != len(wantLines) {
			t.Fatalf("push() emitted %d lines, want %d", len(got), len(wantLines))
		}
		for i, want := range wantLines {
			if got[i].Line != want {
				t.Errorf("line[%d] = %q, want %q", i, got[i].Line, want)
			}
		}
	})

	t.Run("line split across two frames", func(t *testing.T) {
		t.Parallel()

		// Arrange
		s := newLineSplitter()
		var got []LogLine
		emit := func(l LogLine) error {
			got = append(got, l)
			return nil
		}

		// Act
		if err := s.push(streamStdout, []byte("half a "), emit); err != nil {
			t.Fatalf("push() #1 error = %v, want nil", err)
		}
		if err := s.push(streamStdout, []byte("line\n"), emit); err != nil {
			t.Fatalf("push() #2 error = %v, want nil", err)
		}

		// Assert
		if len(got) != 1 {
			t.Fatalf("got %d lines, want 1", len(got))
		}
		if want := "half a line"; got[0].Line != want {
			t.Errorf("Line = %q, want %q", got[0].Line, want)
		}
	})

	t.Run("20 KiB line is cut at 8 KiB and marked truncated", func(t *testing.T) {
		t.Parallel()

		// Arrange
		s := newLineSplitter()
		var got []LogLine
		emit := func(l LogLine) error {
			got = append(got, l)
			return nil
		}
		line := bytes.Repeat([]byte("a"), 20<<10)
		payload := append(line, '\n')

		// Act
		err := s.push(streamStdout, payload, emit)

		// Assert
		if err != nil {
			t.Fatalf("push() error = %v, want nil", err)
		}
		if len(got) != 1 {
			t.Fatalf("got %d lines, want exactly 1 (no garbage remainder line)", len(got))
		}
		if !got[0].Truncated {
			t.Errorf("Truncated = false, want true")
		}
		if len(got[0].Line) != maxLogLineBytes {
			t.Errorf("len(Line) = %d, want %d", len(got[0].Line), maxLogLineBytes)
		}
	})

	t.Run("CRLF is trimmed to a bare line", func(t *testing.T) {
		t.Parallel()

		// Arrange
		s := newLineSplitter()
		var got []LogLine
		emit := func(l LogLine) error {
			got = append(got, l)
			return nil
		}

		// Act
		err := s.push(streamStdout, []byte("line one\r\n"), emit)

		// Assert
		if err != nil {
			t.Fatalf("push() error = %v, want nil", err)
		}
		if len(got) != 1 || got[0].Line != "line one" {
			t.Fatalf("got = %+v, want a single line %q", got, "line one")
		}
	})

	t.Run("push propagates the emit error and stops", func(t *testing.T) {
		t.Parallel()

		// Arrange
		s := newLineSplitter()
		wantErr := errors.New("client gone")
		calls := 0
		emit := func(LogLine) error {
			calls++
			return wantErr
		}

		// Act
		err := s.push(streamStdout, []byte("one\ntwo\n"), emit)

		// Assert
		if !errors.Is(err, wantErr) {
			t.Fatalf("push() error = %v, want %v", err, wantErr)
		}
		if calls != 1 {
			t.Errorf("emit called %d times, want 1 (stop on first error)", calls)
		}
	})
}

// TestLineSplitterOversizedLineAcrossPushes covers the case where an
// oversized line's overflow bytes and its terminating newline arrive in a
// LATER push() call than the one that triggered truncation: the discarding
// state must survive across push() invocations, not just within one.
func TestLineSplitterOversizedLineAcrossPushes(t *testing.T) {
	t.Parallel()

	// Arrange
	s := newLineSplitter()
	var got []LogLine
	emit := func(l LogLine) error {
		got = append(got, l)
		return nil
	}
	line := bytes.Repeat([]byte("a"), 20<<10) // 20 KiB, no newline in this call

	// Act
	if err := s.push(streamStdout, line, emit); err != nil {
		t.Fatalf("push() #1 error = %v, want nil", err)
	}
	if err := s.push(streamStdout, []byte("more overflow, still no newline"), emit); err != nil {
		t.Fatalf("push() #2 error = %v, want nil", err)
	}
	if err := s.push(streamStdout, []byte("tail garbage\n"), emit); err != nil {
		t.Fatalf("push() #3 error = %v, want nil", err)
	}
	if err := s.push(streamStdout, []byte("next line\n"), emit); err != nil {
		t.Fatalf("push() #4 error = %v, want nil", err)
	}

	// Assert
	if len(got) != 2 {
		t.Fatalf("got %d lines, want exactly 2 (one truncated, then the next real line)", len(got))
	}
	if !got[0].Truncated || len(got[0].Line) != maxLogLineBytes {
		t.Errorf("line[0] = %+v, want a truncated %d-byte line", got[0], maxLogLineBytes)
	}
	if got[1].Truncated || got[1].Line != "next line" {
		t.Errorf("line[1] = %+v, want an untruncated %q", got[1], "next line")
	}
}

// TestLineSplitterFlushSkipsEmptyStreams covers flush's skip of a stream
// whose buffer is already empty (its last line ended with a newline),
// alongside one that still holds an unterminated remainder — flush must not
// emit a spurious blank line for the empty one.
func TestLineSplitterFlushSkipsEmptyStreams(t *testing.T) {
	t.Parallel()

	// Arrange
	s := newLineSplitter()
	var got []LogLine
	emit := func(l LogLine) error {
		got = append(got, l)
		return nil
	}
	if err := s.push(streamStdout, []byte("complete stdout line\n"), emit); err != nil {
		t.Fatalf("push() stdout error = %v, want nil", err)
	}
	if err := s.push(streamStderr, []byte("unterminated stderr"), emit); err != nil {
		t.Fatalf("push() stderr error = %v, want nil", err)
	}
	got = nil // discard the stdout line push() already emitted; only flush's output matters below

	// Act
	err := s.flush(emit)

	// Assert
	if err != nil {
		t.Fatalf("flush() error = %v, want nil", err)
	}
	if len(got) != 1 {
		t.Fatalf("flush() emitted %d lines, want 1 (only the unterminated stderr remainder)", len(got))
	}
	if got[0].Stream != streamStderr || got[0].Line != "unterminated stderr" {
		t.Errorf("got = %+v, want (%q, %q)", got[0], streamStderr, "unterminated stderr")
	}
}

func TestLineSplitterFlush(t *testing.T) {
	t.Parallel()

	// Arrange
	s := newLineSplitter()
	var got []LogLine
	emit := func(l LogLine) error {
		got = append(got, l)
		return nil
	}
	if err := s.push(streamStdout, []byte("no trailing newline"), emit); err != nil {
		t.Fatalf("push() error = %v, want nil", err)
	}

	// Act
	err := s.flush(emit)

	// Assert
	if err != nil {
		t.Fatalf("flush() error = %v, want nil", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d lines after flush, want 1", len(got))
	}
	if want := "no trailing newline"; got[0].Line != want {
		t.Errorf("Line = %q, want %q", got[0].Line, want)
	}
}

// TestLineSplitterFlushPropagatesEmitError covers flush's own error path:
// emit failing on the buffered remainder must abort flush and surface the
// error unchanged, the same contract push already honors mid-stream.
func TestLineSplitterFlushPropagatesEmitError(t *testing.T) {
	t.Parallel()

	// Arrange
	s := newLineSplitter()
	noopEmit := func(LogLine) error { return nil }
	if err := s.push(streamStdout, []byte("unterminated remainder"), noopEmit); err != nil {
		t.Fatalf("push() error = %v, want nil", err)
	}
	wantErr := errors.New("client gone")
	failingEmit := func(LogLine) error { return wantErr }

	// Act
	err := s.flush(failingEmit)

	// Assert
	if !errors.Is(err, wantErr) {
		t.Fatalf("flush() error = %v, want %v", err, wantErr)
	}
}

func TestTimestampExtraction(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		line    string
		wantTS  string
		wantTxt string
	}{
		{
			name:    "valid RFC3339Nano prefix is extracted",
			line:    "2026-08-08T10:02:11.441Z hello world",
			wantTS:  "2026-08-08T10:02:11.441Z",
			wantTxt: "hello world",
		},
		{
			name:    "unparsable timestamp keeps the whole line",
			line:    "not-a-timestamp still text",
			wantTS:  "",
			wantTxt: "not-a-timestamp still text",
		},
		{
			name:    "no space at all keeps the whole line",
			line:    "singleword",
			wantTS:  "",
			wantTxt: "singleword",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			// Act
			ts, text := extractTimestamp(tt.line)

			// Assert
			if ts != tt.wantTS {
				t.Errorf("ts = %q, want %q", ts, tt.wantTS)
			}
			if text != tt.wantTxt {
				t.Errorf("text = %q, want %q", text, tt.wantTxt)
			}
		})
	}
}

func TestTimestampThroughLineSplitter(t *testing.T) {
	t.Parallel()

	// Arrange
	s := newLineSplitter()
	var got []LogLine
	emit := func(l LogLine) error {
		got = append(got, l)
		return nil
	}

	// Act
	err := s.push(streamStdout, []byte("2026-08-08T10:02:11.441Z listening on :8080\n"), emit)

	// Assert
	if err != nil {
		t.Fatalf("push() error = %v, want nil", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d lines, want 1", len(got))
	}
	if want := "2026-08-08T10:02:11.441Z"; got[0].Timestamp != want {
		t.Errorf("Timestamp = %q, want %q", got[0].Timestamp, want)
	}
	if want := "listening on :8080"; got[0].Line != want {
		t.Errorf("Line = %q, want %q", got[0].Line, want)
	}
}
