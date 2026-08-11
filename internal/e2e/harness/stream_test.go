// SPDX-License-Identifier: AGPL-3.0-only

//go:build e2e

package harness

import (
	"bufio"
	"io"
	"strings"
	"testing"
)

// TestParseSSEBlock is a pure-function test: no Docker Engine, no
// subprocess, nothing that needs t.Cleanup — it runs the same way
// TestParseEngineHost does in proxy_test.go, and for the same reason: the
// stream reader's one piece of decoding logic must be falsifiable without a
// live agent in the loop. Every case here was actually executed in this
// session (unlike the round trips in internal/e2e/api, which need a Docker
// Engine this environment does not have).
func TestParseSSEBlock(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		input         string
		wantFrame     SSEFrame
		wantKeepalive bool
		wantErr       bool
		wantErrIs     error // when set, the error must satisfy errors.Is against this
	}{
		{
			name:      "a real event frame with id, event, and data",
			input:     "id: 2024-01-01T00:00:00.000000000Z\nevent: log\ndata: {\"ts\":\"2024-01-01T00:00:00.000000000Z\",\"stream\":\"stdout\",\"line\":\"hello\"}\n\n",
			wantFrame: SSEFrame{ID: "2024-01-01T00:00:00.000000000Z", Event: "log", Data: []byte(`{"ts":"2024-01-01T00:00:00.000000000Z","stream":"stdout","line":"hello"}`)},
		},
		{
			name:          "a bare keepalive comment",
			input:         ": keepalive\n\n",
			wantKeepalive: true,
		},
		{
			name:      "an error terminal frame with an empty id",
			input:     "id: \nevent: error\ndata: {\"error\":\"docker engine unavailable\"}\n\n",
			wantFrame: SSEFrame{ID: "", Event: "error", Data: []byte(`{"error":"docker engine unavailable"}`)},
		},
		{
			name:    "a block left incomplete by the connection ending mid-frame is an error, not a partial frame",
			input:   "id: 2024-01-01T00:00:00Z\nevent: log\ndata: {\"line\":\"cut off",
			wantErr: true,
		},
		{
			name:      "EOF with nothing at all read",
			input:     "",
			wantErr:   true,
			wantErrIs: io.EOF,
		},
		{
			name:          "a stray leading blank line is skipped, not surfaced as a phantom frame",
			input:         "\nid: x\nevent: log\ndata: {}\n\n",
			wantFrame:     SSEFrame{ID: "x", Event: "log", Data: []byte("{}")},
			wantKeepalive: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			// Arrange
			r := bufio.NewReader(strings.NewReader(tt.input))

			// Act
			frame, isKeepalive, err := parseSSEBlock(r)

			// Assert
			if tt.wantErr {
				if err == nil {
					t.Fatalf("parseSSEBlock(%q) error = nil, want an error", tt.input)
				}
				if tt.wantErrIs != nil && err != tt.wantErrIs {
					t.Errorf("parseSSEBlock(%q) error = %v, want %v", tt.input, err, tt.wantErrIs)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseSSEBlock(%q) error = %v, want nil", tt.input, err)
			}
			if frame.ID != tt.wantFrame.ID || frame.Event != tt.wantFrame.Event || string(frame.Data) != string(tt.wantFrame.Data) {
				t.Errorf("parseSSEBlock(%q) frame = %+v, want %+v", tt.input, frame, tt.wantFrame)
			}
			if isKeepalive != tt.wantKeepalive {
				t.Errorf("parseSSEBlock(%q) isKeepalive = %v, want %v", tt.input, isKeepalive, tt.wantKeepalive)
			}
		})
	}
}

// TestParseSSEBlockFalsifiability inverts a passing case to prove the
// assertion above is not vacuous: a frame whose id line was dropped must NOT
// be reported as carrying the id the well-formed case asserts. This is the
// e2e analogue of writing the test red first — for a pure function it can
// actually be executed, unlike the live-agent assertions elsewhere in this
// task, which could not run in this session (no Docker Engine, D6).
func TestParseSSEBlockFalsifiability(t *testing.T) {
	t.Parallel()

	// Arrange: the same well-formed frame as the table test above, but with
	// its id: line removed.
	r := bufio.NewReader(strings.NewReader("event: log\ndata: {}\n\n"))

	// Act
	frame, _, err := parseSSEBlock(r)
	if err != nil {
		t.Fatalf("parseSSEBlock: unexpected error = %v", err)
	}

	// Assert: this is the negative of the "real event frame" case above — if
	// parseSSEBlock ever started returning a non-empty ID for a block with no
	// id: line, this would fail, which is what proves the positive case is
	// actually checking the id and not passing by coincidence.
	if frame.ID != "" {
		t.Fatalf("parseSSEBlock with no id: line produced ID = %q, want empty — the positive test's ID assertion would be vacuous otherwise", frame.ID)
	}
}
