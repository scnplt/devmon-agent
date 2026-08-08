package dockerx

import (
	"errors"
	"fmt"

	cerrdefs "github.com/containerd/errdefs"
)

// ErrNotFound is returned when the Engine reports no such object. It is the
// only Engine failure the API distinguishes: the caller is an authenticated
// device, so telling it "that container is gone" is information it is
// entitled to, while every other Engine fault is an upstream problem the
// client can only retry.
var ErrNotFound = errors.New("docker object not found")

// ErrInvalidRef is returned by ValidateRef. It never reaches the Engine.
var ErrInvalidRef = errors.New("invalid docker object reference")

// classify maps a raw Engine error onto the package's sentinel errors,
// wrapping the result with op so a caller can name the failing operation
// without inspecting the underlying error type.
func classify(op string, err error) error {
	if err == nil {
		return nil
	}
	if cerrdefs.IsNotFound(err) {
		return fmt.Errorf("%s: %w", op, ErrNotFound)
	}
	return fmt.Errorf("%s: %w", op, err)
}
