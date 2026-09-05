// SPDX-License-Identifier: AGPL-3.0-only

package dockerx

import "regexp"

// protectedIDPattern matches the two shapes the Docker Engine ever assigns a
// container ID: a 12-hex short ID or a full 64-hex ID. An entry that matches
// this pattern is treated as a possible ID as well as a name; an entry of any
// other length or character set is a name only.
//
// This is deliberately not "hex characters, any length": a short operator
// entry like "cafe" is a perfectly legal container NAME, and if it were also
// tried as an ID prefix it would protect every container on the host whose ID
// happens to start with those four hex digits — an accidental, host-wide
// match the operator never asked for. Fixing the length to exactly 12 or 64
// removes that failure mode entirely.
var protectedIDPattern = regexp.MustCompile(`^(?:[0-9a-f]{12}|[0-9a-f]{64})$`)

// shortIDLength is the length of the Engine's short-ID form. resolveTarget
// always has the full ID by the time it calls matches, but matches also
// accepts a caller that only knows the short form, mirroring the Engine's own
// name-or-ID resolution.
const shortIDLength = 12

// protectedSet is the operator's DEVMON_PROTECTED_CONTAINERS entries, split
// into names and IDs once at startup and never written again. The zero value
// (no maps allocated) matches nothing: nil maps are safe to read, so a
// &Client{} built without going through New — as every pre-existing test in
// this package does — keeps behaving exactly as before this feature.
type protectedSet struct {
	names map[string]struct{}
	ids   map[string]struct{}
}

// newProtectedSet builds a protectedSet from the operator's raw entries.
// Every entry is a name candidate; an entry that also matches
// protectedIDPattern is an ID candidate too. Uppercase hex never matches
// protectedIDPattern (the pattern is lowercase-only, matching how the Engine
// itself always reports IDs), so an uppercase 12-hex entry is name-only by
// construction — Docker would never ask this agent to compare it against a
// real ID.
func newProtectedSet(entries []string) protectedSet {
	names := make(map[string]struct{}, len(entries))
	ids := make(map[string]struct{}, len(entries))
	for _, e := range entries {
		names[e] = struct{}{}
		if protectedIDPattern.MatchString(e) {
			ids[e] = struct{}{}
		}
	}
	return protectedSet{names: names, ids: ids}
}

// matches reports whether id or any of names is in the protected set.
//
// A name matches when trimContainerName(n) equals a configured entry exactly,
// for any n in names — a container can carry a link alias alongside its real
// name, and only one of them needs to match. An ID matches the configured
// entries either as a full ID or, when id is at least shortIDLength characters
// long, by its first shortIDLength characters — never by any other prefix
// length, for the reason protectedIDPattern's doc comment gives.
//
// A 12-hex entry that happens to also be a real container's name matches
// through both routes for two different containers; that is intentional.
// Over-protecting a container the operator did not mean to name is the safe
// direction for a security boundary, and it mirrors Docker's own CLI, which
// resolves a short reference against names before IDs with the same
// ambiguity.
func (p protectedSet) matches(id string, names ...string) bool {
	for _, n := range names {
		if _, ok := p.names[trimContainerName(n)]; ok {
			return true
		}
	}
	if _, ok := p.ids[id]; ok {
		return true
	}
	if len(id) >= shortIDLength {
		if _, ok := p.ids[id[:shortIDLength]]; ok {
			return true
		}
	}
	return false
}

// empty reports whether the set carries no entries, used only to decide
// whether New logs its one-line startup confirmation.
func (p protectedSet) empty() bool {
	return len(p.names) == 0
}
