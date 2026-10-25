#!/bin/sh
# Download the product's modules and build the named tools from tools/go.mod,
# retrying with back-off, so the release jobs survive a refused first
# connection. Seen on the first release: harden-runner (egress-policy: block)
# answered "connection refused" for proxy.golang.org seconds after it started,
# although the host is allow-listed. Later `scripts/dev/t <tool>` calls then
# run from the module and build caches.
#
# Usage: scripts/ci/prefetch.sh <tool import path>...
set -eu

[ $# -gt 0 ] || { echo "usage: scripts/ci/prefetch.sh <tool import path>..." >&2; exit 2; }

# The network part gets a deadline: a connection that hangs instead of
# failing (seen on a release: "Fetch modules and tools" sat silent for 20
# minutes, with the job's timeout as the only limit) becomes a failed attempt
# that is retried. Compiling runs offline with no deadline, so a slow cold
# build is never cut off. timeout -k works in GNU and BSD timeout; where
# there is no timeout command, the commands run without one.
DEADLINE=${PREFETCH_DEADLINE:-120}
limit() {
  if ! command -v timeout >/dev/null 2>&1; then
    "$@"
    return
  fi
  rc=0
  timeout -k 10 "$DEADLINE" "$@" || rc=$?
  case $rc in
    124 | 137) echo "prefetch: '$*' stopped after ${DEADLINE}s with no result (a hung download)" >&2 ;;
  esac
  return "$rc"
}

fetch() {
  limit go mod download || return 1
  # Download what the tools import, without compiling them.
  limit go -C tools list -deps "$@" >/dev/null || return 1
  for tool in "$@"; do
    # -o /dev/null: build into the cache only, never a binary in tools/.
    GOPROXY=off go -C tools build -o /dev/null "$tool" || return 1
  done
}

attempt=1
until fetch "$@"; do
  if [ "$attempt" -ge 5 ]; then
    echo "prefetch: still failing after $attempt attempts. A hung download says so above; a refused or blocked host in the log means the job's harden-runner allowed-endpoints needs it (docs/HANDOVER.md D1); any other error (go.sum, compile, Go version) is not a network problem and retrying will not fix it." >&2
    exit 1
  fi
  echo "prefetch: attempt $attempt failed; retrying in $((attempt * 5)) s" >&2
  sleep $((attempt * 5))
  attempt=$((attempt + 1))
done
echo "prefetch: modules and tools ready"
