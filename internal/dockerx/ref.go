// SPDX-License-Identifier: AGPL-3.0-only

package dockerx

import (
	"fmt"
	"regexp"
)

// refPattern is the boundary check on every container, image, network, or
// volume reference before it reaches the Docker Engine. The moby client
// interpolates the reference directly into the Engine request URL path, so a
// value like "../../info" is not a malformed ID, it is a path traversal
// attempt against the Engine's HTTP API. net/http.ServeMux already refuses a
// "/" inside a "{id}" wildcard and cleans ".." segments, but relying on two
// layers of framework behaviour to keep an attacker-controlled segment out of
// an upstream URL is exactly the kind of assumption that breaks silently on a
// dependency bump. This pattern is validated at the boundary instead, and
// deliberately excludes ":" so digest references (e.g. "sha256:abc...") are
// rejected rather than accepted.
var refPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$`)

// ValidateRef reports whether ref is safe to interpolate into an Engine
// request path. It returns ErrInvalidRef, wrapped with ref for context, when
// ref does not match refPattern.
func ValidateRef(ref string) error {
	if !refPattern.MatchString(ref) {
		return fmt.Errorf("validate ref %q: %w", ref, ErrInvalidRef)
	}
	return nil
}
