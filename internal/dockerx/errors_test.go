package dockerx

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	cerrdefs "github.com/containerd/errdefs"
)

// TestErrInvalidSinceIsDistinctSentinel guards against ErrInvalidSince ever
// being folded into another sentinel (e.g. ErrInvalidRef) by an accidental
// reuse — the two are raised at different validation points (ref vs. resume
// cursor) and a caller must be able to tell them apart via errors.Is.
func TestErrInvalidSinceIsDistinctSentinel(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		err  error
		want error
	}{
		{name: "is itself", err: ErrInvalidSince, want: ErrInvalidSince},
		{name: "wrapped still matches", err: fmt.Errorf("since param: %w", ErrInvalidSince), want: ErrInvalidSince},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			// Act & Assert
			if !errors.Is(tt.err, tt.want) {
				t.Errorf("errors.Is(%v, ErrInvalidSince) = false, want true", tt.err)
			}
			if errors.Is(tt.err, ErrInvalidRef) {
				t.Errorf("errors.Is(%v, ErrInvalidRef) = true, want false", tt.err)
			}
			if errors.Is(tt.err, ErrNotFound) {
				t.Errorf("errors.Is(%v, ErrNotFound) = true, want false", tt.err)
			}
		})
	}
}

func TestClassify(t *testing.T) {
	t.Parallel()

	originalErr := errors.New("boom")

	tests := []struct {
		name            string
		op              string
		err             error
		wantNil         bool
		wantNotFound    bool
		wantNotModified bool
		wantConflict    bool
		wantOriginal    error
		wantOpInError   string
	}{
		{
			name:    "nil error passes through as nil",
			op:      "inspect container",
			err:     nil,
			wantNil: true,
		},
		{
			name:          "not found error maps to ErrNotFound",
			op:            "inspect container",
			err:           cerrdefs.ErrNotFound,
			wantNotFound:  true,
			wantOpInError: "inspect container",
		},
		{
			name:            "not modified error maps to ErrNotModified",
			op:              "start container",
			err:             cerrdefs.ErrNotModified,
			wantNotModified: true,
			wantOpInError:   "start container",
		},
		{
			name:          "conflict error maps to ErrConflict",
			op:            "remove container",
			err:           cerrdefs.ErrConflict,
			wantConflict:  true,
			wantOpInError: "remove container",
		},
		{
			name:          "plain error is wrapped but not classified as not found",
			op:            "list containers",
			err:           originalErr,
			wantOriginal:  originalErr,
			wantOpInError: "list containers",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			// Act
			got := classify(tt.op, tt.err)

			// Assert
			if tt.wantNil {
				if got != nil {
					t.Fatalf("classify(%q, nil) = %v, want nil", tt.op, got)
				}
				return
			}

			if tt.wantNotFound {
				if !errors.Is(got, ErrNotFound) {
					t.Errorf("errors.Is(got, ErrNotFound) = false, want true")
				}
				if !strings.HasPrefix(got.Error(), tt.wantOpInError) {
					t.Errorf("got.Error() = %q, want prefix %q", got.Error(), tt.wantOpInError)
				}
				return
			}

			if tt.wantNotModified {
				if !errors.Is(got, ErrNotModified) {
					t.Errorf("errors.Is(got, ErrNotModified) = false, want true")
				}
				if !strings.HasPrefix(got.Error(), tt.wantOpInError) {
					t.Errorf("got.Error() = %q, want prefix %q", got.Error(), tt.wantOpInError)
				}
				return
			}

			if tt.wantConflict {
				if !errors.Is(got, ErrConflict) {
					t.Errorf("errors.Is(got, ErrConflict) = false, want true")
				}
				if !strings.HasPrefix(got.Error(), tt.wantOpInError) {
					t.Errorf("got.Error() = %q, want prefix %q", got.Error(), tt.wantOpInError)
				}
				return
			}

			if errors.Is(got, ErrNotFound) {
				t.Errorf("errors.Is(got, ErrNotFound) = true, want false")
			}
			if errors.Is(got, ErrNotModified) {
				t.Errorf("errors.Is(got, ErrNotModified) = true, want false")
			}
			if errors.Is(got, ErrConflict) {
				t.Errorf("errors.Is(got, ErrConflict) = true, want false")
			}
			if !errors.Is(got, tt.wantOriginal) {
				t.Errorf("errors.Is(got, originalErr) = false, want true")
			}
			if !strings.HasPrefix(got.Error(), tt.wantOpInError) {
				t.Errorf("got.Error() = %q, want prefix %q", got.Error(), tt.wantOpInError)
			}
		})
	}
}
