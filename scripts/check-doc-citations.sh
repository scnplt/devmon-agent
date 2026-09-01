#!/bin/sh
# Verifies that every citation in docs/*.md still resolves.
#
# Three citation forms are checked:
#   [`path/to/file.go:12-34`](../path/to/file.go)   the line range exists
#   [`README.md`, `## Heading`](../README.md#anchor) the heading exists
#   [`docs/NAME.md`, `## Heading`](NAME.md#anchor)   the heading exists
#   [`install.sh`, `symbol`](../install.sh)          the symbol exists
#
# THREAT-MODEL.md promises every claim traces back to a line of code; this is
# the gate that keeps that promise true as the sources move.
set -eu

cd "$(dirname "$0")/.."

# Each loop below runs in a pipeline subshell, so failures are recorded in a
# file rather than in a variable.
failures=$(mktemp)
trap 'rm -f "$failures"' EXIT

fail() {
	printf 'broken citation: %s: %s\n' "$1" "$2" >&2
	printf 'x' >> "$failures"
}

# Form 1: `file.ext:N` or `file.ext:N-M`
# shellcheck disable=SC2016  # the backticks are markdown, not command substitution
grep -Eon '`[A-Za-z0-9_./-]+\.(go|sh|yaml|yml):[0-9]+(-[0-9]+)?`' docs/*.md |
	while IFS= read -r hit; do
		doc=${hit%%:*}
		rest=${hit#*:}
		lineno=${rest%%:*}
		ref=$(printf '%s' "${rest#*:}" | tr -d '`')
		file=${ref%%:*}
		range=${ref#*:}
		start=${range%%-*}
		end=${range#*-}
		if [ ! -f "$file" ]; then
			fail "$doc:$lineno" "no such file: $file"
			continue
		fi
		total=$(wc -l < "$file")
		if [ "$start" -lt 1 ] || [ "$end" -gt "$total" ]; then
			fail "$doc:$lineno" "$file has $total lines, citation points at $range"
		fi
	done

# Form 2: heading anchors into README.md or into a sibling document under
# docs/. A citation is either (../README.md#anchor) or (NAME.md#anchor), where
# NAME.md lives next to the citing document.
grep -on '(\(\.\./README\|[A-Za-z0-9_-]*\)\.md#[a-z0-9-]*)' docs/*.md |
	while IFS= read -r hit; do
		doc=${hit%%:*}
		rest=${hit#*:}
		lineno=${rest%%:*}
		ref=${rest#*:}
		ref=${ref#\(}
		ref=${ref%\)}
		target=${ref%%#*}
		anchor=${ref#*#}
		case "$target" in
		../README.md) file=README.md ;;
		*) file=docs/$target ;;
		esac
		if [ ! -f "$file" ]; then
			fail "$doc:$lineno" "no such file: $file"
			continue
		fi
		found=$(grep -E '^#{1,6} ' "$file" |
			sed -E 's/^#+ //; s/[^A-Za-z0-9 -]//g' |
			tr '[:upper:] ' '[:lower:]-' |
			grep -cx -- "$anchor" || true)
		if [ "$found" -eq 0 ]; then
			fail "$doc:$lineno" "$file has no heading '#$anchor'"
		fi
	done

# Form 3: named symbols in install.sh
# shellcheck disable=SC2016  # the backticks are markdown, not command substitution
grep -on '`install\.sh`, `[A-Za-z0-9_ /]*`' docs/*.md |
	while IFS= read -r hit; do
		doc=${hit%%:*}
		rest=${hit#*:}
		lineno=${rest%%:*}
		syms=$(printf '%s' "${rest#*, }" | tr -d '`')
		for sym in $(printf '%s' "$syms" | tr '/' ' '); do
			grep -q -- "$sym" install.sh ||
				fail "$doc:$lineno" "install.sh has no '$sym'"
		done
	done

if [ -s "$failures" ]; then
	exit 1
fi
printf 'all doc citations resolve\n'
