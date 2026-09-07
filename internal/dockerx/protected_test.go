// SPDX-License-Identifier: AGPL-3.0-only

package dockerx

import "testing"

// TestProtectedSetMatches exercises newProtectedSet/matches together, covering
// the design's name-vs-ID matching rules: name matching is exact after
// trimming the leading "/", ID matching is only ever a 12-hex short ID or a
// full 64-hex ID (never an arbitrary-length prefix), and a 12-hex entry that
// is also a real container name matches by both routes.
func TestProtectedSetMatches(t *testing.T) {
	t.Parallel()

	const fullID = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	const shortID = "aaaaaaaaaaaa" // fullID's 12-hex prefix
	const otherFullID = "aaaaaaaaaaaabbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

	tests := []struct {
		name    string
		entries []string
		id      string
		names   []string
		want    bool
	}{
		{
			name:    "zero value matches nothing",
			entries: nil,
			id:      fullID,
			names:   []string{"/anything"},
			want:    false,
		},
		{
			name:    "name entry matches a slash-prefixed name",
			entries: []string{"proxy"},
			id:      "irrelevant0000000000",
			names:   []string{"/proxy"},
			want:    true,
		},
		{
			name:    "a different name does not match",
			entries: []string{"proxy"},
			id:      "irrelevant0000000000",
			names:   []string{"/database"},
			want:    false,
		},
		{
			name:    "12-hex entry matches an ID prefix",
			entries: []string{shortID},
			id:      fullID,
			names:   []string{"/unrelated-name"},
			want:    true,
		},
		{
			name:    "12-hex entry also matches a container named that string",
			entries: []string{shortID},
			id:      "totally-different-id-0000000000000000000000000000000000000000",
			names:   []string{"/" + shortID},
			want:    true,
		},
		{
			name:    "64-hex entry is exact only, not a prefix match for another ID",
			entries: []string{fullID},
			id:      otherFullID,
			names:   []string{"/unrelated-name"},
			want:    false,
		},
		{
			name:    "64-hex entry matches the exact full ID",
			entries: []string{fullID},
			id:      fullID,
			names:   []string{"/unrelated-name"},
			want:    true,
		},
		{
			name:    "11-char hex is name-only, does not match an ID prefix",
			entries: []string{shortID[:11]},
			id:      fullID,
			names:   []string{"/unrelated-name"},
			want:    false,
		},
		{
			name:    "13-char hex is name-only, does not match an ID prefix",
			entries: []string{shortID + "a"},
			id:      fullID,
			names:   []string{"/unrelated-name"},
			want:    false,
		},
		{
			name:    "uppercase 12-hex entry is name-only",
			entries: []string{"AAAAAAAAAAAA"},
			id:      fullID,
			names:   []string{"/unrelated-name"},
			want:    false,
		},
		{
			name:    "uppercase 12-hex entry still matches an equally-cased name",
			entries: []string{"AAAAAAAAAAAA"},
			id:      "irrelevant0000000000",
			names:   []string{"/AAAAAAAAAAAA"},
			want:    true,
		},
		{
			name:    "a Names slice with a link alias plus the real name matches on the real name",
			entries: []string{"proxy"},
			id:      "irrelevant0000000000",
			names:   []string{"/linker/alias", "/proxy"},
			want:    true,
		},
		{
			name:    "an ID shorter than 12 chars never panics and does not match",
			entries: []string{shortID},
			id:      "abc",
			names:   []string{"/unrelated-name"},
			want:    false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			// Arrange
			p := newProtectedSet(tt.entries)

			// Act
			got := p.matches(tt.id, tt.names...)

			// Assert
			if got != tt.want {
				t.Errorf("matches(%q, %v) = %v, want %v", tt.id, tt.names, got, tt.want)
			}
		})
	}
}

// TestProtectedSetZeroValueMatchesNothing covers the literal zero value (not
// one built by newProtectedSet(nil)), since every existing test that builds
// &Client{} directly relies on this.
func TestProtectedSetZeroValueMatchesNothing(t *testing.T) {
	t.Parallel()

	var p protectedSet

	if p.matches("anything", "/anything") {
		t.Error("zero value protectedSet matched, want it to match nothing")
	}
}

// TestProtectedSetEmpty covers the empty helper used for the startup log
// decision: nil entries are empty, and even one entry makes it non-empty.
func TestProtectedSetEmpty(t *testing.T) {
	t.Parallel()

	if !newProtectedSet(nil).empty() {
		t.Error("newProtectedSet(nil).empty() = false, want true")
	}
	if newProtectedSet([]string{"proxy"}).empty() {
		t.Error("newProtectedSet([]string{\"proxy\"}).empty() = true, want false")
	}
}
