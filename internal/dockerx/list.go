// SPDX-License-Identifier: AGPL-3.0-only

package dockerx

import "time"

// maxListItems caps every list response. A host with thousands of images would
// otherwise produce a body no phone should receive. The cap is server-side and
// not client-adjustable, consistent with the agent's configuration boundary.
const maxListItems = 500

// truncate caps items at maxListItems and reports whether truncation occurred.
// Shared by every list method so the cap and its boundary behavior (exactly
// maxListItems items is not truncated, one more is) live in a single place.
func truncate[T any](items []T) ([]T, bool) {
	if len(items) > maxListItems {
		return items[:maxListItems], true
	}
	return items, false
}

// defaultLabels returns m unchanged, or a non-nil empty map when m is nil. A
// nil map marshals to JSON null rather than {}, which would force the client
// to handle both a map and a null for the same field.
func defaultLabels(m map[string]string) map[string]string {
	if m == nil {
		return map[string]string{}
	}
	return m
}

// unixToRFC3339 converts Unix seconds to an RFC3339 UTC string, treating 0 as
// absent rather than the epoch: Docker reports Created as 0 when the engine
// has no creation time to give, and formatting that as
// "1970-01-01T00:00:00Z" would present a real-looking but false date to the
// client.
func unixToRFC3339(sec int64) string {
	if sec == 0 {
		return ""
	}
	return time.Unix(sec, 0).UTC().Format(time.RFC3339)
}
