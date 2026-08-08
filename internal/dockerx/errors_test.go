package dockerx

import (
	"errors"
	"strings"
	"testing"

	cerrdefs "github.com/containerd/errdefs"
)

func TestClassify(t *testing.T) {
	t.Parallel()

	originalErr := errors.New("boom")

	tests := []struct {
		name          string
		op            string
		err           error
		wantNil       bool
		wantNotFound  bool
		wantOriginal  error
		wantOpInError string
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

			if errors.Is(got, ErrNotFound) {
				t.Errorf("errors.Is(got, ErrNotFound) = true, want false")
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
