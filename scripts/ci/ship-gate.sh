#!/bin/sh
#
# CI gate: a pull request's head commit must carry a valid `Ship-Stamp:` trailer
# proving it came through the ship workflow (docs/ship.md). Run by the `ship`
# job in .github/workflows/ci.yml. `--selftest` runs the fixtures at the bottom
# and needs no environment beyond git and openssl.
#
# Outcomes: valid, missing, invalid, exempt — plus "malfunction", which blames
# the gate, never the author.
#
# Load-bearing decisions (ported from outfit, where each one is scar tissue):
#   * The stamp must be on the HEAD commit. Accepting any commit in the range
#     let a reused branch inherit a stamp after a squash merge.
#   * The helper comes from the BASE commit, so a PR cannot ship a neutered
#     helper and certify itself. There is no bootstrap fallback: a helper
#     missing at base is a malfunction.
#   * Exemptions: the PR *author* dependabot[bot] (from the event payload, not
#     github.actor), and the `no-ship` label, read live with gh because a job
#     re-run reuses the old event payload. Waive = add the label, re-run.
#
# This is a tripwire for agents that skip the workflow, not a security control
# (docs/security.md).
#
# Environment (gate mode):
#   HEAD, BASE    head and base commit SHAs
#   BRANCH        the PR's head ref
#   AUTHOR        the PR author's login
#   PR_NUMBER     the PR number; labels are fetched with gh (GH_TOKEN, GH_REPO)
#   LABELS        newline-separated label names; used instead of gh when
#                 PR_NUMBER is unset (selftest only)
set -eu

HELPER=scripts/dev/prmeta.sh
WORKFLOW=.github/workflows/ci.yml

malfunction() {
  echo "❌ The ship gate could not run: $1" >&2
  echo "   This is a fault in the gate or the checkout, NOT a problem with your" >&2
  echo "   pull request, and re-stamping will not fix it. Read scripts/ci/ship-gate.sh." >&2
  exit 1
}

gate() {
  : "${HEAD:?HEAD (head sha) is required}"
  : "${BRANCH:?BRANCH (head ref) is required}"
  : "${BASE:?BASE (base sha) is required}"
  author=${AUTHOR:-}

  if [ "$author" = 'dependabot[bot]' ]; then
    echo "✅ exempt: $author is an automation account, no ship stamp required."
    return 0
  fi

  if [ -n "${PR_NUMBER:-}" ]; then
    labels=$(gh pr view "$PR_NUMBER" --json labels --jq '.labels[].name') ||
      malfunction "could not read the labels of PR #$PR_NUMBER with gh"
  else
    labels=${LABELS:-}
  fi
  if printf '%s\n' "$labels" | grep -qx 'no-ship'; then
    echo "✅ exempt: the 'no-ship' label is present; the owner waived this PR."
    return 0
  fi

  git cat-file -e "$BASE:$HELPER" 2>/dev/null ||
    malfunction "$HELPER does not exist at the base commit ($BASE)"
  work=$(mktemp -d)
  git show "$BASE:$HELPER" >"$work/helper.sh" || {
    rm -rf "$work"
    malfunction "could not read $HELPER from the base commit"
  }

  values=$(git log --format='%(trailers:key=Ship-Stamp,valueonly)' -1 "$HEAD" 2>/dev/null) || {
    rm -rf "$work"
    malfunction "could not read the head commit $HEAD"
  }
  found="" ok=""
  while IFS= read -r v; do
    [ -n "$v" ] || continue
    found=yes
    # Split on whitespace with globbing off: the trailer is PR-controlled text.
    set -f
    # shellcheck disable=SC2086
    set -- $v
    set +f
    if [ "${1:-}" = v1 ] && [ $# -eq 2 ] && sh "$work/helper.sh" verify "$BRANCH" "$2"; then
      ok=yes
      break
    fi
  done <<EOF
$values
EOF
  rm -rf "$work"

  if [ -n "$ok" ]; then
    echo "✅ valid: the head commit carries a ship stamp for '$BRANCH'."
    return 0
  fi
  if [ -n "$found" ]; then
    echo "❌ invalid: the head commit has a Ship-Stamp trailer, but it does not match branch '$BRANCH'." >&2
    echo "   The value derives from the branch name, so a renamed branch invalidates it." >&2
    echo "   Re-stamp the head commit as docs/ship.md Step 5 says, and push." >&2
    return 1
  fi
  echo "❌ missing: the head commit has no Ship-Stamp trailer." >&2
  echo "   Every change goes through the ship workflow. Read docs/ship.md in full and follow it." >&2
  return 1
}

# ---------------------------------------------------------------------------
# --selftest

STAMP_ARG=tidewater-0613
FAILURES=0

check() {
  if [ "$3" = "$2" ]; then
    printf '  ok    %-44s %s\n' "$1" "$3"
  else
    printf '  FAIL  %-44s want %s, got %s\n' "$1" "$2" "$3"
    FAILURES=$((FAILURES + 1))
  fi
}

new_repo() { # $1 dir, $2 helper at base (yes/no)
  mkdir -p "$1/scripts/dev"
  git -C "$1" init -q -b main
  git -C "$1" config user.email t@example.com
  git -C "$1" config user.name Test
  git -C "$1" config commit.gpgsign false
  [ "$2" = yes ] && cp "$REAL_ROOT/$HELPER" "$1/$HELPER"
  echo base >"$1/f.txt"
  git -C "$1" add -A && git -C "$1" commit -qm "chore: base"
  git -C "$1" tag base-ref
}

add_commit() { # $1 dir, $2 trailer block ("" = none)
  cp "$REAL_ROOT/$HELPER" "$1/$HELPER"
  date +%s >>"$1/f.txt"
  if [ -n "$2" ]; then
    git -C "$1" add -A && git -C "$1" commit -qm "feat: a change

$2"
  else
    git -C "$1" add -A && git -C "$1" commit -qm "feat: a change"
  fi
}

stamp_for() { sh "$REAL_ROOT/$HELPER" "$STAMP_ARG" stamp "$1"; }

run_gate() { # $1 dir, $2 branch, $3 author, $4 labels
  if (cd "$1" && BASE=$(git rev-parse base-ref) HEAD=$(git rev-parse HEAD) BRANCH=$2 \
    AUTHOR=${3:-someone} LABELS=${4:-} PR_NUMBER='' gate >/dev/null 2>&1); then
    echo accept
  else
    echo reject
  fi
}

selftest() {
  REAL_ROOT=$(cd "$(dirname "$0")/../.." && pwd)
  tmp=$(mktemp -d)
  trap 'rm -rf "$tmp"' EXIT
  echo "ship-gate selftest:"

  d=$tmp/ok
  new_repo "$d" yes
  add_commit "$d" "$(stamp_for feature-x)"
  check "valid: stamp on the head commit" accept "$(run_gate "$d" feature-x)"
  check "invalid: same stamp, other branch" reject "$(run_gate "$d" feature-y)"

  d=$tmp/none
  new_repo "$d" yes
  add_commit "$d" ""
  check "missing: no stamp at all" reject "$(run_gate "$d" feature-x)"
  check "exempt: dependabot[bot] author" accept "$(run_gate "$d" feature-x 'dependabot[bot]')"
  check "not exempt: bare 'dependabot'" reject "$(run_gate "$d" feature-x dependabot)"
  check "exempt: no-ship label" accept "$(run_gate "$d" feature-x someone "$(printf 'bug\nno-ship')")"
  check "not exempt: label containing the token" reject "$(run_gate "$d" feature-x someone not-no-ship)"

  d=$tmp/inh
  new_repo "$d" yes
  add_commit "$d" "$(stamp_for feature-x)"
  add_commit "$d" ""
  check "missing: inherited stamp, not on head" reject "$(run_gate "$d" feature-x)"

  d=$tmp/noh
  new_repo "$d" no
  add_commit "$d" "$(stamp_for feature-x)"
  check "malfunction: helper absent at base" reject "$(run_gate "$d" feature-x)"

  d=$tmp/two
  new_repo "$d" yes
  add_commit "$d" "$(printf 'Ship-Stamp: v1 deadbeefdeadbeef\n%s' "$(stamp_for feature-x)")"
  check "valid: two trailers, one valid" accept "$(run_gate "$d" feature-x)"

  d=$tmp/v2
  new_repo "$d" yes
  add_commit "$d" "Ship-Stamp: v2 deadbeefdeadbeef"
  check "invalid: unknown version prefix" reject "$(run_gate "$d" feature-x)"

  if (sh "$REAL_ROOT/$HELPER" wrong-arg stamp feature-x >/dev/null 2>&1); then
    check "helper refuses a wrong argument" refuse produced
  else
    check "helper refuses a wrong argument" refuse refuse
  fi

  # Against the real repository, not a fixture.
  if [ -f "$REAL_ROOT/$HELPER" ]; then r=present; else r=missing; fi
  check "real repo has the helper" present "$r"
  if grep -q 'scripts/ci/ship-gate.sh' "$REAL_ROOT/$WORKFLOW"; then r=wired; else r=unwired; fi
  check "real ci.yml invokes this gate" wired "$r"

  if [ "$FAILURES" -ne 0 ]; then
    echo "ship-gate selftest: $FAILURES failure(s)" >&2
    exit 1
  fi
  echo "ship-gate selftest: all fixtures behave"
}

case "${1:-}" in
  --selftest) selftest ;;
  *) gate ;;
esac
