#!/bin/sh
# Fails when text that becomes public holds a real ID. AGENTS.md, "Public text
# hygiene".
#
# An ID copied from a real deployment (a UUID: a service, environment or
# cluster ID) names what it came from. Examples in this repo use the all-zero
# UUID. GitHub's own image links (user-attachments/assets/<uuid>) are not
# counted. This is a pattern, not a list of words: a list of internal names
# would publish them.
#
# Usage: public-ids.sh [file...]   (no file: read stdin; directories and binary
# files are skipped)
# Exit 0: clean. Exit 1: prints each hit as file:line (never the ID itself, so
# it is not copied on from here). Exit 2: a file could not be read.
set -eu
export LC_ALL=C

# PATTERN is the one pattern; test/workflows keeps ci.yml's copy equal to it.
PATTERN='[0-9A-Fa-f]{8}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{12}'

scan() { # scan <label>: stdin in, "<label>:<line>" out for each real ID
  sed -E 's#user-attachments/assets/[0-9A-Fa-f-]{36}##g' |
    grep -Eno "$PATTERN" | grep -Ev ':0{8}-0{4}-0{4}-0{4}-0{12}$' |
    awk -F: -v l="$1" '{ print l ":" $1 }' | uniq || true
}

[ $# -gt 0 ] || set -- -
hits=$(for f in "$@"; do
  if [ "$f" = - ]; then
    scan "(standard input)"
    continue
  fi
  [ -f "$f" ] || continue
  [ -r "$f" ] || { echo "public-ids: cannot read $f" >&2; echo "@unreadable"; continue; }
  grep -Iq '' "$f" || continue # binary or empty
  # shellcheck disable=SC2094 # $f is only a label in scan's output, not written
  scan "$f" <"$f"
done)
case $hits in *@unreadable*) exit 2 ;; esac
[ -z "$hits" ] && exit 0
printf '%s\n' "$hits"
echo "public-ids: a real ID (UUID) would go public. Use 00000000-0000-0000-0000-000000000000 in examples, and no names from a real deployment (AGENTS.md, Public text hygiene)." >&2
exit 1
