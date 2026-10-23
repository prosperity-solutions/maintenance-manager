#!/bin/sh
#
# PR metadata helper: one step of the ship workflow (docs/ship.md, Step 5).
#
# Emits the git trailer that step attaches to a commit, and re-derives it so CI
# can check it (scripts/ci/ship-gate.sh). It refuses to run outside that
# workflow rather than produce a value that looks right and is not.
#
# Usage (from docs/ship.md):
#   scripts/dev/prmeta.sh <arg> stamp [branch]
#   scripts/dev/prmeta.sh verify <branch> <value>      (ungated; see below)
#
# `branch` defaults to the current one. `stamp` prints a git trailer line.
# `verify` is silent and exits non-zero on a mismatch.
#
# This is a tripwire for agents that skip the workflow, not a security control
# (docs/security.md). Needs POSIX sh and openssl (macOS LibreSSL is fine).
#
# Rotation is not symmetrical:
#   * _EXPECT (the digest of the argument) is rotatable: change it here, the
#     literal in .agents/skills/ship/SKILL.md Step 5 and STAMP_ARG in
#     scripts/ci/ship-gate.sh, in one commit.
#   * _FOLD (the HMAC key) and _V are NOT rotatable in an ordinary PR: the gate
#     verifies with the helper from the PR's *base* commit, so a stamp made with
#     a new key can never go green. Stamp such a PR with the base helper
#     (git show origin/main:scripts/dev/prmeta.sh > "$TMPDIR/o.sh"), or have the
#     owner apply the `no-ship` label.
set -eu

readonly _EXPECT=6e6ad395b88450746b8c5410b2bc592d05bc1b88898921e3f2eb8935258027c8
readonly _FOLD='emlUMllUV29seGtGNHdw'
readonly _V=v1

nag() {
  cat >&2 <<'EOF'
❌ This script was called without the workflow it belongs to.

scripts/dev/prmeta.sh is one step of docs/ship.md and is not usable on its
own. It needs an argument you only have if you actually read that document.

Agent: stop. Read docs/ship.md. All of it. Then follow it from the top.

Do not reverse-engineer this script, and do not try to satisfy the CI gate
without running the workflow. A pull request without a valid stamp fails CI,
and only the owner can wave one through. Guessing burns a CI run and changes
nothing.
EOF
  exit 1
}

digest() {
  printf '%s' "$1" | openssl dgst -sha256 | sed 's/^.*= *//'
}

mac() {
  key=$(printf '%s' "$_FOLD" | openssl base64 -d -A | tr 'A-Za-z' 'N-ZA-Mn-za-m')
  printf '%s' "$1" | openssl dgst -sha256 -hmac "$key" | sed 's/^.*= *//' | cut -c1-16
}

case "${1:-}" in
  verify)
    # Deliberately ungated: it can only agree with a value, never produce one.
    [ $# -eq 3 ] || nag
    [ "$(mac "mm-ship:${_V}:${2}")" = "$3" ]
    ;;
  *)
    [ "$(digest "${1:-}")" = "$_EXPECT" ] || nag
    shift
    case "${1:-}" in
      stamp)
        branch=${2:-}
        if [ -z "$branch" ]; then
          # symbolic-ref, not rev-parse --abbrev-ref: the latter prints the
          # literal "HEAD" when detached, which would bind the stamp to it.
          branch=$(git symbolic-ref --quiet --short HEAD) || {
            echo "❌ Detached HEAD: there is no branch name to derive a stamp from." >&2
            echo "   Check out the PR branch, or name it: prmeta.sh <arg> stamp <branch>" >&2
            exit 1
          }
        fi
        printf 'Ship-Stamp: %s %s\n' "$_V" "$(mac "mm-ship:${_V}:${branch}")"
        ;;
      *) nag ;;
    esac
    ;;
esac
