package dockerx

import (
	"errors"
	"strings"
	"testing"
)

func TestValidateRef(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		ref     string
		wantErr bool
	}{
		{name: "empty string", ref: "", wantErr: true},
		{name: "bare traversal", ref: "..", wantErr: true},
		{name: "nested traversal", ref: "../../info", wantErr: true},
		{name: "slash separated segments", ref: "a/b", wantErr: true},
		{name: "leading dash", ref: "-abc", wantErr: true},
		{name: "leading dot", ref: ".hidden", wantErr: true},
		{name: "129 character string", ref: strings.Repeat("a", 129), wantErr: true},
		{name: "digest reference", ref: "sha256:ab", wantErr: true},
		{name: "short hex id", ref: "a1b2c3d4e5f6", wantErr: false},
		{name: "name with underscore, dash, dot", ref: "my_app-1.2", wantErr: false},
		{name: "128 character string", ref: strings.Repeat("a", 128), wantErr: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			// Arrange
			ref := tt.ref

			// Act
			err := ValidateRef(ref)

			// Assert
			if tt.wantErr {
				if !errors.Is(err, ErrInvalidRef) {
					t.Fatalf("ValidateRef(%q) error = %v, want ErrInvalidRef", ref, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("ValidateRef(%q) error = %v, want nil", ref, err)
			}
		})
	}
}
