#!/usr/bin/env bash
set -euo pipefail

readonly begin_marker="# BEGIN bifrost"
readonly end_marker="# END bifrost"
readonly host_pattern='(https?://)?bifrost\.riftlabs\.app(:[0-9]+)?'

snippet=${1:-}
existing=${2:-}
if [[ -z $snippet || ! -f $snippet ]]; then
	echo "usage: render-caddyfile.sh SNIPPET [EXISTING]" >&2
	exit 2
fi

awk -v begin="$begin_marker" -v end="$end_marker" -v host_re="$host_pattern" '
function braces(line, opening, closing, value) {
	value = line
	opening = gsub(/\{/, "", value)
	value = line
	closing = gsub(/\}/, "", value)
	return opening - closing
}
function is_host(line) {
	return line ~ "^[[:space:]]*" host_re "[[:space:]]*\\{[[:space:]]*$"
}
BEGIN { managed = 0; skipping = 0; depth = 0 }
{
	if ($0 == begin) {
		if (managed) { print "nested " begin " marker" > "/dev/stderr"; exit 1 }
		managed = 1
		next
	}
	if ($0 == end) {
		if (!managed) { print "unexpected " end " marker" > "/dev/stderr"; exit 1 }
		managed = 0
		next
	}
	if (managed) next
	if (skipping) {
		depth += braces($0)
		if (depth <= 0) skipping = 0
		next
	}
	if ($0 ~ /bifrost\.riftlabs\.app/ && !is_host($0) && $0 !~ /^[[:space:]]*#/) {
		print "cannot safely replace Bifrost host in: " $0 > "/dev/stderr"
		exit 1
	}
	if (is_host($0)) {
		depth = braces($0)
		if (depth <= 0) { print "invalid Bifrost site block" > "/dev/stderr"; exit 1 }
		skipping = 1
		next
	}
	print
}
END {
	if (managed) { print "unclosed " begin " marker" > "/dev/stderr"; exit 1 }
	if (skipping) { print "unclosed Bifrost site block" > "/dev/stderr"; exit 1 }
}
' "${existing:-/dev/null}" | awk '
NF {
	for (i = 0; i < blank_lines; i++) print ""
	print
	blank_lines = 0
	next
}
{ blank_lines++ }
'

if [[ -n $existing && -s $existing ]]; then
	printf '\n'
fi
cat "$snippet"
