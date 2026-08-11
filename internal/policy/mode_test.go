// SPDX-License-Identifier: AGPL-3.0-only

package policy

import (
	"strings"
	"testing"
)

func TestParseMode(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		in      string
		want    Mode
		wantErr bool
	}{
		{name: "empty string defaults to middle tier", in: "", want: ModeDefault},
		{name: "read-only parses", in: "read-only", want: ModeReadOnly},
		{name: "default parses", in: "default", want: ModeDefault},
		{name: "full parses", in: "full", want: ModeFull},
		{name: "unknown mode is rejected", in: "admin", wantErr: true},
		{name: "mixed case is rejected", in: "Read-Only", wantErr: true},
		{name: "upper case is rejected", in: "FULL", wantErr: true},
		{name: "surrounding whitespace is rejected", in: " full ", wantErr: true},
		{name: "underscore spelling is rejected", in: "read_only", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			// Arrange / Act
			got, err := ParseMode(tt.in)

			// Assert
			if tt.wantErr {
				if err == nil {
					t.Fatalf("ParseMode(%q) = %v, want error", tt.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseMode(%q) unexpected error: %v", tt.in, err)
			}
			if got != tt.want {
				t.Errorf("ParseMode(%q) = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}

// TestParseModeErrorNamesValidModes guards the operator-facing message: a bad
// value must tell the operator every accepted value, not just that it was wrong.
func TestParseModeErrorNamesValidModes(t *testing.T) {
	t.Parallel()

	// Arrange / Act
	_, err := ParseMode("admin")

	// Assert
	if err == nil {
		t.Fatal("ParseMode(\"admin\") = nil error, want error")
	}
	for _, want := range []string{"read-only", "default", "full"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name valid mode %q", err, want)
		}
	}
}

func TestModeString(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   Mode
		want string
	}{
		{name: "read-only", in: ModeReadOnly, want: "read-only"},
		{name: "default", in: ModeDefault, want: "default"},
		{name: "full", in: ModeFull, want: "full"},
		{name: "out of range is not silently a valid name", in: Mode(42), want: "unknown(42)"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			// Arrange / Act
			got := tt.in.String()

			// Assert
			if got != tt.want {
				t.Errorf("Mode(%d).String() = %q, want %q", int(tt.in), got, tt.want)
			}
		})
	}
}

// TestModeAllows covers the full 7 operations x 3 tiers matrix. The tiers are
// supersets, so each row is monotonic: once an operation is allowed at a tier it
// stays allowed at every higher tier.
func TestModeAllows(t *testing.T) {
	t.Parallel()

	tests := []struct {
		op                              Operation
		readOnly, defaultMode, fullMode bool
	}{
		{op: OpRead, readOnly: true, defaultMode: true, fullMode: true},
		{op: OpLogs, readOnly: true, defaultMode: true, fullMode: true},
		{op: OpStart, readOnly: false, defaultMode: true, fullMode: true},
		{op: OpRestart, readOnly: false, defaultMode: true, fullMode: true},
		{op: OpStop, readOnly: false, defaultMode: true, fullMode: true},
		{op: OpKill, readOnly: false, defaultMode: false, fullMode: true},
		{op: OpDelete, readOnly: false, defaultMode: false, fullMode: true},
	}

	for _, tt := range tests {
		t.Run(string(tt.op), func(t *testing.T) {
			t.Parallel()

			cases := []struct {
				mode Mode
				want bool
			}{
				{mode: ModeReadOnly, want: tt.readOnly},
				{mode: ModeDefault, want: tt.defaultMode},
				{mode: ModeFull, want: tt.fullMode},
			}
			for _, c := range cases {
				// Arrange / Act
				got := c.mode.Allows(tt.op)

				// Assert
				if got != c.want {
					t.Errorf("Mode(%s).Allows(%s) = %v, want %v", c.mode, tt.op, got, c.want)
				}
			}
		})
	}
}

// TestModeAllowsUnknownOperationFailsClosed is the guard that keeps a later
// phase from accidentally granting a capability it forgot to register in
// minMode. Every tier, including the most permissive, must deny it.
func TestModeAllowsUnknownOperationFailsClosed(t *testing.T) {
	t.Parallel()

	for _, m := range []Mode{ModeReadOnly, ModeDefault, ModeFull} {
		// Arrange / Act
		got := m.Allows(Operation("exec"))

		// Assert
		if got {
			t.Errorf("Mode(%s).Allows(\"exec\") = true, want false (unknown ops must fail closed)", m)
		}
	}
}
